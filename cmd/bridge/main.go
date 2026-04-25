// Command bridge is the 1-bit-bridge server CLI.
//
// Subcommands:
//
//	bridge init    first-time setup: config, TLS cert, launchd/systemd unit
//	bridge serve   run the HTTPS server (default port 7788)
//	bridge pair    mint a new bearer token for an iOS client
//	bridge scan    force a full library rescan
//	bridge version print version and protocol version
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/admin"
	"github.com/acoseac/1-bit-bridge/internal/api"
	"github.com/acoseac/1-bit-bridge/internal/auth"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/enrich"
	bridgefs "github.com/acoseac/1-bit-bridge/internal/fs"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	bridgemdns "github.com/acoseac/1-bit-bridge/internal/mdns"
	servertls "github.com/acoseac/1-bit-bridge/internal/tls"
	"github.com/acoseac/1-bit-bridge/internal/updater"
	"github.com/acoseac/1-bit-bridge/internal/version"
)

// updateInfoAdapter bridges *updater.Updater to api.UpdaterStatus +
// admin's read-side without coupling those packages to the updater
// type. Trivial; lives here at the wiring point so the api / admin
// packages stay agnostic of where their update info comes from.
//
// dataDir + binaryPath are needed for the install path so the
// adapter can construct InstallOptions on each call without making
// the admin package aware of either. canInstall is captured at
// construction time from runtime.GOOS so the dashboard can gate the
// Install button on platform support.
type updateInfoAdapter struct {
	u          *updater.Updater
	sessions   *updater.Tracker
	dataDir    string
	binaryPath string
	canInstall bool
}

func (a updateInfoAdapter) UpdateInfo() api.UpdateInfo {
	s := a.u.Status()
	return api.UpdateInfo{
		LatestVersion:    s.LatestVersion,
		UpdateAvailable:  s.UpdateAvailable,
		ReleaseNotesURL:  s.ReleaseNotesURL,
		MinClientVersion: version.MinClientVersion,
	}
}

func (a updateInfoAdapter) Status() admin.UpdateStatus {
	s := a.u.Status()
	return admin.UpdateStatus{
		CurrentVersion:   s.CurrentVersion,
		LatestVersion:    s.LatestVersion,
		UpdateAvailable:  s.UpdateAvailable,
		ReleaseNotesURL:  s.ReleaseNotesURL,
		Channel:          s.Channel,
		LastCheck:        s.LastCheck,
		LastError:        s.LastError,
		MinClientVersion: version.MinClientVersion,
		CanInstall:       a.canInstall,
	}
}

func (a updateInfoAdapter) CheckNow(ctx context.Context) admin.UpdateStatus {
	a.u.CheckNow(ctx)
	return a.Status()
}

func (a updateInfoAdapter) Install(ctx context.Context, force bool) (admin.UpdateStatus, error) {
	st, err := a.u.Install(ctx, updater.InstallOptions{
		DataDir:    a.dataDir,
		BinaryPath: a.binaryPath,
		Force:      force,
		Sessions:   a.sessions,
	})
	return admin.UpdateStatus{
		CurrentVersion:   st.CurrentVersion,
		LatestVersion:    st.LatestVersion,
		UpdateAvailable:  st.UpdateAvailable,
		ReleaseNotesURL:  st.ReleaseNotesURL,
		Channel:          st.Channel,
		LastCheck:        st.LastCheck,
		LastError:        st.LastError,
		MinClientVersion: version.MinClientVersion,
		CanInstall:       a.canInstall,
	}, mapUpdaterError(err)
}

func (a updateInfoAdapter) Rollback(force bool) error {
	return mapUpdaterError(a.u.Rollback(updater.InstallOptions{
		DataDir:    a.dataDir,
		BinaryPath: a.binaryPath,
		Force:      force,
		Sessions:   a.sessions,
	}))
}

// mapUpdaterError translates internal/updater's typed sentinel
// errors to the admin-package equivalents so handlers_api.go's
// classifyUpdateError can switch on errors.Is without importing
// internal/updater. The original error message is preserved as the
// %w child so the operator-facing detail still threads through.
//
// New sentinel pairings land here as the Phase C / future work
// expands the install error surface.
func mapUpdaterError(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, updater.ErrNoUpdate):
		return fmt.Errorf("%w: %s", admin.ErrUpdateNoUpdate, err.Error())
	case errors.Is(err, updater.ErrActiveSessions):
		return fmt.Errorf("%w: %s", admin.ErrUpdateActiveSessions, err.Error())
	case errors.Is(err, updater.ErrInstallNotSupported):
		return fmt.Errorf("%w: %s", admin.ErrUpdateNotSupported, err.Error())
	case errors.Is(err, updater.ErrPathNotWritable):
		return fmt.Errorf("%w: %s", admin.ErrUpdatePathNotWritable, err.Error())
	default:
		return err
	}
}

