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
	"log"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/auth"
	"github.com/acoseac/1-bit-bridge/internal/backup"
	"github.com/acoseac/1-bit-bridge/internal/config"
	bridgefs "github.com/acoseac/1-bit-bridge/internal/fs"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static/*
var staticFS embed.FS

// Deps bundles the runtime state the admin console reads and mutates. All
// fields are required unless marked optional.
type Deps struct {
	Cfg      *config.Config // mutable — admin edits in place and calls Save(CfgPath)
	CfgPath  string         // path to bridge.yaml, for Cfg.Save()
	Auth     *auth.Store    // token list / mint / revoke
	Manifest *manifest.Store
	Scanner  *manifest.Scanner
	Resolver *bridgefs.Resolver

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
}

// pages maps the URL-friendly page name to its template filename.
var pages = map[string]string{
	"dashboard": "dashboard.html",
	"library":   "library.html",
	"devices":   "devices.html",
	"settings":  "settings.html",
}

// New constructs an admin Server. Call Handler to get the http.Handler for
// ListenAndServe, or Serve to run a background listener with graceful
// shutdown.
func New(deps Deps) (*Server, error) {
	if deps.Cfg == nil || deps.CfgPath == "" {
		return nil, fmt.Errorf("admin: Cfg and CfgPath are required")
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
	mux.HandleFunc("GET /devices", s.pageDevices)
	mux.HandleFunc("GET /settings", s.pageSettings)

	// JSON API.
	mux.HandleFunc("GET /api/stats", s.apiStats)
	mux.HandleFunc("GET /api/endpoints", s.apiEndpoints)
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
	mux.HandleFunc("POST /api/restart", s.apiRestart)
	mux.HandleFunc("GET /api/pair-qr", s.apiPairQR)
	mux.HandleFunc("GET /api/backups", s.apiBackupsList)
	mux.HandleFunc("POST /api/backups", s.apiBackupsCreate)
	mux.HandleFunc("GET /api/cert", s.apiCertInfo)

	// Static. The embed keeps files at "static/app.css", not "app.css",
	// so we serve the fs directly — the request path already matches.
	mux.Handle("GET /static/", http.FileServerFS(staticFS))

	return loopbackOnly(mux)
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
// Mirrors the pattern in `apiScan` — keep them in sync if either
// changes.
func (s *Server) spawnBackgroundScan(label string) {
	ctx := s.scanCtx()
	go func() {
		if _, err := s.deps.Scanner.Scan(ctx); err != nil && !errors.Is(err, ctx.Err()) {
			fmt.Fprintf(os.Stderr, "admin: %s: %v\n", label, err)
		}
	}()
}

// Serve binds to deps.Cfg.AdminAddress and blocks until ctx is done.
// Returns on listener error or after graceful shutdown. Intended for
// serveCmd; tests should use Handler + httptest.
func (s *Server) Serve(ctx context.Context) error {
	addr := s.deps.Cfg.AdminAddress
	if addr == "" {
		addr = config.DefaultAdminAddress
	}
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("admin listen %s: %w", addr, err)
	}
	srv := &http.Server{
		Handler: s.Handler(),
		// Slowloris-class defense. The admin console binds loopback
		// only by config validation, but a misbehaving local client
		// (or a buggy admin script) trickling bytes 1/sec could
		// otherwise tie up an FD indefinitely. Pre-fix only
		// `ReadHeaderTimeout` was set; without `ReadTimeout` /
		// `WriteTimeout` / `IdleTimeout`, headers parsed quickly
		// but a slow body or response held the connection open
		// (PR #N).
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(lis) }()
	log.Printf("admin: console listening on http://%s/", lis.Addr())
	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutCtx)
	case err := <-errCh:
		if err != nil && err != http.ErrServerClosed {
			return err
		}
		return nil
	}
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
