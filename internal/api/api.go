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
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

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

// Server owns the http.Handler and the per-request state it needs.
type Server struct {
	cfg                  *config.Config
	store                *auth.Store
	resolver             *bridgefs.Resolver
	manifest             ManifestProvider
	artworkDirs          ArtworkDirProvider
	mbidProbe            MBIDProbe
	updater              UpdaterStatus
	sessions             SessionTracker
	pairing              *pairing.Store
	pairingRateLimiter   *pairingRateLimiter
	certNotAfter         time.Time            // zero when not wired (test harnesses)
	variantStore         VariantStore         // nil unless WithUpscale(true, vs) called
	upscaleEnabled       bool                 // mirrors cfg.Upscale.Enabled (and sox-probe outcome)
	upscaleEnqueuer      UpscaleEnqueuer      // nil unless WithUpscaleEnqueuer wired (Phase 2.5)
	upscaleStatsProvider UpscaleStatsProvider // nil unless WithUpscaleStats wired (v1.2 management UI)
	batchCoordinator     BatchCoordinator     // nil unless WithBatchCoordinator wired (v1.3 operator-driven upscale)
	eventBroker          *eventBroker         // nil disables /v1/events (back-compat for test harnesses)
	manifestRateLimiter  *manifestRateLimiter // per-token-ID token-bucket for /v1/manifest
	reachability         *reachabilityCache   // per-root probe TTL cache used by /v1/list, /v1/stat, /v1/health
	fingerprint          string
	startedAt            time.Time
}

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
	LookupVariant(sourcePath, variantID string) (*VariantRecord, error)
}

