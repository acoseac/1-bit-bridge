// Package doctor runs environment preflight checks for `bridge init`
// and `bridge doctor`. Each check is a pure function with a stable name
// and a one-line hint on failure — enough for an operator to know what
// to fix without reading the source.
//
// The contract is deliberately small so the same function powers the
// CLI ("bridge doctor") and an eventual admin-console panel.
package doctor

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/acoustid"
	servertls "github.com/acoseac/1-bit-bridge/internal/tls"
	"github.com/acoseac/1-bit-bridge/internal/transcode"
)

// probeTimeout bounds every subprocess this package spawns, wrapped around
// the INCOMING context so a caller's cancellation aborts early while the cap
// still applies to a background-context caller.
//
// The two toolchain checks already had this via transcode.ProbeSox /
// acoustid.Probe (both 2 s, both ctx-wrapping); the value is repeated here
// for the two sites that shell out directly — `systemctl --user
// show-environment` and lsof. Neither had ANY bound: a user systemd/DBus
// session that stops answering blocked `GET /api/doctor`'s goroutine past
// client disconnect (the admin http.Server deliberately sets no
// WriteTimeout, so nothing reaps it), and lsof stat()s mount points while
// building its device cache, so a wedged network mount — the shape of the
// rclone FUSE mount on the production VPS — hung `bridge doctor` with no
// output at all.
const probeTimeout = 2 * time.Second

// Status is the outcome of a single check.
type Status string

const (
	OK   Status = "ok"
	Warn Status = "warn"
	Fail Status = "fail"
)

// Check-name slugs. Stable identifiers consumers use as keys for test
// assertions, admin-UI mapping, or CI dashboards. Extracted because
// SonarCloud go:S1192 flagged the repeated literals across the doctor
// per-check builders (warn/fail/ok branches in checkConfigDir,
// checkTLSCert, checkLibraryRoots, checkServiceManager).
const (
	checkNameConfigDir      = "config-dir"
	checkNameTLSCert        = "tls-cert"
	checkNameLibraryRoots   = "library-roots"
	checkNameServiceManager = "service-manager"
	checkNameAudioToolchain = "audio-toolchain"
	checkNameFingerprint    = "fingerprint-toolchain"
)

// Check is one line of the doctor report.
type Check struct {
	// Name is a stable slug (kebab-case, no spaces). Consumers use it as
	// a key for test assertions, admin-UI mapping, or CI dashboards.
	Name string
	// Status is ok / warn / fail.
	Status Status
	// Summary is a short human-readable description of what was tested.
	Summary string
	// Hint (fail/warn only) tells the operator how to fix it.
	Hint string
}

// Deps bundles the inputs doctor needs. All fields are required; an
// empty ConfigDir is treated as "use the per-OS default", an empty
// DataDir likewise. LibraryRoots may be empty (first-run, no config
// yet), in which case the library-roots check is skipped.
type Deps struct {
	ConfigDir    string
	DataDir      string
	LibraryRoots []string
	// APIPort is the main HTTPS port the server binds, typically 7788.
	APIPort int
	// AdminPort is the loopback admin console port, typically 7789.
	AdminPort int
	// OwnPIDFile, when set, points at the file `bridge serve` writes
	// when it's running. A port bound by this PID is treated as OK
	// (doctor must be idempotent while the server is running). Empty
	// skips the own-PID check — any bind is fail.
	OwnPIDFile string
	// OwnedPorts lists ports the CALLER knows it bound itself.
	//
	// Only an in-process caller can populate this honestly — the admin
	// console running inside `bridge serve`, which bound those listeners
	// and does not have to deduce anything. It is checked BEFORE the
	// bind probe, so it needs no attribution and cannot be defeated by
	// the capability/dumpable=0 problem that makes port→pid attribution
	// impossible for an unprivileged observer.
	//
	// The CLI leaves this empty and keeps using OwnPIDFile, which
	// answers the same question by a weaker mechanism because from
	// outside the process there is nothing better.
	//
	// A field rather than a functional option, deliberately: OwnPIDFile
	// is the same kind of caller assertion and is a field, and adding an
	// options mechanism for one flag would leave this package with two
	// ways to say the same sort of thing.
	OwnedPorts []int
	// LibraryWatchEnabled mirrors cfg.LibraryWatch.Enabled. When
	// true on Linux, the doctor's inotify watch-limit check
	// activates — the operator gets a warning if their kernel
	// budget would be exhausted by the configured roots before
	// the bridge tries to register watches at runtime.
	LibraryWatchEnabled bool
	// UpscaleEnabled / AnalysisEnabled mirror cfg.Upscale.Enabled /
	// cfg.Analysis.Enabled. When either is true, checkAudioToolchain
	// verifies sox is present AND its build has FLAC support (the
	// bridge forces `-t flac`, so a FLAC-less sox would fail every
	// job at runtime). Both false → the check is a no-op "not enabled".
	UpscaleEnabled  bool
	AnalysisEnabled bool

	// FingerprintEnabled mirrors cfg.Fingerprint.Enabled. When true,
	// checkFingerprintToolchain verifies fpcalc is present AND an AcoustID key
	// is configured. False → the check is a no-op "not enabled".
	FingerprintEnabled bool
	// FingerprintHasAPIKey reports whether a key resolved from either the
	// environment or the config. The doctor never sees the key itself: there
	// is no reason for a diagnostic report to carry a credential, and reports
	// get pasted into issues.
	FingerprintHasAPIKey bool
}

