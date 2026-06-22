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

	servertls "github.com/acoseac/1-bit-bridge/internal/tls"
	"github.com/acoseac/1-bit-bridge/internal/transcode"
)

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
func Run(d Deps) Report {
	checks := []func(Deps) Check{
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
	}
	out := make([]Check, 0, len(checks))
	for _, fn := range checks {
		out = append(out, fn(d))
	}
	return Report{Checks: out}
}

// --- individual checks ---

func checkPlatform(d Deps) Check {
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

func checkConfigDir(d Deps) Check {
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

func checkTLSCert(d Deps) Check {
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

func checkAPIPort(d Deps) Check {
	return checkPort("port-api", d.APIPort, d.OwnPIDFile)
}

func checkAdminPort(d Deps) Check {
	return checkPort("port-admin", d.AdminPort, d.OwnPIDFile)
}

// checkPort probes `port` on 127.0.0.1. If the bind succeeds the port is
// reported free; if it fails with "address already in use" and the
// holding PID matches our OwnPIDFile, we report ok — doctor is
// idempotent while the server is running. Any other binder is a fail.
//
// Limitation: this probes loopback ONLY, so a conflict bound to a
// specific non-loopback interface (e.g. 192.168.1.5:port) isn't detected
// here and would surface later as EADDRINUSE from `bridge serve`'s
// wildcard bind. The loopback-only probe is deliberate: the admin port
// binds loopback, so a wildcard probe here would false-fail it whenever
// any unrelated service holds the same port on another interface.
func checkPort(name string, port int, ownPIDFile string) Check {
	if port == 0 {
		return warn(name, "no port set", "pass Deps."+name+"Port")
	}
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	lis, err := net.Listen("tcp", addr)
	if err == nil {
		_ = lis.Close()
		return ok(name, fmt.Sprintf("free (:%d)", port))
	}
	// Port is bound. Is it us?
	if ownPIDFile != "" {
		if ownPID, readErr := readPID(ownPIDFile); readErr == nil && ownPID > 0 {
			found, probeErr := isPIDListeningOnPort(port, ownPID)
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

func checkLibraryRoots(d Deps) Check {
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

func checkServiceManager(d Deps) Check {
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
		cmd := exec.Command("systemctl", "--user", "show-environment")
		if err := cmd.Run(); err != nil {
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

func checkBrowserOpener(d Deps) Check {
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
func checkAudioToolchain(d Deps) Check {
	if !d.UpscaleEnabled && !d.AnalysisEnabled {
		return ok(checkNameAudioToolchain, "not enabled (sox not required)")
	}
	info, err := transcode.ProbeSox(context.Background())
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

// ErrHasFail is returned by Run when the caller passes StopOnFail.
var ErrHasFail = errors.New("doctor reports one or more failing checks")