// VariantRecord is the minimum metadata the api needs to (a) decide
// freshness and (b) serve the sidecar bytes. Mirrors the on-disk
// columns the manifest package writes. Pointer return from
// LookupVariant lets `nil` distinguish "no such row" from "row
// exists but freshness fails" — caller can still surface a
// targeted 404 vs 410 from the same return shape.
type VariantRecord struct {
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
	BuildManifestPage(cursor string, limit int) (*ManifestPage, error)
	IsScanning() bool
	LastFullScan() time.Time
	TracksIndexed() int
	// PendingDeletions returns the total count of rows across tracks
	// and folders with missing_count > 0 — surfaced on ScanState. May
	// return 0 for an unwired or pre-v5 store; never errors today.
	PendingDeletions() int64
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
	HasTrackWithArtworkMBID(mbid string) bool
	HasTrackWithArtistMBID(mbid string) bool
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
	return &Server{
		cfg:                 cfg,
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
		return func() {}
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

// Handler returns the root http.Handler, pre-wrapped with the
// X-Bridge-Protocol middleware.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", s.health)
	mux.HandleFunc("GET /v1/list", s.authed(s.list))
	mux.HandleFunc("GET /v1/stat", s.authed(s.stat))
	mux.HandleFunc("GET /v1/read", s.authed(s.read))
	mux.HandleFunc("GET /v1/download", s.authed(s.download))
	// Manifest is the bridge's most expensive endpoint — 50k-track
	// libraries produce 100+ MB streams. authed() runs first so the
	// limiter can key its per-token bucket on the validated Token.ID.
	// Reversing the order would key on something pre-auth (IP, which is
	// unreliable behind Tailscale CGNAT) and is exactly the design we
	// ruled out at plan time.
	mux.HandleFunc("GET /v1/manifest", s.authed(s.rateLimitManifest(s.manifestHandler)))
	mux.HandleFunc("GET /v1/artwork/{mbid}", s.authed(s.artwork))
	mux.HandleFunc("GET /v1/artist-image/{mbid}", s.authed(s.artistImage))
	// Pairing routes are unauthenticated by design — pollSecret is the
	// auth where needed, the captured cert pin is the trust anchor for
	// the rest. Always registered so a 404 from the unregistered route
	// can't be confused with a successful "no such request" response;
	// when no Store is wired the handlers themselves return
	// `pairing_not_supported`.
	mux.HandleFunc("POST /v1/upscale", s.authed(s.upscaleRequest))
	mux.HandleFunc("GET /v1/upscale/stats", s.authed(s.upscaleStats))
	mux.HandleFunc("POST /v1/upscale/batch", s.authed(s.upscaleBatchSubmit))
	mux.HandleFunc("GET /v1/upscale/batches", s.authed(s.upscaleBatchList))
	mux.HandleFunc("DELETE /v1/upscale/batches/{id}", s.authed(s.upscaleBatchCancel))
	mux.HandleFunc("GET /v1/events", s.authed(s.events))
	mux.HandleFunc("POST /v1/pairing/requests", s.pairingRequest)
	mux.HandleFunc("GET /v1/pairing/{requestID}", s.pairingPoll)
	mux.HandleFunc("GET /v1/pairing/{requestID}/events", s.pairingEvents)
	mux.HandleFunc("DELETE /v1/pairing/{requestID}", s.pairingDelete)
	// Chain order matters:
	//   - `recoverer` innermost so it can catch panics in the mux/handlers
	//     and emit a sanitized 500 before unwinding.
	//   - `requestLogging` wraps recoverer so the captured status (e.g.
	//     500 on the recovery path) and request_id are emitted on the
	//     same line.
	//   - `protocolHeader` outermost is unchanged — header injection is
	//     independent of the request body and can't fail.
	return protocolHeader(requestLogging(recoverer(mux)))
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

	// CertNotAfter is the on-disk TLS certificate's `NotAfter` (UTC).
	// Lets iOS surface a "Bridge cert expires in X days — re-pair to
	// refresh" warning before the cert actually expires and TLS
	// handshakes start failing at Apple's ATS layer (Apple's 397-day
	// cap means operators must re-pair roughly annually). Additive
	// field; pre-bridge-with-this-PR servers omit it and iOS treats
	// absence as "no expiry info, never warn". Pointer (not bare
	// `time.Time`) because Go's `omitempty` doesn't treat the zero
	// time as empty — and emitting `0001-01-01T00:00:00Z` from a
	// test harness or a parse failure would actively confuse clients.
	CertNotAfter *time.Time `json:"certNotAfter,omitempty"`

	// Features advertises capability flags the server supports.
	// Additive over the wire (omitempty); iOS consults this list to
	// skip belt-and-braces recovery paths when the server already
	// provides the underlying guarantee. Stable string keys, never
	// repurpose. Pre-feature-flag bridges omit the field entirely;
	// iOS treats absence as "feature absent" so any client-side
	// recovery path runs unconditionally on those bridges.
	//
	// Current keys (kept alpha-sorted on the wire):
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
	scanState := ScanState{}
	if s.manifest != nil {
		scanState.IsScanning = s.manifest.IsScanning()
		scanState.LastFullScan = s.manifest.LastFullScan()
		scanState.TracksIndexed = s.manifest.TracksIndexed()
		scanState.PendingDeletions = s.manifest.PendingDeletions()
	}
	resp := HealthResponse{
		ProtocolVersion: version.ProtocolVersion,
		ServerVersion:   version.ServerVersion,
		LibraryName:     s.cfg.LibraryName,
		LibraryRoots:    libraryRootBasenames(s.cfg.LibraryRoots),
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
	// Capability flags — see HealthResponse.Features doc above for the
	// stable-key convention. Order kept stable alphabetically so any
	// client comparing /v1/health response fingerprints (e.g. for
	// content-equality short-circuit caches) doesn't churn on every
	// poll. New keys append in alpha order.
	// `upscaleCompleteEvents` gated on `s.upscaleEnabled` because iOS
	// will skip 4-of-5 ladder rungs when the flag is present and wait
	// for an `upscale.complete` SSE event — but a bridge with upscale
	// disabled has no transcode pool and `SetOnJobComplete` was never
	// called, so no event ever arrives. iOS would spend 10 minutes
	// silently waiting on a single backstop rung that may not even
	// land any variants. Mirrors how `resp.UpscaleEnabled` already
	// gates the feature visibility. (Greptile P1 on PR #187.)
	if s.upscaleEnabled {
		// `operatorDrivenUpscale` requires BOTH the upscale switch
		// AND a wired `batchCoordinator`. iOS keys off this flag to
		// hide the legacy wand UX — if we advertise it without the
		// coordinator, iOS hides its surface and the
		// /v1/upscale/batch endpoints surface 503 with nothing to
		// fall back to. Two-condition gate avoids that gap. Per
		// CodeRabbit major on PR #204 round 2.
		if s.batchCoordinator != nil {
			resp.Features = []string{"operatorDrivenUpscale", "upscaleCompleteEvents", "variantBumpsIndex"}
		} else {
			resp.Features = []string{"upscaleCompleteEvents", "variantBumpsIndex"}
		}
	} else {
		resp.Features = []string{"variantBumpsIndex"}
	}
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
// caching.
func (s *Server) reachableEndpoints() []string {
	_, portStr, err := net.SplitHostPort(s.cfg.ListenAddress)
	if err != nil {
		return nil
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 {
		return nil
	}
	return advertise.URLs(advertise.Params{
		Port:            port,
		CustomEndpoints: s.cfg.CustomEndpoints,
	})
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
	q := r.URL.Query()
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
		body, err := s.manifest.BuildManifestPage(cursor, limit)
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
		w.Header().Set("Content-Encoding", "gzip")
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
		gz = gzip.NewWriter(dw)
		bodyWriter = gz
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
		if closeErr := gz.Close(); closeErr != nil && writeErr == nil {
			writeErr = closeErr
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
		w.Header().Del("Content-Encoding")
		w.Header().Del("Vary")
	}

	if writeErr != nil {
		if !dw.written {
			// Headers haven't been committed yet. Strip the gzip-
			// negotiation headers BEFORE writeError so the JSON
			// error body isn't misinterpreted as gzip by the client
			// (URLSession would surface a transport error and
			// silently retry, masking the real DB failure).
			w.Header().Del("Content-Encoding")
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
