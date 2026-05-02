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
	"bufio"
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
	"sync"
	"syscall"
	"time"

	"github.com/mattn/go-isatty"

	"github.com/acoseac/1-bit-bridge/internal/admin"
	"github.com/acoseac/1-bit-bridge/internal/advertise"
	"github.com/acoseac/1-bit-bridge/internal/api"
	"github.com/acoseac/1-bit-bridge/internal/auth"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/enrich"
	bridgefs "github.com/acoseac/1-bit-bridge/internal/fs"
	"github.com/acoseac/1-bit-bridge/internal/logging"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	bridgemdns "github.com/acoseac/1-bit-bridge/internal/mdns"
	"github.com/acoseac/1-bit-bridge/internal/pairing"
	"github.com/acoseac/1-bit-bridge/internal/supervision"
	servertls "github.com/acoseac/1-bit-bridge/internal/tls"
	"github.com/acoseac/1-bit-bridge/internal/transcode"
	"github.com/acoseac/1-bit-bridge/internal/updater"
	"github.com/acoseac/1-bit-bridge/internal/version"
)

// Package-scoped logger for the cmd/bridge wiring layer. Most
// startup output is `fmt.Fprintf(stdout/stderr)` because it's user-
// facing; this is for backend-style telemetry from helpers like
// upscaleStatsAdapter that fire on every request and shouldn't spam
// the operator's terminal but should be visible via `bridge logs`.
var logger = logging.Component("bridge")

// variantStoreAdapter implements api.VariantStore on top of a
// manifest.Provider. Just translates between the two packages'
// equivalent record shapes — the api package can't import the
// manifest package directly (would create an upward cycle), so
// this thin adapter lives at the wiring point. Same pattern as
// MBIDProbe / ManifestProvider.
type variantStoreAdapter struct {
	provider *manifest.Provider
}

func (a *variantStoreAdapter) LookupVariant(sourcePath, variantID string) (*api.VariantRecord, error) {
	v, err := a.provider.LookupVariant(sourcePath, variantID)
	if err != nil {
		return nil, err
	}
	if v == nil {
		return nil, nil
	}
	return &api.VariantRecord{
		SidecarPath:   v.SidecarPath,
		SourceMTimeNS: v.SourceMTimeNS,
		SourceSize:    v.SourceSize,
	}, nil
}

// upscaleEnqueuerAdapter implements api.UpscaleEnqueuer on top
// of a transcode.Pool plus a manifest.Store + bridgefs.Resolver.
// Per-call work:
//  1. Resolve the library-relative path to abs via the canonical
//     resolver (handles single-root + multi-root layouts).
//  2. Fetch the Track row from the store to read source rate /
//     isDSD for eligibility.
//  3. Pick target rate via transcode.ResolveTargetRate; skip
//     when the source is already at/above it (returns
//     ErrUpscaleIneligible).
//  4. Construct a JobSpec, capture freshness from disk, hand to
//     the Pool. Translate transcode.ErrQueueFull → the api
//     package's ErrUpscaleQueueFull sentinel so the handler can
//     `errors.Is` cleanly without importing transcode.
type upscaleEnqueuerAdapter struct {
	pool      *transcode.Pool
	store     *manifest.Store
	resolver  *bridgefs.Resolver
	cfg       *config.Config
	outputDir string
}

