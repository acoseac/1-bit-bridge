// Package api implements the HTTP/2 handlers for the v1 wire protocol (see
// PROTOCOL.md at the repo root).
//
// v1 endpoints exposed by this package:
//
//	GET /v1/health              — no auth, liveness / pairing probe
//	GET /v1/list                — directory listing (authed)
//	GET /v1/stat                — file metadata (authed)
//	GET /v1/read                — byte-range read; Range header required (authed)
//	GET /v1/download            — full-file streaming download (authed)
//	GET /v1/manifest            — library track manifest (authed)
//	GET /v1/artwork/{mbid}      — album artwork blob (authed)
//	GET /v1/artist-image/{mbid} — artist portrait by MusicBrainz ID (authed)
//
// Pairing itself is handled by the admin console (see internal/admin),
// not by a /v1 endpoint — the iOS client posts the bearer token and
// pinned fingerprint to the admin host once, then talks to /v1 only.
//
// Every response carries the X-Bridge-Protocol header. Authenticated
// endpoints run through the authed() middleware, which requires
// Authorization: Bearer <token> and validates via the auth.Store.
package api

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/admin"
	"github.com/acoseac/1-bit-bridge/internal/advertise"
	"github.com/acoseac/1-bit-bridge/internal/auth"
	"github.com/acoseac/1-bit-bridge/internal/config"
	bridgefs "github.com/acoseac/1-bit-bridge/internal/fs"
	"github.com/acoseac/1-bit-bridge/internal/logging"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/pairing"
	"github.com/acoseac/1-bit-bridge/internal/version"
)

// ManifestPage is the typed JSON-serializable shape returned by
// `BuildManifestPage`. Aliased to `manifest.Manifest` so the wire
// shape stays defined in exactly one place; the alias keeps the
// `api` package's interface contract type-safe without forcing
// callers to learn about `internal/manifest`.
//
// The `internal/api → internal/manifest` import is unidirectional:
// manifest never imports api (the existing `VariantLookup` /
// `VariantRecord` mirror pattern at api.go:105 / provider.go:26 is
// what defends the OTHER direction; `Manifest` doesn't need a
// mirror because nothing in manifest reaches into api for it).
type ManifestPage = manifest.Manifest

var logger = logging.Component("api")

// headerContentEncoding is the canonical HTTP header name written and
// deleted across the manifest gzip negotiation path. Extracted to
// satisfy SonarCloud go:S1192; the value is fixed by RFC 7231 §3.1.2.2.
const headerContentEncoding = "Content-Encoding"

// manifestGzipPool reuses gzip.Writer instances across concurrent
// /v1/manifest requests. `gzip.NewWriter` allocates a ~256 KB
// LZ77 window buffer per call; under high concurrency (many iOS
// clients hitting album-sync at the same time) the per-request
// allocation cost is measurable in heap pressure AND GC pauses.
// Pool keeps the buffers warm.
//
// Lifecycle contract (see manifestHandler for the live usage):
//
//  1. Get → returns either a fresh writer (cold path, first request)
//     or a previously-Put one (warm path).
//  2. Reset(w) → re-binds the writer to the new ResponseWriter. MUST
//     come before any Write call; otherwise the gzip framing would
//     stream into whatever ResponseWriter the previous user had.
//  3. Write through the writer.
//  4. Close → writes the gzip trailer (CRC + ISIZE). MUST run
//     before Put or the response is truncated mid-trailer.
//  5. Put → returns the writer to the pool.
//
// `Close()` may fail when the client has disconnected mid-response
// (broken pipe writing the trailer). The writer's INTERNAL state
// is still clean — the next `Reset(w2)` re-binds cleanly to a
// fresh writer. Log the close error at debug per the same
// convention as writeJSON's encoder discard. The defer in
// manifestHandler runs Put unconditionally so a panic mid-stream
// (rare; the inner WriteManifest catches its own SQLite errors)
// still returns the buffer.
var manifestGzipPool = sync.Pool{
	New: func() any {
		// io.Discard as a sentinel target — every consumer calls
		// Reset(realWriter) immediately after Get, so the
		// io.Discard binding is never actually written to. NewWriter
		// requires a non-nil io.Writer at construction time.
		return gzip.NewWriter(io.Discard)
	},
}

// Server owns the http.Handler and the per-request state it needs.
type Server struct {
	cfgHolder              *config.RuntimeConfig
	store                  *auth.Store
	resolver               *bridgefs.Resolver
	manifest               ManifestProvider
	artworkDirs            ArtworkDirProvider
	mbidProbe              MBIDProbe
	updater                UpdaterStatus
	sessions               SessionTracker
	pairing                *pairing.Store
	pairingRateLimiter     *pairingRateLimiter
	certNotAfter           time.Time                    // zero when not wired (test harnesses)
	leCertNotAfterProvider func() time.Time             // public-mode autocert; nil unless WithLECertExpiry wired
	variantStore           VariantStore                 // nil unless WithUpscale(true, vs) called
	variantDeleter         VariantDeleter               // nil unless WithVariantDeleter wired (variant-lifecycle delete)
	inflightDropper        InflightDropper              // nil unless WithInflightDropper wired (transcode pool dedup)
	upscaleEnabled         bool                         // mirrors cfg.Upscale.Enabled (and sox-probe outcome)
	carPlayOptimizeEnabled bool                         // gated AND-wise on upscaleEnabled by the wiring layer
	dlnaEnabled            bool                         // mirrors cfg.DLNA.Enabled AND shouldEnableDLNA(...) — opt-in LAN-only DLNA MediaServer
	rendererDiscovery      RendererDiscoverySnapshotter // nil unless WithRendererDiscovery wired — opt-in SSDP MediaRenderer cache for /v1/renderers
	upscaleEnqueuer        UpscaleEnqueuer              // nil unless WithUpscaleEnqueuer wired (Phase 2.5)
	upscaleStatsProvider   UpscaleStatsProvider         // nil unless WithUpscaleStats wired (v1.2 management UI)
	batchCoordinator       BatchCoordinator             // nil unless WithBatchCoordinator wired (v1.3 operator-driven upscale)
	eventBroker            *eventBroker                 // nil disables /v1/events (back-compat for test harnesses)
	manifestRateLimiter    *manifestRateLimiter         // per-token-ID token-bucket for /v1/manifest
	reachability           *reachabilityCache           // per-root probe TTL cache used by /v1/list, /v1/stat, /v1/health
	fingerprint            string
	startedAt              time.Time

	// tailscaleStatus is the embedded-tsnet status provider used by
	// `reachableEndpoints` to advertise the bridge's `*.ts.net` URL +
	// tailnet IPs in tsnet mode. Nil unless `WithTailscaleStatus` or
	// `SetTailscaleStatus` is wired by cmd/bridge. Stays nil in unit
	// tests that don't exercise the advertising path.
	//
	// 5s TTL cache below mirrors the `reachabilityCache` pattern —
	// `/v1/health` is unauthenticated, so the underlying tsnet
	// LocalClient IPC must not be invoked once per request under a
	// LAN-flood scenario. The lock serializes concurrent callers
	// onto a single in-flight Status() probe; subsequent callers
	// observe the cached value.
	tailscaleStatus        admin.TailscaleProvider
	tailscaleStatusMu      sync.Mutex
	tailscaleStatusCache   admin.TailscaleStatus
	tailscaleStatusFetched time.Time
}

// ConfigHolder exposes the API server's live runtime-config holder so
// cmd wiring can share one source of truth with admin writers.
func (s *Server) ConfigHolder() *config.RuntimeConfig { return s.cfgHolder }

// ErrUpscaleQueueFull is the typed sentinel UpscaleEnqueuer
// returns when the underlying worker pool can't accept another
// job. The HTTP handler maps it to a 503 `queue_full` response.
// Defined in the api package so the handler can `errors.Is`
// without importing internal/transcode (which would invert the
// dependency direction — api defines abstractions, transcode is
// an implementation detail consumed via the adapter at
// cmd/bridge wiring).
var ErrUpscaleQueueFull = errors.New("upscale queue is full")

// VariantStore is the optional interface the `?variant=<id>` branch
// of /v1/download uses to look up a variant's cached metadata.
// Nil-safe — when `s.variantStore` is nil the download handler
// returns 404 `variant_not_found` for any request that carries the
// parameter (matches the "feature unavailable" behaviour iOS
// already handles for pre-v1.2 bridges).
//
// **Freshness check happens in the api**, not here. The api has
// the canonical `bridgefs.Resolver` (path validation + traversal
// guard already exercised on every other handler), and uses the
// `os.FileInfo` it already stat'd for the source file in
// serveFile. This avoids a duplicate path-resolution code path in
// the manifest package — which Gemini bot review on PR #108
// identified as broken in single-root mode (the manifest's
// hand-rolled basename-stripping assumed multi-root layout) AND
// flagged by CodeQL as "uncontrolled data used in path
// expression". Both go away when the api owns the source-side
// stat call and only asks the variant store for "do you have
// this row, and what's its recorded provenance".
//
// `internal/manifest.Provider` satisfies this in production via
// a thin LookupVariant wrapper around the SQLite row read.
type VariantStore interface {
	LookupVariant(ctx context.Context, sourcePath, variantID string) (*VariantRecord, error)
}

