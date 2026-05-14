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

	// Fingerprint is the TLS cert SHA-256 in colon-hex form. Shown in the
	// pairing modal and the dashboard.
	Fingerprint string

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

	// ProjectedSize estimates the on-disk size of a FLAC
	// variant produced from (sourceSize, sourceRate, sourceBits)
	// at (targetRate, targetBits). Wired to
	// `transcode.ProjectedSize` (with `DefaultCompressionFactor`
	// baked in) via a closure in cmd/bridge/main.go so the admin
	// package doesn't import internal/transcode. Mirrors the
	// UpscaleStats / UpscalePrecheck decoupling pattern.
	//
	// Nil when upscale is disabled — the projection endpoint
	// surfaces a clean 503 in that case.
	ProjectedSize func(sourceSize int64, sourceRate, sourceBits, targetRate, targetBits int) int64

	// AvailableDiskSpace probes free bytes on the volume holding
	// `dir`. Wired to `transcode.AvailableDiskSpace`. Nil-safe
	// alongside ProjectedSize (both wired together when upscale
	// is enabled, both nil when disabled).
	AvailableDiskSpace func(dir string) (int64, error)

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
type AdminBatchCoordinator interface {
	Submit(ctx context.Context, libraryRelPath string, targetRate, targetBits int) (AdminBatchSubmitResult, error)
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
	ID             string    `json:"id"`
	Path           string    `json:"path"`
	TargetRate     int       `json:"targetRate"`
	TargetBits     int       `json:"targetBits"`
	Status         string    `json:"status"`
	TotalFiles     int       `json:"totalFiles"`
	ProcessedFiles int       `json:"processedFiles"`
	FailedFiles    int       `json:"failedFiles"`
	Error          string    `json:"error,omitempty"`
	CreatedAt      time.Time `json:"createdAt"`
	UpdatedAt      time.Time `json:"updatedAt"`
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
	CLIAvailable      bool       `json:"cliAvailable"`
	NodeName          string     `json:"nodeName,omitempty"`
	MagicDNSName      string     `json:"magicDNSName,omitempty"`
	HTTPSCertsEnabled bool       `json:"httpsCertsEnabled"`
	CertPresent       bool       `json:"certPresent"`
	CertNotAfter      *time.Time `json:"certNotAfter,omitempty"`
	CertPath          string     `json:"certPath,omitempty"`
	MagicDNSURL       string     `json:"magicDNSURL,omitempty"`
	LastError         string     `json:"lastError,omitempty"`
	LastChecked       *time.Time `json:"lastChecked,omitempty"`
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
	// DeferredReason is the most-recent gate-refusal explanation
	// from the auto-installer. Empty when the previous cycle
	// either installed the candidate, found no candidate, or
	// hadn't yet polled. Currently the only populated reason is
	// the MinClientVersion compat gate ("would orphan device(s):
	// X"); future gates can extend the same field. Surfaced in
	// the dashboard as a yellow "held update" card.
	DeferredReason string `json:"deferredReason,omitempty"`
}

// Server owns the admin listener + mux. One per process.
type Server struct {
	deps Deps

	// mu serializes mutations that touch Cfg / Save / SetRoots / Wipe so
	// two admin clients can't race the YAML rewrite.
	mu sync.Mutex

	// pageTmpls is one template bundle per page. Each bundle pre-parses
	// layout.html + the page's own .html file so rendering is a single
	// ExecuteTemplate("layout", …) call.
	pageTmpls map[string]*template.Template

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
	"jobs":              "jobs.html",
	"devices":           "devices.html",
	"settings":          "settings.html",
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
	return &Server{deps: deps, pageTmpls: tmpls}, nil
}