// Report is the collection of checks from a single doctor run.
type Report struct {
	Checks []Check
}

// OKCount / WarnCount / FailCount are the tallies printed in the CLI
// footer.
func (r *Report) OKCount() int   { return r.count(OK) }
func (r *Report) WarnCount() int { return r.count(Warn) }
func (r *Report) FailCount() int { return r.count(Fail) }

func (r *Report) count(s Status) int {
	n := 0
	for _, c := range r.Checks {
		if c.Status == s {
			n++
		}
	}
	return n
}

// HasFail returns true if any check failed. init() uses this to bail
// before touching the config file.
func (r *Report) HasFail() bool { return r.FailCount() > 0 }

// Run executes every check against d and returns the report.
//
// EVERY check takes the context, including the ones that have nothing to do
// with it today. The alternative — ctx only where it's currently needed —
// makes adding a check that shells out a two-step change, and the step
// that's easy to miss is the one that matters: half the checks here exec
// something, and the failure mode of forgetting is an unbounded hang on a
// request-path goroutine, which is exactly the bug this signature exists to
// close.
func Run(ctx context.Context, d Deps) Report {
	checks := []func(context.Context, Deps) Check{
		checkPlatform,
		checkConfigDir,
		checkTLSCert,
		checkAPIPort,
		checkAdminPort,
		checkLibraryRoots,
		checkServiceManager,
		checkBrowserOpener,
		checkInotifyLimit,
		checkAudioToolchain,
		checkFingerprintToolchain,
	}
	out := make([]Check, 0, len(checks))
	for _, fn := range checks {
		out = append(out, fn(ctx, d))
	}
	return Report{Checks: out}
}

// --- individual checks ---

func checkPlatform(_ context.Context, d Deps) Check {
	// Everything we ship a binary for.
	supportedOS := map[string]bool{"darwin": true, "linux": true, "windows": true}
	supportedArch := map[string]bool{"amd64": true, "arm64": true}
	if supportedOS[runtime.GOOS] && supportedArch[runtime.GOARCH] {
		return ok("platform", fmt.Sprintf("%s/%s", runtime.GOOS, runtime.GOARCH))
	}
	return fail("platform",
		fmt.Sprintf("%s/%s", runtime.GOOS, runtime.GOARCH),
		"bridge ships binaries for darwin/linux/windows on amd64 or arm64; other combos must build from source")
}

func checkConfigDir(_ context.Context, d Deps) Check {
	dir := d.ConfigDir
	if dir == "" {
		return warn(checkNameConfigDir, "no config dir set", "pass Deps.ConfigDir so doctor can verify write access")
	}
	// Ensure it exists (create if missing — init() does this anyway,
	// but doctor running standalone should report the same outcome
	// whether or not init has been attempted). 0o700 matches init.go's
	// owner-only hardening so doctor doesn't leave a wider-mode dir.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fail(checkNameConfigDir, dir, "can't create: "+err.Error())
	}
	// Touch a temp file to verify write access — MkdirAll's success
	// isn't proof (the dir could exist read-only).
	probe := filepath.Join(dir, ".doctor-probe")
	if err := os.WriteFile(probe, []byte("probe"), 0o600); err != nil {
		return fail(checkNameConfigDir, dir, "not writable: "+err.Error())
	}
	_ = os.Remove(probe)
	return ok(checkNameConfigDir, dir)
}

