// Package admin serves the local-only web console for operating a running
// bridge instance — adding/removing library roots, pairing/revoking client
// devices, and surfacing scan + uptime stats.
//
// Trust model: the admin listener binds a loopback address (default
// 127.0.0.1:7789). Anyone on the host already has read access to the token
// store and sqlite DB, so adding an auth layer on top would be theatre.
// Loopback binding is enforced in two places — config.validateLoopbackAddress
// at load time and a RemoteAddr check in the Handler as a belt-and-braces
// runtime guard so a future misconfiguration (e.g. forgetting to bind the
// listener to 127.0.0.1) still refuses LAN traffic.
//
// Mutations (add root, pair device, revoke, settings edit) go through a
// single mutex on the Server so two operators hitting the UI simultaneously
// can't interleave a config.Save against each other. In practice the admin
// surface is single-user, so the mutex is a correctness guard rather than
// a performance concern.
package admin

import (
	"context"
	"crypto/tls"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"golang.org/x/sync/singleflight"

	"github.com/acoseac/1-bit-bridge/internal/adminauth"
	"github.com/acoseac/1-bit-bridge/internal/auth"
	"github.com/acoseac/1-bit-bridge/internal/backup"
	"github.com/acoseac/1-bit-bridge/internal/config"
	bridgefs "github.com/acoseac/1-bit-bridge/internal/fs"
	"github.com/acoseac/1-bit-bridge/internal/logging"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/pairing"
)

var logger = logging.Component("admin")