func (a *upscaleEnqueuerAdapter) EnqueueOne(libraryRelativePath string) error {
	abs, err := a.resolver.Resolve(libraryRelativePath)
	if err != nil {
		return api.ErrUpscaleSourceMissing
	}
	// Use Lookup (not Get) here because iOS hands in a path that's
	// been through `share.normalize(path:)` — lowercase + leading
	// slash — while the manifest stores the FS-canonical case.
	// LookupTrack does an exact-match fast path first (handles
	// case-sensitive filesystems correctly) then falls back to a
	// LOWER() compare via the v3 functional index. The scanner's
	// hot loop deliberately stays on GetTrack so two distinct
	// case-colliding rows can't alias each other (Qodo on PR #126).
	track, err := a.store.LookupTrack(libraryRelativePath)
	if err != nil {
		// Surface DB errors as a generic 5xx upstream rather
		// than silently enqueuing — the resumability check
		// below depends on the same store, so a sick DB would
		// likely double-convert tracks (Gemini medium on
		// PR #109).
		return fmt.Errorf("get track row: %w", err)
	}
	if track == nil {
		// File exists on disk but no manifest row yet — scanner
		// hasn't seen it. The HTTP handler treats this as an
		// ineligible candidate (silent reject) rather than an
		// error; the user has no remediation path beyond
		// triggering a rescan.
		return api.ErrUpscaleIneligible
	}
	// Eligibility gate: PCM only, source rate present, source
	// rate strictly below target rate.
	if track.IsDSD != nil && *track.IsDSD {
		return api.ErrUpscaleIneligible
	}
	if track.SampleRate == nil {
		return api.ErrUpscaleIneligible
	}
	sourceHz := int(*track.SampleRate)
	target, err := transcode.ResolveTargetRate(a.cfg.Upscale.EffectiveTargetRate(), sourceHz)
	if err != nil {
		return fmt.Errorf("resolve target rate: %w", err)
	}
	if target == 0 {
		return api.ErrUpscaleIneligible
	}
	// Use the manifest's canonical-case path — NOT the iOS-shaped
	// input — for `SourceLibraryRel`. The variant insert hits a
	// `FOREIGN KEY (source_path) REFERENCES tracks(path)` constraint
	// on the case-sensitive PRIMARY KEY; passing the lowercase iOS
	// shape through to UpsertVariant makes the FK fail at write
	// time with `constraint failed: FOREIGN KEY constraint failed`,
	// after sox has already done the work. PR #126 introduced this
	// regression: LookupTrack finds the row via case-folded fallback
	// but the spec then carried the unmatched input forward.
	// `track.Path` is the authoritative form the manifest stores.
	spec := transcode.JobSpec{
		SourceAbsPath:    abs,
		SourceLibraryRel: track.Path,
		SourceSampleRate: sourceHz,
		TargetSampleRate: target,
		TargetBits:       a.cfg.Upscale.EffectiveTargetBits(),
		Quality:          transcode.QualityVeryHigh,
		OutputDir:        a.outputDir,
	}
	if err := spec.FreshnessFromFile(); err != nil {
		return api.ErrUpscaleSourceMissing
	}
	// Resumability: skip when a fresh sidecar already exists.
	// Same handle-the-error policy as the parent track lookup —
	// a sick DB shouldn't silently re-convert a track.
	// Use track.Path (canonical) — same rationale as the spec
	// construction above. Either path would work here (LookupVariant
	// case-folds), but the canonical form keeps every downstream
	// store call consistent.
	existing, getVErr := a.store.LookupVariant(track.Path, spec.VariantID())
	if getVErr != nil {
		return fmt.Errorf("get variant row: %w", getVErr)
	}
	if existing != nil {
		if existing.SourceMTimeNS == spec.SourceMTimeNS && existing.SourceSize == spec.SourceSize {
			return api.ErrUpscaleIneligible
		}
	}
	enqueueErr := a.pool.Enqueue(spec)
	switch {
	case errors.Is(enqueueErr, transcode.ErrQueueFull):
		return api.ErrUpscaleQueueFull
	case errors.Is(enqueueErr, transcode.ErrPoolClosed):
		// Should never reach here in production — Stop()
		// only fires during graceful shutdown when no new
		// HTTP requests are accepted. Surface as
		// upscale-disabled so the iOS toast reads "feature
		// unavailable" rather than a confusing transient
		// error (CodeRabbit nit on PR #109).
		return api.ErrUpscaleSourceMissing
	default:
		return enqueueErr
	}
}