func checkTLSCert(_ context.Context, d Deps) Check {
	if d.DataDir == "" {
		return warn(checkNameTLSCert, "no data dir set",
			"pass Deps.DataDir so doctor can inspect cert state")
	}
	certPath, keyPath := servertls.DefaultPaths(d.DataDir)
	certExists := fileExists(certPath)
	keyExists := fileExists(keyPath)
	switch {
	case certExists && keyExists:
		return ok(checkNameTLSCert, "present")
	case !certExists && !keyExists:
		// Fresh install — init() will mint on first serve.
		return ok(checkNameTLSCert, "absent (init will mint)")
	default:
		// One file without the other is an error no automatic recovery
		// handles safely — deleting the survivor would break existing
		// client pins.
		return fail(checkNameTLSCert, "partial state",
			fmt.Sprintf("found %q but not its pair; remove the orphan and re-run init",
				firstPresent(certPath, keyPath, certExists, keyExists)))
	}
}

func checkAPIPort(ctx context.Context, d Deps) Check {
	if owned := ownedPortCheck("port-api", d.APIPort, d.OwnedPorts); owned != nil {
		return *owned
	}
	return checkPort(ctx, "port-api", d.APIPort, d.OwnPIDFile)
}

func checkAdminPort(ctx context.Context, d Deps) Check {
	if owned := ownedPortCheck("port-admin", d.AdminPort, d.OwnedPorts); owned != nil {
		return *owned
	}
	return checkPort(ctx, "port-admin", d.AdminPort, d.OwnPIDFile)
}

// ownedPortCheck short-circuits a port check the caller has told us it
// bound itself, returning nil when the port isn't claimed.
//
// Takes the port explicitly rather than re-deriving it from `name`. The
// first version matched `name == "port-admin"` to pick between
// d.APIPort and d.AdminPort — inside a helper that was already being
// handed the name — so a third port check would have silently been
// graded against the API port.
//
// This runs BEFORE the bind probe, which is the whole point: an
// in-process caller doesn't need to deduce ownership from a bind failure
// and an lsof lookup, and on a capability-granted binary that deduction
// is impossible anyway (dumpable=0 denies port→pid attribution to any
// unprivileged observer — see portowner_linux.go). Probing here would
// also be actively wrong: the port IS in use, by us, so the probe can
// only fail.
func ownedPortCheck(name string, port int, ownedPorts []int) *Check {
	if port == 0 {
		return nil
	}
	for _, p := range ownedPorts {
		if p == port {
			c := ok(name, fmt.Sprintf("bound by this bridge (:%d)", port))
			return &c
		}
	}
	return nil
}

// listenFunc is the TCP bind probe used by checkPort. A package var so
// tests can inject a synthetic bind failure (e.g. a non-EADDRINUSE error
// like EACCES) deterministically — the same test-seam convention as
// portProbeAvailable. Production code MUST NOT mutate it.
var listenFunc = net.Listen

// probeBind attempts a bind and immediately releases it, returning the
// bind error (nil when the address was free). Split out so checkPort can
// ask the same question of each address family.
func probeBind(addr string) error {
	lis, err := listenFunc("tcp", addr)
	if err == nil {
		_ = lis.Close()
	}
	return err
}