// artworkDirBridge lets cmd/bridge expose the enricher's cache dir to
// internal/api without importing internal/enrich from there.
type artworkDirBridge string

func (a artworkDirBridge) ArtworkCacheDir() string { return string(a) }

// shutdownGrace is how long we wait for in-flight requests to drain before
// forcing the listener closed.
const shutdownGrace = 5 * time.Second

// maybeRollbackOnBoot consults <dataDir>/update-state.json and acts
// on whatever the previous install attempt's outcome was:
//
//   - first boot after a successful install: stamp installedAt and
//     retain bridge.bak for one more boot.
//   - boot after that: delete bridge.bak.
//   - first boot after a FAILED install (we're not at TargetVersion):
//     restore bridge.bak and clear the marker. Service manager will
//     then respawn into the restored old binary on next exit.
//   - everything else: no-op.
//
// Failures here are logged but non-fatal — a botched rollback still
// lets the server start up; the operator can recover manually.
func maybeRollbackOnBoot(stderr io.Writer, dataDir, binaryPath string) {
	st, err := updater.LoadState(dataDir)
	if err != nil {
		fmt.Fprintf(stderr, "updater: load state: %v\n", err)
		return
	}
	action := updater.DecideBootAction(st, version.ServerVersion, time.Now().UTC())
	switch action {
	case updater.BootNoop:
		return
	case updater.BootInstallSucceeded:
		// New binary booted; record installedAt so the NEXT boot
		// knows it can clean up the .bak.
		st.Status = "installed"
		st.InstalledAt = time.Now().UTC()
		if err := updater.SaveState(dataDir, st); err != nil {
			fmt.Fprintf(stderr, "updater: mark installed: %v\n", err)
		}
	case updater.BootCleanupBak:
		// Second boot after a successful install: clean up.
		if err := updater.RemoveBackup(binaryPath, ".bak"); err != nil {
			fmt.Fprintf(stderr, "updater: remove .bak: %v\n", err)
		}
		if err := updater.ClearState(dataDir); err != nil {
			fmt.Fprintf(stderr, "updater: clear state: %v\n", err)
		}
	case updater.BootInstallFailed:
		// New binary didn't come up at the expected version.
		// Restore the previous binary and clear the marker so the
		// next exit (planned or via service-manager respawn) lands
		// us back on the old version.
		fmt.Fprintf(stderr, "updater: install of %s did not reach the expected version (running %s); rolling back to .bak\n",
			st.TargetVersion, version.ServerVersion)
		if err := updater.RollbackBinary(binaryPath, ".bak"); err != nil {
			fmt.Fprintf(stderr, "updater: rollback failed: %v (manual recovery needed at %s)\n",
				err, binaryPath)
		}
		if err := updater.ClearState(dataDir); err != nil {
			fmt.Fprintf(stderr, "updater: clear state: %v\n", err)
		}
	case updater.BootClearAbandoned:
		// Marker is older than the recency window — nothing
		// actionable, just clean up.
		if err := updater.ClearState(dataDir); err != nil {
			fmt.Fprintf(stderr, "updater: clear abandoned state: %v\n", err)
		}
	}
}