// VariantRecord is the minimum metadata the api needs to (a) decide
// freshness and (b) serve the sidecar bytes. Mirrors the on-disk
// columns the manifest package writes. Pointer return from
// LookupVariant lets `nil` distinguish "no such row" from "row
// exists but freshness fails" — caller can still surface a
// targeted 404 vs 410 from the same return shape.
//
// `SourcePath` and `VariantID` are the CANONICAL row values
// (case-preserved, matching the SwiftData `Track.path` byte-for-
// byte). LookupVariant resolves case-insensitively (iOS sends
// `share.normalize`d paths) but the returned record carries
// the canonical form so the reactive cleanup path in
// `serveVariant` can delete the right row AND emit an SSE
// `upscale.deleted` event whose paths match `Track.path` for
// iOS-side reverse-index resolution. CodeRabbit Major on PR
// #209 caught this: without the canonical values, a case-folded
// request would silently no-op DeleteVariant while the SSE
// fired with the wrong-case path.
type VariantRecord struct {
	SourcePath    string
	VariantID     string
	SidecarPath   string
	SourceMTimeNS int64
	SourceSize    int64
}

// SessionTracker is the optional interface serveFile uses to record
// active file-serving requests so the updater can refuse to swap-
// and-restart while a download is in flight. Nil-safe — when
// `s.sessions` is nil serveFile skips the bookkeeping (test
// harnesses, pre-Phase-B bridges).
//
// `internal/updater.Tracker` satisfies this interface in production.
type SessionTracker interface {
	Begin()
	End()
}

// ManifestProvider is the interface /v1/manifest and /v1/health use to
// read the indexed library state. internal/manifest implements it via
// the Scanner + Store pair; tests can pass a small stub.
type ManifestProvider interface {
	// WriteManifest streams the legacy non-paginated manifest as JSON
	// straight to w. Bounding peak memory is mandatory: the prior
	// in-memory builder OOM-killed Pi-class hosts on 50k-track
	// libraries. Mid-stream errors are returned but unrecoverable on
	// the wire (headers and prefix already sent); the handler logs and
	// the truncated body fails iOS-side decode, which retries.
	//
	// `ctx` is checked inside the per-row stream loop so a client
	// disconnect mid-response (slow network, iOS app backgrounded)
	// terminates the SQLite scan instead of running it to EOF holding
	// connection resources.
	WriteManifest(ctx context.Context, w io.Writer, since time.Time) error
	// BuildManifestPage is the v1.1 paginated-manifest variant used
	// when the client asks for `?limit=`. `cursor=""` requests the
	// first page. Callers iterate until the returned page's
	// `NextCursor` is nil. Returns a fully-materialised value because
	// per-page output is bounded by the page-size cap (5000) and the
	// JSON writer can buffer the whole page without OOM risk.
	BuildManifestPage(ctx context.Context, cursor string, limit int) (*ManifestPage, error)
	IsScanning() bool
	LastFullScan() time.Time
	TracksIndexed(ctx context.Context) int
	// PendingDeletions returns the total count of rows across tracks
	// and folders with missing_count > 0 — surfaced on ScanState. May
	// return 0 for an unwired or pre-v5 store; never errors today.
	PendingDeletions(ctx context.Context) int64
}

// MBIDProbe is an optional interface the artwork + artist-image
// handlers use to distinguish "MBID known to the server but not
// cached yet" (return 202 + Retry-After so iOS retries) from
// "genuinely unknown MBID" (return 404 so iOS treats as terminal).
//
// Nil-safe — when `s.mbidProbe` is nil the handlers fall back to the
// pre-v1.1 behaviour of 404-on-miss. `internal/manifest.Provider`
// satisfies this interface in production.
type MBIDProbe interface {
	HasTrackWithArtworkMBID(ctx context.Context, mbid string) bool
	HasTrackWithArtistMBID(ctx context.Context, mbid string) bool
}

// UpdaterStatus is the optional interface the /v1/health handler uses
// to populate the additive `latestServerVersion` / `updateAvailable` /
// `updateReleaseNotesURL` / `minClientVersion` fields. Nil-safe — when
// `s.updater` is nil those fields stay omitempty-empty and the wire
// shape matches the pre-Phase-A response exactly.
//
// `internal/updater.Updater` satisfies this interface in production.
// The `MinClientVersion` returned here is read from
// `version.MinClientVersion` so it advertises the floor THIS bridge
// needs from its iOS clients (not the floor a candidate update would
// require — that's a Phase B / C concern).
type UpdaterStatus interface {
	UpdateInfo() UpdateInfo
}

// UpdateInfo is the wire shape /v1/health embeds. Lives in this
// package (not internal/updater) so the iOS-facing fields don't import
// the updater type machinery and tests can hand-craft an UpdaterStatus
// stub.
type UpdateInfo struct {
	LatestVersion    string
	UpdateAvailable  bool
	ReleaseNotesURL  string
	MinClientVersion string
}

// New constructs a Server. fingerprint is the SHA-256 of the TLS cert, used
// for display in /v1/health (iOS pins by this value). mp can be nil during
// early boot / tests — /v1/manifest will return 503 until it's populated.
func New(cfg *config.Config, store *auth.Store, mp ManifestProvider, fingerprint string) *Server {
	holder := config.NewRuntimeConfig(cfg)
	return &Server{
		cfgHolder:           holder,
		store:               store,
		resolver:            bridgefs.New(cfg.LibraryRoots),
		manifest:            mp,
		pairingRateLimiter:  newPairingRateLimiter(),
		manifestRateLimiter: newManifestRateLimiter(cfg.Limits.Manifest.EffectiveRPM(), cfg.Limits.Manifest.EffectiveBurst()),
		reachability:        newReachabilityCache(),
		fingerprint:         fingerprint,
		startedAt:           time.Now().UTC(),
	}
}

// WithArtworkDirs attaches an artwork cache dir provider. Separate from
// New to avoid churning every call site when optional features are added.
func (s *Server) WithArtworkDirs(ad ArtworkDirProvider) *Server {
	s.artworkDirs = ad
	return s
}

// WithMBIDProbe attaches an MBIDProbe so the artwork / artist-image
// handlers can return 202 + Retry-After for known-but-uncached MBIDs
// instead of a flat 404. Optional — omitting it preserves v1.0
// behaviour (404 on every cache miss). `internal/manifest.Provider`
// satisfies the interface in production.
func (s *Server) WithMBIDProbe(p MBIDProbe) *Server {
	s.mbidProbe = p
	return s
}

// WithUpdater attaches an UpdaterStatus so /v1/health can advertise
// the latest available bridge release to iOS clients. Optional —
// omitting it preserves the pre-Phase-A wire shape (the four new
// `omitempty` fields stay absent from the JSON response).
func (s *Server) WithUpdater(u UpdaterStatus) *Server {
	s.updater = u
	return s
}

// WithSessionTracker attaches the inflight-download counter the
// updater consults before swapping the binary and restarting.
// Optional — omitting it disables the gate entirely (tests). The
// counter is incremented around `serveFile` so /v1/read and
// /v1/download both count.
func (s *Server) WithSessionTracker(t SessionTracker) *Server {
	s.sessions = t
	return s
}

// WithUpscale wires the v1.2 PCM-upscaling feature into the
// server. `enabled` mirrors `cfg.Upscale.Enabled` AND the result
// of the cmd/bridge sox-on-PATH startup probe (a true config
// setting whose probe failed lands here as false — graceful
// degradation, the rest of the server keeps running). `vs` may
// be nil when enabled=false; when enabled=true it MUST be the
// VariantStore implementation that knows where the sidecars
// live.
//
// Effect on the wire:
//   - /v1/health reports `upscaleEnabled` matching `enabled`.
//   - /v1/manifest emits per-Track `variants` slices iff
//     `enabled` (cleared in the manifest provider otherwise).
//   - /v1/download honors `?variant=<id>` iff `enabled` AND
//     `vs != nil`; 404 `variant_not_found` otherwise.
func (s *Server) WithUpscale(enabled bool, vs VariantStore) *Server {
	s.upscaleEnabled = enabled
	if enabled {
		s.variantStore = vs
	}
	return s
}

// WithCarPlayOptimize toggles the CarPlay-optimize feature
// advertisement (the `carPlayOptimize` entry in /v1/health.features).
// Caller is responsible for the AND-gate with upscale enablement —
// the wire-emit branch in /v1/health additionally requires
// `s.upscaleEnabled` (optimize shares the SoX pool with upscale and
// has no meaning without it).
func (s *Server) WithCarPlayOptimize(enabled bool) *Server {
	s.carPlayOptimizeEnabled = enabled
	return s
}

// WithDLNA toggles the `dlnaServer` advertisement in
// /v1/health.features. The bridge's actual DLNA MediaServer runs on
// its own parallel http.Server bound LAN-only (and optionally
// tsnet-routed) — wired separately in `cmd/bridge/main.go`. This
// option is the iOS-visible capability flag so the app's
// `OutputPickerSheet` can surface DLNA-capable bridges as a
// renderer-discovery source candidate.
//
// **Public-mode refusal applies upstream**: caller MUST consult
// `dlna.ShouldEnableDLNA(cfg, deploymentMode)` before calling this
// with `enabled = true`. Passing `true` from a public-mode deploy
// would advertise a capability the actual server refuses to bind —
// the upstream gate enforces the safety invariant; this method is
// the bookkeeping for the capability advertisement.
func (s *Server) WithDLNA(enabled bool) *Server {
	s.dlnaEnabled = enabled
	return s
}