// checkPort probes `port` on 127.0.0.1. If the bind succeeds the port is
// reported free; if it fails with "address already in use" and the
// holding PID matches our OwnPIDFile, we report ok — doctor is
// idempotent while the server is running. Any other binder is a fail.
// A bind failure that ISN'T EADDRINUSE (e.g. EACCES on a privileged port
// without elevation) is a Warn, not a Fail — it's a privilege/environment
// issue, not a port conflict.
//
// Limitation: this probes loopback ONLY, so a conflict bound to a
// specific non-loopback interface (e.g. 192.168.1.5:port) isn't detected
// here and would surface later as EADDRINUSE from `bridge serve`'s
// wildcard bind. The loopback-only probe is deliberate: the admin port
// binds loopback, so a wildcard probe here would false-fail it whenever
// any unrelated service holds the same port on another interface.
func checkPort(ctx context.Context, name string, port int, ownPIDFile string) Check {
	if port == 0 {
		return warn(name, "no port set", "pass Deps."+name+"Port")
	}
	// Probe BOTH address families. Binding only 127.0.0.1 reports a port
	// as free when something holds it on IPv6 alone — `[::]:port` under
	// `bindv6only`, or an explicit `[::1]:port`. The bridge's own default
	// listen address is a wildcard, so this is not exotic: doctor said
	// "free", init proceeded, and serve then failed to bind. The port is
	// occupied if EITHER family says so.
	v4err := probeBind(net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	v6err := probeBind(net.JoinHostPort("::1", strconv.Itoa(port)))

	inUse := isAddrInUse(v4err) || isAddrInUse(v6err)
	if !inUse && (v4err == nil || v6err == nil) {
		// At least one family bound cleanly and neither reported a
		// conflict. The other family failing is an environment fact, not
		// a conflict — a v4-only host returns EADDRNOTAVAIL for ::1 —
		// and must not be reported as a problem.
		return ok(name, fmt.Sprintf("free (:%d)", port))
	}
	// Neither family bound. Report against whichever error is
	// informative, preferring IPv4 since that is the one an operator
	// will recognise.
	err := v4err
	if err == nil {
		err = v6err
	}
	// Only "address already in use" means the port is genuinely occupied.
	// Other bind failures — EACCES (a privileged port <1024 without
	// elevation), EADDRNOTAVAIL, a transient network error — are
	// environment/privilege problems, NOT a port conflict. Reporting them as
	// the hard "another process owns this port" Fail would be wrong and would
	// block `bridge init`; degrade to a Warn that names the real cause.
	//
	// isAddrInUse is platform-split rather than a bare
	// errors.Is(err, syscall.EADDRINUSE): on Windows that constant is an
	// INVENTED value (syscall.APPLICATION_ERROR + iota, per
	// zerrors_windows.go's "Invented values to support what package os and
	// others expects"), while a real bind conflict is WSAEADDRINUSE (10048),
	// which stdlib syscall doesn't even define and nothing translates. The
	// bare form is therefore always false on Windows — which silently
	// degraded every real conflict to a Warn (letting `bridge init` proceed
	// into a serve that can't bind) AND made the native GetExtendedTcpTable
	// owner attribution below unreachable there.
	if !inUse {
		return warn(name, fmt.Sprintf(":%d not bindable", port),
			"couldn't bind to probe this port ("+err.Error()+"); "+
				"ports below 1024 need elevation, or the configured address may be invalid — check bridge.yaml")
	}
	// Port is in use. Is it us?
	if ownPIDFile != "" {
		if ownPID, readErr := readPID(ownPIDFile); readErr == nil && ownPID > 0 {
			found, probeErr := isPIDListeningOnPort(ctx, port, ownPID)
			switch {
			case probeErr != nil:
				// The probe MECHANISM failed (e.g. an antivirus blocked
				// the iphlpapi.dll load on Windows, or lsof errored). We
				// genuinely can't attribute the port, so degrade to Warn
				// rather than a hard Fail that would cry wolf about the
				// bridge's own port on a live install. (Fail-safe: a broken
				// probe must never break a healthy install.)
				return warn(name, fmt.Sprintf(":%d in use", port),
					"port is bound but the owner probe failed on this host; "+
						"if it's your running bridge this is expected, otherwise "+
						"stop the other process or change the address in bridge.yaml")
			case found:
				return ok(name, fmt.Sprintf("bound by our own bridge (pid %d)", ownPID))
			case pidAliveFunc(ownPID):
				// The probe ran cleanly and did NOT name our PID, yet the
				// PID we recorded at startup is still running. On a bridge
				// that binds a privileged port through a file capability
				// (`setcap cap_net_bind_service=+ep`, which the deployment
				// runbook prescribes so a non-root service can bind :443)
				// this is the EXPECTED result, not a conflict: that binary
				// runs with dumpable=0, so no unprivileged observer can
				// attribute the port to a pid — lsof, `ss -p` and a direct
				// readlink of /proc/<pid>/fd all fail identically.
				//
				// Last resort before giving up: ask whether the listener is
				// at least owned by OUR USER. On Linux that survives
				// dumpable=0 (see portowner_linux.go); everywhere else it
				// answers "don't know" and we fall through to the Warn.
				if owned, ownErr := portOwnerFunc(port); ownErr == nil && owned {
					return ok(name, fmt.Sprintf(
						"in use by a process running as this user (uid %d; pid attribution blocked — capability-bound binary)",
						os.Getuid()))
				}
				// "Our recorded pid is alive and something holds the port"
				// is materially different from "we have no idea who owns
				// this", and only the second deserves a Fail.
				return warn(name, fmt.Sprintf(":%d in use", port),
					fmt.Sprintf("our bridge (pid %d) is still running, but this host would not attribute "+
						"the port to it — a binary granted cap_net_bind_service runs with dumpable=0, which "+
						"blocks port→pid attribution for any non-root observer. If that is this install, "+
						"this is expected; otherwise stop the other process or change the address in bridge.yaml",
						ownPID))
			}
		}
	}
	// Couldn't attribute the bound port to our own bridge. If the owner
	// probe isn't available on this host at all, we genuinely can't tell
	// "our running bridge" apart from a real conflict — Warn instead of a
	// hard Fail that cries wolf on every `bridge doctor` run on a live
	// install (a host with no lsof and no native probe). (goreview F9)
	if !portProbeAvailable() {
		return warn(name, fmt.Sprintf(":%d in use", port),
			"port is bound but the owner couldn't be identified on this host; "+
				"if it's your running bridge this is expected, otherwise stop the "+
				"other process or change the address in bridge.yaml")
	}
	return fail(name, fmt.Sprintf(":%d in use", port),
		"another process owns this port; stop it or pick a different address in bridge.yaml")
}

func checkLibraryRoots(_ context.Context, d Deps) Check {
	if len(d.LibraryRoots) == 0 {
		return ok(checkNameLibraryRoots, "none configured (init will prompt)")
	}
	missing := []string{}
	unreadable := []string{}
	empty := []string{}
	for _, r := range d.LibraryRoots {
		info, err := os.Stat(r)
		if err != nil {
			missing = append(missing, r)
			continue
		}
		if !info.IsDir() {
			unreadable = append(unreadable, r+" (not a directory)")
			continue
		}
		entries, err := os.ReadDir(r)
		if err != nil {
			unreadable = append(unreadable, r+" ("+err.Error()+")")
			continue
		}
		if len(entries) == 0 {
			empty = append(empty, r)
		}
	}
	if len(missing)+len(unreadable) > 0 {
		problems := append(append([]string{}, missing...), unreadable...)
		return fail(checkNameLibraryRoots, fmt.Sprintf("%d problem(s)", len(problems)),
			"fix or remove: "+strings.Join(problems, "; "))
	}
	if len(empty) > 0 {
		return warn(checkNameLibraryRoots, fmt.Sprintf("%d empty root(s)", len(empty)),
			"empty root (scan will find 0 tracks): "+strings.Join(empty, "; "))
	}
	return ok(checkNameLibraryRoots, fmt.Sprintf("%d root(s) reachable", len(d.LibraryRoots)))
}

func checkServiceManager(ctx context.Context, d Deps) Check {
	switch runtime.GOOS {
	case "darwin":
		if _, err := exec.LookPath("launchctl"); err != nil {
			return fail(checkNameServiceManager, "launchctl missing",
				"`launchctl` is part of macOS; missing implies a broken install — use `bridge init --no-service` to skip")
		}
		return ok(checkNameServiceManager, "launchctl available")
	case "linux":
		// A user-level systemd install needs a DBus session. Detect by
		// running `systemctl --user show-environment`; it prints
		// something only if the user-bus is reachable.
		//
		// CommandContext-bounded: an unresponsive (as opposed to absent)
		// systemd/DBus session leaves this blocked indefinitely, and the
		// admin console reaches it from a request goroutine that nothing
		// else reaps.
		probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
		defer cancel()
		cmd := exec.CommandContext(probeCtx, "systemctl", "--user", "show-environment")
		if err := cmd.Run(); err != nil {
			// THREE distinct outcomes, not two. `probeCtx.Err()` is
			// non-nil for a caller cancellation as well as for the local
			// deadline, so keying the message off it alone reports a
			// wedged DBus session "after 2s" when the admin client
			// actually disconnected at 50ms. Check the INCOMING ctx
			// first — an aborted probe learned nothing about systemd and
			// must not claim otherwise.
			if ctxErr := ctx.Err(); ctxErr != nil {
				return warn(checkNameServiceManager, "systemd probe aborted",
					"the probe was cancelled before it finished ("+ctxErr.Error()+
						"); this says nothing about the systemd session — re-run when the caller isn't going away")
			}
			if probeCtx.Err() != nil {
				return warn(checkNameServiceManager, "systemd probe timed out",
					"`systemctl --user show-environment` did not answer within "+probeTimeout.String()+
						"; the user DBus session may be wedged — check `systemctl --user status`")
			}
			return warn(checkNameServiceManager, "no user systemd session",
				"headless session? use `bridge init --no-service` and run `bridge serve` yourself")
		}
		return ok(checkNameServiceManager, "systemctl --user reachable")
	case "windows":
		dir := windowsStartupDir()
		if dir == "" {
			return warn(checkNameServiceManager, "can't resolve Startup folder",
				"set %APPDATA% and re-run — doctor needs it to place the login shortcut")
		}
		if err := probeWritable(dir); err != nil {
			return fail(checkNameServiceManager, "Startup folder not writable",
				dir+": "+err.Error())
		}
		return ok(checkNameServiceManager, "Startup folder writable: "+dir)
	default:
		return warn(checkNameServiceManager, runtime.GOOS+" unsupported",
			"no service-install path for this OS; run `bridge serve` manually")
	}
}

func checkBrowserOpener(_ context.Context, d Deps) Check {
	var candidates []string
	switch runtime.GOOS {
	case "darwin":
		candidates = []string{"open"}
	case "linux":
		candidates = []string{"xdg-open"}
	case "windows":
		candidates = []string{"cmd.exe", "cmd"}
	}
	for _, c := range candidates {
		if _, err := exec.LookPath(c); err == nil {
			return ok("browser-opener", c)
		}
	}
	return warn("browser-opener", "no opener found",
		"install missing; bridge will still print the admin URL for you to paste manually")
}

// checkAudioToolchain verifies the sox dependency for the offline-decode
// features (upscaling / audio analysis). It is a no-op "not enabled" when
// neither feature is on — doctor must not nag about an optional dependency
// a minimal install never uses (and `bridge init` preflight, which doesn't
// set the flags, always sees "not enabled").
//
// When a feature IS enabled it checks more than presence: the bridge forces
// `-t flac` for every conversion, so a sox built WITHOUT FLAC passes the
// bare runnable check but fails every job at runtime — a silent,
// hard-to-diagnose failure. ProbeSox's FormatsKnown lets us stay
// conservative: a confirmed FLAC-absence fails the check; an unparseable
// `sox --help` is treated as "FLAC present" rather than crying wolf.
func checkAudioToolchain(ctx context.Context, d Deps) Check {
	if !d.UpscaleEnabled && !d.AnalysisEnabled {
		return ok(checkNameAudioToolchain, "not enabled (sox not required)")
	}
	info, err := transcode.ProbeSox(ctx)
	if err != nil {
		if errors.Is(err, transcode.ErrSoxMissing) {
			return fail(checkNameAudioToolchain, "sox not found",
				"upscaling/analysis is enabled but sox isn't on PATH; install it (e.g. `brew install sox`, `sudo apt install sox`) or disable the feature in bridge.yaml")
		}
		return fail(checkNameAudioToolchain, "sox not runnable",
			"sox is on PATH but failed to run: "+err.Error())
	}
	if info.FormatsKnown && !info.HasFLAC {
		return fail(checkNameAudioToolchain, "sox lacks FLAC support",
			"the installed sox build can't handle FLAC, which the bridge's internal pipeline requires; Debian/Ubuntu: `sudo apt install libsox-fmt-all`, elsewhere reinstall sox with FLAC")
	}
	if info.Version != "" {
		return ok(checkNameAudioToolchain, fmt.Sprintf("sox %s, FLAC supported", info.Version))
	}
	return ok(checkNameAudioToolchain, "sox present, FLAC supported")
}

// checkFingerprintToolchain verifies the acoustic-fingerprinting fallback can
// actually run when it is switched on.
//
// Two prerequisites, and BOTH are silent failures without this check: fpcalc
// on PATH, and an AcoustID application key. Missing either degrades the
// feature to off at startup with a single stderr line that scrolls away, so
// `bridge doctor` is where an operator finds out why fingerprinting never
// resolved anything.
//
// Mirrors checkAudioToolchain's shape, including the no-op when the feature is
// off — a host that will never fingerprint should not be nagged about a binary
// it does not need.
func checkFingerprintToolchain(ctx context.Context, d Deps) Check {
	if !d.FingerprintEnabled {
		return ok(checkNameFingerprint, "not enabled (fpcalc not required)")
	}
	info, err := acoustid.Probe(ctx)
	if err != nil {
		if errors.Is(err, acoustid.ErrFpcalcMissing) {
			return fail(checkNameFingerprint, "fpcalc not found",
				"fingerprinting is enabled but fpcalc isn't on PATH; install Chromaprint "+
					"(macOS: `brew install chromaprint`; Debian/Ubuntu: `sudo apt install libchromaprint-tools` "+
					"— note the binary is in the -tools package, not libchromaprint1; "+
					"Windows: `winget install AcoustID.Chromaprint`; Alpine: `apk add chromaprint`) "+
					"or disable the feature in bridge.yaml")
		}
		return fail(checkNameFingerprint, "fpcalc not runnable",
			"fpcalc is on PATH but failed to run: "+err.Error())
	}
	if !d.FingerprintHasAPIKey {
		return fail(checkNameFingerprint, "no AcoustID API key",
			"fingerprinting is enabled and fpcalc works, but no AcoustID key is configured; "+
				"register a free application key at https://acoustid.org/new-application and set "+
				"ACOUSTID_API_KEY (preferred) or fingerprint.apiKey in bridge.yaml")
	}
	if info.Version != "" {
		return ok(checkNameFingerprint, fmt.Sprintf("fpcalc %s, API key configured", info.Version))
	}
	return ok(checkNameFingerprint, "fpcalc present, API key configured")
}

// --- helpers ---

func ok(name, summary string) Check {
	return Check{Name: name, Status: OK, Summary: summary}
}

func warn(name, summary, hint string) Check {
	return Check{Name: name, Status: Warn, Summary: summary, Hint: hint}
}

func fail(name, summary, hint string) Check {
	return Check{Name: name, Status: Fail, Summary: summary, Hint: hint}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func firstPresent(certPath, keyPath string, certExists, keyExists bool) string {
	if certExists {
		return certPath
	}
	if keyExists {
		return keyPath
	}
	return ""
}

// readPID reads a bare-integer pidfile. Returns (0, err) on missing or
// malformed file — caller treats either as "no own-pid info".
func readPID(path string) (int, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		return 0, err
	}
	return n, nil
}

// probeWritable creates and removes a probe file to verify write access.
// Callers use it when os.Stat + mode bits isn't reliable (Windows
// permission model differs from unix).
func probeWritable(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	probe := filepath.Join(dir, ".doctor-probe")
	f, err := os.OpenFile(probe, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_ = f.Close()
	_ = os.Remove(probe)
	return nil
}

// windowsStartupDir resolves the per-user Startup folder. It first asks
// the SHGetKnownFolderPath API (FOLDERID_Startup, via knownStartupDir),
// which is robust against roaming / redirected enterprise profiles; on
// any failure (or off Windows) it falls back to the canonical
// %APPDATA%-relative path:
//
//	%APPDATA%\Microsoft\Windows\Start Menu\Programs\Startup
//
// Returns "" only when both the known-folder API and %APPDATA% are
// unavailable.
func windowsStartupDir() string {
	if p, ok := knownStartupDir(); ok {
		return p
	}
	appdata := os.Getenv("APPDATA")
	if appdata == "" {
		return ""
	}
	return filepath.Join(appdata, "Microsoft", "Windows", "Start Menu", "Programs", "Startup")
}

// isPIDListeningOnPort and portProbeAvailable are platform-provided —
// the lsof-backed unix implementation lives in doctor_notwindows.go and
// the native iphlpapi.dll implementation in doctor_windows.go. The "is it
// us?" branch of checkPort calls them; see their per-platform docs for the
// (found, error) contract.
//
// isPIDListeningOnPort takes the context on BOTH platforms even though only
// the unix one spawns a subprocess to bound. One signature keeps the caller
// from having to know which platform can hang, and hands the next
// implementation the context already.

// pidAliveFunc and portOwnerFunc indirect the two platform-provided probes
// that back checkPort's last-resort attribution arms, so tests can drive
// those branches deterministically: neither "a PID that is definitely
// dead" nor "a listener owned by a different user" can be conjured
// portably on demand, and asserting them against whatever the host happens
// to look like is how a test ends up passing for the wrong reason.
//
// Same seam convention as listenFunc and portProbeAvailable above.
// Production code MUST NOT mutate them.
var (
	pidAliveFunc  = pidAlive
	portOwnerFunc = portOwnedByThisUser
)

// ErrHasFail is returned by Run when the caller passes StopOnFail.
var ErrHasFail = errors.New("doctor reports one or more failing checks")