func main() {
	// Windows-service dispatch. When SCM launches bridge.exe, os.Args
	// is the service binary's configured ImagePath (`bridge.exe serve
	// --config <path>`), and stdout/stderr aren't attached to a console.
	// runAsWindowsService translates SCM Stop into ctx cancel, so the
	// existing graceful-shutdown path in serveCmd runs unchanged.
	//
	// The stub in service_other.go always returns false on non-Windows,
	// so this branch is a no-op off Windows.
	if isWindowsService() {
		redirectServiceIO() // stdout/stderr → %PROGRAMDATA%\1-bit-bridge\bridge.log
		// Surface service-dispatch errors to whatever stdio we have so
		// operators see them in the log. Previously this was
		// `_ = runAsWindowsService(...)`, which meant a service that
		// failed on boot (e.g. svc.Run couldn't register with the SCM,
		// or the subcommand exited non-zero) became a clean exit 0 —
		// SCM would just retry silently per its restart policy, leaving
		// no trace of what actually broke.
		if err := runAsWindowsService(
			context.Background(),
			"1-bit-bridge",
			func(ctx context.Context) error {
				code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
				if code != 0 {
					return fmt.Errorf("subcommand exited with code %d", code)
				}
				return nil
			},
			os.Stderr,
		); err != nil {
			fmt.Fprintf(os.Stderr, "service dispatch: %v\n", err)
			os.Exit(1)
		}
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

// run parses argv (without the program name) and dispatches to a subcommand.
// It is extracted from main so tests can drive it without spawning a process.
// Exit codes: 0 success, 1 subcommand failure, 2 usage error.
//
// ctx is used by serveCmd to trigger graceful shutdown (signal from main or
// cancellation from a test).
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		usage(stderr)
		return 2
	}
	switch args[0] {
	case "init":
		return initCmd(args[1:], os.Stdin, stdout, stderr)
	case "serve":
		return serveCmd(ctx, args[1:], stdout, stderr)
	case "pair":
		return pairCmd(args[1:], stdout, stderr)
	case "scan":
		return scanCmd(args[1:], stdout, stderr)
	case "doctor":
		return doctorCmd(args[1:], stdout, stderr)
	case "update":
		return updateCmd(ctx, args[1:], os.Stdin, stdout, stderr)
	case "version":
		fmt.Fprintf(stdout, "1-bit-bridge %s (protocol v%d)\n", version.ServerVersion, version.ProtocolVersion)
		return 0
	case "-h", "--help", "help":
		usage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "unknown subcommand: %s\n\n", args[0])
		usage(stderr)
		return 2
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `1-bit-bridge — companion server for the 1-bit iOS app.

Usage:
  bridge <subcommand> [flags]

Subcommands:
  init     First-time setup: writes config, mints TLS cert, installs service.
  serve    Run the HTTPS server.
  pair     Generate a new bearer token for an iOS client.
  scan     Force a full library rescan.
  doctor   Preflight: check ports, directories, service manager before init.
  update   Check for / install a new bridge release from GitHub.
  version  Print version and protocol version.

Run "bridge <subcommand> -h" for subcommand-specific flags.

First-time install:
  bridge init                    # writes config + installs launchd/systemd unit
                                 # prints admin console URL at http://127.0.0.1:7789/
`)
}

func serveCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "bridge.yaml", "path to config file")
	addrOverride := fs.String("addr", "", "override listenAddress from config (host:port)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "config load failed: %v\n", err)
		return 2
	}
	if err := bridgefs.ValidateRoots(cfg.LibraryRoots); err != nil {
		fmt.Fprintf(stderr, "config: %v\n", err)
		return 2
	}
	if *addrOverride != "" {
		// Validate the override the same way config.Validate validates
		// ListenAddress — otherwise an invalid value like "notaport"
		// slips through and only surfaces much later as a net.Listen
		// failure with an opaque error.
		if _, _, err := net.SplitHostPort(*addrOverride); err != nil {
			fmt.Fprintf(stderr, "invalid --addr %q: %v\n", *addrOverride, err)
			return 2
		}
		cfg.ListenAddress = *addrOverride
	}

	// Resolve TLS material (default: dataDir/server.{crt,key}; overridable
	// via cfg.TLSCertPath / cfg.TLSKeyPath).
	certPath, keyPath := cfg.TLSCertPath, cfg.TLSKeyPath
	if certPath == "" || keyPath == "" {
		certPath, keyPath = servertls.DefaultPaths(cfg.DataDir)
	}
	hostname, _ := os.Hostname()
	cert, fingerprint, err := servertls.LoadOrGenerate(certPath, keyPath, hostname)
	if err != nil {
		fmt.Fprintf(stderr, "TLS material: %v\n", err)
		return 1
	}

	store, err := auth.OpenStore(filepath.Join(cfg.DataDir, "tokens.json"))
	if err != nil {
		fmt.Fprintf(stderr, "open token store: %v\n", err)
		return 1
	}
	defer func() {
		// Flush any LastUsedAt updates debounced by Validate so a
		// just-before-exit hit doesn't lose its timestamp.
		if err := store.FlushLastUsed(); err != nil {
			fmt.Fprintf(stderr, "auth: flush LastUsedAt on shutdown: %v\n", err)
		}
	}()

	manifestStore, err := manifest.OpenStore(manifest.DefaultDBPath(cfg.DataDir))
	if err != nil {
		fmt.Fprintf(stderr, "open manifest store: %v\n", err)
		return 1
	}
	defer manifestStore.Close()
	scanner := manifest.NewScanner(cfg.LibraryRoots, manifestStore)
	provider := manifest.NewProvider(manifestStore, scanner)

	// Fire up the periodic scanner in the background. It runs an initial
	// scan on startup, then rescans every cfg.ScanInterval().
	//
	// scanCtx derives from serveCmd's parent ctx so a SIGINT (or
	// any other parent cancel) propagates straight to the scanner,
	// enricher, and updater goroutines that share this context. The
	// previous version derived from context.Background() and relied
	// on the deferred scanCancel() to fire — which works in steady
	// state but trips the contextcheck linter and means the
	// background workers can't observe cancellation until serveCmd's
	// shutdown path runs.
	scanCtx, scanCancel := context.WithCancel(ctx)
	defer scanCancel()
	go scanner.RunPeriodic(scanCtx, cfg.ScanInterval())

	// Fire up the MusicBrainz/CoverArt enricher in the background. It
	// pulls un-enriched tracks from the store and fills in
	// MusicBrainzAlbumID / ArtworkMBID, caching cover images under
	// <dataDir>/artwork/.
	userAgent := fmt.Sprintf(
		"%s/%s (+https://github.com/acoseac/1-bit-bridge)",
		"1-bit-bridge", version.ServerVersion,
	)
	mbClient := enrich.NewMusicBrainzClient("", userAgent, nil)
	caaClient := enrich.NewCoverArtClient("", userAgent, nil)
	deezerClient := enrich.NewDeezerClient("", userAgent, nil)
	artworkDir := filepath.Join(cfg.DataDir, "artwork")
	enricher := enrich.NewEnricher(manifestStore, mbClient, caaClient, deezerClient, artworkDir)
	go enricher.Run(scanCtx)

	// Sessions tracker counts inflight /v1/read + /v1/download
	// requests. The Install path consults Inflight() before
	// swapping the binary so Hugo 2 / XMOS DAC DoP-lock loss can't
	// happen via a mid-stream restart.
	//
	// Constructed BEFORE the updater so the Phase C auto-installer's
	// InstallOptions can reference it.
	sessions := updater.NewTracker()

	// Resolve the running binary path once at startup. Install
	// swaps the file at this exact path. os.Executable() may
	// return an error in unusual environments (deleted binary
	// running, embedded test); fall back to argv[0] so the
	// failure surfaces later in Install's preflight rather than
	// blocking the whole server boot.
	binaryPath, exeErr := os.Executable()
	if exeErr != nil {
		fmt.Fprintf(stderr, "updater: os.Executable failed (install path may not work): %v\n", exeErr)
		binaryPath = os.Args[0]
	}

	// Boot-time rollback housekeeping: read update-state.json and
	// either confirm the install succeeded (mark installed, retain
	// .bak for one boot) or restore .bak when the new version
	// failed to come up cleanly. Failures are logged but
	// non-fatal — operator can still recover by hand.
	maybeRollbackOnBoot(stderr, cfg.DataDir, binaryPath)

	// Background updater: polls GitHub Releases on a configurable
	// cadence (Phase A), exposes operator-triggered Install via the
	// admin console + CLI (Phase B), and optionally auto-installs
	// inside a quiet-hours window with the same safeties as Phase
	// B's manual path (Phase C).
	//
	// Lives off scanCtx so a SIGINT cancels it cleanly alongside
	// the scanner. Poll failures are non-fatal — the bridge serves
	// fine without update awareness; the admin UI shows "couldn't
	// reach GitHub" in the LastError field.
	updOpts := updater.Options{
		AutoInstall: cfg.Update.AutoInstall,
	}
	if cfg.Update.CheckIntervalHours > 0 {
		updOpts.CheckInterval = time.Duration(cfg.Update.CheckIntervalHours) * time.Hour
	}
	if cfg.Update.QuietHours != "" {
		// Validate already passed; this can't fail.
		start, end, _ := config.ParseQuietHours(cfg.Update.QuietHours)
		updOpts.QuietHoursStart = start
		updOpts.QuietHoursEnd = end
	}
	if cfg.Update.AutoInstall && runtime.GOOS != "windows" {
		// Auto-install only wires the install opts when (a) the
		// operator opted in via config and (b) the platform
		// supports the swap. On Windows the toggle is a no-op
		// (consistent with Phase B's CanInstall=false on Windows).
		updOpts.AutoInstallOpts = &updater.InstallOptions{
			DataDir:    cfg.DataDir,
			BinaryPath: binaryPath,
			Sessions:   sessions,
			Force:      false,
		}
		// On successful auto-install we exit; service-manager
		// (launchd / systemd) respawns into the new binary. The
		// Phase B maybeRollbackOnBoot then verifies version-match
		// and either confirms or rolls back.
		updOpts.AutoInstallRestart = func() {
			fmt.Fprintln(stdout, "Restarting after auto-install (service manager will respawn).")
			scanCancel()
			os.Exit(0)
		}
	}
	upd := updater.New(updOpts)

	updAdapter := updateInfoAdapter{
		u:          upd,
		sessions:   sessions,
		dataDir:    cfg.DataDir,
		binaryPath: binaryPath,
		// Phase B implements the swap path on darwin + linux only.
		// Surfaced as a capability flag so the dashboard hides the
		// Install button on Windows rather than letting the operator
		// click through to a 501.
		canInstall: runtime.GOOS != "windows",
	}

	apiSrv := api.New(cfg, store, provider, fingerprint).
		WithArtworkDirs(artworkDirBridge(artworkDir)).
		WithMBIDProbe(provider).
		WithUpdater(updAdapter).
		WithSessionTracker(sessions)
	httpSrv := &http.Server{
		Addr:      cfg.ListenAddress,
		Handler:   apiSrv.Handler(),
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{*cert}, MinVersion: tls.VersionTLS12},
		// Defence-in-depth against slow-loris / half-open sockets.
		// WriteTimeout is deliberately left UNSET (zero) because
		// `/v1/download` streams multi-GB DSD files to iOS (and needs
		// many minutes under slow Wi-Fi / Tailscale relays); setting
		// WriteTimeout would cut the response mid-flight and crash
		// Hugo 2's DoP lock. ReadHeaderTimeout + ReadTimeout guard the
		// request side only; IdleTimeout drains kept-alive connections.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// Admin console: plain HTTP on a loopback address (default
	// 127.0.0.1:7789). Shares the api server's Resolver so hot-add/remove
	// of library roots lands on both sides in lockstep.
	//
	// We resolve the config file path to absolute here so admin.Cfg.Save
	// writes to the right file even if the operator changes cwd post-boot
	// (shouldn't happen, but let's not trip them up).
	absCfgPath, _ := filepath.Abs(*configPath)
	adminSrv, err := admin.New(admin.Deps{
		Cfg:         cfg,
		CfgPath:     absCfgPath,
		Auth:        store,
		Manifest:    manifestStore,
		Scanner:     scanner,
		Resolver:    apiSrv.Resolver(),
		Fingerprint: fingerprint,
		StartedAt:   time.Now().UTC(),
		ScanCtx:     scanCtx,
		Updater:     updAdapter,
	})
	if err != nil {
		fmt.Fprintf(stderr, "admin: %v\n", err)
		return 1
	}
	adminCtx, adminCancel := context.WithCancel(context.Background())
	defer adminCancel()
	adminErr := make(chan error, 1)
	go func() {
		adminErr <- adminSrv.Serve(adminCtx)
	}()

	// Listen first so we can report the actual bound address (useful when
	// cfg.ListenAddress is ":0" — which test code uses).
	lis, err := net.Listen("tcp", cfg.ListenAddress)
	if err != nil {
		fmt.Fprintf(stderr, "listen %s: %v\n", cfg.ListenAddress, err)
		return 1
	}

	fmt.Fprintf(stdout, "1-bit-bridge v%s (protocol v%d) — listening on https://%s\n",
		version.ServerVersion, version.ProtocolVersion, lis.Addr())
	fmt.Fprintf(stdout, "Library: %q (roots: %v)\n", cfg.LibraryName, cfg.LibraryRoots)
	fmt.Fprintf(stdout, "TLS fingerprint (pin this on the iOS side):\n  %s\n", fingerprint)
	fmt.Fprintf(stdout, "Admin console: http://%s/ — add library folders, pair devices, view stats\n", cfg.AdminAddress)

	// Advertise on mDNS so iOS clients on the same LAN auto-discover
	// this server. Failures are non-fatal — mDNS is a nice-to-have,
	// and the server runs fine without it (users connect by IP).
	boundAddr, _ := lis.Addr().(*net.TCPAddr)
	var advertiser *bridgemdns.Advertiser
	if boundAddr != nil {
		a, err := bridgemdns.Advertise(bridgemdns.Config{
			InstanceName:    cfg.LibraryName,
			Port:            boundAddr.Port,
			ProtocolVersion: version.ProtocolVersion,
			LibraryName:     cfg.LibraryName,
		})
		if err != nil {
			fmt.Fprintf(stderr, "mDNS advertise failed (non-fatal): %v\n", err)
		} else {
			advertiser = a
			fmt.Fprintf(stdout, "mDNS: advertising as %q on %s\n", cfg.LibraryName, bridgemdns.Service)
		}
	}
	if advertiser != nil {
		defer advertiser.Close()
	}

	fmt.Fprintln(stdout, "Press Ctrl-C to shut down.")

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- httpSrv.ServeTLS(lis, "", "")
	}()

	select {
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(stderr, "server error: %v\n", err)
			return 1
		}
	case err := <-adminErr:
		// The admin console's bind can fail after main's first listen
		// succeeds (e.g. another process already owns :7789). Previously
		// this was swallowed via a fire-and-forget goroutine, leaving the
		// operator with a silently-broken admin URL — this case surfaces
		// it at startup, matches the signal the serveErr branch gives for
		// the main API listener.
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(stderr, "admin server: %v\n", err)
			// Tear down the main API listener cleanly before exit so
			// in-flight iOS requests get `http.ErrServerClosed` rather
			// than a socket RST mid-stream. Without this, a 404 on
			// :7789 binds → process-exit on 1 leaves the :7788 server
			// to be killed by the runtime's ungraceful goroutine halt.
			shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
			defer cancel()
			_ = httpSrv.Shutdown(shutdownCtx)
			return 1
		}
	case <-ctx.Done():
		fmt.Fprintln(stdout, "\nShutting down...")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()
		if err := httpSrv.Shutdown(shutdownCtx); err != nil {
			fmt.Fprintf(stderr, "shutdown: %v\n", err)
			return 1
		}
	}
	return 0
}