// RendererDiscoverySnapshotter is the abstraction the api package
// consumes for the renderer cache backing `GET /v1/renderers`. The
// concrete implementation lives in `internal/dlna/discovery` — this
// shape keeps the api package free of a hard dependency on the
// discovery package (which transitively pulls in net.UDPConn etc.).
//
// Returning a `[]any`-shaped slice would force the handler to do
// runtime type checks; returning a typed `[]discovery.RendererInfo`
// would create the dependency we're avoiding. The third option —
// re-declaring the wire DTO locally — duplicates the contract.
// We accept the duplication risk via a structural contract:
// `internal/dlna/discovery.RendererInfo` MUST stay structurally
// assignable to the local `RendererInfo` type below. A compile-
// time assertion in the wiring layer (`cmd/bridge/dlna_wiring.go`)
// catches any drift.
type RendererDiscoverySnapshotter interface {
	// Snapshot returns the live cache contents as a slice of
	// the api-local RendererInfo shape. Empty slice (NOT nil)
	// when no renderers are cached.
	Snapshot() []RendererInfo
}

// RendererInfo is the api-package-local wire shape for
// `GET /v1/renderers`. Structurally mirrors
// `internal/dlna/discovery.RendererInfo`; the wiring adapter in
// `cmd/bridge/dlna_wiring.go` converts between the two via a
// trivial value copy.
//
// **JSON encoding stability**: field names + tags pinned by
// `renderer_dto_test.go` in the discovery package + the
// `renderers_handler_test.go` in this package. Changes require
// the Mirror-PR convention.
type RendererInfo struct {
	UDN               string    `json:"udn"`
	FriendlyName      string    `json:"friendlyName"`
	Manufacturer      string    `json:"manufacturer,omitempty"`
	ModelDescription  string    `json:"modelDescription,omitempty"`
	ModelName         string    `json:"modelName,omitempty"`
	ControlURL        string    `json:"controlURL"`
	EventURL          string    `json:"eventURL,omitempty"`
	SinkProtocolInfos []string  `json:"sinkProtocolInfos,omitempty"`
	LastSeenAt        time.Time `json:"lastSeenAt"`
}

// RenderersResponse is the top-level shape of `GET /v1/renderers`.
type RenderersResponse struct {
	Renderers []RendererInfo `json:"renderers"`
}

// WithRendererDiscovery wires the SSDP MediaRenderer cache that
// backs `GET /v1/renderers` AND the `rendererDiscovery` flag in
// `/v1/health.features`. Passing nil leaves the endpoint
// unregistered (returns 404) and the flag absent.
//
// **Capability flag gating**: presence of the flag in
// `/v1/health.features` is gated AND-wise on `s.dlnaEnabled`
// (sibling DLNA MediaServer must be running) so iOS clients get
// a consistent capability picture — running renderer discovery
// without the MediaServer is a coherent operator choice but
// outside the v1 supported surface.
func (s *Server) WithRendererDiscovery(snap RendererDiscoverySnapshotter) *Server {
	s.rendererDiscovery = snap
	return s
}

// WithUpscaleEnqueuer attaches the long-lived transcode worker
// pool's job-submit interface so the v1.2 `POST /v1/upscale`
// endpoint can hand off track / folder requests. Optional —
// when nil the endpoint returns `503 upscale_disabled`.
//
// Wired in cmd/bridge serve startup IFF `cfg.Upscale.Enabled &&
// sox-on-PATH probe passed`. The adapter at the wiring point
// translates `transcode.ErrQueueFull` into the api package's
// `ErrUpscaleQueueFull` sentinel so the handler can map cleanly
// to the wire response.
func (s *Server) WithUpscaleEnqueuer(e UpscaleEnqueuer) *Server {
	s.upscaleEnqueuer = e
	return s
}

// StartPairingRateLimitGC kicks off the per-IP rate-limiter's
// background sweep on a hidden goroutine. Called once from
// `cmd/bridge` after `apiSrv := api.New(...).WithPairing(...)`. The
// returned func, called on shutdown, signals the goroutine to exit
// cleanly. Tests don't need to call this — the GC interval is hours,
// so leaving it idle is harmless, and table-driven tests construct
// fresh limiters via `New` anyway.
func (s *Server) StartPairingRateLimitGC() (stopFn func()) {
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		s.pairingRateLimiter.runGC(stop)
		close(done)
	}()
	return func() {
		close(stop)
		<-done
	}
}

// StartManifestRateLimitReaper drains idle per-token entries from the
// /v1/manifest limiter map on a 10-minute tick. Mirrors
// StartPairingRateLimitGC's lifecycle contract — caller passes the
// returned func to defer so the goroutine exits cleanly on shutdown.
// Test harnesses can skip the call; the reaper is purely a memory-
// pressure mitigation for long-running bridges with high client churn.
func (s *Server) StartManifestRateLimitReaper() (stopFn func()) {
	if s.manifestRateLimiter == nil {
		return func() {
			// No-op stopFn — the reaper goroutine was never
			// spawned (limiter not wired). Return the same
			// shape as the live path so callers can `defer` it
			// unconditionally.
		}
	}
	stop := make(chan struct{})
	s.manifestRateLimiter.Start(stop)
	return func() { close(stop) }
}

// WithPairing attaches the in-memory pairing.Store that backs the
// admin-approval pairing flow (POST /v1/pairing/requests, GET/DELETE
// /v1/pairing/{id}). Optional — when nil the routes return 404
// `pairing_not_supported` and iOS falls back to manual token entry.
// Older bridges that don't ship this package keep the same wire
// behaviour for free (404 from the unregistered route).
func (s *Server) WithPairing(p *pairing.Store) *Server {
	s.pairing = p
	return s
}

// WithCertExpiry attaches the on-disk TLS cert's NotAfter date so
// /v1/health can surface it to iOS. Wired from cmd/bridge after
// `tls.Inspect(certPath)`. Test harnesses can omit this; the
// HealthResponse field is `omitempty` (pointer) so the zero-time
// stays off the wire.
func (s *Server) WithCertExpiry(notAfter time.Time) *Server {
	s.certNotAfter = notAfter
	return s
}

// WithLECertExpiry wires the live Let's Encrypt cert expiry provider.
// Closure-based (not a stamped time.Time) because autocert renews the
// cert in the background — every /v1/health call should reflect the
// CURRENT cached cert's `NotAfter`, not whatever was on disk when the
// bridge started. cmd/bridge passes the same `autocert.Manager.Status`
// closure the admin tile reads from, so /v1/health and the admin tile
// always agree about expiry.
//
// Provider may return zero time when no cert is cached yet (autocert
// hasn't completed the first mint, or the manager isn't enabled).
// HealthResponse omits the field in that case — iOS treats absence as
// "no LE cert" (= LAN-only / loopback bridge), which is the right
// fallback for a public-mode bridge mid-first-handshake too.
//
// Pass `nil` (or skip the call) to opt out — loopback installs never
// wire it.
func (s *Server) WithLECertExpiry(provider func() time.Time) *Server {
	s.leCertNotAfterProvider = provider
	return s
}

// WithTailscaleStatus wires the embedded-tsnet status provider used by
// `reachableEndpoints` to advertise the bridge's `*.ts.net` URL +
// tailnet IPs in tsnet mode. Builder form for consistency with the
// surrounding chain in `cmd/bridge/main.go`. Pass `nil` (or skip the
// call entirely) to opt out — the advertising path no-ops cleanly.
//
// In `cli` / `disabled` modes the provider is consulted but the
// mode-gate in `reachableEndpoints` short-circuits before any
// advertisement is emitted, so wiring it unconditionally is safe.
func (s *Server) WithTailscaleStatus(p admin.TailscaleProvider) *Server {
	s.tailscaleStatusMu.Lock()
	s.tailscaleStatus = p
	s.tailscaleStatusMu.Unlock()
	return s
}

// SetTailscaleStatus is the deferred-setter counterpart to
// `WithTailscaleStatus`. Two-phase init sites in cmd/bridge that
// construct the api server BEFORE the tailscale status source is
// available call this after both are ready. Idiomatic for components
// whose lifecycles span early-setup + network-binding phases (the
// embedded tsnet Server is created after `api.New(...)` in some
// configurations and `newTailscaleAdminSource` needs it as input).
func (s *Server) SetTailscaleStatus(p admin.TailscaleProvider) {
	s.tailscaleStatusMu.Lock()
	s.tailscaleStatus = p
	s.tailscaleStatusMu.Unlock()
}

// tailscaleStatusTTL bounds how often `/v1/health` advertising will
// re-probe the underlying tsnet LocalClient. Mirrors `reachabilityTTL`
// (the existing 5-second cache for /v1/health-adjacent network probes).
// The endpoint is unauthenticated; without this cap a LAN flood could
// thrash the IPC channel. 5s is a comfortable upper bound on the
// time-to-discover-newly-approved-tsnet-node UX (iOS polls /v1/health
// every 15s so cache-miss latency is bounded by the cache TTL, not the
// poll cadence).
const tailscaleStatusTTL = 5 * time.Second