// upscaleStatsAdapter implements api.UpscaleStatsProvider. Mirrors
// the admin /api/upscale/stats handler's data sources field-for-
// field so the operator's Settings tile and a paired iOS client see
// the same numbers.
//
// Two pool-related closures (rather than a captured `*transcode.Pool`):
// the pool reference itself can be nil (operator never enabled the
// feature) AND the operator can flip `cfg.Upscale.Enabled = false`
// mid-flight without restart, leaving a live but logically-disabled
// Pool. The closures evaluate both conditions at snapshot time so
// `enabled` and the `pool` payload move together — same gating the
// admin `UpscaleStats` closure already uses.
//
// **Known limitation**: `cfg.Upscale.Enabled` is read here without
// synchronization while the admin PATCH handler writes the same
// field under `admin.Server.mu`. This data race already existed in
// the admin tile's closure (cmd/bridge/main.go:909) and is out-of-
// scope for this endpoint addition; the proper fix is an `atomic.Bool`
// on `*config.Config` (touching admin's writer too). Worst case
// today: a single 5 s poll snapshot reads a racing flag value and
// reports `enabled` inconsistently with the freshly-PATCHed state;
// the next poll converges.
//
// Sox precheck is TTL-cached (mirrors `admin.Server.cachedSoxAvailability`,
// also 30 s) so the per-5-s poll doesn't shell out 12×/min — the
// precheck forks `sox --version`, which is cheap but not free, and
// gemini-code-assist reasonably flagged the per-call cost on PR #111.
type upscaleStatsAdapter struct {
	pool    func() *transcode.Pool
	enabled func() bool
	store   *manifest.Store

	soxMu sync.Mutex
	soxAt time.Time
	soxOK bool
}

const upscaleStatsSoxTTL = 30 * time.Second

func (a *upscaleStatsAdapter) UpscaleStatsSnapshot() api.UpscaleStats {
	var snap api.UpscaleStats
	if a.enabled() {
		if p := a.pool(); p != nil {
			s := p.Stats()
			snap.Pool = &api.UpscalePoolStats{
				Workers:  s.Workers,
				QueueCap: s.QueueCap,
				QueueLen: s.QueueLen,
				Inflight: s.Inflight,
				Enqueued: s.Enqueued,
				Done:     s.Done,
				Failed:   s.Failed,
			}
		}
	}
	snap.Enabled = (snap.Pool != nil)
	soxOK := a.cachedSoxOK()
	snap.SoxAvailable = &soxOK
	if a.store != nil {
		count, bytes, err := a.store.CountVariants()
		if err != nil {
			// Same degrade-and-log policy the admin tile uses
			// (PR #110): a SQL failure here shouldn't blank the
			// whole response. Live counters still go out; the
			// caller sees `cachedVariants: 0, cachedBytes: 0` and
			// the operator can find the failure in logs.
			logger.Warn("upscale stats: count variants", "err", err)
		} else {
			snap.CachedVariants = count
			snap.CachedBytes = bytes
		}
	}
	return snap
}