func pairCmd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("pair", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "bridge.yaml", "path to config file")
	name := fs.String("name", "", "client name (e.g. \"iPhone 15 Pro\")")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *name == "" {
		fmt.Fprintln(stderr, "pair: --name is required")
		return 2
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "config load failed: %v\n", err)
		return 2
	}
	store, err := auth.OpenStore(filepath.Join(cfg.DataDir, "tokens.json"))
	if err != nil {
		fmt.Fprintf(stderr, "open token store: %v\n", err)
		return 1
	}
	raw, tok, err := store.Mint(*name)
	if err != nil {
		fmt.Fprintf(stderr, "mint token: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, "Paired successfully.")
	fmt.Fprintf(stdout, "  Device: %s\n", tok.Name)
	fmt.Fprintf(stdout, "  ID:     %s\n", tok.ID)
	fmt.Fprintf(stdout, "\nBearer token (copy this into the 1-bit iOS app; it won't be shown again):\n")
	fmt.Fprintf(stdout, "  %s\n", raw)
	return 0
}

func scanCmd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("scan", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "bridge.yaml", "path to config file")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "config load failed: %v\n", err)
		return 2
	}
	store, err := manifest.OpenStore(manifest.DefaultDBPath(cfg.DataDir))
	if err != nil {
		fmt.Fprintf(stderr, "open manifest store: %v\n", err)
		return 1
	}
	defer store.Close()
	scanner := manifest.NewScanner(cfg.LibraryRoots, store)

	fmt.Fprintf(stdout, "Scanning %v ...\n", cfg.LibraryRoots)
	start := time.Now()
	n, err := scanner.Scan(context.Background())
	if err != nil {
		fmt.Fprintf(stderr, "scan error: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "Scan complete: %d tracks indexed in %s\n", n, time.Since(start).Round(time.Millisecond))
	return 0
}