// cachedTailscaleStatus returns a per-Server 5s-TTL-cached snapshot of
// the embedded tsnet node's state. Uses `Status()` (cheap snapshot),
// NOT `RefreshNow(ctx)` — the admin tile uses the latter for "force a
// fresh probe" semantics; this code path is on an unauthenticated
// hot route and must not allow per-request IPC.
//
// Returns the zero value when no provider is wired (test harnesses,
// pre-tsnet bridges). Downstream callers gate on `BackendState ==
// "Running" && MagicDNSName != ""` so the zero value is safe to use
// without an explicit nil check at the call site.
func (s *Server) cachedTailscaleStatus() admin.TailscaleStatus {
	s.tailscaleStatusMu.Lock()
	defer s.tailscaleStatusMu.Unlock()
	if s.tailscaleStatus == nil {
		return admin.TailscaleStatus{}
	}
	if !s.tailscaleStatusFetched.IsZero() &&
		time.Since(s.tailscaleStatusFetched) < tailscaleStatusTTL {
		return s.tailscaleStatusCache
	}
	s.tailscaleStatusCache = s.tailscaleStatus.Status()
	s.tailscaleStatusFetched = time.Now()
	return s.tailscaleStatusCache
}

// EventPublisher returns the broker as the Server-facing
// `EventPublisher` interface so upstream services (transcode pool,
// pairing store) can publish events without taking a hard dependency
// on the api package. When the broker isn't wired (test harnesses,
// pre-this-PR bridges) the returned publisher silently drops
// publishes — same back-compat shape every upstream path already
// handles.
func (s *Server) EventPublisher() EventPublisher {
	if s.eventBroker == nil {
		return nopEventPublisher{}
	}
	return s.eventBroker
}

// StartEventBroker spins up the in-process event-bus goroutine that
// backs GET /v1/events. Same pattern as `StartPairingRateLimitGC` —
// returns a stopFn that drains the broker on shutdown. Wired from
// cmd/bridge after `apiSrv := api.New(...)`.
func (s *Server) StartEventBroker() (stopFn func()) {
	if s.eventBroker == nil {
		s.eventBroker = newEventBroker()
	}
	return s.eventBroker.Start()
}

// WithUpscaleStats attaches the snapshot provider for GET
// /v1/upscale/stats. Optional — when nil the endpoint returns the
// zero-value UpscaleStats (`enabled=false`, no pool, no sox
// availability), which iOS treats as "feature off" without
// distinguishing missing-endpoint from disabled-feature. Lets older
// bridges expose the route without the wiring overhead.
//
// Wired in cmd/bridge serve startup with the same closure the admin
// `/api/upscale/stats` tile already consumes — the two surfaces
// stay in lockstep so the admin operator and the paired iOS client
// see the same numbers.
func (s *Server) WithUpscaleStats(p UpscaleStatsProvider) *Server {
	s.upscaleStatsProvider = p
	return s
}

// Resolver returns the internal path Resolver so the admin console can
// call SetRoots when adding or removing a library root at runtime. The
// api handlers and admin handlers deliberately share the same Resolver
// instance — otherwise a hot add/remove would update one without the
// other and /v1/list would serve stale top-level entries.
func (s *Server) Resolver() *bridgefs.Resolver { return s.resolver }

// StartedAt returns the server's construction timestamp. Used by the
// SSE pairing-event publisher in cmd/bridge to populate the
// `bridgeStartedAt` field, matching the polling endpoint's contract:
// iOS observes a value change between events and treats it as
// "bridge restarted, abandon this in-flight pairing request."
func (s *Server) StartedAt() time.Time { return s.startedAt }

func (s *Server) withAltSvc(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.cfgHolder.Load().DisableHTTP3 {
			_, requestPort, err := net.SplitHostPort(r.Host)
			if err != nil {
				// Default fallback for standard port/tsnet connections
				requestPort = "443"
			}
			w.Header().Set("Alt-Svc", fmt.Sprintf(`h3=":%s"; ma=2592000`, requestPort))
		} else {
			// Actively instruct the client to flush its cached protocol upgrade routes
			w.Header().Set("Alt-Svc", "clear")
		}
		next.ServeHTTP(w, r)
	})
}

// Handler returns the root http.Handler, pre-wrapped with the
// X-Bridge-Protocol middleware. Outermost wrapper is withAltSvc so
// protocol upgrades (or cache-clear directives) land even on 500
// panic recovery paths.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	// Iterate the single-source-of-truth registry (see
	// route_classification.go). `boundedRoute` entries get
	// per-route `SetWriteDeadline` via boundedHandler;
	// `streamingRoute` entries are registered raw so the
	// long-lived SSE / multi-GB-download paths aren't cut off
	// mid-write.
	//
	// Authed / rate-limit / manifest middleware chains are
	// baked into the registry entries' `handler` field — the
	// wrapping order matters there (authed before rate-limit
	// so the limiter keys on Token.ID, not IP).
	for _, rt := range s.routeRegistry() {
		h := rt.handler
		if rt.kind == boundedRoute {
			h = boundedHandler(h)
		}
		mux.HandleFunc(rt.pattern, h)
	}
	// Chain order matters:
	//   - `recoverer` innermost so it can catch panics in the mux/handlers
	//     and emit a sanitized 500 before unwinding.
	//   - `requestLogging` wraps recoverer so the captured status (e.g.
	//     500 on the recovery path) and request_id are emitted on the
	//     same line.
	//   - `protocolHeader` wraps logging so the version header is injected.
	//   - `withAltSvc` outermost ensures protocol hints reach the client
	//     even on failure paths.
	return s.withAltSvc(protocolHeader(requestLogging(recoverer(mux))))
}