// adminMaxBodyBytes caps the JSON request body size every admin
// handler accepts. 1 MiB is generous for any current admin operation
// (token mint, settings patch, library-root edit, backup restore
// confirmation) but rejects multi-gigabyte payloads before the JSON
// decoder allocates. The admin listener binds loopback by default so
// the practical attack surface is thin, but defense-in-depth is
// cheap — a misbehaving local tool or future remote-admin bridge
// shouldn't be able to OOM the server with a large request body.
const adminMaxBodyBytes = 1 << 20

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static/*
var staticFS embed.FS

// Deps bundles the runtime state the admin console reads and mutates. All
// fields are required unless marked optional.
type Deps struct {
	CfgHolder *config.RuntimeConfig // live config snapshot holder
	CfgPath   string                // path to bridge.yaml, for Config.Save()
	Auth      *auth.Store           // token list / mint / revoke
	Manifest  *manifest.Store
	Scanner   *manifest.Scanner
	Resolver  *bridgefs.Resolver

	// Fingerprint is the self-signed TLS cert SHA-256 in colon-hex form.
	// Shown in the dashboard cert tile (the LAN pin operators verify) and
	// used as the pairing-QR fallback fingerprint.
	Fingerprint string

	// FingerprintForHost returns the cert fingerprint a device will capture
	// when it dials the given hostname (SNI), routed through the same SNI
	// cert switcher the listener serves with. The pairing-QR baker uses it
	// so the QR advertises the cert the device ACTUALLY sees — the public
	// domain's autocert/LE fingerprint for a public dial URL, the
	// self-signed LAN fingerprint otherwise. Pre-fix the QR always baked
	// the self-signed fingerprint, which a public-mode device (connecting
	// over the LE-served public endpoint) could never match. Wired in
	// cmd/bridge/main.go to certManager.FingerprintForServerName. Nil in
	// loopback wiring / tests — callers fall back to Fingerprint.
	FingerprintForHost func(host string) string

	// StartedAt is used to render uptime. Typically time.Now().UTC() at
	// the moment the serve command completes its init.
	StartedAt time.Time

	// Restart is called when the operator clicks "Restart now" on a
	// restart-required settings edit. Nil means os.Exit(0) — fine when
	// running under launchd/systemd which will relaunch.
	Restart func()

	// ScanCtx is the parent context for admin-triggered scans. serveCmd
	// should pass the same context it passes to scanner.RunPeriodic so a
	// shutdown cancels any admin-triggered scan along with the periodic
	// one. Nil defaults to context.Background() — only acceptable for
	// tests that don't care about goroutine cleanup.
	ScanCtx context.Context

	// Updater is the optional read-side of the update poller. Wired via
	// an adapter in cmd/bridge/main.go so this package doesn't import
	// internal/updater. Nil-safe — when absent, the dashboard's update
	// tile shows "not configured" and the /api/updates endpoint
	// returns the same fallback shape.
	Updater UpdateProvider

	// BackupSources is the resolved set of state-file paths the
	// admin's "Snapshot now" button hands to `backup.Snapshot`.
	// Injected from `cmd/bridge/main.go` via the same
	// `buildBackupSources` helper the CLI uses, so paths can't drift
	// between the two surfaces (CodeRabbit + Gemini both flagged the
	// prior local helper as a divergence risk on PR #44). Zero-value
	// is treated as "feature not wired" and the API endpoints return
	// 503 — fine for tests that don't construct the admin server
	// with backup wiring.
	BackupSources backup.Sources

	// Tailscale is the read+refresh side of the Tailscale HTTPS
	// auto-pilot. Nil-safe — when absent, the dashboard's Tailscale
	// tile shows "not configured" and the /api/tailscale endpoints
	// return the same fallback shape. Wired via an adapter in
	// cmd/bridge/main.go so this package doesn't import
	// internal/tailscale or the cmd/bridge auto-pilot type.
	Tailscale TailscaleProvider

	// Pairing backs the admin-approval pairing flow. Optional — when
	// nil, /api/pairing returns an empty list and the approve / decline
	// handlers reply 503 (so a misconfigured deployment surfaces a
	// distinct error rather than silently dropping operator clicks).
	// The iOS-facing /v1/pairing/* endpoints are gated by the api
	// package's own pairing wiring; both sides receive the same Store
	// from cmd/bridge/main.go.
	Pairing *pairing.Store

	// UpscalePrecheck probes whether the upscale feature can run on
	// this host (sox on PATH, --version returns within 2 s). Wired
	// to `transcode.PrecheckSox` via a closure in cmd/bridge/main.go
	// so this package doesn't import internal/transcode (matches the
	// MBIDProbe / UpdateProvider decoupling pattern). Nil-safe — when
	// absent the Settings response omits the `upscaleSoxAvailable`
	// field and the UI hides the warning banner.
	UpscalePrecheck func() error

	// UpscaleSoxFLAC reports whether the host sox build has FLAC
	// support (hasFLAC) and whether that could be determined at all
	// (known). The bridge forces `-t flac` for every conversion, so a
	// sox WITHOUT FLAC passes UpscalePrecheck but fails every job at
	// runtime — this lets the Settings tile warn about that narrower
	// case. Wired to a transcode.ProbeSox closure in cmd/bridge/main.go
	// (same decoupling as UpscalePrecheck). Nil-safe: absent → the
	// FLAC field is omitted and no FLAC warning renders. known=false
	// (closure absent, or `sox --help` unparseable) is treated
	// conservatively as "don't assert" rather than "FLAC missing".
	UpscaleSoxFLAC func() (hasFLAC, known bool)

	// UpscaleStats returns a snapshot of the long-lived
	// transcode pool's counters (workers, queue length, in-
	// flight jobs, lifetime totals). Wired via a closure in
	// cmd/bridge/main.go so the admin package stays decoupled
	// from internal/transcode. The closure returns nil when
	// the feature is off (Pool isn't instantiated); the admin
	// endpoint then omits the `pool` field instead of
	// surfacing zero-padded clutter ("0/0 queue, 0 inflight"
	// would suggest the pool exists but is idle, which is
	// semantically wrong).
	UpscaleStats func() *UpscalePoolStats

	// UpscaleBusy is a CHEAP "is the pool actively processing" probe
	// (inflight or queued > 0) — atomic counters + a map-len, NO DB. The
	// SSE loop uses it to gate the live worker grid onto the fast (500 ms)
	// tick WHILE a batch runs, so sub-5s jobs are visible at per-second
	// resolution; idle bridges fall back to the 5 s medium tick (no cost).
	// Nil-safe: absent → never fast-tick the upscale frame.
	UpscaleBusy func() bool

	// ArtistImageMBIDs enumerates the artist MBIDs with a cached image
	// file on disk (lowercase-keyed set). Wired to a closure over
	// `enrich.CachedArtistImageMBIDs(artworkDir)` in cmd/bridge/main.go
	// (UpscalePrecheck decoupling pattern — admin never imports
	// internal/enrich). Feeds the dashboard's artist-image have/missing
	// coverage stats; called only inside the 60s-TTL enrichment-meta
	// snapshot, so the directory read stays off any hot path. Nil-safe:
	// absent → the artistImages coverage field is omitted.
	ArtistImageMBIDs func() (map[string]struct{}, error)

	// HarvestForceSubmit zeroes the Atlas bulk-harvest client's
	// last-submit stamp so its next tick (≤ PollInterval) re-submits the
	// full library — Atlas then re-attempts every unresolved bio /
	// description (submission is idempotent server-side). Returns whether
	// the nudge was actually applied. Wired in cmd/bridge/main.go only
	// when the harvest client is running; nil (or false) surfaces as
	// `harvestResubmitted: false` on the retry response.
	HarvestForceSubmit func() bool

	// FingerprintForget drops the in-process AcoustID outcome cache for a
	// path prefix ("" = the whole library), returning how many entries went.
	//
	// Paired with Store.ClearAcoustIDSuppression*: the persisted markers (a
	// no-match verdict, or an apply-time tag veto) and this cache are two
	// layers of the SAME suppression, and the sweeper consults the cache
	// FIRST. Clearing only the database would leave every file answered during
	// the current process suppressed until a restart, so "Retry missing" would
	// appear to work while doing nothing for exactly the files the operator
	// just watched fail.
	//
	// Wired in cmd/bridge/main.go only when fingerprinting is enabled; nil is
	// a no-op, which is correct — with no sweeper there is no cache to clear.
	FingerprintForget func(prefix string) int

	// EnrichSkipReasons returns the enricher's process-lifetime tally of
	// why it stopped short, keyed by bounded reason (no_search_terms /
	// no_mb_match / mb_error). Wired to enrich.Enricher.SkipReasons in
	// cmd/bridge/main.go — same decoupling as ArtistImageMBIDs, so admin
	// still never imports internal/enrich. Nil-safe: absent → the
	// skipReasons field is omitted from the misses response.
	EnrichSkipReasons func() map[string]int64

	// ArtworkPath / ArtistImagePath / BookletPath resolve cache-file
	// paths for the inspector's loopback byte-serving routes
	// (/api/library/artwork|artist-image|booklet). Closures over
	// enrich.ArtworkCachePath / enrich.ArtistImagePath /
	// api.BookletPath in cmd/bridge/main.go — admin imports neither
	// package (ArtistImageMBIDs decoupling precedent). The handlers
	// validate ids against the same bounded-alphabet regexes as the
	// /v1 twins BEFORE any path join, so the closures only ever see
	// traversal-free values. Nil-safe: absent → the routes 404 and
	// the UI falls back to icon-only tiles / no booklet links.
	ArtworkPath     func(mbid string, size int) string
	ArtistImagePath func(mbid string) string
	BookletPath     func(mbid string) string

	// BookletNudge prioritizes one release on the harvest booklet
	// fetch sweep (harvest client NudgeBookletFetch — non-blocking,
	// 32-buffered, drops are benign: the row still drains via
	// BookletsToFetch in order). Nil when the harvest client isn't
	// running.
	BookletNudge func(mbid string)

	// AnalysisActive reports the LIVE runtime state of the audio-
	// analysis feature — i.e. the startup-computed `analysisActive`
	// (config flag AND sox-precheck outcome), NOT the persisted config
	// flag. The two diverge after a restart-required PATCH: the config
	// holder reflects the new value immediately, but the runtime stays
	// at its startup value until restart. Wiring this lets
	// /api/analysis/stats.enabled agree with /v1/health's `waveform`
	// flag (also startup-wired). Nil-safe: when absent the handler
	// falls back to the persisted config + sox derivation (test
	// harnesses). Mirrors the intent of the upscale tile's pool-derived
	// `enabled`. Wired in cmd/bridge/main.go.
	AnalysisActive func() bool

	// AnalysisPoolStats returns a snapshot of the long-lived
	// analyze.Pool's counters. Reuses the UpscalePoolStats DTO —
	// analyze.PoolStats' field set matches transcode.PoolStats
	// one-for-one (ActiveWorkers stays empty; the analysis pool has no
	// per-worker grid). Same closure decoupling + nil semantics as
	// UpscaleStats: nil when the feature is off (pool not
	// instantiated), and the `pool` field is omitted rather than
	// zero-padded. Wired in cmd/bridge/main.go.
	AnalysisPoolStats func() *UpscalePoolStats

	// AnalysisSweep returns the serve-side auto-analysis sweeper's
	// lifecycle snapshot (running / last sweep timestamps + counts /
	// next due). Ephemeral "since process start" state recorded by
	// cmd/bridge's sweepStatus; nil-safe — absent omits the `sweep`
	// field.
	// DoctorRun executes the preflight checks and returns the report,
	// wired by cmd/bridge so internal/admin needs no dependency on
	// internal/doctor and the Deps assembly (config paths, roots, ports,
	// feature flags) lives in ONE place instead of being duplicated here
	// where it would drift from the CLI's.
	//
	// The wiring passes the ports this process actually bound, so the port
	// checks are answered from knowledge rather than deduced from a bind
	// probe — which is both faster and immune to the capability/dumpable=0
	// attribution problem that defeats every unprivileged probe.
	//
	// Nil disables the console's doctor panel (the handler says so rather
	// than erroring).
	DoctorRun func(ctx context.Context) *DoctorReport

	AnalysisSweep func() *AnalysisSweepState

	// TriggerAnalysisSweep queues an out-of-band auto-analysis sweep by
	// nudging the sweeper's buffered-1 channel (non-blocking send,
	// coalescing — the "Analyze now" button). It only signals the
	// already-bgWriters-joined sweeper goroutine, so no goroutine or
	// WaitGroup concerns live on the admin side. Nil when analysis is
	// inactive; the endpoint then 503s.
	TriggerAnalysisSweep func() bool

	// AnalysisSchemaVersion is analyze.WaveformSchemaVersion, passed by
	// value so neither this package nor internal/manifest imports
	// internal/analyze (the existing import-avoidance around the
	// waveforms dir). Feeds the coverage query's fresh-vs-stale split;
	// empty disables the coverage tile.
	AnalysisSchemaVersion string

	// FingerprintState returns the acoustic-fingerprint job's full admin
	// snapshot: config flag, runtime active/degraded verdict, and the
	// sweeper's lifecycle recorder. Wired in cmd/bridge/main.go for
	// every serve (even feature-off — the card then explains WHY it's
	// off). Nil-safe: absent omits the `fingerprint` field (test
	// harnesses).
	FingerprintState func() *FingerprintJobState

	// TriggerFingerprintSweep — the fingerprint twin of
	// TriggerAnalysisSweep (the "Sweep now" button). Nil when the
	// feature is inactive; the endpoint then 503s.
	TriggerFingerprintSweep func() bool

	// TriggerDuplicatesPass nudges the duplicates stamping sweeper
	// (cmd/bridge runDuplicatesSweeper) to re-evaluate suppression under
	// the CURRENT duplicates.filter policy — the hot-apply half of the
	// settings PATCH: flipping the policy fires this instead of setting
	// RestartRequired. Coalescing non-blocking send (nudgeTriggerClosure);
	// nil when unwired (test harnesses) — the PATCH then still persists
	// and the next full scan applies the policy.
	TriggerDuplicatesPass func() bool

	// SmartMixRun / BackupRun expose the smart-mix regenerator's and
	// backup ticker's last/next-run recorders for the Jobs page cards.
	// Nil-safe: absent omits the field (feature off or test harness).
	SmartMixRun func() *JobRunState
	BackupRun   func() *JobRunState

	// DuplicatesSweepRun exposes the duplicates stamping sweeper's
	// recorder for the Jobs card. Nil-safe: absent omits the run field
	// (the card still renders policy + summary numbers).
	DuplicatesSweepRun func() *JobRunState

	// ProjectedSize estimates the on-disk size of a FLAC
	// variant produced from (sourceSize, sourceRate, sourceBits)
	// at (targetRate, targetBits). Wired to
	// `transcode.ProjectedSize` (with `DefaultCompressionFactor`
	// baked in) via a closure in cmd/bridge/main.go. Mirrors the
	// UpscaleStats / UpscalePrecheck pattern: what the closure buys
	// is decoupling from the live Pool's RUNTIME STATE (nil here ==
	// "feature off", which this package can then report without
	// knowing why), not import avoidance — internal/admin does import
	// internal/transcode for pure functions + consts
	// (transcode.OutputDirFor, RequiredBytesWithMargin,
	// DefaultDiskSafetyMargin).
	//
	// Nil when upscale is disabled — the projection endpoint
	// surfaces a clean 503 in that case.
	ProjectedSize func(sourceSize int64, sourceRate, sourceBits, targetRate, targetBits int) int64

	// AvailableDiskSpace probes free bytes on the volume holding
	// `dir`. Wired to `transcode.AvailableDiskSpace`. Nil-safe
	// alongside ProjectedSize (both wired together when upscale
	// is enabled, both nil when disabled).
	AvailableDiskSpace func(dir string) (int64, error)

	// OptimizeEligible is the per-track gate for kind="optimize"
	// projections / batches. Wired to `transcode.OptimizeEligible`
	// in cmd/bridge/main.go. Tracks failing the gate (DSD, lossy
	// codecs, or already-at-CarPlay-floor like 16/44.1) fold into
	// the projection's `unknownFormatFiles` counter so the UI's
	// "X tracks skipped" copy reconciles with the JSON payload.
	// Nil-safe: when absent the projection endpoint serves only
	// the upscale kind and surfaces a 503 for kind=optimize.
	OptimizeEligible func(sourcePath, codec string, sourceRate, sourceBits int) bool

	// TargetRateForOptimize returns the family-preserving target
	// rate (44100 or 48000) for a given source rate. Wired to
	// `transcode.TargetRateForOptimize` in cmd/bridge/main.go.
	// Called per-track inside the optimize projection loop so
	// mixed-family folders (44.1k + 96k FLAC sharing one album)
	// produce honest per-track size estimates.
	// Nil-safe alongside OptimizeEligible.
	TargetRateForOptimize func(sourceRate int) int

	// BatchCoordinator is the v1.3 admin Library Inspector's gateway
	// to the transcode.Coordinator. Wired to a closure-based adapter
	// in cmd/bridge/main.go (same decoupling pattern as
	// UpscaleStats / UpscaleEnqueuer). Nil-safe — when absent the
	// admin Library Inspector renders the folder tree but the
	// "Upscale this folder" trigger surfaces a 503.
	BatchCoordinator AdminBatchCoordinator

	// VariantDeleter is the admin-side gateway to the same
	// list/unlink/DeleteVariant/SSE-publish pipeline that powers
	// the public `DELETE /v1/upscale/variants` endpoint. Wired in
	// cmd/bridge to an adapter around `api.Server.RunVariantDelete`
	// so the admin console and the iOS app go through exactly one
	// code path on the way out — no risk of drift between the two
	// destructive surfaces. Nil-safe: when absent (upscale disabled
	// on this bridge OR pre-feature build) the admin handler
	// surfaces 503 service_unavailable, matching the
	// `BatchCoordinator == nil` shape on the same page.
	VariantDeleter AdminVariantDeleter

	// IsSupervised reports whether the current process is running
	// under launchd / systemd / Windows SCM — i.e. whether
	// `os.Exit(0)` will trigger an automatic relaunch. Threaded
	// through to `settingsResponse.IsSupervised` so the admin UI
	// can show "Restart now" (auto-relaunch promised) versus
	// "Stop now (manual restart required)" (operator must run the
	// service back up themselves). Wired in `cmd/bridge/main.go`
	// from `supervision.IsSupervised()`. Defaults to false in
	// test harnesses, which yields the conservative — never-
	// promise-relaunch — UI wording, never the lying one.
	IsSupervised bool

	// AdminAuth is the admin credentials + session store. Required
	// in public mode (the boundary middleware refuses to start
	// without it); ignored in loopback mode where there's no auth
	// layer at all. Wired in cmd/bridge/main.go.
	//
	// When non-nil AND IsPublic() is true, the session middleware
	// is active: pages require a valid session cookie and the API
	// returns JSON 401 on missing/invalid credentials. When nil,
	// the historical loopback-only behaviour is preserved.
	AdminAuth *adminauth.Store

	// LoginLimiter is the failed-login rate limiter. Required
	// alongside AdminAuth in public mode; ignored in loopback. The
	// limiter's own goroutine is managed by its NewRateLimiter /
	// Stop API — admin doesn't own its lifecycle.
	LoginLimiter *adminauth.RateLimiter

	// TLSConfig, when non-nil, makes Serve wrap the underlying
	// net.Listener in tls.NewListener using this config — i.e. the
	// admin console serves HTTPS instead of plain HTTP. Required
	// when the bridge terminates TLS for the admin console
	// itself (public mode + autocert.enabled). Nil when:
	//   - loopback mode (historical no-TLS contract); OR
	//   - public mode with AdminTLSTerminatedByProxy=true
	//     (reverse proxy fronts TLS, bridge serves plain HTTP on
	//     a private interface).
	//
	// Wired in cmd/bridge/main.go via certManager.AdminTLSConfig().
	TLSConfig *tls.Config

	// AutocertStatus surfaces a per-request snapshot of the
	// autocert.Manager's live state for the dashboard tile.
	// Nil-safe — when absent the tile renders "not configured"
	// and /api/autocert/status returns the same disabled shape.
	// Wired via a closure in cmd/bridge/main.go so this package
	// doesn't import internal/tlsacme.
	AutocertStatus func() AutocertStatusSnapshot

	// MDNSToggle is the hot-reload callback for the mDNS
	// advertiser. The Settings PATCH handler fires it after
	// persisting a `mdns.enabled` change, with the new resolved
	// value. main.go wires it to its mdnsLifecycle.Set; nil
	// (test wiring) means the change persists but the runtime
	// state doesn't flip — operator restart picks it up.
	MDNSToggle func(enabled bool)

	// TailscaleDisable is the hot-reload callback for the
	// Tailscale auto-pilot. The Settings PATCH handler fires it
	// on the any→disabled transition so the running auto-pilot
	// cancels its ctx + clears the LE cert from certManager.
	// Transitions INTO cli/tsnet still require restart (auto-
	// pilot + listener composition need a clean boot).
	TailscaleDisable func()

	// UPnPUpstream is the admin-side gateway to the upstream
	// MediaServer feature (bridge PR E). Wired to an adapter around
	// the cmd/bridge upnpUpstreamLifecycle so the Devices page can
	// surface per-server discovery state + ingest stats + a
	// force-rescan button. Nil-safe — when absent (operator hasn't
	// enabled `upnpUpstream.enabled` in bridge.yaml), the relevant
	// admin endpoints return a stable error envelope the frontend
	// uses to hide the UPnP card.
	UPnPUpstream UPnPUpstreamProvider
}

// AutocertStatusSnapshot mirrors tlsacme.Status for the admin
// surface, defined here so the admin package stays decoupled from
// internal/tlsacme. Wired via the AutocertStatus closure.
type AutocertStatusSnapshot struct {
	Domain      string    `json:"domain,omitempty"`
	CertPresent bool      `json:"certPresent"`
	NotAfter    time.Time `json:"notAfter,omitempty"`
	LastError   string    `json:"lastError,omitempty"`
	LastCheck   time.Time `json:"lastCheck,omitempty"`
}

// AdminBatchCoordinator is the admin-side interface the Library
// Inspector's "Upscale this folder" button + the Jobs page consume.
// Implemented by an adapter in cmd/bridge/main.go around
// transcode.Coordinator — same UpscaleEnqueuer / UpscaleStats
// decoupling pattern. The methods mirror `api.BatchCoordinator`
// but use admin-package wire shapes (`AdminBatchRow`, etc.) so
// the admin package stays free of internal/api.
//
// `Submit` takes a `context.Context` so the HTTP request's
// cancellation propagates through to the coordinator and any
// downstream listings the coordinator does internally
// (manifest projection walk). Per Gemini high on PR #202.
//
// `SubmitOptimize` is the kind="optimize" sibling — it enrolls a
// path into a CarPlay-optimized batch (16-bit, family-preserving
// 44.1k/48k FLAC). The target params are auto-derived per-track
// (see transcode.TargetRateForOptimize) so there's no rate/bits
// argument; the result struct's `TargetRate`/`TargetBits` reflect
// the most common per-track resolution (mixed-family scopes use
// 0 to signal "varies"). Added in the Library Inspector tile-redesign
// PR alongside the per-tile "Generate CarPlay-optimized variants"
// affordance.
type AdminBatchCoordinator interface {
	Submit(ctx context.Context, libraryRelPath string, targetRate, targetBits int) (AdminBatchSubmitResult, error)
	SubmitOptimize(ctx context.Context, libraryRelPath string) (AdminBatchSubmitResult, error)
	Cancel(idHex string) error
	ListBatches(limit int) ([]AdminBatchRow, error)
	Throughput() AdminBatchThroughput
}

// AdminBatchSubmitResult is the wire shape returned by the admin
// Submit endpoint. Mirrors transcode.SubmitResult + api.BatchSubmitResult
// field-for-field but lives here so the admin templates can
// JSON-decode without importing either of those packages.
type AdminBatchSubmitResult struct {
	BatchID            string `json:"batchID"`
	Path               string `json:"path"`
	TargetRate         int    `json:"targetRate"`
	TargetBits         int    `json:"targetBits"`
	TotalFiles         int    `json:"totalFiles"`
	AlreadyCovered     int    `json:"alreadyCovered"`
	ProjectedSizeBytes int64  `json:"projectedSizeBytes"`
	AvailableBytes     int64  `json:"availableBytes"`
	EnqueuedCount      int    `json:"enqueuedCount"`
}

// AdminBatchRow is the per-row wire shape returned by the admin
// list endpoint (Jobs page consumer).
//
// `CreatedAt` / `UpdatedAt` are `time.Time` (RFC 3339-encoded on
// the wire) rather than int64 nanoseconds. JavaScript's Number
// type can't safely represent 64-bit ns timestamps — values above
// 2^53 round, causing the Jobs page to render a date a few
// hundred milliseconds off. The string form parses safely via
// `new Date(...)`. Per Gemini high on PR #202.
type AdminBatchRow struct {
	ID             string `json:"id"`
	Path           string `json:"path"`
	TargetRate     int    `json:"targetRate"`
	TargetBits     int    `json:"targetBits"`
	Status         string `json:"status"`
	TotalFiles     int    `json:"totalFiles"`
	ProcessedFiles int    `json:"processedFiles"`
	FailedFiles    int    `json:"failedFiles"`
	// SkippedFiles is the count of projection-eligible tracks that
	// Submit/SubmitOptimize did NOT enqueue (already at target,
	// lossy, DSD, unknown format, etc.). Distinct from FailedFiles
	// (per-job SoX failures during the run). The Jobs page renders
	// "X tracks skipped" as a sub-line whenever this is > 0.
	SkippedFiles int       `json:"skippedFiles,omitempty"`
	Error        string    `json:"error,omitempty"`
	CreatedAt    time.Time `json:"createdAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
}

// AdminBatchThroughput is the wire shape returned by the
// throughput proxy. Mirrors transcode.ThroughputSnapshot.
type AdminBatchThroughput struct {
	JobsPerHour float64 `json:"jobsPerHour"`
	EtaSeconds  float64 `json:"etaSeconds"`
	Samples     int     `json:"samples"`
}

// AdminBatchInsufficientDiskSpace is the typed-error shape
// returned when the coordinator's pre-flight refuses a submit.
type AdminBatchInsufficientDiskSpace struct {
	ProjectedBytes int64
	RequiredBytes  int64
	AvailableBytes int64
}

func (e *AdminBatchInsufficientDiskSpace) Error() string {
	return "upscale batch: insufficient disk space"
}

// AdminVariantDeleter is the admin-side interface the
// `DELETE /api/upscale/variants` handler consumes. The single
// `Delete` method takes a parsed admin-package request and
// returns a parsed admin-package response — the adapter in
// cmd/bridge/main.go translates to/from the `api` package's
// `VariantDeleteRequest` / `VariantDeleteResponse` shapes so the
// admin package stays free of `internal/api` (same decoupling
// pattern as `AdminBatchCoordinator`).
//
// Errors:
//   - `ErrAdminVariantDeleterUnavailable` (sentinel) → handler emits
//     503 service_unavailable. Distinguishes "feature off on this
//     bridge" from "feature on but listing failed".
//   - Any other error → handler emits 500 internal, with the error
//     surface text in the response body.
type AdminVariantDeleter interface {
	Delete(ctx context.Context, req AdminVariantDeleteRequest) (AdminVariantDeleteResponse, error)
}

// AdminVariantDeleteRequest is the parsed-and-validated input to
// the admin handler's call into the deleter. Exactly one of `All` /
// `Prefix` / `Path` is set on any valid request; the handler
// short-circuits the unscoped form behind a typed-phrase
// confirmation in the UI (matches the `bridge artwork --gc`
// `--confirm` CLI convention) plus the existing `?confirm=true`
// query gate for direct callers.
type AdminVariantDeleteRequest struct {
	All    bool
	Prefix string
	Path   string
	// Kind narrows the deletion to one variant kind ("upscale" /
	// "optimize"); empty preserves pre-feature behaviour (deletes
	// BOTH kinds matching the path scope). Wire-shape mirror of
	// `api.VariantDeleteRequest.Kind`; the adapter in
	// cmd/bridge/main.go translates the field across the
	// admin↔api boundary. The Library Inspector's per-kind drawer
	// Delete buttons set this to scope the destructive action.
	Kind string
}

// AdminVariantDeleteResponse is the wire shape returned on
// success. Same field set as the public endpoint's response so
// the admin UI can render "deleted N variants, freed Y bytes"
// directly from this body.
type AdminVariantDeleteResponse struct {
	DeletedCount int      `json:"deletedCount"`
	FreedBytes   int64    `json:"freedBytes"`
	DeletedPaths []string `json:"deletedPaths"`
}

// ErrAdminVariantDeleterUnavailable is the sentinel error the
// `DELETE /api/upscale/variants` handler matches against to emit
// 503 — wired to wrap `api.ErrVariantDeleteUnavailable` in the
// cmd/bridge adapter so the admin package stays decoupled from
// the api package's error definitions.
var ErrAdminVariantDeleterUnavailable = errors.New("variant deleter not wired")

// UpscalePoolStats mirrors `transcode.PoolStats` field-for-
// field but lives here so the admin package compiles without
// importing internal/transcode. The wiring closure in
// cmd/bridge/main.go translates between the two value types.
type UpscalePoolStats struct {
	Workers  int    `json:"workers"`
	QueueCap int    `json:"queueCap"`
	QueueLen int    `json:"queueLen"`
	Inflight int    `json:"inflight"`
	Enqueued uint64 `json:"enqueued"`
	Done     uint64 `json:"done"`
	Failed   uint64 `json:"failed"`
	// ActiveWorkers is the live per-worker grid (one entry per worker
	// slot, Busy=false for idle). Drives the Jobs page's "Workers" panel.
	// Carries StartedAtUnixMs (not a ticking elapsed) so the SSE upscale
	// frame stays diff-stable while a job runs — the browser ticks the
	// elapsed display locally.
	ActiveWorkers []ActiveWorkerView `json:"activeWorkers,omitempty"`
}

// ActiveWorkerView is one worker's live slot in the upscale pool — the
// SSE worker grid on the Jobs page renders one row per entry. Mirror of
// transcode.ActiveJobView; cmd/bridge maps between the two so
// internal/admin doesn't import internal/transcode (same decoupling as
// the UpscaleStats closure).
type ActiveWorkerView struct {
	WorkerID         int    `json:"workerId"`
	Busy             bool   `json:"busy"`
	SourceRel        string `json:"sourceRel,omitempty"`
	SourceSampleRate int    `json:"sourceSampleRate,omitempty"`
	SourceBits       int    `json:"sourceBits,omitempty"`
	TargetSampleRate int    `json:"targetSampleRate,omitempty"`
	TargetBits       int    `json:"targetBits,omitempty"`
	Quality          string `json:"quality,omitempty"`
	Kind             string `json:"kind,omitempty"`
	StartedAtUnixMs  int64  `json:"startedAtUnixMs,omitempty"`
}

// AnalysisSweepState is the auto-analysis sweeper's lifecycle snapshot
// — the `sweep` object on /api/analysis/stats and the SSE `analysis`
// frame. Timestamps are *time.Time so `omitempty` genuinely drops the
// zero value (the PR #68 omitempty-time.Time lesson: a bare time.Time
// would emit "0001-01-01T00:00:00Z"). NextDueAt moves once per tick
// arm — deliberately NOT a ticking countdown, which would churn the SSE
// diff every frame (the PR #107 UptimeSec lesson); the browser computes
// the countdown locally.
type AnalysisSweepState struct {
	Running        bool                 `json:"running"`
	LastStartedAt  *time.Time           `json:"lastStartedAt,omitempty"`
	LastFinishedAt *time.Time           `json:"lastFinishedAt,omitempty"`
	NextDueAt      *time.Time           `json:"nextDueAt,omitempty"`
	Last           *AnalysisSweepCounts `json:"last,omitempty"`
}

// AnalysisSweepCounts is the last completed sweep's candidate
// breakdown — the exact per-run numbers from
// collectAnalysisCandidates (the SQL coverage tile is the approximate
// whole-library view; these are the sweeper's own truth).
type AnalysisSweepCounts struct {
	Total          int  `json:"total"`
	UpToDate       int  `json:"upToDate"`
	DSDExcluded    int  `json:"dsdExcluded"`
	ZeroByte       int  `json:"zeroByte"`
	Missing        int  `json:"missing"`
	Enqueued       int  `json:"enqueued"`
	QueueSaturated bool `json:"queueSaturated,omitempty"`
}

// FingerprintJobState is the acoustic-fingerprint card's snapshot on
// /api/jobs. Enabled is the config flag; Active the runtime verdict
// (flag AND fpcalc AND AcoustID key at startup); DegradedReason the
// bounded key explaining an Enabled-but-inactive state
// ("fpcalc_missing" / "no_api_key"). Lifecycle fields follow
// AnalysisSweepState's shape and rules (pointer timestamps, no ticking
// countdowns).
type FingerprintJobState struct {
	Enabled        bool                    `json:"enabled"`
	Active         bool                    `json:"active"`
	DegradedReason string                  `json:"degradedReason,omitempty"`
	Running        bool                    `json:"running"`
	LastStartedAt  *time.Time              `json:"lastStartedAt,omitempty"`
	LastFinishedAt *time.Time              `json:"lastFinishedAt,omitempty"`
	NextDueAt      *time.Time              `json:"nextDueAt,omitempty"`
	Last           *FingerprintSweepCounts `json:"last,omitempty"`
}

// FingerprintSweepCounts is the last completed fingerprint sweep's
// outcome: candidates examined, tracks the audio identified, and rows
// re-queued for the enricher.
type FingerprintSweepCounts struct {
	Candidates int `json:"candidates"`
	Resolved   int `json:"resolved"`
	Requeued   int `json:"requeued"`
}

// JobRunState is the minimal last/next-run shape shared by background
// jobs that don't carry a per-run breakdown (smart-mix regenerator,
// backup ticker). Same timestamp rules as AnalysisSweepState.
type JobRunState struct {
	Running        bool       `json:"running"`
	LastStartedAt  *time.Time `json:"lastStartedAt,omitempty"`
	LastFinishedAt *time.Time `json:"lastFinishedAt,omitempty"`
	NextDueAt      *time.Time `json:"nextDueAt,omitempty"`
}

// TailscaleProvider is the read+refresh side of the Tailscale auto-pilot
// the admin tile reads. The adapter in cmd/bridge/main.go wraps the
// process-scoped autopilot so the wire shape lives entirely in this
// package — keeps internal/admin decoupled from cmd/bridge's
// implementation choices (background renewer cadence, mint trigger
// strings, etc.).
type TailscaleProvider interface {
	Status() TailscaleStatus
	RefreshNow(ctx context.Context) TailscaleStatus
}

// TailscaleStatus is the JSON shape /api/tailscale/status returns.
// Mirrors `cmd/bridge/tailscaleStatus` but lives here so the admin
// package compiles without importing cmd/bridge.
//
// Optional time fields are pointers (Qodo on PR #102): a non-pointer
// `time.Time` with `json:",omitempty"` still serialises the zero
// value `"0001-01-01T00:00:00Z"` because `omitempty` doesn't recognise
// time-zero. Pointer form honours `omitempty` correctly. Matches the
// `tokenRow.ExpiresAt *time.Time` precedent.
//
// `MagicDNSURL` is the operator-facing bridge URL on the magic-DNS
// endpoint, including the configured listen port (NOT a hard-coded
// `:7788` — operators using non-default `cfg.ListenAddress` need the
// right URL surfaced for manual recovery, CodeRabbit on PR #102).
type TailscaleStatus struct {
	CLIAvailable bool `json:"cliAvailable"`
	// Mode is the configured tailscale.mode ("cli" / "tsnet" /
	// "disabled"). Threaded into the snapshot so the admin tile can
	// distinguish "operator set mode=disabled" from "mode=cli but the
	// tailscale CLI isn't on this host" — the two used to collapse into
	// an identical misleading "Disabled" badge because the tile read
	// only runtime CLI detection. Empty only in legacy snapshots.
	Mode string `json:"mode,omitempty"`
	// PublicMode reports whether the bridge is in public/autocert mode,
	// where the Tailscale auto-pilot doesn't apply at all. The tile
	// greys out / hides itself in that case rather than showing a
	// "Disabled" badge that reads as a misconfiguration.
	PublicMode        bool       `json:"publicMode"`
	NodeName          string     `json:"nodeName,omitempty"`
	MagicDNSName      string     `json:"magicDNSName,omitempty"`
	HTTPSCertsEnabled bool       `json:"httpsCertsEnabled"`
	CertPresent       bool       `json:"certPresent"`
	CertNotAfter      *time.Time `json:"certNotAfter,omitempty"`
	CertPath          string     `json:"certPath,omitempty"`
	MagicDNSURL       string     `json:"magicDNSURL,omitempty"`
	LastError         string     `json:"lastError,omitempty"`
	LastChecked       *time.Time `json:"lastChecked,omitempty"`

	// BackendState is the upstream `ipnstate.Status.BackendState`
	// surfaced for consumers that need to gate on whether the embedded
	// tsnet node is fully up. One of: "NoState", "NeedsLogin",
	// "NeedsMachineAuth", "Stopped", "Starting", "Running". Empty when
	// the source is the CLI auto-pilot (cli-mode adapter) — that path
	// drives its own check via `CLIAvailable` + `MagicDNSName` for
	// existing consumers; new consumers consult this explicit field.
	BackendState string `json:"backendState,omitempty"`

	// TailscaleIPs are the tsnet node's tailnet-assigned addresses
	// (CGNAT 100.x IPv4 + ULA fd7a:115c:a1e0::/48 IPv6) as strings,
	// surfaced so the api layer can advertise them in
	// `/v1/health.endpoints` for iOS clients that need an IP fallback
	// when MagicDNS resolution misses. Empty in cli-mode; populated
	// only by the embedded-tsnet path. `omitempty` keeps the existing
	// admin-tile JSON shape unchanged when the source doesn't populate.
	TailscaleIPs []string `json:"tailscaleIPs,omitempty"`
}

// UpdateProvider is the read-side of the updater used by the admin
// console. Implemented by the adapter in cmd/bridge/main.go around
// internal/updater.Updater. CheckNow takes a context so a slow GitHub
// response can be cancelled if the operator's browser disconnects.
//
// Install and Rollback return errors the admin handler classifies
// via errors.Is against the package-level sentinels below
// (ErrNoUpdate / ErrActiveSessions / ErrInstallNotSupported /
// ErrPathNotWritable). The adapter is responsible for mapping
// internal/updater's typed errors onto these admin-facing sentinels
// so this package stays decoupled from internal/updater's API.
type UpdateProvider interface {
	Status() UpdateStatus
	CheckNow(ctx context.Context) UpdateStatus
	Install(ctx context.Context, force bool) (UpdateStatus, error)
	Rollback(force bool) error
}

// Sentinel errors for the install / rollback paths. Defined in
// admin/ rather than re-exported from internal/updater so the wire
// shape lives entirely in this package — handlers_api.go classifies
// via errors.Is, not string-substring (which was the original
// implementation; PR #42 review flagged the fragility).
//
// The adapter in cmd/bridge/main.go translates internal/updater's
// equivalent errors to these via fmt.Errorf("%w: %s", ErrXxx, ...)
// so the admin handler can switch on errors.Is without reaching
// into the updater package.
var (
	ErrUpdateNoUpdate        = errors.New("no update available")
	ErrUpdateActiveSessions  = errors.New("active downloads in flight")
	ErrUpdateInstallInFlight = errors.New("an install is already in progress")
	ErrUpdateNotSupported    = errors.New("self-install not supported on this platform")
	ErrUpdatePathNotWritable = errors.New("binary path not writable")
)

// UpdateStatus is the wire shape /api/updates returns. Decoupled from
// internal/updater so the admin package compiles without importing it.
//
// CanInstall is the platform-capability flag the dashboard template
// uses to gate the "Install & restart" button. False on Windows
// (and any future platform where the swap path is unimplemented) so
// the operator never sees a button that returns 501. The adapter
// in cmd/bridge/main.go fills this from runtime.GOOS at construction
// time — capability is fixed for the lifetime of the process.
type UpdateStatus struct {
	CurrentVersion   string    `json:"currentVersion"`
	LatestVersion    string    `json:"latestVersion,omitempty"`
	UpdateAvailable  bool      `json:"updateAvailable"`
	ReleaseNotesURL  string    `json:"releaseNotesURL,omitempty"`
	Channel          string    `json:"channel"`
	LastCheck        time.Time `json:"lastCheck,omitempty"`
	LastError        string    `json:"lastError,omitempty"`
	MinClientVersion string    `json:"minClientVersion,omitempty"`
	CanInstall       bool      `json:"canInstall"`
	// CanRollback reports whether a previous binary is actually on disk
	// to roll back TO. POST /api/updates/rollback has existed with no
	// caller since it shipped, and adding a button without this flag
	// would have been worse than no button: rollback is a binary swap,
	// so an operator pressing it with nothing staged gets an error where
	// they expected a recovery. Filled by the cmd/bridge adapter, which
	// stats the .bak the installer leaves beside the live binary.
	CanRollback bool `json:"canRollback"`
	// DeferredReason is the most-recent gate-refusal explanation
	// from the auto-installer. Empty when the previous cycle
	// either installed the candidate, found no candidate, or
	// hadn't yet polled. Currently the only populated reason is
	// the MinClientVersion compat gate ("would orphan device(s):
	// X"); future gates can extend the same field. Surfaced in
	// the dashboard as a yellow "held update" card.
	DeferredReason string `json:"deferredReason,omitempty"`
	// RejectedVersion is a release the operator rolled back on this
	// host. The auto-installer refuses to re-install it (only a
	// strictly newer release gets through), so surfacing it is what
	// explains a bridge that sees an update and deliberately never
	// takes it. Empty when nothing has been rolled back. Filled by the
	// cmd/bridge adapter from the updater's on-disk marker — the same
	// place the auto-install gate reads it, so the two can't disagree.
	RejectedVersion string `json:"rejectedVersion,omitempty"`
}

// Server owns the admin listener + mux. One per process.
type Server struct {
	deps Deps

	// mu serializes the roots add/remove critical sections (pre-checks
	// against Scanner.Roots(), manifest wipe, SetRoots fan-out) and the
	// settings PATCH's commit→side-effect ordering, plus token
	// mint/revoke. The config clone→Save→Store sequence itself is
	// serialized process-wide by CfgHolder.Update's write lock — see
	// config.RuntimeConfig.
	mu sync.Mutex

	// pageTmpls is one template bundle per page. Each bundle pre-parses
	// layout.html + the page's own .html file so rendering is a single
	// ExecuteTemplate("layout", …) call.
	pageTmpls map[string]*template.Template

	// loginTmpl is the standalone login template — does NOT extend
	// layout.html (the layout includes admin nav, which an unauthenticated
	// visitor must not see). Parsed once in New().
	loginTmpl *template.Template

	// bgScans tracks every spawnBackgroundScan goroutine in flight so
	// graceful shutdown waits for an admin-triggered scan to finish
	// (or its context to cancel) instead of letting the process exit
	// mid-write to the SQLite store. The ScanCtx is what each scan
	// observes for cancellation; this WG is the coordination point
	// for "have all spawned scans drained their post-scan cleanup
	// before we return from Serve". Capped by the same 5s shutdown
	// grace as the HTTP listener.
	bgScans sync.WaitGroup

	// soxAvailability cache. The /api/upscale/stats handler is
	// polled every 5 s by the Settings page; per-call PrecheckSox
	// would shell out 12×/min on every open Settings tab and pay
	// up to 2 s per probe (CodeRabbit major on PR #110). The TTL
	// is soxAvailabilityCacheTTL — short enough that an operator
	// installing sox sees the UI reflect within ~30 s, long
	// enough that Settings polling stays cheap.
	soxAvailabilityMu sync.Mutex
	soxAvailability   bool
	soxAvailabilityAt time.Time

	// composition cache (dashboard master-quality breakdown).
	// Manifest.FormatDistribution does a full-table json_extract scan,
	// far too expensive for the SSE hot path — cache the bucketed
	// snapshot for compositionCacheTTL. compositionSF single-flights the
	// recompute so the 30s SSE tick / initial-emit across N open tabs
	// collapses to one scan after expiry, not N concurrent scans.
	compositionMu sync.Mutex
	composition   compositionResponse
	compositionAt time.Time
	compositionSF singleflight.Group

	// analysis-coverage cache (Jobs page analysed-vs-eligible bar).
	// Plain-column SQL (single pass over tracks ⋈ track_analysis, ~ms
	// at 20k rows) but /api/jobs is POLLED — 30s TTL + singleflight so
	// N open tabs collapse to one query per window (the composition
	// cache's shape, lighter TTL since the query is cheap).
	analysisCoverageMu sync.Mutex
	analysisCoverage   *jobsAnalysisCoverage
	analysisCoverageAt time.Time
	analysisCoverageSF singleflight.Group

	// lastBackupAt caches the newest snapshot's timestamp for the jobs
	// card. Same TTL + singleflight shape as analysisCoverage above,
	// for the same reason: /api/jobs is polled every 10s per open tab,
	// and the underlying backup.List walks every snapshot directory
	// reading a manifest out of each — filesystem work that has no
	// business happening once per poll per tab.
	//
	// invalidateLastBackup() clears it so an operator-triggered snapshot
	// shows up immediately rather than up to a TTL later; waiting would
	// just move the confusion from "why is this slow" to "did my backup
	// work", and produce the refresh-spam this cache exists to remove.
	lastBackupMu    sync.Mutex
	lastBackupAt    time.Time // when the cache was filled
	lastBackupAtVal *time.Time
	lastBackupSF    singleflight.Group

	// enrichment cache (dashboard enrichment-progress breakdown). Same shape
	// and rationale as the composition cache above: EnrichmentBreakdown is a
	// full-table json_extract scan (the matched/missing split), too expensive
	// for the SSE hot path — cache the snapshot for enrichmentCacheTTL and
	// single-flight the recompute so the 30s SSE tick / initial-emit across N
	// open tabs collapses to one scan after expiry.
	enrichmentMu sync.Mutex
	enrichment   enrichmentResponse
	enrichmentAt time.Time
	enrichmentSF singleflight.Group

	// enrichment-meta cache (dashboard coverage stats: artist images /
	// artist bios / album descriptions). AtlasMetaBreakdownCounts is a
	// full-table json_extract CTE and the artist-image set is an
	// os.ReadDir — both far too expensive per tick, and both slow-moving
	// (they change on enrichment/harvest progress, not per request) — so
	// the composed part is cached for enrichmentMetaCacheTTL (60s,
	// composition pattern) and the recompute is single-flighted.
	enrichmentMetaMu sync.Mutex
	enrichmentMeta   enrichmentMetaPart
	enrichmentMetaAt time.Time
	enrichmentMetaSF singleflight.Group

	// enrichment-retry rate guard: POST /api/enrichment/retry is refused
	// (429) within enrichRetryMinInterval of the previous accepted call.
	// The reset itself is idempotent — this is UX politeness plus
	// protection against a panic-clicked button re-queueing the enricher
	// (and its MusicBrainz pacing budget) over and over.
	enrichRetryMu sync.Mutex
	enrichRetryAt time.Time

	// library-meta caches: inspector tile artwork/booklet refs, and the
	// About card's per-folder detail. Both front a
	// StreamTrackMetaRefsUnderPrefix json_extract subtree walk
	// (full-table at the root) — composition cost class — so each
	// path's response is cached for libMetaCacheTTL and the recompute
	// is single-flighted per path. Click-driven only; the SSE publisher
	// never touches these.
	//
	// Two caches rather than one keyed map: each is typed to its own
	// response DTO (so nothing hands writeJSON an `any`), keeps its own
	// libMetaCacheMaxEntries budget, and owns its own singleflight
	// Group — see the libMetaCache docblock for why sharing a Group
	// across two response types is unsafe. Both are keyed by bare
	// normalised path and swept together by libMetaInvalidateUnder.
	libMetaRefs   libMetaCache[libraryMetaRefsResponse]
	libMetaDetail libMetaCache[libraryMetaDetailResponse]
	// libMetaMisses backs GET /api/enrichment/misses. Keyed by bare
	// normalised path only — `facet` and `limit` narrow the cached
	// snapshot in the handler rather than joining the key, so switching
	// facets in the UI can't re-walk the library once per facet.
	libMetaMisses libMetaCache[enrichmentMissesResponse]

	// library-meta retry guard: POST /api/library/enrichment/retry is
	// per-PATH rate-limited (60s per normalized folder) so an operator
	// can queue retries for DIFFERENT folders back-to-back while a
	// panic-clicked button on one folder still 429s. Upstream services
	// are protected by the enricher/harvest clients' own pacing — the
	// guard is UX politeness, not the rate limiter. The map is pruned
	// opportunistically (entries older than the window).
	libMetaRetryMu sync.Mutex
	libMetaRetryAt map[string]time.Time

	// libMetaHarvestAt is the library-wide gate for the retry's
	// HarvestForceSubmit facet — that facet is inherently global (the
	// harvest submit has no per-folder scope), so two different-folder
	// retries within the window only fire it once.
	libMetaHarvestAt time.Time

	// stats DB-read last-good cache. getStatsSnapshot bounds its four
	// best-effort DB reads with snapshotDBTimeout; on error/timeout it
	// serves this last-good statsDBPart so the dashboard tiles don't flash
	// zero during scan-time lock contention (mirrors the last-good degrade
	// getCompositionSnapshot already does for the heavier format scan).
	// getStatsSnapshot runs from BOTH the REST handler and the SSE
	// publisher, so guard with a dedicated mutex.
	statsMu      sync.Mutex
	statsDB      statsDBPart
	statsDBValid bool

	// boundAdminAddr is the address the TCP listener was actually
	// bound to, recorded in Serve() once net.Listen succeeds. It is
	// used by originMatchesAdmin() so that CSRF validation reflects
	// the port the server is *currently* listening on rather than
	// the AdminAddress value in the live config (which may have been
	// updated via a PATCH before a restart).
	boundAdminAddr string
}

// pages maps the URL-friendly page name to its template filename.
var pages = map[string]string{
	"dashboard":         "dashboard.html",
	"library":           "library.html",
	"library_inspector": "library_inspector.html",
	"duplicates":        "duplicates.html",
	"jobs":              "jobs.html",
	"devices":           "devices.html",
	"upnp":              "upnp.html",
	"data":              "data.html",
	"settings":          "settings.html",
	"smartmixes":        "smartmixes.html",
	"diagnostics":       "diagnostics.html",
}

// New constructs an admin Server. Call Handler to get the http.Handler for
// ListenAndServe, or Serve to run a background listener with graceful
// shutdown.
func New(deps Deps) (*Server, error) {
	if deps.CfgHolder == nil || deps.CfgHolder.Load() == nil || deps.CfgPath == "" {
		return nil, fmt.Errorf("admin: CfgHolder and CfgPath are required")
	}
	if deps.Auth == nil || deps.Manifest == nil || deps.Scanner == nil || deps.Resolver == nil {
		return nil, fmt.Errorf("admin: Auth, Manifest, Scanner, Resolver are required")
	}
	tmpls := make(map[string]*template.Template, len(pages))
	for name, file := range pages {
		t, err := template.New("").Funcs(tmplFuncs).ParseFS(
			templateFS,
			"templates/layout.html",
			"templates/"+file,
		)
		if err != nil {
			return nil, fmt.Errorf("admin: parse %s: %w", file, err)
		}
		tmpls[name] = t
	}
	// login.html is standalone — must NOT include layout.html (which
	// embeds the admin nav). Parsed into its own template object so
	// an unauthenticated visitor never sees nav links to gated pages.
	loginTmpl, err := template.New("").Funcs(tmplFuncs).ParseFS(
		templateFS,
		"templates/login.html",
	)
	if err != nil {
		return nil, fmt.Errorf("admin: parse login.html: %w", err)
	}
	return &Server{deps: deps, pageTmpls: tmpls, loginTmpl: loginTmpl}, nil
}

// Handler returns the root http.Handler for the admin console. Exposed
// separately so httptest can drive it without a real listener.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Pages.
	mux.HandleFunc("GET /{$}", s.pageDashboard)
	mux.HandleFunc("GET /library", s.pageLibrary)
	mux.HandleFunc("GET /library/inspector", s.pageLibraryInspector)
	mux.HandleFunc("GET /library/duplicates", s.pageDuplicates)
	mux.HandleFunc("GET /jobs", s.pageJobs)
	mux.HandleFunc("GET /devices", s.pageDevices)
	mux.HandleFunc("GET /upnp", s.pageUPnP)
	mux.HandleFunc("GET /data", s.pageData)
	mux.HandleFunc("GET /smartmixes", s.pageSmartMixes)
	mux.HandleFunc("GET /settings", s.pageSettings)
	mux.HandleFunc("GET /diagnostics", s.pageDiagnostics)

	// JSON API.
	mux.HandleFunc("GET /api/stats", s.apiStats)
	mux.HandleFunc("GET /api/sources", s.apiSources)
	mux.HandleFunc("GET /api/enrichment", s.apiEnrichment)
	mux.HandleFunc("GET /api/enrichment/misses", s.apiEnrichmentMisses)
	mux.HandleFunc("POST /api/enrichment/retry", s.apiEnrichmentRetry)
	mux.HandleFunc("GET /api/endpoints", s.apiEndpoints)
	mux.HandleFunc("GET /api/events", s.apiEvents)
	mux.HandleFunc("GET /api/updates", s.apiUpdatesGet)
	mux.HandleFunc("POST /api/updates/check", s.apiUpdatesCheck)
	mux.HandleFunc("POST /api/updates/install", s.apiUpdatesInstall)
	mux.HandleFunc("POST /api/updates/rollback", s.apiUpdatesRollback)
	mux.HandleFunc("POST /api/scan", s.apiScan)
	mux.HandleFunc("GET /api/roots", s.apiRootsList)
	mux.HandleFunc("POST /api/roots", s.apiRootsAdd)
	mux.HandleFunc("DELETE /api/roots", s.apiRootsRemove)
	mux.HandleFunc("GET /api/devices", s.apiDevicesList)
	mux.HandleFunc("GET /api/playlists", s.apiPlaylistsList)
	mux.HandleFunc("GET /api/playlists/detail", s.apiPlaylistDetail)
	mux.HandleFunc("GET /api/playlists/export", s.apiPlaylistExport)
	mux.HandleFunc("GET /api/favorites", s.apiFavorites)
	mux.HandleFunc("GET /api/history", s.apiHistorySummary)
	mux.HandleFunc("GET /api/history/events", s.apiHistoryEvents)
	mux.HandleFunc("GET /api/history/export", s.apiHistoryExport)
	mux.HandleFunc("GET /api/smart-playlists", s.apiSmartPlaylistsList)
	mux.HandleFunc("POST /api/smart-playlists/regenerate", s.apiSmartPlaylistsRegenerate)
	mux.HandleFunc("POST /api/smart-playlists/{slug}/regenerate", s.apiSmartPlaylistRegenerateOne)
	mux.HandleFunc("POST /api/smart-playlists/{slug}/save-as-playlist", s.apiSmartPlaylistSaveAsPlaylist)
	mux.HandleFunc("POST /api/smart-playlists/{slug}/cover", s.apiUploadSmartMixCover)
	mux.HandleFunc("DELETE /api/smart-playlists/{slug}/cover", s.apiDeleteSmartMixCover)
	mux.HandleFunc("POST /api/playlists/{id}/cover", s.apiUploadPlaylistCover)
	mux.HandleFunc("DELETE /api/playlists/{id}/cover", s.apiDeletePlaylistCover)
	mux.HandleFunc("GET /api/tokens", s.apiTokensList)
	mux.HandleFunc("POST /api/tokens", s.apiTokensMint)
	mux.HandleFunc("DELETE /api/tokens/{id}", s.apiTokensRevoke)
	mux.HandleFunc("POST /api/tokens/{id}/rotate", s.apiTokensRotate)
	mux.HandleFunc("PATCH /api/tokens/{id}", s.apiTokensSetLifecycle)
	mux.HandleFunc("GET /api/settings", s.apiSettingsGet)
	mux.HandleFunc("PATCH /api/settings", s.apiSettingsPatch)
	mux.HandleFunc("GET /api/upscale/stats", s.apiUpscaleStats)
	mux.HandleFunc("GET /api/analysis/stats", s.apiAnalysisStats)
	mux.HandleFunc("POST /api/analysis/sweep", s.apiAnalysisSweep)
	mux.HandleFunc("GET /api/jobs", s.apiJobs)
	mux.HandleFunc("GET /api/diagnostics", s.apiDiagnostics)
	mux.HandleFunc("GET /api/doctor", s.apiDoctor)
	mux.HandleFunc("POST /api/fingerprint/sweep", s.apiFingerprintSweep)
	mux.HandleFunc("GET /api/duplicates/summary", s.apiDuplicatesSummary)
	mux.HandleFunc("GET /api/duplicates/groups", s.apiDuplicatesGroups)
	mux.HandleFunc("POST /api/duplicates/sweep", s.apiDuplicatesSweep)
	mux.HandleFunc("GET /api/library/browse", s.apiLibraryBrowse)
	mux.HandleFunc("GET /api/library/browse-projection", s.apiLibraryBrowseProjection)
	mux.HandleFunc("GET /api/library/search", s.apiLibrarySearch)
	// Inspector metadata layer (handlers_library_meta.go): tile
	// artwork/booklet refs, About-card detail, folder-scoped retry,
	// and the loopback byte routes serving the enrichment caches.
	mux.HandleFunc("GET /api/library/enrichment", s.apiLibraryEnrichmentRefs)
	mux.HandleFunc("GET /api/library/enrichment/detail", s.apiLibraryEnrichmentDetail)
	mux.HandleFunc("POST /api/library/enrichment/retry", s.apiLibraryEnrichmentRetryScoped)
	mux.HandleFunc("GET /api/library/artwork/{mbid}", s.apiLibraryArtwork)
	mux.HandleFunc("GET /api/library/artist-image/{mbid}", s.apiLibraryArtistImage)
	mux.HandleFunc("GET /api/library/booklet/{mbid}", s.apiLibraryBooklet)
	mux.HandleFunc("POST /api/upscale/batch", s.apiUpscaleBatchSubmit)
	mux.HandleFunc("GET /api/upscale/batches", s.apiUpscaleBatchList)
	mux.HandleFunc("DELETE /api/upscale/batches/{id}", s.apiUpscaleBatchCancel)
	mux.HandleFunc("DELETE /api/upscale/variants", s.apiUpscaleVariantsDelete)
	mux.HandleFunc("GET /api/upscale/target", s.apiUpscaleTargetGet)
	mux.HandleFunc("PATCH /api/upscale/target", s.apiUpscaleTargetPatch)
	mux.HandleFunc("GET /api/upscale/variants-dir", s.apiVariantsDirGet)
	mux.HandleFunc("POST /api/upscale/variants-dir", s.apiVariantsDirPatch)
	mux.HandleFunc("GET /api/upnp/servers", s.apiUPnPServers)
	mux.HandleFunc("GET /api/upnp/discovered", s.apiUPnPDiscovered)
	mux.HandleFunc("POST /api/upnp/servers", s.apiUPnPServerAdd)
	// UDN identity rides on the QUERY STRING, NOT a path segment. The
	// adapter accepts UDN OR ManualDescriptionURL as identity (the
	// fallback for SSDP-unreachable servers), and a manual URL like
	// `http://192.168.0.62:8200/rootDesc.xml` would never match a
	// single-segment `{udn}` wildcard — Go's net/http multiplexer
	// unescapes `%2F` to `/` and path-cleans before matching, so the
	// encoded URL splits into multiple segments. Query strings bypass
	// the cleaning entirely. Per Gemini HIGH on PR #357 round-2.
	mux.HandleFunc("DELETE /api/upnp/servers", s.apiUPnPServerRemove)
	mux.HandleFunc("PATCH /api/upnp/servers", s.apiUPnPServerUpdate)
	mux.HandleFunc("POST /api/upnp/rescan", s.apiUPnPRescan)
	mux.HandleFunc("POST /api/restart", s.apiRestart)
	mux.HandleFunc("GET /api/pair-qr", s.apiPairQR)
	mux.HandleFunc("GET /api/backups", s.apiBackupsList)
	mux.HandleFunc("POST /api/backups", s.apiBackupsCreate)
	mux.HandleFunc("GET /api/cert", s.apiCertInfo)
	mux.HandleFunc("GET /api/tailscale/status", s.apiTailscaleStatus)
	mux.HandleFunc("POST /api/tailscale/refresh-cert", s.apiTailscaleRefreshCert)
	mux.HandleFunc("GET /api/autocert/status", s.apiAutocertStatus)
	mux.HandleFunc("GET /api/pairing", s.apiPairingList)
	mux.HandleFunc("POST /api/pairing/{id}/approve", s.apiPairingApprove)
	mux.HandleFunc("POST /api/pairing/{id}/decline", s.apiPairingDecline)

	// Prometheus exposition. Loopback-bound HERE at registration —
	// scrapers must run on the same host (local Prometheus / Grafana
	// Alloy / node_exporter sidecar). The outer `boundaryMiddleware`
	// loopback wrap is BYPASSED in public mode (the listener is
	// exposed beyond loopback by design), so the endpoint needs its
	// own loopbackOnly gate to stay same-host-only regardless of
	// deployment mode. `/metrics` is also on `isAuthBypassPath`, so a
	// cookie-less local scraper isn't 302'd to /login in public mode —
	// the loopback gate is the sole trust boundary and matches the
	// "scrapers run on the same host" intent. The CSRF guard is a
	// no-op for GET so the bare promhttp handler passes through safely.
	mux.Handle("GET /metrics", loopbackOnly(promhttp.Handler()))

	// Static. The embed keeps files at "static/app.css", not "app.css",
	// so we serve the fs directly — the request path already matches.
	mux.Handle("GET /static/", http.FileServerFS(staticFS))

	// Login routes. Registered on the same mux as the other pages —
	// the sessionMiddleware's bypass list keeps /login reachable
	// without a session.
	mux.HandleFunc("GET /login", s.pageLogin)
	mux.HandleFunc("POST /login", s.apiLogin)
	mux.HandleFunc("POST /logout", s.apiLogout)

	// Layer order (outer → inner):
	//   1. boundaryMiddleware — applies loopbackOnly in loopback
	//      mode; passthrough in public mode (the listener is
	//      already exposed beyond loopback by design).
	//   2. csrfGuard — Content-Type strict + Origin allowlist on
	//      body-bearing mutations. Defends against drive-by
	//      cross-origin browser POSTs.
	//   3. sessionMiddleware — public-mode only: requires a valid
	//      bridge_admin_session cookie. Loopback-mode passthrough.
	//      Bypass list covers /login, /static/*, /favicon*.
	//   4. mux — the actual handlers.
	//
	// The two layers defend different threats: boundary protects
	// the network surface; sessionMiddleware authenticates the
	// operator. csrfGuard sits between them so login POSTs get the
	// strict Content-Type + Origin check before reaching the
	// session-aware handler.
	return s.boundaryMiddleware(s.csrfGuard(s.sessionMiddleware(mux)))
}

// scanCtx returns the parent context for admin-triggered scans.
func (s *Server) scanCtx() context.Context {
	if s.deps.ScanCtx != nil {
		return s.deps.ScanCtx
	}
	return context.Background()
}

// spawnBackgroundScan fires a scanner goroutine that survives the
// handler's request lifecycle. Used by `apiRootsAdd` / `apiRootsRemove`
// for both happy-path rescans and Save-failure compensating scans.
//
// The `contextcheck` linter requires the context to be captured
// outside the goroutine (not via a method call inside the closure),
// so we resolve `scanCtx()` up front and pass it through. Errors are
// logged (not returned) because the caller has already written the
// HTTP response; any failure here is operator-facing only, and cancels
// from a shutting-down `ScanCtx` are suppressed to keep logs quiet
// during normal teardown. Labelled so the log line identifies which
// handler path produced the error.
//
// `apiScan` calls this directly. All admin-initiated scans MUST
// route through this helper — don't reintroduce a raw `go func()`.
func (s *Server) spawnBackgroundScan(label string) {
	ctx := s.scanCtx()
	s.bgScans.Add(1)
	go func() {
		defer s.bgScans.Done()
		if _, err := s.deps.Scanner.Scan(ctx); err != nil && !errors.Is(err, ctx.Err()) {
			logger.Error("background scan failed", "label", label, "err", err)
		}
	}()
}

// Serve binds to deps.Cfg.AdminAddress and blocks until ctx is done.
// Returns on listener error or after graceful shutdown. Intended for
// serveCmd; tests should use Handler + httptest.
func (s *Server) Serve(ctx context.Context) error {
	cfg := s.deps.CfgHolder.Load()
	addr := cfg.AdminAddress
	if addr == "" {
		addr = config.DefaultAdminAddress
	}
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("admin listen %s: %w", addr, err)
	}
	s.boundAdminAddr = lis.Addr().String()
	// Public-mode direct-TLS path (PR 3): wrap the TCP listener
	// in tls.NewListener using the cmd-side certManager's
	// AdminTLSConfig. Same SNI switcher serves the public API,
	// so the admin console gets the LE cert for the operator's
	// domain SNI and the self-signed cert for direct-IP /
	// unknown SNI. cmd-side wiring sets TLSConfig to nil for
	// loopback mode and for public mode with
	// AdminTLSTerminatedByProxy=true.
	scheme := "http"
	if s.deps.TLSConfig != nil {
		lis = tls.NewListener(lis, s.deps.TLSConfig)
		scheme = "https"
	}
	srv := &http.Server{
		Handler: s.Handler(),
		// BaseContext derives every request's r.Context() from the
		// parent shutdown context. Long-lived endpoints (the SSE
		// stream at /api/events in particular) select on
		// r.Context().Done() to bail at shutdown — without
		// BaseContext, http.Server.Shutdown waits for the connection
		// to idle out, which an SSE handler never does, blocking the
		// 5 s grace window. Per-request handlers that already use
		// r.Context() (e.g. apiUpdatesCheck's GitHub poll) inherit
		// the same cancellation.
		BaseContext: func(_ net.Listener) context.Context { return ctx },
		// Slowloris-class defense. The admin console binds loopback
		// only by config validation, but a misbehaving local client
		// (or a buggy admin script) trickling bytes 1/sec could
		// otherwise tie up an FD indefinitely. Pre-fix only
		// `ReadHeaderTimeout` was set; adding `ReadTimeout` and
		// `IdleTimeout` closes the request-side gap (PR #75).
		//
		// `WriteTimeout` is deliberately left UNSET (zero) — the
		// admin handler exposes long-running synchronous endpoints
		// like `/api/updates/install` (binary download + swap) and
		// `/api/backups` (tarball snapshot) that legitimately take
		// minutes on large libraries. A 60s WriteTimeout would
		// tear the response connection mid-flight and leave the
		// operator's UI stuck on "loading" while the server-side
		// operation continues in the background. qodo bot review
		// on PR #75 caught this. The Slowloris-class write
		// trickle is not a meaningful threat on a loopback admin
		// listener — IdleTimeout reaps the kept-alive socket pool
		// which is the realistic FD-exhaustion vector. SSE clients
		// stay on a single long-lived request so neither ReadTimeout
		// nor IdleTimeout applies mid-stream.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(lis) }()
	logger.Info("console listening", "url", fmt.Sprintf("%s://%s/", scheme, lis.Addr()))
	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		shutErr := srv.Shutdown(shutCtx)
		// Wait for any spawnBackgroundScan goroutine to finish before
		// returning — capped at the same 5s grace via shutCtx so a
		// stuck scanner can't wedge process exit indefinitely.
		// `s.deps.ScanCtx` is the scanner's cancellation source; the
		// outer process already cancels it during shutdown, so a
		// healthy scan honours the cancel and drains promptly.
		drained := make(chan struct{})
		go func() {
			s.bgScans.Wait()
			close(drained)
		}()
		select {
		case <-drained:
		case <-shutCtx.Done():
		}
		return shutErr
	case err := <-errCh:
		// errors.Is, not ==: net/http returns ErrServerClosed unwrapped today,
		// so this is behaviour-identical now — but a wrapped one would read as
		// a real listener failure and turn a clean shutdown into an error
		// return. Matches how cmd/bridge and internal/dlna already test for it.
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	}
}

// csrfGuard rejects drive-by cross-origin POSTs from a page running
// in the user's browser. Loopback binding alone doesn't defend
// against this threat — a malicious page at attacker.com runs in
// the same user-agent that has loopback access, so the OS-level
// allow-list is irrelevant.
//
// Two layered checks:
//
//  1. **Strict Content-Type on body-bearing mutations** (primary
//     defense). For POST/PATCH/PUT/DELETE with a body, Content-Type
//     must start with `application/json`. text/plain,
//     application/x-www-form-urlencoded, and multipart/form-data are
//     rejected with 415. Browsers MUST do a CORS preflight OPTIONS
//     for cross-origin application/json — and the admin mux doesn't
//     register OPTIONS handlers for /api/*, so the preflight fails
//     and the real request never fires. Bodyless mutating POSTs
//     (e.g. /api/scan, /api/restart) pass without a Content-Type
//     check because there's no JSON body to abuse — handlers that
//     DO read a body return 400 on empty input via the JSON
//     decoder's natural error path.
//
//  2. **Origin allowlist (reject-if-mismatched, not reject-if-
//     missing)**. When the Origin header is present, it must match
//     the configured AdminAddress host:port. When absent, allow —
//     Firefox/Safari sometimes omit Origin entirely for loopback
//     navigations or send "null", and failing closed locks
//     legitimate operators out. Referer is intentionally not
//     consulted (same flakiness, no marginal benefit beyond a
//     strict Content-Type).
//
// GET / HEAD pass through unconditionally — no body to parse, no
// state mutation. OPTIONS (which we don't handle) returns 405 from
// the mux as today.
func (s *Server) csrfGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}

		// Body-bearing mutating request? Require application/json.
		// Empty-body requests pass without Content-Type since there's
		// no decode surface to attack. net/http strips the
		// Transfer-Encoding header during request parsing and surfaces
		// chunked transfers via `r.TransferEncoding` and ContentLength
		// = -1, so a header-based "is the body chunked" check would be
		// dead code (CodeRabbit Major on PR #76). ContentLength != 0
		// catches both the known-length (>0) and chunked (-1) cases;
		// only ContentLength == 0 is the no-body case we want to
		// allow through.
		hasBody := r.ContentLength != 0 || len(r.TransferEncoding) > 0
		if hasBody {
			ct := r.Header.Get("Content-Type")
			// Strip parameters: "application/json; charset=utf-8"
			// → "application/json". Compare case-insensitively per
			// RFC 7231 §3.1.1.1 — "Application/JSON" is valid.
			if i := strings.Index(ct, ";"); i >= 0 {
				ct = ct[:i]
			}
			ct = strings.TrimSpace(strings.ToLower(ct))
			if ct != "application/json" {
				http.Error(w, "admin refused: Content-Type must be application/json", http.StatusUnsupportedMediaType)
				return
			}
		}

		// Origin header check, when present. The admin server binds
		// to deps.Cfg.AdminAddress — a same-origin request from the
		// admin UI carries Origin: http://127.0.0.1:7789 (or
		// whatever the configured host:port resolves to in the
		// browser). A cross-origin POST from attacker.com would
		// carry Origin: https://attacker.com.
		if origin := r.Header.Get("Origin"); origin != "" {
			if !s.originMatchesAdmin(origin) {
				http.Error(w, "admin refused: cross-origin request", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// originMatchesAdmin compares an Origin header against the admin
// listener's allowed origin set. Three branches by deployment
// posture:
//
//  1. **Loopback mode** (historical): the admin listener binds
//     a loopback address and the browser carries
//     Origin: http://127.0.0.1:7789 (or http://localhost:7789).
//     Both sides must resolve to the same loopback listener.
//
//  2. **Public mode + direct TLS** (bridge terminates TLS itself
//     against the operator's domain): allowlist is the operator's
//     domain + admin listen port.
//
//  3. **Public mode + reverse proxy** (Caddy / nginx terminates
//     TLS, bridge serves plain HTTP on a private interface):
//     allowlist is the operator's domain, HOST-MATCH ONLY (no
//     port check). The bridge can't know which public port the
//     operator chose on their proxy — the browser's
//     Origin: https://bridge.example.com may resolve to :443 or
//     :8443 depending on the reverse-proxy config.  The session
//     cookie's SameSite=Strict + Secure attributes are the
//     load-bearing defense against rogue cross-origin browsers;
//     the host match is the additional pin.
//
// Empty Autocert.Domain in public mode falls back to the
// listener-bound address (test wiring). SNI normalization on the
// host comparison: case-insensitive + trailing-dot tolerant
// (mirrors the TLS-side normalization in PR 3).
func (s *Server) originMatchesAdmin(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		// Malformed or "null" Origin — refuse rather than guess.
		return false
	}
	// Use url.URL.Hostname() / Port() instead of net.SplitHostPort on
	// u.Host: those methods strip IPv6 brackets and handle the
	// no-port case in one step. SplitHostPort would surface IPv6
	// addresses like `[::1]:7789` as host="" + error on the no-port
	// variant, breaking origin like `http://[::1]:7789` (Gemini Major
	// on PR #76).
	originHost := u.Hostname()
	originPort := u.Port()
	if originPort == "" {
		// Default-port inference for origin scheme (port-elided
		// origin → "http://localhost"). Only http/https are
		// reasonable.
		switch strings.ToLower(u.Scheme) {
		case "https":
			originPort = "443"
		case "http":
			originPort = "80"
		default:
			return false
		}
	}
	cfg := s.deps.CfgHolder.Load()
	if cfg != nil && cfg.IsPublic() {
		return s.originMatchesPublicMode(originHost, originPort, cfg)
	}
	return s.originMatchesLoopbackMode(originHost, originPort)
}

// originMatchesLoopbackMode is the historical Origin allowlist:
// host:port match against the admin listener with loopback-name
// equivalence.
func (s *Server) originMatchesLoopbackMode(originHost, originPort string) bool {
	adminAddr := s.boundAdminAddr
	if adminAddr == "" {
		cfg := s.deps.CfgHolder.Load()
		adminAddr = cfg.AdminAddress
		if adminAddr == "" {
			adminAddr = config.DefaultAdminAddress
		}
	}
	adminHost, adminPort, err := net.SplitHostPort(adminAddr)
	if err != nil {
		// Misconfigured listener — treat as block to surface the
		// regression rather than silently accept.
		return false
	}
	if originPort != adminPort {
		return false
	}
	// Treat 127.0.0.1, ::1, and localhost as equivalent — they all
	// resolve to the same loopback listener.
	return loopbackHostname(originHost) && loopbackHostname(adminHost)
}

// originMatchesPublicMode is the public-mode allowlist. Falls back
// to the listener address when Autocert.Domain is unset (test
// wiring); enforces host equality only behind a reverse proxy
// (operator's public port is opaque to the bridge).
func (s *Server) originMatchesPublicMode(originHost, originPort string, cfg *config.Config) bool {
	expectedHost := strings.TrimSuffix(strings.ToLower(cfg.Autocert.Domain), ".")
	if expectedHost == "" {
		// No domain configured — fall back to listener address
		// (test wiring; production should always set Autocert.Domain
		// in public mode).
		adminAddr := s.boundAdminAddr
		if adminAddr == "" {
			adminAddr = cfg.AdminAddress
		}
		h, _, err := net.SplitHostPort(adminAddr)
		if err != nil {
			return false
		}
		expectedHost = strings.ToLower(h)
	}
	originHostNorm := strings.TrimSuffix(strings.ToLower(originHost), ".")
	if originHostNorm != expectedHost {
		return false
	}
	if cfg.Deployment.AdminTLSTerminatedByProxy {
		// Operator's reverse proxy chooses the public port;
		// SameSite=Strict + Secure cookies are the load-bearing
		// defense, host match is the pin.
		return true
	}
	// Direct-TLS path: enforce the admin listener port.
	adminAddr := s.boundAdminAddr
	if adminAddr == "" {
		adminAddr = cfg.AdminAddress
	}
	_, adminPort, err := net.SplitHostPort(adminAddr)
	if err != nil {
		return false
	}
	return originPort == adminPort
}

// loopbackHostname returns true if h is one of the conventional
// loopback names/literals. Used by the Origin allowlist so a
// browser that resolved http://localhost:7789 doesn't get rejected
// against an AdminAddress of 127.0.0.1:7789.
func loopbackHostname(h string) bool {
	// Trim the FQDN root dot as well as case-folding: "localhost." is a
	// fully-qualified spelling of localhost that browsers accept and send
	// verbatim in an Origin header, so without this an operator reaching
	// the console at http://localhost.:7789 fails Origin validation.
	// originMatchesPublicMode already normalises the trailing dot; this is
	// the loopback-side twin.
	h = strings.TrimSuffix(strings.ToLower(h), ".")
	if h == "localhost" {
		return true
	}
	// Accept either a bare IP literal or a bracketed IPv6 form
	// ("[::1]"). url.URL.Hostname() already strips brackets, but
	// callers in tests / older callers may pass a raw host string
	// from net.SplitHostPort. Trimming both bracket bytes is safe
	// — they're not valid IP-literal characters (Gemini Minor on
	// PR #76).
	if ip := net.ParseIP(strings.Trim(h, "[]")); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// loopbackOnly is a belt-and-braces middleware that refuses non-loopback
// RemoteAddr connections even if the listener was misconfigured. The
// primary defense is the loopback-only bind enforced by config validation;
// this catches regressions where the listener binding drifts (e.g. a
// future "expose on LAN via Tailscale" feature being wired up without
// also adding an auth layer).
func loopbackOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			http.Error(w, "admin refused: bad remote addr", http.StatusForbidden)
			return
		}
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			http.Error(w, "admin refused: non-loopback remote", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// restart fires the configured restart callback (or os.Exit(0) by default).
// Called by the restart endpoint after a non-hot-reloadable settings
// change; service-manager (launchd / systemd) relaunches the process.
func (s *Server) restart() {
	if s.deps.Restart != nil {
		s.deps.Restart()
		return
	}
	// launchd and systemd user units both have KeepAlive / Restart=always
	// by default in the templates shipped via `bridge init`, so a plain
	// exit-0 lands us back on our feet within a second or so.
	os.Exit(0)
}