// cachedSoxOK returns the most recent `transcode.PrecheckSox` result
// or runs a fresh probe when the cache is older than
// `upscaleStatsSoxTTL`. Mirrors `admin.Server.cachedSoxAvailability`'s
// 30 s TTL so the operator's Settings tile and the iOS-facing
// endpoint stay aligned on what the host reports.
func (a *upscaleStatsAdapter) cachedSoxOK() bool {
	a.soxMu.Lock()
	defer a.soxMu.Unlock()
	if !a.soxAt.IsZero() && time.Since(a.soxAt) < upscaleStatsSoxTTL {
		return a.soxOK
	}
	a.soxOK = (transcode.PrecheckSox() == nil)
	a.soxAt = time.Now()
	return a.soxOK
}

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
		DeferredReason:   s.DeferredReason,
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
		// Init slog AFTER the service IO redirect — the redirected
		// os.Stderr is what we want telemetry to land in (the
		// service log file).
		logging.Init(os.Stderr)
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

	// Init slog telemetry. CLI commands keep their `fmt.Fprintf`
	// stdout/stderr surfaces — slog is for backend telemetry only
	// (scanner / enricher / admin / etc.) and lands on stderr by
	// default, matching the service log destination on Windows
	// (post-redirect) and stderr on macOS / Linux.
	logging.Init(os.Stderr)
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
		// Bare `bridge` on a real TTY drops into the launcher menu.
		// Pipes / non-TTY (CI scripts, `bridge | cat`, automation)
		// fall through to the existing usage + exit 2 — automation
		// MUST keep seeing the original behavior.
		//
		// menuLoop owns its own ctx (context.Background) and creates
		// per-action signal scopes inside dispatch — see the comment
		// on menuLoop. The serve subcommand path keeps using ctx as
		// before.
		if isatty.IsTerminal(os.Stdin.Fd()) && isatty.IsTerminal(os.Stdout.Fd()) {
			return menuLoop(context.Background(), bufio.NewReader(os.Stdin), stdout, stderr)
		}
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
	case "upscale":
		return upscaleCmd(ctx, args[1:], stdout, stderr)
	case "doctor":
		return doctorCmd(args[1:], stdout, stderr)
	case "update":
		return updateCmd(ctx, args[1:], os.Stdin, stdout, stderr)
	case "backup":
		return backupCmd(args[1:], stdout, stderr)
	case "restore":
		return restoreCmd(args[1:], os.Stdin, stdout, stderr)
	case "token":
		return tokenCmd(args[1:], stdout, stderr)
	case "cert":
		return certCmd(args[1:], os.Stdin, stdout, stderr)
	case "status":
		return statusCmd(ctx, args[1:], stdout, stderr)
	case "logs":
		return logsCmd(ctx, args[1:], stdout, stderr)
	case "library":
		return libraryCmd(ctx, args[1:], stdout, stderr)
	case "start":
		return startCmd(args[1:], stdout, stderr)
	case "stop":
		return stopCmd(args[1:], stdout, stderr)
	case "restart":
		return restartCmd(args[1:], stdout, stderr)
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
  start    Boot the installed service (launchd / systemd / SCM).
  stop     Stop the installed service.
  restart  Bounce the installed service.
  status   Probe the running bridge — track count, endpoints, uptime.
  logs     Tail the per-OS bridge log file. -f to follow.
  library  Manage library roots: bridge library add|remove <path>.
  pair     Generate a new bearer token for an iOS client.
  scan     Force a full library rescan.
  upscale  Generate high-rate FLAC sidecars from PCM sources (requires sox + opt-in flag).
  doctor   Preflight: check ports, directories, service manager before init.
  update   Check for / install a new bridge release from GitHub.
  backup   Snapshot bridge state into <dataDir>/backups/<timestamp>/.
  restore  Restore bridge state from a snapshot directory.
  token    Manage paired tokens (list / rotate / expire / revoke).
  cert     Inspect or rotate the TLS cert (info / rotate).
  version  Print version and protocol version.

Run "bridge <subcommand> -h" for subcommand-specific flags.

First-time install:
  bridge init                    # writes config + installs launchd/systemd unit
                                 # prints admin console URL at http://127.0.0.1:7789/