// HealthResponse is the /v1/health JSON body. Field ordering must stay
// stable because the iOS side golden-decodes this shape (see PROTOCOL.md).
//
// `Endpoints` is the additive v1 extension that lets iOS self-discover
// LAN ↔ Tailscale alternates without re-pairing. Empty when the bridge
// can't enumerate interfaces (e.g. test harness) — iOS falls back to
// the URL it was paired on. Adding fields here is backwards-compatible:
// the iOS decoder uses Codable with default values for anything it
// doesn't know about.
//
// The four `*Update*` / `MinClientVersion` fields are the Phase A
// additive extension (see PROTOCOL.md "Updates" section). They are
// only populated when an UpdaterStatus has been wired via WithUpdater
// AND the updater has at least one successful poll cached; absent
// otherwise so the wire shape stays identical to the pre-update
// response on a bridge that doesn't yet have an updater. iOS treats
// all four as optional and falls back to "no update info" rather
// than a hard error if any are missing.
type HealthResponse struct {
	ProtocolVersion       int       `json:"protocolVersion"`
	ServerVersion         string    `json:"serverVersion"`
	LibraryName           string    `json:"libraryName"`
	LibraryRoots          []string  `json:"libraryRoots"`
	CertFingerprint       string    `json:"certFingerprint"`
	StartedAt             time.Time `json:"startedAt"`
	ScanState             ScanState `json:"scanState"`
	Endpoints             []string  `json:"endpoints,omitempty"`
	LatestServerVersion   string    `json:"latestServerVersion,omitempty"`
	UpdateAvailable       bool      `json:"updateAvailable,omitempty"`
	UpdateReleaseNotesURL string    `json:"updateReleaseNotesURL,omitempty"`
	MinClientVersion      string    `json:"minClientVersion,omitempty"`
	// UpscaleEnabled mirrors the runtime config flag
	// `cfg.Upscale.Enabled` AND the result of the sox-on-PATH
	// startup probe (a true config setting whose probe failed
	// reports false here — see cmd/bridge/main.go's serve-side
	// degradation). iOS uses this to gate every variant-related
	// UI surface for that bridge: when false, the picker / glyph
	// / context menu items are all hidden so SMB and bridge rows
	// look identical. Pre-v1.2 servers omit the field entirely;
	// iOS treats absence as `false` (no variant chrome). Pointer
	// type so a true `false` value distinguishes "feature
	// supported but disabled" from "no field on the wire".
	UpscaleEnabled *bool `json:"upscaleEnabled,omitempty"`

	// CertNotAfter is the **on-disk self-signed TLS certificate's**
	// `NotAfter` (UTC). This is the cert that fronts LAN, mDNS,
	// IP-literal, and Tailscale-IP SNI handshakes — i.e. the cert
	// iOS captures + pins at pairing time. Lets iOS surface a
	// "Bridge cert expires in X days — re-pair to refresh" warning
	// before the cert actually expires and TLS handshakes start
	// failing at Apple's ATS layer (Apple's 397-day cap means
	// operators must re-pair roughly annually).
	//
	// **In public mode this is NOT the cert the public-domain
	// connection sees** — `LECertNotAfter` (below) carries the
	// Let's Encrypt cert's expiry, which is the operative cert when
	// iOS dials `https://<autocert.domain>/`. Both fields are
	// emitted in public mode so iOS / operator tooling can render
	// the right warning per posture (the self-signed cert is still
	// the pinned cert iOS used at pair-time, so its expiry still
	// matters for the "re-pair" prompt; the LE cert expiry matters
	// for the public-trust handshake).
	//
	// Additive field; pre-bridge-with-this-PR servers omit it and
	// iOS treats absence as "no expiry info, never warn". Pointer
	// (not bare `time.Time`) because Go's `omitempty` doesn't treat
	// the zero time as empty — and emitting `0001-01-01T00:00:00Z`
	// from a test harness or a parse failure would actively confuse
	// clients.
	CertNotAfter *time.Time `json:"certNotAfter,omitempty"`

	// LECertNotAfter is the public-domain Let's Encrypt certificate's
	// `NotAfter` (UTC), populated only in public mode (autocert
	// enabled + cert minted on disk). Surfaces the ~90-day LE rotation
	// expiry distinctly from `CertNotAfter` (the self-signed cert
	// pinned by iOS at LAN pair-time, capped at 397 days). Without
	// this, an operator glancing at `/v1/health` on a freshly-deployed
	// public VPS would see the 397-day self-signed expiry and
	// reasonably assume their LE cert lasts that long too — masking
	// the real ~60-day "rotation is about to happen" signal.
	//
	// Loopback bridges omit this field; pre-autocert servers omit it.
	// iOS treats absence as "no LE cert" (= LAN-only bridge). Pointer
	// for the same `omitempty`-vs-zero-time reason as `CertNotAfter`.
	LECertNotAfter *time.Time `json:"leCertNotAfter,omitempty"`

	// Features advertises capability flags the server supports.
	// Additive over the wire (omitempty); iOS consults this list to
	// skip belt-and-braces recovery paths when the server already
	// provides the underlying guarantee. Stable string keys, never
	// repurpose. Pre-feature-flag bridges omit the field entirely;
	// iOS treats absence as "feature absent" so any client-side
	// recovery path runs unconditionally on those bridges.
	//
	// Current keys (kept alpha-sorted on the wire):
	//   - "deleteVariants" (v1.3): bridge supports DELETE
	//     /v1/upscale/variants AND fires `upscale.deleted` SSE
	//     events on operator-driven cleanup + integrity-ticker
	//     reconciliation. iOS reads it on the same probe to know
	//     the wand chrome can revert passively when a variant
	//     disappears server-side. Gated on `variantDeleter != nil`
	//     AND `upscaleEnabled`.
	//   - "operatorDrivenUpscale" (v1.3): upscaling is managed in
	//     the bridge's admin Library Inspector, not per-tap from
	//     the iOS app. iOS gates ALL legacy upscale UI surfaces
	//     (BridgeUpscaleControl wand, TrackSourceGlyph, long-press
	//     "Upscale this track" menu items, BridgeUpscaleManagement
	//     Section) on the ABSENCE of this flag. Pre-v1.3 bridges
	//     omit it; iOS keeps the legacy wand UX alive against
	//     those. v1.3+ bridges advertise it; iOS surfaces only the
	//     mini-player haptic-press toggle + Settings device toggle
	//     for variant override.
	//   - "pairingEventsSupported" (v1.4): the bridge backs
	//     `GET /v1/pairing/{id}/events` with the same SSE broker
	//     that powers `/v1/events`. iOS's BridgeJoinSession can
	//     opt into the push transport up-front when the flag is
	//     present (vs probing then falling back to 2 s polling on
	//     404). Gated on `eventBroker != nil && pairing != nil`
	//     — both routes must be live for the consumer to see
	//     useful events.
	//   - "pushEventsSupported" (v1.4): the bridge backs
	//     `GET /v1/events` with a live SSE broker. Future iOS /
	//     third-party clients can branch on this flag rather than
	//     attempting the connect-and-fall-back dance on every cold
	//     start. Gated on `eventBroker != nil`.
	//   - "upscaleCompleteEvents": bridge publishes a per-job
	//     `upscale.complete` SSE event after `UpsertVariant` commits.
	//     iOS uses this to promote the wand chrome to "Ready" within
	//     ~1-2 s of bridge-side completion (vs the legacy 8 s first
	//     ladder rung), and skips 4-of-5 polling-ladder rungs when
	//     present, keeping only the +600 s safety backstop.
	//   - "variantBumpsIndex": UpsertVariant / DeleteVariant bump
	//     `tracks.indexed_at` for the parent row, so iOS delta-sync
	//     surfaces variant changes without needing a full rescan.
	//     iOS gates its +600s "silent fullRescan recovery" rung on
	//     absence of this flag.
	Features []string `json:"features,omitempty"`

	// Roots is the per-root reachability snapshot. Populated whenever the
	// bridge has at least one configured library root; absent on bridges
	// with zero roots (test harness). Lets iOS surface a "Library X
	// offline" hint in a single /v1/health call instead of paginating
	// /v1/list to discover offline roots. Probes are cached for
	// reachabilityTTL (5 s) so the steady-state /v1/health poll cadence
	// doesn't repeatedly stat every network mount.
	//
	// Pre-1.2 iOS ignores the field. iOS 1.2+ uses Reason as a stable
	// machine-readable code mapped to a localized UI string.
	Roots []RootStatus `json:"roots,omitempty"`
}

// RootStatus reports the reachability of a single library root. Wire-DTO
// only — populated server-side from reachabilityStatus. Name is the
// root's basename (same identifier iOS sees on /v1/list); Reason is the
// stable code emitted by reachabilityCache.probe.
type RootStatus struct {
	Name      string `json:"name"`
	Reachable bool   `json:"reachable"`
	Reason    string `json:"reason,omitempty"`
}

// ScanState reports the scanner's current status. Real fields populate once
// the manifest package lands; the shape is stubbed in for iOS decoder
// stability.
type ScanState struct {
	IsScanning    bool      `json:"isScanning"`
	LastFullScan  time.Time `json:"lastFullScan,omitempty"`
	TracksIndexed int       `json:"tracksIndexed"`

	// PendingDeletions reports the count of rows across `tracks` and
	// `folders` whose missing_count is > 0 but haven't yet reached the
	// configured delete threshold — i.e. rows the scanner has marked
	// as "missing this pass" but is granting the configured grace
	// period before reaping. Surfaced for the admin dashboard "X rows
	// pending deletion" hint and as a diagnostic signal on /v1/health:
	// a steadily climbing value suggests a flaky mount or partial
	// network failure that errorSubtrees isn't catching. Pre-v1.2.x
	// bridges (before migration v5) omit the field entirely; iOS and
	// admin UI treat absence as "no pending deletions, nothing to
	// surface". Additive field, no protocol bump.
	PendingDeletions int64 `json:"pendingDeletions,omitempty"`
}