// Handler returns the root http.Handler for the admin console. Exposed
// separately so httptest can drive it without a real listener.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Pages.
	mux.HandleFunc("GET /{$}", s.pageDashboard)
	mux.HandleFunc("GET /library", s.pageLibrary)
	mux.HandleFunc("GET /library/inspector", s.pageLibraryInspector)
	mux.HandleFunc("GET /jobs", s.pageJobs)
	mux.HandleFunc("GET /devices", s.pageDevices)
	mux.HandleFunc("GET /settings", s.pageSettings)

	// JSON API.
	mux.HandleFunc("GET /api/stats", s.apiStats)
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
	mux.HandleFunc("GET /api/tokens", s.apiTokensList)
	mux.HandleFunc("POST /api/tokens", s.apiTokensMint)
	mux.HandleFunc("DELETE /api/tokens/{id}", s.apiTokensRevoke)
	mux.HandleFunc("POST /api/tokens/{id}/rotate", s.apiTokensRotate)
	mux.HandleFunc("PATCH /api/tokens/{id}", s.apiTokensSetLifecycle)
	mux.HandleFunc("GET /api/settings", s.apiSettingsGet)
	mux.HandleFunc("PATCH /api/settings", s.apiSettingsPatch)
	mux.HandleFunc("GET /api/upscale/stats", s.apiUpscaleStats)
	mux.HandleFunc("GET /api/library/browse", s.apiLibraryBrowse)
	mux.HandleFunc("GET /api/library/browse-projection", s.apiLibraryBrowseProjection)
	mux.HandleFunc("GET /api/library/search", s.apiLibrarySearch)
	mux.HandleFunc("POST /api/upscale/batch", s.apiUpscaleBatchSubmit)
	mux.HandleFunc("GET /api/upscale/batches", s.apiUpscaleBatchList)
	mux.HandleFunc("DELETE /api/upscale/batches/{id}", s.apiUpscaleBatchCancel)
	mux.HandleFunc("DELETE /api/upscale/variants", s.apiUpscaleVariantsDelete)
	mux.HandleFunc("GET /api/upscale/target", s.apiUpscaleTargetGet)
	mux.HandleFunc("PATCH /api/upscale/target", s.apiUpscaleTargetPatch)
	mux.HandleFunc("POST /api/restart", s.apiRestart)
	mux.HandleFunc("GET /api/pair-qr", s.apiPairQR)
	mux.HandleFunc("GET /api/backups", s.apiBackupsList)
	mux.HandleFunc("POST /api/backups", s.apiBackupsCreate)
	mux.HandleFunc("GET /api/cert", s.apiCertInfo)
	mux.HandleFunc("GET /api/tailscale/status", s.apiTailscaleStatus)
	mux.HandleFunc("POST /api/tailscale/refresh-cert", s.apiTailscaleRefreshCert)
	mux.HandleFunc("GET /api/pairing", s.apiPairingList)
	mux.HandleFunc("POST /api/pairing/{id}/approve", s.apiPairingApprove)
	mux.HandleFunc("POST /api/pairing/{id}/decline", s.apiPairingDecline)

	// Static. The embed keeps files at "static/app.css", not "app.css",
	// so we serve the fs directly — the request path already matches.
	mux.Handle("GET /static/", http.FileServerFS(staticFS))

	// Layer order: outer = loopbackOnly (drops non-loopback peers
	// before we evaluate anything else); inner = csrfGuard (rejects
	// drive-by browser cross-origin POSTs that the loopback bind
	// can't catch on its own — a malicious page in the user's
	// browser is on the same loopback as the user). The two layers
	// defend different threats.
	return loopbackOnly(s.csrfGuard(mux))
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
			fmt.Fprintf(os.Stderr, "admin: %s: %v\n", label, err)
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
	logger.Info("console listening", "url", fmt.Sprintf("http://%s/", lis.Addr()))
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
		if err != nil && err != http.ErrServerClosed {
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
// listener's host:port. Tolerant of the listener binding 127.0.0.1
// while the browser navigates via http://localhost — both resolve
// to the same listener and `loopbackOnly` already enforces the IP
// is loopback, so the only thing the Origin check adds is "did this
// request originate from a page served by us, or from elsewhere".
//
// Empty AdminAddress (test wiring) means "any same-loopback Origin
// is fine" — the loopbackOnly outer layer is the actual gate.
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
		// reasonable for a loopback admin console.
		switch strings.ToLower(u.Scheme) {
		case "https":
			originPort = "443"
		case "http":
			originPort = "80"
		default:
			return false
		}
	}
	// Use the address the listener was actually bound to so that a
	// pending config change (AdminAddress PATCH before restart) does
	// not shift the CSRF allowlist while the server is still on the
	// old port. Fall back to the live config only if boundAdminAddr
	// is not yet set (defensive; should not happen in practice).
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
	// resolve to the same loopback listener. `loopbackOnly` has
	// already enforced the underlying IP is loopback, so this is
	// just user-agent normalization.
	return loopbackHostname(originHost) && loopbackHostname(adminHost)
}

// loopbackHostname returns true if h is one of the conventional
// loopback names/literals. Used by the Origin allowlist so a
// browser that resolved http://localhost:7789 doesn't get rejected
// against an AdminAddress of 127.0.0.1:7789.
func loopbackHostname(h string) bool {
	h = strings.ToLower(h)
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