`)
}

// serveOpts bundles the inputs runServe needs. Lifted out of serveCmd's
// flag parsing so PR 2's interactive "Start now" picker (and PR 3's
// menu launcher) can drive a serve session in-process without
// re-parsing flags. Each call gets a fresh, signal-wired ctx from the
// caller — never share a parent ctx across multiple runServe
// invocations or the second call sees a canceled parent and shuts
// down instantly (Go contexts can't be un-canceled).
type serveOpts struct {
	configPath   string
	addrOverride string
}

func serveCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "bridge.yaml", "path to config file")
	addrOverride := fs.String("addr", "", "override listenAddress from config (host:port)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	return runServe(ctx, serveOpts{configPath: *configPath, addrOverride: *addrOverride}, stdout, stderr)
}

// runServe is the library-callable serve loop. Identical behavior to
// the flag-driven serveCmd path — same TLS material, same admin
// listener, same SIGINT graceful-shutdown — just with the inputs
// pre-parsed. Returns the exit code the CLI would.
func runServe(ctx context.Context, opts serveOpts, stdout, stderr io.Writer) int {
	configPath := &opts.configPath
	addrOverride := &opts.addrOverride
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
	// Serve loads the existing cert if any; the SAN-stale check
	// inside LoadOrGenerateWithOptions warns at startup when the
	// on-disk cert's SANs don't cover the currently-advertised
	// endpoints (Tailscale IPs/DNS, custom URLs). We pass the broader
	// option set so that warning fires correctly — first-mint inside
	// `bridge serve` also picks up the broader set on a fresh data
	// dir without a prior `bridge init`.
	sanCfg := advertise.CertSANConfig{CustomEndpoints: cfg.CustomEndpoints}
	tlsOpts := servertls.GenerateOptions{
		Hostname:      hostname,
		ExtraDNSNames: advertise.GatherCertSANDNS(sanCfg),
		ExtraIPs:      advertise.GatherCertSANIPs(sanCfg),
	}
	cert, fingerprint, err := servertls.LoadOrGenerateWithOptions(certPath, keyPath, tlsOpts)
	if err != nil {
		fmt.Fprintf(stderr, "TLS material: %v\n", err)
		return 1
	}

	// SNI cert switcher. Routes incoming TLS handshakes to the
	// self-signed cert (LAN/mDNS/IP-literal SNI — iOS pins this
	// fingerprint at first contact) or to a Tailscale-issued LE cert
	// when the SNI ends in the local node's MagicDNS suffix. The LE
	// cert is loaded asynchronously by the auto-mint goroutine below;
	// until that completes the manager falls through to self-signed
	// for every connection (= today's behaviour).
	certManager := servertls.NewManager(cert)

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
	// Single source of truth for the artwork cache directory. The
	// scanner writes scanner-side `local-<sha256>-500.jpg` here when
	// it finds embedded ID3 APIC art or a folder-level cover.jpg /
	// folder.jpg; the enricher writes MusicBrainz `<mbid>-500.jpg`
	// here for its CAA / iTunes path; the API handler reads from the
	// same directory and serves both transparently via the relaxed
	// /v1/artwork mbid regex.
	artworkDir := filepath.Join(cfg.DataDir, "artwork")
	scanner := manifest.NewScanner(cfg.LibraryRoots, manifestStore, artworkDir)
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

	// Tailscale HTTPS auto-pilot: detect the local node's MagicDNS
	// name + mint a Let's Encrypt cert via `tailscale cert` if the
	// tailnet has HTTPS Certificates enabled. The cert is fed into
	// `certManager`'s SNI switcher so connections to
	// `<node>.<tailnet>.ts.net:7788` get the LE cert (ATS-trusted,
	// no pinning needed) while LAN/mDNS/IP-literal connections keep
	// getting the self-signed cert (iOS pins fingerprint at first
	// contact). Detection + mint runs in a goroutine so a slow
	// Tailscale CLI doesn't block the listen step. A renewer ticks
	// every 24 h and re-mints inside the 14-day pre-expiry window.
	// All errors logged but never fatal — bridge runs identically
	// to today on hosts without Tailscale installed.
	tailscaleAuto := newTailscaleAutoPilot(cfg.DataDir, cfg.ListenAddress, certManager, stderr)
	tailscaleAuto.Start(scanCtx)

	// Optional fsnotify-based instant-update watcher. Off by default
	// (cfg.LibraryWatch.Enabled). When on, the periodic full scan
	// remains the safety net — the watcher just shortens the
	// time-to-visibility for newly-dropped files in the common case.
	// Failure to construct the watcher (e.g. older kernel without
	// inotify support) is non-fatal — log a Warn and continue
	// periodic-only.
	if cfg.LibraryWatch.Enabled {
		debounce := time.Duration(cfg.LibraryWatch.EffectiveDebounceSeconds()) * time.Second
		watcher, werr := manifest.NewWatcher(scanner, debounce)
		if werr != nil {
			fmt.Fprintf(stderr, "library watcher: %v (periodic scan still active)\n", werr)
		} else {
			go func() {
				if err := watcher.Run(scanCtx); err != nil {
					fmt.Fprintf(stderr, "library watcher exited: %v\n", err)
				}
			}()
		}
	}

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
	// artworkDir is defined above (single source of truth) and shared
	// with the scanner so scanner-side `local-*` files and enricher-
	// side `<mbid>-*` files cohabit one directory.
	enricher := enrich.NewEnricher(manifestStore, mbClient, caaClient, deezerClient, artworkDir)
	go enricher.Run(scanCtx)

	// Periodic state-snapshot ticker. Captures bridge.db / tokens.json /
	// cert / key / config into <dataDir>/backups/<timestamp>/ at the
	// configured cadence (default 24h). Uses the same scanCtx as the
	// other periodic workers so a SIGINT cancels it cleanly. Snapshots
	// are best-effort — failures are logged but never crash serve.
	//
	// `EffectiveIntervalHours` returns 0 when the operator has explicitly
	// disabled the ticker (`intervalHours: 0`); we skip the goroutine in
	// that case. The on-demand CLI path stays available regardless.
	backupSources := buildBackupSources(cfg, *configPath)
	if hrs := cfg.Backup.EffectiveIntervalHours(); hrs > 0 {
		backupInterval := time.Duration(hrs) * time.Hour
		go runBackupTicker(scanCtx, backupSources, cfg.Backup.EffectiveKeep(), backupInterval, stdout, stderr)
	}

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
		// AutoInstall is on every platform now that Phase B-Windows
		// (PR #48) wired the rename-trick swap with SCM-stop
		// coordination. The auto-installer still gates on the
		// session tracker, quiet-hours, and the Phase C compat
		// gate identically across platforms.
		AutoInstall: cfg.Update.AutoInstall,
		// Compat-gate token snapshot. The updater calls this on each
		// install attempt to decide whether the candidate's
		// MinClientVersion would orphan a still-paired older client.
		TokenSnapshot: store.List,
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
	if cfg.Update.AutoInstall {
		// Auto-install wires the install opts when the operator
		// opted in via config. Phase B-Windows (PR #48) added the
		// SCM-coordinated rename-trick swap, so Windows is now a
		// supported auto-install platform — same gate sequence as
		// darwin/linux.
		updOpts.AutoInstallOpts = &updater.InstallOptions{
			DataDir:    cfg.DataDir,
			BinaryPath: binaryPath,
			Sessions:   sessions,
			Force:      false,
		}
		// On successful auto-install we exit; service-manager
		// (launchd / systemd / SCM) respawns into the new binary.
		// The Phase B `maybeRollbackOnBoot` housekeeping then
		// verifies version-match and either confirms or rolls back.
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
		// Phase B-Windows (PR #48) wired the swap path on Windows
		// alongside darwin/linux. CanInstall is true everywhere
		// the binary builds.
		canInstall: true,
	}

	// pairing.Store backs the admin-approval pairing flow (POST/GET/DELETE
	// /v1/pairing/*). In-memory: pending requests are ephemeral by design,
	// and a bridge restart is detected by iOS via the bridgeStartedAt echo.
	// Approve mints a real bearer token via auth.Store.Mint; an undelivered
	// approval (TTL+grace without iOS DELETE ack) revokes the minted token
	// to prevent orphans after a network blip mid-handoff.
	pairingStore := pairing.NewStore(pairing.Options{
		RevokeToken: store.Revoke,
	})
	defer pairingStore.Close()

	// Upscale feature gate: config flag + sox-on-PATH startup probe.
	// `cfg.Upscale.Enabled == true` AND a working sox in PATH are
	// the joint precondition for the feature. A missing sox with
	// the flag on logs an error and degrades to "feature off"
	// in-memory — the rest of the server keeps running unaffected.
	// iOS sees `upscaleEnabled: false` on /v1/health in either
	// disabled case.
	upscaleActive := cfg.Upscale.Enabled
	if upscaleActive {
		if err := transcode.PrecheckSox(); err != nil {
			fmt.Fprintf(stderr, "upscale: feature is enabled in bridge.yaml but sox is not available — disabling: %v\n", err)
			upscaleActive = false
		}
	}
	provider.SetUpscaleEnabled(upscaleActive)

	apiSrv := api.New(cfg, store, provider, fingerprint).
		WithArtworkDirs(artworkDirBridge(artworkDir)).
		WithMBIDProbe(provider).
		WithUpdater(updAdapter).
		WithSessionTracker(sessions).
		WithPairing(pairingStore).
		WithUpscale(upscaleActive, &variantStoreAdapter{provider: provider})

	// Background sweep for the pairing rate-limiter's per-IP map.
	// Hourly cadence drops limiters untouched for ≥ 6 h, keeping the
	// map bounded under high churn (operator deep-links + diverse
	// client-IP set on the LAN). Stop fn is deferred so the goroutine
	// exits cleanly on shutdown.
	stopRLGC := apiSrv.StartPairingRateLimitGC()
	defer stopRLGC()

	// Phase 2.5: long-lived transcode worker pool inside `bridge
	// serve`. Only instantiated when the feature is fully active
	// — saves goroutines + a manifest store reference when the
	// operator hasn't opted in, and matches the "off means
	// completely off" guarantee the iOS gating relies on.
	//
	// Constructed AFTER apiSrv so the adapter can borrow the
	// api server's Resolver instance via `apiSrv.Resolver()`
	// instead of building a snapshot from cfg.LibraryRoots.
	// Critical: the api Resolver hot-reloads via SetRoots when
	// the admin removes/adds a library root at runtime; a
	// snapshot resolver would silently keep routing to the old
	// root set and the upscale endpoint would either resolve
	// stale paths or 404 on freshly-added ones (Qodo bug 2 on
	// PR #109).
	//
	// Pool lives for the rest of serveCmd's lifetime; deferred
	// Stop() drains in-flight sox processes during graceful
	// shutdown (SIGTERM from the service manager → cancellable
	// via `transcode.Pool.stopCtx`). The defer fires AFTER
	// httpSrv.Shutdown completes, so accepting POST /v1/upscale
	// can't race the pool teardown.
	var upscalePool *transcode.Pool
	if upscaleActive {
		upscalePool = transcode.NewPool(manifestStore, cfg.Upscale.EffectiveWorkers(), cfg.Upscale.EffectiveQueueCap())
		defer upscalePool.Stop()
		apiSrv.WithUpscaleEnqueuer(&upscaleEnqueuerAdapter{
			pool:      upscalePool,
			store:     manifestStore,
			resolver:  apiSrv.Resolver(),
			cfg:       cfg,
			outputDir: filepath.Join(cfg.DataDir, "transcoded"),
		})
	}

	// /v1/upscale/stats wiring. Always registered so paired iOS
	// clients can render a clean "feature off" state on bridges where
	// the operator hasn't enabled upscaling — same nil-safe contract
	// the admin tile uses. The closure mirrors the admin
	// /api/upscale/stats handler exactly so the operator's Settings
	// page and the iOS management section show the same numbers.
	//
	// Three sources combined:
	//   1. Live pool counters — only when upscalePool != nil AND
	//      cfg.Upscale.Enabled (operator can disable mid-flight via a
	//      PATCH; the long-lived Pool stays alive until restart, but
	//      we honour the live flag and report no pool to keep the wire
	//      semantics in lockstep with /v1/health.upscaleEnabled).
	//   2. Cached-variants count + total bytes from `track_variants`
	//      — survives across restarts and reflects historical work,
	//      so it stays non-zero when the feature was disabled without
	//      `--gc`. SQL failure degrades to "0 cached" with a logged
	//      warning rather than turning the whole response into a 5xx.
	//   3. Sox-availability probe — same `transcode.PrecheckSox` the
	//      admin tile consumes, gated by the same 30 s TTL cache the
	//      admin handler uses (the admin cache holds it; we re-probe
	//      directly here, accepting one extra fork-exec per 5 s poll
	//      since iOS only polls when the management page is fore-
	//      grounded — typically zero polls per minute on average).
	apiSrv.WithUpscaleStats(&upscaleStatsAdapter{
		pool:    func() *transcode.Pool { return upscalePool },
		enabled: func() bool { return upscalePool != nil && cfg.Upscale.Enabled },
		store:   manifestStore,
	})
	httpSrv := &http.Server{
		Addr:    cfg.ListenAddress,
		Handler: apiSrv.Handler(),
		TLSConfig: &tls.Config{
			GetCertificate: certManager.Get,
			MinVersion:     tls.VersionTLS12,
		},
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
		Cfg:             cfg,
		CfgPath:         absCfgPath,
		Auth:            store,
		Manifest:        manifestStore,
		Scanner:         scanner,
		Resolver:        apiSrv.Resolver(),
		Fingerprint:     fingerprint,
		StartedAt:       time.Now().UTC(),
		ScanCtx:         scanCtx,
		Updater:         updAdapter,
		BackupSources:   backupSources,
		Tailscale:       tailscaleAdminAdapter{auto: tailscaleAuto},
		Pairing:         pairingStore,
		IsSupervised:    supervision.IsSupervised(),
		UpscalePrecheck: transcode.PrecheckSox,
		UpscaleStats: func() *admin.UpscalePoolStats {
			// Snapshot the pool's live counters when the
			// feature is active. Two off-paths return nil
			// so the admin handler omits the `pool` field
			// entirely instead of surfacing zero-padded
			// clutter on the Settings page:
			//
			//   1. upscalePool == nil — sox-precheck demoted
			//      the feature at startup OR the operator
			//      never enabled it.
			//   2. cfg.Upscale.Enabled == false — operator
			//      just PATCHed the flag off; the long-
			//      lived Pool is still alive until restart,
			//      but the contract is "feature is off
			//      live", so don't surface live counters
			//      (CodeRabbit minor on PR #110 — the iOS-
			//      facing /v1/health.upscaleEnabled and the
			//      admin tile's `enabled` field both gate
			//      on this).
			if upscalePool == nil || !cfg.Upscale.Enabled {
				return nil
			}
			s := upscalePool.Stats()
			return &admin.UpscalePoolStats{
				Workers:  s.Workers,
				QueueCap: s.QueueCap,
				QueueLen: s.QueueLen,
				Inflight: s.Inflight,
				Enqueued: s.Enqueued,
				Done:     s.Done,
				Failed:   s.Failed,
			}
		},
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
	// Same artwork-cache directory the long-running serve mode uses.
	// Standalone `bridge scan` runs a one-shot full pass and exits;
	// without this, scanner-side local-artwork extraction would be a
	// no-op for the CLI scan path.
	artworkDir := filepath.Join(cfg.DataDir, "artwork")
	scanner := manifest.NewScanner(cfg.LibraryRoots, store, artworkDir)

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