// ErrorResponse matches the shape documented in PROTOCOL.md ("short-code" +
// human detail). Keep this struct in sync with the error table there.
type ErrorResponse struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	cfg := s.cfgHolder.Load()
	scanState := ScanState{}
	if s.manifest != nil {
		scanState.IsScanning = s.manifest.IsScanning()
		scanState.LastFullScan = s.manifest.LastFullScan()
		scanState.TracksIndexed = s.manifest.TracksIndexed(r.Context())
		scanState.PendingDeletions = s.manifest.PendingDeletions(r.Context())
	}
	resp := HealthResponse{
		ProtocolVersion: version.ProtocolVersion,
		ServerVersion:   version.ServerVersion,
		LibraryName:     cfg.LibraryName,
		LibraryRoots:    libraryRootBasenames(cfg.LibraryRoots),
		CertFingerprint: s.fingerprint,
		StartedAt:       s.startedAt,
		ScanState:       scanState,
		Endpoints:       s.reachableEndpoints(),
	}
	if s.updater != nil {
		info := s.updater.UpdateInfo()
		resp.LatestServerVersion = info.LatestVersion
		resp.UpdateAvailable = info.UpdateAvailable
		resp.UpdateReleaseNotesURL = info.ReleaseNotesURL
		resp.MinClientVersion = info.MinClientVersion
	}
	// Capability flag — non-nil pointer so the wire shape says
	// `false` explicitly (not omitted) when the server supports
	// the feature but has it turned off. Older servers without
	// the field surface as nil on iOS, which iOS treats as false.
	upscaleEnabled := s.upscaleEnabled
	resp.UpscaleEnabled = &upscaleEnabled
	if !s.certNotAfter.IsZero() {
		notAfter := s.certNotAfter
		resp.CertNotAfter = &notAfter
	}
	// Public-mode autocert expiry — read live so background renewals
	// surface on the next /v1/health probe without restart. Zero
	// time means "no cert cached yet" (autocert hasn't completed the
	// first mint, or provider isn't wired); leave the field off the
	// wire in that case.
	if s.leCertNotAfterProvider != nil {
		if lena := s.leCertNotAfterProvider(); !lena.IsZero() {
			leNotAfter := lena
			resp.LECertNotAfter = &leNotAfter
		}
	}
	// Capability flags — see HealthResponse.Features doc above for the
	// stable-key convention. Order kept stable alphabetically so any
	// client comparing /v1/health response fingerprints (e.g. for
	// content-equality short-circuit caches) doesn't churn on every
	// poll. New keys append in alpha order.
	//
	// Gating rules:
	//   - `upscaleCompleteEvents`, `deleteVariants`, `operatorDrivenUpscale`:
	//     gated on `s.upscaleEnabled` because iOS will skip
	//     4-of-5 ladder rungs when `upscaleCompleteEvents` is
	//     present and wait for an `upscale.complete` SSE event —
	//     but a bridge with upscale disabled has no transcode pool
	//     and `SetOnJobComplete` was never called, so no event ever
	//     arrives. Mirrors how `resp.UpscaleEnabled` already gates
	//     the feature visibility. (Greptile P1 on PR #187.)
	//   - `operatorDrivenUpscale` additionally requires a wired
	//     `batchCoordinator` — without it the /v1/upscale/batch
	//     endpoints surface 503 with nothing to fall back to.
	//     (CodeRabbit major on PR #204 round 2.)
	//   - `deleteVariants` additionally requires a wired
	//     `variantDeleter` so a feature-disabled deploy advertises
	//     honestly.
	//   - `pushEventsSupported` / `pairingEventsSupported`: gated on
	//     `s.eventBroker != nil` (and `s.pairing != nil` for the
	//     pairing variant). Orthogonal to `upscaleEnabled` — the
	//     event surfaces are wired by `StartEventBroker` regardless
	//     of whether the transcode pool exists.
	//
	// Alpha-sort stays correct by construction: each conditional
	// appends in lex order, terminal `variantBumpsIndex` ends every
	// path. Capacity 10 covers the current maximum (carPlayOptimize +
	// deleteVariants + diagnosticsSummary + dlnaServer +
	// operatorDrivenUpscale + pairingEventsSupported +
	// pushEventsSupported + rendererDiscovery +
	// upscaleCompleteEvents + variantBumpsIndex).
	feats := make([]string, 0, 10)
	if s.upscaleEnabled {
		if s.carPlayOptimizeEnabled {
			feats = append(feats, "carPlayOptimize")
		}
		if s.variantDeleter != nil {
			feats = append(feats, "deleteVariants")
		}
	}
	// `diagnosticsSummary` advertises `/v1/diagnostics`. Always-on in
	// this build — the endpoint is unconditionally wired. Lexically
	// lands between `deleteVariants` and `dlnaServer`, which is why
	// the upscale gate above is split into two blocks instead of a
	// single contiguous one.
	feats = append(feats, "diagnosticsSummary")
	// `dlnaServer` advertises the bridge's opt-in LAN-only DLNA
	// MediaServer (`internal/dlna/` package, parallel http.Server bound
	// LAN/Tailnet-only). iOS clients can use this to choose between
	// SSDP-discovered renderers and bridge-routed DLNA delivery; the
	// actual MediaServer binding lives in cmd/bridge/main.go and refuses
	// to start in public deployment mode regardless of this flag.
	if s.dlnaEnabled {
		feats = append(feats, "dlnaServer")
	}
	if s.upscaleEnabled {
		if s.batchCoordinator != nil {
			feats = append(feats, "operatorDrivenUpscale")
		}
	}
	if s.eventBroker != nil {
		if s.pairing != nil {
			feats = append(feats, "pairingEventsSupported")
		}
		feats = append(feats, "pushEventsSupported")
	}
	// `rendererDiscovery` advertises the bridge's SSDP MediaRenderer
	// cache + `GET /v1/renderers` endpoint (PR 5). Gated AND-wise
	// on `s.dlnaEnabled` (sibling DLNA MediaServer must be running
	// so the UDN namespace stays coherent) AND `s.rendererDiscovery
	// != nil` (operator opted in via `cfg.DLNA.Discovery.Enabled
	// = true` AND the LAN-eligible interface picker succeeded).
	// Alpha-sorted between `pushEventsSupported` and
	// `upscaleCompleteEvents` (p < r < u).
	if s.dlnaEnabled && s.rendererDiscovery != nil {
		feats = append(feats, "rendererDiscovery")
	}
	if s.upscaleEnabled {
		feats = append(feats, "upscaleCompleteEvents")
	}
	feats = append(feats, "variantBumpsIndex")
	resp.Features = feats
	// Per-root reachability (v1.2 additive). Probed through the same TTL
	// cache the list/stat handlers use, so a 1Hz iOS /v1/health poll
	// doesn't restat network mounts every tick. Omitted entirely (via
	// omitempty) on bridges with no configured roots — test harness shape.
	if s.reachability != nil {
		resp.Roots = s.probeAllRoots(r.Context())
	}
	writeJSON(w, http.StatusOK, resp)
}

// reachableEndpoints enumerates LAN + mDNS + Tailscale URLs for the
// bridge on every /v1/health call. Fresh on each call so adding /
// removing a network interface (Tailscale up, Wi-Fi down) takes effect
// on the next heartbeat without requiring a restart. Cost is a
// `net.Interfaces()` + `.Addrs()` walk — cheap enough to not warrant
// caching for the host-network part.
//
// In `tsnet` mode the embedded tsnet node's MagicDNSName + tailnet IPs
// are appended from `s.cachedTailscaleStatus()` (5s TTL — see
// `tailscaleStatusTTL`). The append happens BEFORE the class-stable
// sort + dedupe so that a duplicate URL (e.g. an operator who
// hardcoded the magic-DNS URL into `customEndpoints` as a pre-fix
// workaround) ends up classified as `ClassTailscaleDNS` rather than
// `ClassCustom`, keeping the URL ranked correctly in the iOS endpoint
// selector's hint order.
func (s *Server) reachableEndpoints() []string {
	cfg := s.cfgHolder.Load()
	_, portStr, err := net.SplitHostPort(cfg.ListenAddress)
	if err != nil {
		return nil
	}
	// `port` retained only for the >0 sanity check on the listen
	// address; everything downstream uses `portStr` directly so we
	// don't `strconv.Itoa(port)` once per synthesized endpoint.
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 {
		return nil
	}
	// **Public-mode short-circuit (PR 5)**: VPS deployments must
	// not leak internal hostnames / LAN IPs / Tailscale URLs to
	// iOS clients. The only endpoints the operator wants
	// advertised are the customEndpoints they explicitly declared
	// (operator's public domain via reverse proxy + optional
	// alt routes) and the autocert public domain. Skip the host-
	// interface walk + the Tailscale append entirely.
	if cfg.IsPublic() {
		eps := publicModeEndpoints(cfg, portStr)
		return classStableUniqueURLs(eps)
	}
	eps := advertise.Endpoints(advertise.Params{
		Port:            port,
		CustomEndpoints: cfg.CustomEndpoints,
	})
	// Tailscale advertising now flows ENTIRELY through the api-layer
	// provider for both `cli` and `tsnet` modes. The advertise package
	// is no longer Tailscale-aware (no CLI shell-out, no utun
	// interface-walk gate). `disabled` mode skips this path entirely.
	//
	// Empty mode is resolved to "cli" here matching `config.TailscaleConfig
	// .EffectiveMode()`'s default at validate time. Operators with no
	// `tailscale:` block in YAML (the common pre-tsnet shape) carry
	// `cfg.Tailscale.Mode == ""`; treating that as a skip would silently
	// drop the host's Tailscale advertising those operators relied on
	// before PR #267 (a regression Gemini caught on PR #269).
	mode := cfg.Tailscale.Mode
	if mode == "" {
		mode = "cli"
	}
	if mode == "cli" || mode == "tsnet" {
		eps = s.appendTailscaleEndpoints(eps, portStr, mode)
	}
	return classStableUniqueURLs(eps)
}

// appendTailscaleEndpoints adds the local Tailscale node's
// MagicDNSName + tailnet IPs to `eps` when the cached `TailscaleProvider`
// snapshot reports the node is fully up. Gate logic differs by mode:
//
//   - `tsnet`: `BackendState == "Running"` (the embedded node's in-process
//     LE cert is provisioned only at Running; pre-auth states would
//     route iOS to a self-signed cert and trip ATS — the same failure
//     PR #267 closed).
//   - `cli`: `CertPresent` (the LE cert is on disk under
//     `data/tailscale/lecert/{...}.crt` once `tailscale cert` minted it;
//     pre-mint, the SNI switcher falls through to the self-signed cert
//     for any `.ts.net` SNI and ATS rejects → operator sees TLS error).
//     `BackendState` is intentionally NOT consulted in cli mode — the
//     cli-side `tailscale.Detect()` reads only `Self.{HostName,DNSName,
//     TailscaleIPs}` + `MagicDNSSuffix`, never the daemon's BackendState
//     (it's not a useful signal there: `tailscale status --json`
//     succeeding at all implies the daemon is up).
//
// No-op when:
//   - the provider isn't wired (`s.tailscaleStatus` nil — collapses
//     `cachedTailscaleStatus` to a zero value, which fails both gates),
//   - `MagicDNSName == ""` (MagicDNS can be tailnet-disabled in any
//     mode; the LE cert covers the hostname only so IP-only
//     advertising offers no usable TLS path).
func (s *Server) appendTailscaleEndpoints(eps []advertise.Endpoint, portStr string, mode string) []advertise.Endpoint {
	snap := s.cachedTailscaleStatus()
	if snap.MagicDNSName == "" {
		return eps
	}
	switch mode {
	case "tsnet":
		if snap.BackendState != "Running" {
			return eps
		}
	case "cli":
		if !snap.CertPresent {
			return eps
		}
	default:
		// Defensive — the caller already gates on mode but a future
		// refactor that widens the call site shouldn't accidentally
		// advertise without a known-good gate.
		return eps
	}
	magicDNS := strings.TrimSuffix(snap.MagicDNSName, ".")
	eps = append(eps, advertise.Endpoint{
		URL:   fmt.Sprintf("https://%s", net.JoinHostPort(magicDNS, portStr)),
		Class: advertise.ClassTailscaleDNS,
	})
	for _, ipStr := range snap.TailscaleIPs {
		// IP strings come from either `ipnstate.Status.Self.TailscaleIPs`
		// (tsnet mode) or `tailscale status --json`'s `Self.TailscaleIPs`
		// (cli mode) — both are guaranteed well-formed addresses.
		// `strings.Contains(":")` is a sufficient v4-vs-v6 discriminator
		// without `net.ParseIP`'s 16-byte allocation per IP.
		class := advertise.ClassTailscaleV4
		if strings.Contains(ipStr, ":") {
			class = advertise.ClassTailscaleV6
		}
		eps = append(eps, advertise.Endpoint{
			URL:   fmt.Sprintf("https://%s", net.JoinHostPort(ipStr, portStr)),
			Class: class,
		})
	}
	return eps
}

// publicModeEndpoints returns the endpoint set for a public-mode
// VPS deployment: the operator-declared customEndpoints and the
// autocert public domain. LAN / mDNS / Tailscale enumeration is
// intentionally skipped — those would leak VPS-internal
// hostnames (private RFC 1918 addresses, the host's docker0
// bridge, …) to iOS clients, and the operator's iOS app dials
// only the public domain anyway.
//
// The autocert domain is synthesized as `https://<domain>` when
// port is the https default (:443) or `https://<domain>:<port>`
// otherwise. Matching the shape `cmd/bridge/init.go` writes into
// the customEndpoints seed avoids near-duplicate entries when
// the operator declares the same domain in both fields and the
// dedupe downstream collapses the pair instead of admitting two
// URLs that differ only by the implicit https default
// (`https://h` vs `https://h:443` would otherwise be distinct
// strings to classStableUniqueURLs — Gemini medium on PR #295).
func publicModeEndpoints(cfg *config.Config, portStr string) []advertise.Endpoint {
	var eps []advertise.Endpoint
	for _, raw := range cfg.CustomEndpoints {
		if raw = strings.TrimSpace(raw); raw != "" {
			eps = append(eps, advertise.Endpoint{URL: raw, Class: advertise.ClassCustom})
		}
	}
	if d := strings.TrimSpace(cfg.Autocert.Domain); d != "" {
		u := "https://" + d
		if portStr != "443" {
			u += ":" + portStr
		}
		eps = append(eps, advertise.Endpoint{URL: u, Class: advertise.ClassCustom})
	}
	return eps
}

// classStableUniqueURLs applies the class-stable sort + dedupe + URL
// flatten that `reachableEndpoints` returns. Sort happens BEFORE
// dedupe so that a duplicate URL across classes (the canonical
// case: an operator hardcoded the magic-DNS URL into
// `customEndpoints` as a pre-tsnet-auto-advertising workaround,
// producing both a `ClassCustom` and a `ClassTailscaleDNS` entry
// for the same URL) lands in the result under the lower-numbered
// (higher-priority) class — `ClassTailscaleDNS` (3) wins over
// `ClassCustom` (7). A dedupe-first pass would keep insertion-order
// (`ClassCustom` first) and demote the URL to the bottom of the
// ranking.
func classStableUniqueURLs(eps []advertise.Endpoint) []string {
	sort.SliceStable(eps, func(i, j int) bool {
		return eps[i].Class < eps[j].Class
	})
	seen := make(map[string]bool, len(eps))
	unique := eps[:0]
	for _, e := range eps {
		if seen[e.URL] {
			continue
		}
		seen[e.URL] = true
		unique = append(unique, e)
	}
	out := make([]string, len(unique))
	for i, e := range unique {
		out[i] = e.URL
	}
	return out
}

// manifestHandler serves GET /v1/manifest?since=<rfc3339> with the current
// library index (or a since-filtered delta), OR — if `?limit=` is set —
// one page of a paginated full-manifest walk driven by `?cursor=`.
// Returns 503 if no manifest provider is wired up yet (early boot /
// test harness).
//
// Query parameter combinations:
//   - no params               → full manifest, v1.0 behaviour
//   - ?since=<rfc3339>        → since-delta (never paginated, by
//     construction small)
//   - ?limit=N[&cursor=<opq>] → paginated full manifest (v1.1). Empty
//     cursor requests the first page; the
//     caller iterates until NextCursor is nil.
//
// `?limit=` + `?since=` together returns 400 — mixing the two would
// need a composite (timestamp, path) cursor for ordering stability,
// and since-deltas are bounded anyway.
func (s *Server) manifestHandler(w http.ResponseWriter, r *http.Request) {
	if s.manifest == nil {
		writeError(w, http.StatusServiceUnavailable, "scan_in_progress", "manifest not ready")
		return
	}
	// safeQuery preserves literal '+' in query values — load-bearing for
	// RFC3339 ?since= timestamps with positive timezone offsets like
	// 2026-05-23T15:40:00+02:00, which the stdlib's r.URL.Query() would
	// otherwise decode to a space and break time.Parse downstream. See
	// internal/api/query.go for the rationale.
	q := safeQuery(r)
	sinceRaw := q.Get("since")
	limitRaw := q.Get("limit")

	if sinceRaw != "" && limitRaw != "" {
		writeError(w, http.StatusBadRequest, "bad_request",
			"`since` and `limit` are mutually exclusive; pagination applies to full-manifest requests only")
		return
	}

	// Paginated path.
	if limitRaw != "" {
		limit, err := strconv.Atoi(limitRaw)
		if err != nil || limit <= 0 {
			writeError(w, http.StatusBadRequest, "bad_request",
				"limit must be a positive integer, got "+limitRaw)
			return
		}
		// Cap the page size at a reasonable ceiling so a client
		// requesting `limit=10000000` can't allocate a huge response
		// that DoSes the server or the iOS side's JSON decoder.
		const maxPageLimit = 5000
		if limit > maxPageLimit {
			limit = maxPageLimit
		}
		cursor := q.Get("cursor")
		body, err := s.manifest.BuildManifestPage(r.Context(), cursor, limit)
		if err != nil {
			writeErrorLog(w, r, http.StatusInternalServerError, "internal",
				"the bridge couldn't build this manifest page", err)
			return
		}
		writeJSON(w, http.StatusOK, body)
		return
	}

	// Legacy single-shot path (full manifest or since-delta). Streamed
	// to bound peak memory on Pi-class hosts with 50k-track libraries —
	// the prior in-memory builder allocated >200 MB during a single
	// request and OOM-killed the process. Headers go out before the
	// first track row; mid-stream errors can only be logged.
	var since time.Time
	if sinceRaw != "" {
		parsed, err := time.Parse(time.RFC3339Nano, sinceRaw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_request",
				"since must be an RFC3339 timestamp, got "+sinceRaw)
			return
		}
		since = parsed
	}
	// Defer WriteHeader until the streamer writes its first byte, so
	// failures inside WriteManifest's pre-stream DB reads (ListFolders,
	// CountTracks, EnrichmentCounts) can still surface as a structured
	// 5xx instead of a 200-with-truncated-body that breaks iOS-side
	// JSON decode (Qodo #1 on PR #70). Once any byte has gone out we
	// can only log the error — headers are committed.
	w.Header().Set("Content-Type", "application/json; charset=utf-8")

	// Negotiate gzip on the wire. JSON manifests compress 3–5× on
	// typical libraries (50k tracks: ~150 MB identity → ~30–40 MB
	// gzip). iOS URLSession sends `Accept-Encoding: gzip, deflate,
	// br` by default and transparently decompresses, so the win is
	// fully wire-side with no client change. SSE endpoints
	// (events.go, pairing.go) explicitly set Content-Encoding:
	// identity to defeat buffering middleware and have their own
	// trip-wire tests — which is why this gzip lives at the
	// manifest handler ONLY, not as global middleware.
	useGzip := acceptsGzip(r)
	if useGzip {
		w.Header().Set("Vary", "Accept-Encoding")
		w.Header().Set(headerContentEncoding, "gzip")
	}

	dw := &deferredStatusWriter{w: w, status: http.StatusOK}

	// Build the writer chain. The gzip writer sits between the
	// JSON encoder and the deferredStatusWriter so:
	//   - WriteManifest writes JSON tokens to gz
	//   - gz emits compressed bytes to dw
	//   - dw triggers WriteHeader(200) on the first compressed byte
	// The pre-stream-DB-error path stays intact: if WriteManifest
	// returns before producing any byte, gz hasn't been written
	// either, dw.written is false, and the early-error branch below
	// strips the encoding headers and emits a structured 5xx.
	var bodyWriter io.Writer = dw
	var gz *gzip.Writer
	if useGzip {
		gz = manifestGzipPool.Get().(*gzip.Writer)
		gz.Reset(dw)
		bodyWriter = gz
		// `Put` AFTER `Close` per the documented contract — see
		// manifestGzipPool's docblock. The defer is unconditional
		// so a panic mid-WriteManifest still returns the writer
		// to the pool (the Close path further down is the happy-
		// path trailer-flush; this defer is the safety net).
		defer manifestGzipPool.Put(gz)
	}

	// Pass r.Context() so a client disconnect mid-stream (slow network,
	// iOS backgrounded mid-sync, attacker slow-reading) interrupts the
	// SQLite scan within the next per-row check instead of running to
	// EOF holding the read lock and CPU.
	writeErr := s.manifest.WriteManifest(r.Context(), bodyWriter, since)

	// Flush the gzip trailer ONLY if any compressed byte already
	// reached the wire. `dw.written` is the authoritative signal:
	// gzip.Writer emits its 10-byte header on first Write, and that
	// first byte goes through dw — so gzip-was-touched ⇔ dw.written.
	// On the early-error path (WriteManifest returns before writing
	// anything), gz.Close() would emit a ~20-byte gzip header+trailer
	// for an empty stream, committing WriteHeader(200) and stealing
	// the structured 5xx the error branch below tries to emit. Skip
	// the close in that case and let the error path write a clean
	// uncompressed 500.
	if useGzip && dw.written {
		if closeErr := gz.Close(); closeErr != nil {
			if writeErr == nil {
				writeErr = closeErr
			} else {
				// A close error AFTER a non-nil writeErr (mid-
				// stream DB fault) is the broken-pipe trailer-
				// write — usually `connection reset by peer`.
				// Pre-pool this was silently masked; now log at
				// debug so production monitoring can correlate
				// trailer-failures with the underlying writeErr
				// when investigating a "client disconnect rate
				// spike" alert.
				logger.Debug("manifest gzip trailer Close failed",
					"err", closeErr, "writeErr", writeErr)
			}
		}
	} else if useGzip && writeErr == nil {
		// WriteManifest succeeded but produced zero bytes (empty
		// library — no tracks, no folders). The negotiation headers
		// were set up-front, but a zero-byte response with
		// Content-Encoding: gzip is invalid (gzip needs at least the
		// 10-byte header + 8-byte trailer). iOS URLSession honors
		// Content-Encoding and would surface a transport error trying
		// to decompress nothing. Strip the encoding headers so the
		// client sees a plain empty 200 — same shape as the pre-stream
		// error path below, without the 5xx.
		w.Header().Del(headerContentEncoding)
		w.Header().Del("Vary")
	}

	if writeErr != nil {
		if !dw.written {
			// Headers haven't been committed yet. Strip the gzip-
			// negotiation headers BEFORE writeError so the JSON
			// error body isn't misinterpreted as gzip by the client
			// (URLSession would surface a transport error and
			// silently retry, masking the real DB failure).
			w.Header().Del(headerContentEncoding)
			w.Header().Del("Vary")
			writeError(w, http.StatusInternalServerError, "internal", writeErr.Error())
			return
		}
		// Demote client-disconnect errors to debug — context.Canceled
		// is the normal outcome when an iOS client backgrounds the app
		// mid-sync or a slow network drops the connection. Pre-fix
		// these surfaced at .Error() and would have triggered
		// false-positive alerts in production monitoring (gemini medium
		// review on PR #117). DeadlineExceeded gets the same treatment
		// — it represents a client-imposed deadline, not a server
		// failure. Any other mid-stream error (DB read fault, fs
		// unmount) still logs at .Error().
		if errors.Is(writeErr, context.Canceled) || errors.Is(writeErr, context.DeadlineExceeded) {
			logger.Debug("manifest stream cancelled by client", "err", writeErr)
		} else {
			logger.Error("manifest stream", "err", writeErr)
		}
	}
}

// deferredStatusWriter holds back WriteHeader until the wrapped writer
// is first written to. Used by the streaming-manifest path so a DB
// failure before the first body byte can still produce a structured
// HTTP error response. After the first Write, behaviour is identical
// to the underlying ResponseWriter.
type deferredStatusWriter struct {
	w       http.ResponseWriter
	status  int
	written bool
}

func (d *deferredStatusWriter) Write(b []byte) (int, error) {
	if !d.written {
		d.w.WriteHeader(d.status)
		d.written = true
	}
	return d.w.Write(b)
}

// libraryRootBasenames returns just the last path component for each
// configured root. We deliberately don't ship the full server-side absolute
// path to the iOS client — paths are library-relative on the wire, and the
// basename is enough context for the UI to show "Music", "Classical", etc.
func libraryRootBasenames(roots []string) []string {
	out := make([]string, len(roots))
	for i, r := range roots {
		out[i] = filepath.Base(r)
	}
	return out
}

// protocolHeader injects the X-Bridge-Protocol header on every response so
// the iOS side can catch version mismatches at handshake time without
// parsing the body.
func protocolHeader(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Bridge-Protocol", strconv.Itoa(version.ProtocolVersion))
		next.ServeHTTP(w, r)
	})
}

// authed wraps a handler so it only runs if the request carries a valid
// bearer token. Unauthenticated requests return a 401 JSON error.
//
// On a successful Validate, the matched token's ID is fed into
// auth.Store.RecordClientVersion alongside the request's
// X-Client-Version header so the updater can later refuse auto-installs
// that would orphan an old iOS client. The pre-check against
// `tok.LastClientVersion` (cheap — `Validate` returns a copy under
// the same mutex it took for LastUsedAt) skips the whole lock-+
// linear-scan in the common case where the version hasn't changed
// since last request.
//
// The matched Token is otherwise not propagated to the wrapped handler
// — if a future endpoint needs to know which client it's serving,
// thread it through the request context here at that point.
func (s *Server) authed(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		raw := extractBearer(r)
		if raw == "" {
			writeError(w, http.StatusUnauthorized, "unauthorized", "missing bearer token")
			return
		}
		tok, ok := s.store.Validate(raw)
		if !ok {
			writeError(w, http.StatusUnauthorized, "unauthorized", "invalid bearer token")
			return
		}
		if cv := r.Header.Get("X-Client-Version"); cv != "" && cv != tok.LastClientVersion {
			s.store.RecordClientVersion(tok.ID, cv)
		}
		// Thread the validated token ID into the request context so
		// downstream middleware (today: rateLimitManifest for keying
		// the per-client manifest bucket) doesn't need to re-extract
		// or re-validate. Unwrapped via tokenIDFromContext.
		next(w, r.WithContext(withTokenID(r.Context(), tok.ID)))
	}
}

// extractBearer returns the token from an "Authorization: Bearer <token>"
// header, or "" if absent/malformed. The "Bearer" prefix is matched
// case-insensitively to match common client behavior.
func extractBearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if h == "" {
		return ""
	}
	const prefix = "bearer "
	if len(h) < len(prefix) {
		return ""
	}
	if !strings.EqualFold(h[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(h[len(prefix):])
}

// writeJSON serializes v as compact application/json with the given
// status code. Call this for every public-API success response.
//
// Compact (no SetIndent) because clients are exclusively machine
// consumers (iOS BridgeSourceClient, admin-tile XHR), and the largest
// responses — /v1/manifest pages and the polled /v1/upscale/stats —
// can run into tens of MB on big libraries where the indent overhead
// is unjustifiable. Admin endpoints (/api/...) live in
// internal/admin/handlers_api.go and run their own already-compact
// writeJSON.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	// Encode errors usually mean the client disconnected mid-
	// response (`broken pipe` / `connection reset by peer`).
	// These are common in mobile networks (iOS app backgrounded,
	// Wi-Fi roam, Tailscale relay drop) and not operator-
	// actionable — log at debug level so they're visible under
	// the per-request log when investigating, but don't bubble
	// up to error-level alerts. Same level the broken-pipe
	// branch of writeErrorLog inherits.
	//
	// Headers + status are already written by the time Encode
	// fails so we can't surface anything on the wire; the body
	// is half-written and the client will fail to decode. The
	// log line is the only diagnostic surface.
	if err := json.NewEncoder(w).Encode(v); err != nil {
		logger.Debug("writeJSON: encode failed",
			"status", status, "err", err)
	}
}

// writeError serializes an ErrorResponse with the given status + short code
// and human-readable message. Matches the table in PROTOCOL.md.
func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, ErrorResponse{Error: code, Message: message})
}

// writeErrorLog is writeError plus a structured-log record with the
// underlying error attached. Use this whenever the message exposed to
// the client would otherwise be the raw err.Error() string — record
// the diagnostic detail server-side under the per-request logger
// (request_id correlation) and respond with a sanitized, stable message
// the iOS surface can translate cleanly.
//
// userMsg should:
//   - For 4xx codes: be SPECIFIC and ACTIONABLE — the user can fix the
//     request. Examples: "request body must be JSON", "invalid path format".
//   - For 5xx codes: be GENERIC — the user can't act on the cause. Examples:
//     "the bridge encountered an internal error". NEVER include filesystem
//     paths, SQL fragments, or decoder internals in 5xx bodies; the typed
//     code + the server log already carry the diagnostic detail.
//
// Log level mirrors HTTP semantics: 4xx → Warn (client-side issue,
// expected on misbehaving callers and not actionable for the operator),
// 5xx → Error (server-side fault worth surfacing in alerting). Without
// this split, alerting on slog.Error counts would fire on every 400
// "bad JSON" and drown the real signals — caught by gemini bot review
// on PR #191.
//
// err is the original Go error, logged but not surfaced on the wire. May
// be nil — in that case the message is still emitted (so the helper works
// at sites where the failure is a state check, not a wrapped error).
func writeErrorLog(w http.ResponseWriter, r *http.Request, status int, code, userMsg string, err error) {
	if err != nil {
		l := LoggerFromContext(r.Context())
		switch {
		case status >= 500:
			l.Error("request failed", "code", code, "status", status, "err", err)
		default:
			l.Warn("request failed", "code", code, "status", status, "err", err)
		}
	}
	writeError(w, status, code, userMsg)
}
