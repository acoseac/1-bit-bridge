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
	"io/fs"
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
// checkTLSCert, checkLibraryRoots, checkServiceManager), and again at the
// port checks' third use (#1022).
const (
	checkNamePortAPI        = "port-api"
	checkNamePortAdmin      = "port-admin"
	checkNameConfigDir      = "config-dir"
	checkNameTLSCert        = "tls-cert"
	checkNameLibraryRoots   = "library-roots"
	checkNameServiceManager = "service-manager"
	checkNameBrowserOpener  = "browser-opener"
	checkNameAudioToolchain = "audio-toolchain"
	// checkNameDSDRenderToolchain is the ffmpeg-decoder check behind the
	// DSD → PCM renditions (`upscale.dsdRender.enabled`).
	checkNameDSDRenderToolchain = "dsd-render-toolchain"
	checkNameFingerprint        = "fingerprint-toolchain"
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
	// ConfigFile is the caller's bridge.yaml lookup: which file it found
	// and whether that file loaded (checkConfigFile). Nil means the
	// caller did not look one up, which the check reports as skipped.
	// `bridge init`'s preflight leaves it nil on purpose: it grades the
	// install it is about to write, and a broken existing config must
	// not block the re-init that replaces it. Nor may its port checks,
	// over the bridge's own listeners: for that config init sets
	// OwnPIDPortsUnknown, so they grade init's defaults and the bridge
	// recorded in the data dir init writes excuses a port it is seen
	// listening on.
	//
	// When it records a config that did not load, the port checks are
	// not run: APIPort and AdminPort are then the caller's defaults, not
	// the config's (ungradedConfigPortCheck). When it records one this
	// user cannot read, or a named one that is not there, config-dir is
	// not run either (checkConfigDir). Neither decline applies to the
	// launcher's lookup (ConfigFile.PreSetup).
	ConfigFile *ConfigFile
	// APIPort is the main HTTPS port the server binds, typically 7788.
	APIPort int
	// AdminPort is the loopback admin console port, typically 7789.
	AdminPort int
	// TLSCertPath / TLSKeyPath mirror cfg.TLSCertPath / cfg.TLSKeyPath:
	// the cert pair `bridge serve` would actually load. BOTH empty (the
	// usual case, and what `bridge init` passes) falls back to
	// `<DataDir>/server.{crt,key}` — the same
	// `cfg.TLSCertPath`-or-defaults resolution serve and `bridge cert`
	// apply, and config validation already refuses one without the
	// other.
	//
	// Before this existed the cert checks always looked at the DataDir
	// defaults, so on an install with an explicit path they graded a
	// cert nobody serves — reporting "absent (init will mint)" about a
	// bridge whose cert is fine, or a partial-state fail about two
	// files it does not use.
	TLSCertPath string
	TLSKeyPath  string

	// CertSANs returns the SAN inputs a cert minted RIGHT NOW would
	// carry: the hostname, the Tailscale MagicDNS name and CGNAT
	// addresses, every up non-loopback interface IP, and the hosts of
	// `cfg.customEndpoints`. checkTLSCertSANs compares them against the
	// cert on disk.
	//
	// cmd/bridge wires it from the SAME helper `bridge serve` hands
	// LoadOrGenerateWithOptions and `bridge cert rotate` mints from, so
	// the doctor's verdict is a claim about what a rotation would
	// produce rather than a second opinion about it.
	//
	// Optional, like LibraryHasCodec: nil SKIPS the check rather than
	// guessing — `bridge doctor` with no readable config has no
	// customEndpoints to gather and would grade against a narrower set
	// than serve uses.
	//
	// It takes the context for the reason every check here does, but
	// what actually bounds it is its own probe: the Tailscale CLI call
	// underneath caps at 1.5 s and is TTL-cached for 30 s across the
	// process, which is what keeps `/api/doctor` on a settings-page
	// render from forking one per fetch. It does not observe
	// cancellation.
	CertSANs func(context.Context) servertls.GenerateOptions

	// OwnPIDFile, when set, points at the file `bridge serve` writes
	// when it's running. A port bound by this PID is treated as OK
	// (doctor must be idempotent while the server is running). Empty
	// skips the own-PID check — any bind is fail.
	OwnPIDFile string
	// OwnPIDPortsUnknown says the caller could not read which ports the
	// bridge recorded in OwnPIDFile binds: `bridge init` over an install
	// whose config is there and does not load. The pid file then comes
	// from the data dir init writes, and the ports graded are init's own
	// choice, so the only evidence that the recorded bridge holds one is
	// the probe seeing it listen there (checkChosenPort). Its being alive
	// says nothing about these ports, since a live bridge binds what its
	// config says and nothing here says what that was.
	OwnPIDPortsUnknown bool
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

	// DSDRenderEnabled mirrors cfg.Upscale.DSDRender.Enabled (folded with
	// cfg.Upscale.Enabled by the caller — the master toggle covers it).
	// When true, checkDSDRenderToolchain verifies ffmpeg is present with
	// the four dsd_* decoders (and warns without the dst one). When false
	// the check is a no-op — except that a library holding DSF / DFF gets
	// a tip, because the feature exists for exactly that library and is
	// opt-in by design.
	DSDRenderEnabled bool

	// FingerprintEnabled mirrors cfg.Fingerprint.Enabled. When true,
	// checkFingerprintToolchain verifies fpcalc is present AND an AcoustID key
	// is configured. False → the check is a no-op "not enabled".
	FingerprintEnabled bool
	// FingerprintHasAPIKey reports whether a key resolved from either the
	// environment or the config. The doctor never sees the key itself: there
	// is no reason for a diagnostic report to carry a credential, and reports
	// get pasted into issues.
	FingerprintHasAPIKey bool

	// LibraryHasCodec answers "does the indexed library contain any track
	// with this codec?". Optional: nil (a caller with no manifest to hand —
	// a first run, or a `bridge doctor` before any scan) SKIPS the checks
	// that depend on it rather than guessing. An error is likewise treated
	// as "don't know", never as "no".
	LibraryHasCodec func(ctx context.Context, codec string) (bool, error)

	// RelocatedSidecars counts the sidecar rows whose recorded path lies
	// outside the directory that kind of sidecar is currently written
	// to — see checkSidecarPaths. Optional, like LibraryHasCodec: nil
	// skips the check; an error is reported as "could not read", never
	// as "fine".
	RelocatedSidecars func(ctx context.Context) (RelocatedSidecars, error)

	// VariantsIndex compares the variant catalog with the sidecar files on
	// disk — see checkVariantsIndex. The other direction from
	// RelocatedSidecars, and the one nothing else reports: rows that went
	// missing while their files stayed. Optional on the same terms: nil
	// skips the check, an error is reported rather than answered as fine.
	//
	// It WALKS the variants directory, so the probe bounds itself (see
	// cmd/bridge's wiring) — `/api/doctor` is fetched on a settings-page
	// render, and an unbounded walk of a 100k-sidecar tree there would
	// turn a diagnostic into a load source.
	VariantsIndex func(ctx context.Context) (VariantsIndex, error)

	// LogPath is the file the service unit redirects this bridge's stderr
	// to — packaging.DefaultLogPath(). Empty (a foreground `bridge serve`,
	// which logs to its terminal) makes checkLogSize a no-op rather than a
	// complaint about a file that does not exist.
	LogPath string

	// Managed mirrors cfg.Deployment.IsManaged(): a control plane runs
	// this process and whoever is reading the report has no shell on the
	// host.
	//
	// It skips the two checks whose entire content is advice for the
	// person who STARTED the bridge — "no user systemd session, use
	// `bridge init --no-service`" and "no browser opener, install one".
	// Both are correct about a hosted appliance and both are addressed to
	// nobody: measured against a live tenant they were the only two
	// warnings in the report, so a healthy install read as two problems
	// with instructions its reader could not follow.
	//
	// Skipped rather than answered: the checks are not wrong, they are
	// about a machine that is not the reader's.
	Managed bool
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
		checkConfigFile,
		checkConfigDir,
		checkTLSCert,
		checkTLSCertSANs,
		checkAPIPort,
		checkAdminPort,
		checkLibraryRoots,
		checkServiceManager,
		checkBrowserOpener,
		checkInotifyLimit,
		checkAudioToolchain,
		checkDSDRenderToolchain,
		checkFingerprintToolchain,
		checkLogSize,
		checkSidecarPaths,
		checkVariantsIndex,
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

// checkConfigDir vouches that the bridge can create and write the directory
// its config lives in, where `bridge init`, config.Save and a relative
// dataDir all write. Its two probes, a MkdirAll and a write, answer for
// whoever runs doctor, so they run only for a user who can be the bridge's
// or is about to run `bridge init` there. A config this user cannot read
// says it is neither, and a named config that is not there leaves nothing
// to vouch for, so both get "not checked" instead. The launcher's
// pre-setup row is the exception to the first (ConfigFile.ungraded).
func checkConfigDir(_ context.Context, d Deps) Check {
	dir := d.ConfigDir
	if dir == "" {
		return warn(checkNameConfigDir, "no config dir set", "pass Deps.ConfigDir so doctor can verify write access")
	}
	switch d.ConfigFile.ungraded() {
	case configNotThere:
		// A config the caller NAMED that is not there (config-file FAILs
		// it) leaves this check nothing to vouch for, and the create below
		// would make the named file's directory: a typo'd `--config
		// /x/bridge.yml` left /x behind, 0700, and reported it ok beside
		// "does not exist". A diagnostic must not have that side effect.
		// The pre-setup lookups (no --config, or the launcher's row)
		// record no error for a config that is not there yet, so they
		// still create the dir init will use.
		return ok(checkNameConfigDir, "not checked: the named config does not exist")
	case configUnreadable:
		// A config this user may not read (config-file WARNs it) means
		// this user is not the one the bridge runs as, since the bridge
		// reads it at every start. The probes below would answer for this
		// user: measured with the 0700 config dir `bridge init` makes and
		// doctor run by another user, the write FAILed "not writable", so
		// the run exited 1 and advised `bridge init --skip-doctor`. Where
		// this user may write, they vouch for a directory the bridge may
		// not. Both are facts about the run, which config-file reports
		// once, at the severity #985 chose, so the verdict must not depend
		// on either probe. That is why this comes before the create as
		// well, which fails on its own when the config's parent cannot be
		// traversed. ok, not checked, and why, as the port checks answer
		// (ungradedConfigPortCheck).
		//
		// Only this error. A config that does not load was read by this
		// user, who can be the bridge's, and it names its directory as
		// well as one that loads: the directory comes from the path. And
		// not on the launcher's row, whose user is the one about to run
		// `bridge init` here whatever the row found: Setup's preflight
		// probes this directory as that user, so the row does too.
		return ok(checkNameConfigDir, "not checked: the config in it is not readable by this user")
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

// certPaths resolves the cert pair the running bridge would load,
// applying the same `cfg.TLSCertPath`-or-defaults fallback as
// `bridge serve` and `bridge cert`. Returns ("", "") when there is
// nothing to resolve from, which both cert checks report as a warn
// rather than answering about a path they invented.
func certPaths(d Deps) (certPath, keyPath string) {
	if d.TLSCertPath != "" && d.TLSKeyPath != "" {
		return d.TLSCertPath, d.TLSKeyPath
	}
	if d.DataDir == "" {
		return "", ""
	}
	return servertls.DefaultPaths(d.DataDir)
}

// checkTLSCert reports the cert pair's presence AND its remaining
// validity.
//
// Expiry matters here and not only in the startup log because Apple
// ATS rejects an expired cert at the handshake layer, before
// `URLSessionDelegate` is consulted — so pinning cannot save it and
// every paired device stops working at once, with the only signal
// being a log line on a host the operator may not be watching. The
// threshold is servertls.ExpiryWarningWindow, the same one
// LoadOrGenerate warns on, so doctor and the next `bridge serve`
// cannot disagree.
//
// An expired cert is a WARN, not a fail: `bridge init` bails on a
// fail, and refusing to initialise a bridge because its old cert
// lapsed would block the very run that mints a new one. The summary
// says "EXPIRED" in as many words instead.
func checkTLSCert(_ context.Context, d Deps) Check {
	certPath, keyPath := certPaths(d)
	if certPath == "" {
		return warn(checkNameTLSCert, "no data dir set",
			"pass Deps.DataDir so doctor can inspect cert state")
	}
	certExists := fileExists(certPath)
	keyExists := fileExists(keyPath)
	switch {
	case certExists && keyExists:
		return tlsCertPairCheck(certPath, keyPath)
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

// tlsCertPairCheck grades a present cert pair: does it LOAD, and how
// long is it good for.
//
// The split between fail and warn here is "can `bridge serve` start":
// a pair it cannot load is a fail, like the partial-state branch above
// and for the same reason; an expired or expiring certificate loads
// fine and serve starts, so it is a warn about the clients.
func tlsCertPairCheck(certPath, keyPath string) Check {
	info, err := servertls.Inspect(certPath)
	if err != nil {
		// The pair is there and the cert half will not parse. "present"
		// was the old answer and it is a confident wrong one.
		return fail(checkNameTLSCert, "present but unreadable",
			fmt.Sprintf("%s did not parse as a certificate (%v) — `bridge serve` will fail to load it. "+
				"Remove the cert and key and re-run `bridge init`, or restore them from a backup.", certPath, err))
	}
	// A certificate that parses says nothing about the key beside it,
	// and a mismatched pair is reachable: GenerateWithOptions commits
	// the two files in two renames, and a crash between them leaves a
	// new cert with the old key — a residual its own docblock records.
	// Measured on that state, Inspect returns a clean 396-day verdict
	// while `bridge serve` exits on "private key does not match public
	// key".
	if err := servertls.VerifyKeyPair(certPath, keyPath); err != nil && !errors.Is(err, fs.ErrPermission) {
		return fail(checkNameTLSCert, "present, but the cert and key are not a pair",
			fmt.Sprintf("`bridge serve` loads both files together and will not start: %v. "+
				"This is what an interrupted `bridge cert rotate` leaves behind. Re-run `bridge cert rotate` "+
				"— it re-mints BOTH files — then re-pair every paired device.", err))
	}
	// A permission failure on the key is deliberately NOT a finding.
	// The key is 0600 and owned by the service user; on the public-mode
	// layout the operator running `bridge doctor` is somebody else, and
	// the bridge reads it perfectly well. That is a fact about this
	// doctor run, not about the bridge — the same reason config
	// `Validate()` does not stat the library roots. Expiry still grades,
	// because it was read from the cert, which is 0644.
	//
	// ONE `now` for both ends of the validity window, so the two
	// comparisons below cannot straddle a tick.
	now := time.Now()
	// The window has a FAR end too, and a cert that has not started is
	// rejected by clients exactly like an expired one. `LoadX509KeyPair`
	// does not look at dates, so the pair check above passes and this
	// would otherwise read `present, expires in 396 days` about a cert
	// nothing will accept. Reachable on this product's hardware: the
	// mint allows one hour of clock skew (`NotBefore: now-1h`), so a
	// host whose clock was further ahead than that when the cert was
	// minted — a NUC or Pi with no RTC, before NTP lands — leaves a
	// NotBefore in the future once the clock is corrected. Moving the
	// data directory off such a host is this check's own subject.
	if info.NotBefore.After(now) {
		return warn(checkNameTLSCert,
			fmt.Sprintf("present, NOT YET VALID (starts %s)", info.NotBefore.UTC().Format(time.RFC3339)),
			servertls.NotYetValidRemediation)
	}
	// Remaining validity comes from NotAfter directly, NOT from
	// DaysUntilExpiry: that count truncates toward zero, so a cert with
	// 30 days 23 hours left reads as 30 and would trip a
	// `days*24h <= ExpiryWarningWindow` test while `logIfExpiringSoon`,
	// which compares `time.Until(NotAfter)`, stays quiet — a 23-hour
	// window in which doctor and the next `bridge serve` disagree, which
	// is the one thing this grading exists not to do. The day count is
	// for the sentence only.
	remaining := info.NotAfter.Sub(now)
	switch {
	case remaining <= 0:
		return warn(checkNameTLSCert, fmt.Sprintf("present, EXPIRED %s", expiryPhrase(info.DaysUntilExpiry)),
			"an expired certificate is rejected at the TLS handshake layer before pinning is consulted, so every "+
				"paired device fails to connect. "+servertls.RotationRemediation)
	case remaining <= servertls.ExpiryWarningWindow:
		return warn(checkNameTLSCert, fmt.Sprintf("present, expires %s", expiryPhrase(info.DaysUntilExpiry)),
			"renew before it lapses — an expired certificate is rejected at the TLS handshake layer, so every "+
				"paired device fails to connect. "+servertls.RotationRemediation)
	default:
		return ok(checkNameTLSCert, fmt.Sprintf("present, expires %s", expiryPhrase(info.DaysUntilExpiry)))
	}
}

// expiryPhrase renders CertInfo.DaysUntilExpiry as the tail of a
// sentence. DISPLAY ONLY — the day count truncates toward zero, so no
// branch is taken on it; its caller decides from the exact remaining
// duration. Inspect's -1 sentinel for "already past NotAfter" is why
// this reads the sign rather than the magnitude on the expired side.
func expiryPhrase(days int) string {
	switch {
	case days < 0:
		return "(past its NotAfter)"
	case days == 0:
		return "in under a day"
	case days == 1:
		return "in 1 day"
	default:
		return fmt.Sprintf("in %d days", days)
	}
}

// RunPortChecks runs the two listen-port checks and nothing else.
//
// `bridge init` needs them TWICE against different inputs. The
// preflight grades the install that is already at the target path (see
// withExistingInstallDeps) — right for the certificate, whose SAN set
// and validity window are facts about a file init does not rewrite.
// The ports are not like that: init may be about to SAVE different
// ones, and a config saved with a port something else holds produces a
// `bridge serve` that cannot bind, having just been told the host was
// fine.
//
// Narrow rather than a second full Run: everything else in the report
// is unchanged by the config init is about to write, and re-running the
// toolchain probes would double the preflight's wall clock for a second
// copy of the same answers.
//
// `api` and `admin` select which ports to grade, because the caller has
// a reason to ask about one and not the other and no honest way to say
// so otherwise: port 0 is a legal value with its own verdict, so it
// cannot double as "skip this one".
//
// A caller grading a port it is about to CHOOSE should also clear
// Deps.OwnPIDFile. The "is it us?" ladder below checkPort's conflict
// branch answers ok or warn — never fail — whenever our own recorded
// pid is alive and the owner probe could not rule it out, which is right
// for a port the running bridge is supposed to hold and wrong for one it
// is not: a live bridge binds what ITS config says, so it cannot
// legitimately own a port that is not in it, and the excuse then hides a
// conflict that will stop the next serve from binding (CodeRabbit on
// #970). The exception is Deps.OwnPIDPortsUnknown, where no config says
// which ports the bridge binds and the pid file is already confined to
// the one arm that does not need one: the recorded bridge seen listening
// on the port.
func RunPortChecks(ctx context.Context, d Deps, api, admin bool) Report {
	var checks []Check
	if api {
		checks = append(checks, checkAPIPort(ctx, d))
	}
	if admin {
		checks = append(checks, checkAdminPort(ctx, d))
	}
	return Report{Checks: checks}
}

func checkAPIPort(ctx context.Context, d Deps) Check {
	return checkListenPort(ctx, d, checkNamePortAPI, d.APIPort)
}

func checkAdminPort(ctx context.Context, d Deps) Check {
	return checkListenPort(ctx, d, checkNamePortAdmin, d.AdminPort)
}

// checkListenPort is the ladder both port checks climb, for the port it is
// handed with the name it is handed: a config that did not load declines
// (ungradedConfigPortCheck), then a port the caller bound answers
// (ownedPortCheck), then the bind probe (checkPort, or checkChosenPort when
// the recorded bridge's ports are unknown). The port is passed, never
// derived from the name, for the reason ownedPortCheck gives.
func checkListenPort(ctx context.Context, d Deps, name string, port int) Check {
	if c := ungradedConfigPortCheck(name, d.ConfigFile); c != nil {
		return *c
	}
	if owned := ownedPortCheck(name, port, d.OwnedPorts); owned != nil {
		return *owned
	}
	if d.OwnPIDPortsUnknown {
		return checkChosenPort(ctx, name, port, d.OwnPIDFile)
	}
	return checkPort(ctx, name, port, d.OwnPIDFile)
}

// ungradedConfigPortCheck answers a port check whose port no config set:
// one was named or found and did not load (config-file says why). It
// returns nil when the config loaded, when none was named or found, and
// when there was no lookup.
//
// The caller seeds Deps with the default ports and replaces them only from
// a config that loads, and the pid file's path comes from the same config.
// So what reaches this check is a guess at the port, with no pid file to
// recognise the bridge by. Graded, it answered "another process owns this
// port" about the bridge's own listeners wherever those are the defaults:
// `bridge doctor` run by a user who cannot read a service-owned config
// FAILed both ports on every host (#1021's table). Where they are not, it
// answered "free" about ports nothing binds, a check passing because the
// thing it guards is absent.
//
// So the line says it was not checked, and why, and leaves the verdict to
// config-file, which gives it once at the severity #985 chose: a warn for
// a config this user cannot read (a fact about the run), a fail for one
// that is not there or does not load. It is ok, like config-dir's "not
// checked" for a named config that is not there, and it names no port,
// since the guessed one would read as the install's.
//
// It comes before OwnedPorts and the bind probe because the port is the
// guess: the answer must not depend on whether a port the install may not
// use is bound, or by whom.
//
// A lookup that found nothing where nothing was named is not this case.
// That is doctor run before `bridge init`, and the defaults are then the
// ports init will write, so they are graded. Nor is a nil lookup: `bridge
// init`'s preflight and its second port pass grade the ports they were
// handed. Nor is the launcher's row (a PreSetup lookup), which previews
// that preflight for the user about to run it: the defaults are the ports
// Setup will write, and its preflight grades them whatever it finds at the
// target, a config this user cannot read included (ConfigFile.ungraded).
func ungradedConfigPortCheck(name string, c *ConfigFile) *Check {
	var why string
	switch c.ungraded() {
	case configUnreadable:
		why = "the config that sets this port is not readable by this user"
	case configNotThere:
		why = "the named config does not exist"
	case configDoesNotLoad:
		why = "the config that sets this port does not load"
	default:
		return nil
	}
	r := ok(name, "not checked: "+why)
	return &r
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
// pidAliveFunc and portOwnerFunc. Production code MUST NOT mutate it.
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
// idempotent while the server is running. Any other binder is a fail,
// except behind a recorded bridge that is alive and that the owner probe
// could neither name nor rule out: that one may be ours unseen, and gets
// ok or warn (the liveness arm below). A bind failure that ISN'T
// EADDRINUSE (e.g. EACCES on a privileged port without elevation) is a
// Warn, not a Fail — it's a privilege/environment issue, not a port
// conflict.
//
// Limitation: this probes loopback ONLY, so a conflict bound to a
// specific non-loopback interface (e.g. 192.168.1.5:port) isn't detected
// here and would surface later as EADDRINUSE from `bridge serve`'s
// wildcard bind. The loopback-only probe is deliberate: the admin port
// binds loopback, so a wildcard probe here would false-fail it whenever
// any unrelated service holds the same port on another interface.
func checkPort(ctx context.Context, name string, port int, ownPIDFile string) Check {
	if c, inUse := bindVerdict(name, port); !inUse {
		return c
	}
	// Port is in use. Is it us?
	if ownPIDFile != "" {
		if ownPID, readErr := readPID(ownPIDFile); readErr == nil && ownPID > 0 {
			found, seen, probeErr := ownerProbeFunc(ctx, port, ownPID)
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
				// PID we recorded at startup is still running. That is the
				// EXPECTED result, not a conflict, on a bridge that binds a
				// privileged port through a file capability (`setcap
				// cap_net_bind_service=+ep`, which the deployment runbook
				// prescribes so a non-root service can bind :443): that
				// binary runs with dumpable=0, so no unprivileged observer
				// can attribute the port to a pid — lsof, `ss -p` and a
				// direct readlink of /proc/<pid>/fd all fail identically.
				//
				// It is not the only way here, and this arm used to explain
				// every arrival as that one. A bridge running as another
				// user is as hidden, on every unix. A host with no lsof
				// off Linux asks nothing. lsof may name the process that
				// holds the port. And a probe that saw everything there
				// was to see (Windows' listener table, /proc reading all
				// of our descriptors) rules our pid out. So the text is
				// the probe's account of what it saw (ownerSighting).
				//
				// A pid the account rules out holds nothing on this port,
				// running or not: its descriptors were all read and none
				// is a listener on the port, or the table names every
				// listener and ours is not among them. The port is another
				// process's, and FAILs as it does with no live pid behind
				// it. The uid arm below used to answer ok for
				// it on Linux whenever that other process ran as this
				// user: a bridge still running on the ports of the config
				// it started with, whose config was then edited to a port
				// something else holds, read ok, `bridge doctor --config`
				// (the runbook's check before a restart) exited 0, and the
				// restarted bridge could not bind: #970's defect, in this
				// ladder (#1028's row L4).
				if seen.ruledOut {
					return fail(name, fmt.Sprintf(":%d in use", port), liveUnseenHint(ownPID, seen))
				}
				// Last resort before giving up: ask whether the listener is
				// at least owned by OUR USER. On Linux that survives
				// dumpable=0 (see portowner_linux.go); everywhere else it
				// answers "don't know" and we fall through to the Warn.
				// Only a probe that could not rule our pid out gets here,
				// so a listener of this uid may be our bridge, unseen.
				if owned, ownErr := portOwnerFunc(port); ownErr == nil && owned {
					return ok(name, fmt.Sprintf("in use by a process running as this user (uid %d; %s)",
						os.Getuid(), seen.account()))
				}
				// "Our recorded pid is alive and something holds the port"
				// is materially different from "we have no idea who owns
				// this", and only the second deserves a Fail.
				return warn(name, fmt.Sprintf(":%d in use", port), liveUnseenHint(ownPID, seen))
			}
		}
	}
	// No live pid of ours to attribute the port to: none was given (the
	// caller saying no bridge of ours can hold it, which init's second
	// port pass and a first install both say), none could be read, or the
	// one recorded is not running. That is a conflict on every host.
	//
	// This used to end in `if !portProbeAvailable() { return warn(…) }`,
	// goreview F9's answer to a LIVE bridge that a host without lsof could
	// not attribute. The liveness arm above has answered that case since
	// #640, so all the fallback still saw was this one, where lsof cannot
	// change the answer: with no pid there is nothing to ask it, and a pid
	// that is not running holds nothing for it to find. Its absence alone
	// turned the Fail into a warn, and `bridge init` on such a host saved a
	// port another process held.
	return fail(name, fmt.Sprintf(":%d in use", port), anotherProcessOwnsPort)
}

// anotherProcessOwnsPort is the hint on a held port with no live bridge of
// ours behind it.
const anotherProcessOwnsPort = "another process owns this port; stop it or pick a different address in bridge.yaml"

// liveUnseenHint is checkPort's hint when the recorded bridge is alive and
// the owner probe did not see it on the port: the probe's account (s), then
// advice that fits it. Where what the probe saw rules the bridge out
// (ownerSighting.ruledOut), the check FAILs and the hint says to stop the
// holder. Where it could have missed the bridge, the check warns, and the
// hint says why and keeps the hedge.
func liveUnseenHint(ownPID int, s ownerSighting) string {
	lead := fmt.Sprintf("our bridge (pid %d) is still running, but %s", ownPID, s.account())
	if s.ruledOut {
		return lead + ": stop the process that holds the port, or change the address in bridge.yaml"
	}
	return lead + s.because() + ". If our bridge is what holds the port, this is expected; " +
		"otherwise stop the other process or change the address in bridge.yaml"
}

// chosenUnseenHint is checkChosenPort's refusal of a port the recorded bridge
// was not seen holding while it is alive, built like liveUnseenHint. Only a
// probe that could have missed the bridge leaves "stop that bridge and
// re-run" as a way through; one whose account rules the bridge out does
// not.
func chosenUnseenHint(ownPIDFile string, ownPID int, s ownerSighting) string {
	lead := fmt.Sprintf("the bridge recorded in %s (pid %d) is running, but %s", ownPIDFile, ownPID, s.account())
	if s.ruledOut {
		return lead + ": stop the process that holds the port and re-run"
	}
	return lead + s.because() + ", and with no config that loads nothing says it binds this port. " +
		"If it does, stop that bridge and re-run; otherwise stop the process that holds the port"
}

// checkChosenPort grades a port the caller is choosing for a bridge whose
// ports it could not read (Deps.OwnPIDPortsUnknown): `bridge init` over an
// install whose config is there and does not load. A held port is excused
// only when the probe sees the bridge recorded in ownPIDFile listening on
// it, which is the one answer that says a restart of that bridge frees it.
//
// checkPort's other arms read the recorded bridge's LIVENESS as evidence
// that a held port is its own: a probe that failed warns, and a live pid
// the probe did not name, and could not rule out, warns, or is ok on Linux
// when the listener runs as this user. That is sound for a port the
// bridge's config names, and here no config names one. init writes its
// defaults, and an install that had moved off them (because something else
// holds 7788, say) has a live bridge on its own ports while another process
// holds the one init writes.
// Excused, that port is saved, and the restarted bridge cannot bind it:
// #970's defect, which the second port pass avoids by clearing the pid file
// for a port the run is choosing. Cleared here, the bridge's own listeners
// read as another process's, and the re-init that would replace a broken
// config refuses: the defect this exists for.
//
// So the verdict turns on attribution alone. A probe that failed is a FAIL
// too, since nothing else says the recorded bridge binds this port, and
// liveness only picks the hint.
func checkChosenPort(ctx context.Context, name string, port int, ownPIDFile string) Check {
	c, inUse := bindVerdict(name, port)
	if !inUse {
		return c
	}
	conflict := fmt.Sprintf(":%d in use", port)
	ownPID, err := readPID(ownPIDFile)
	if err != nil || ownPID <= 0 {
		return fail(name, conflict, anotherProcessOwnsPort)
	}
	found, seen, probeErr := ownerProbeFunc(ctx, port, ownPID)
	switch {
	case probeErr == nil && found:
		return ok(name, fmt.Sprintf("bound by our own bridge (pid %d)", ownPID))
	case !pidAliveFunc(ownPID):
		return fail(name, conflict, anotherProcessOwnsPort)
	case probeErr != nil:
		return fail(name, conflict, fmt.Sprintf(
			"the owner probe failed (%s), and with no config that loads nothing says the bridge recorded in %s "+
				"(pid %d) binds this port. If it does, stop that bridge and re-run; otherwise stop the process "+
				"that holds the port",
			oneLine(probeErr.Error()), ownPIDFile, ownPID))
	default:
		return fail(name, conflict, chosenUnseenHint(ownPIDFile, ownPID, seen))
	}
}

// bindVerdict is the bind probe both port ladders start from (checkPort
// and checkChosenPort). It returns inUse when something holds the port,
// and otherwise the check to report: free, not bindable, or no port set.
func bindVerdict(name string, port int) (Check, bool) {
	if port == 0 {
		return warn(name, "no port set", "pass Deps."+name+"Port"), false
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
		return ok(name, fmt.Sprintf("free (:%d)", port)), false
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
	// owner attribution unreachable there, since both ladders ask it only
	// about a port this reports held.
	if !inUse {
		return warn(name, fmt.Sprintf(":%d not bindable", port),
			"couldn't bind to probe this port ("+err.Error()+"); "+
				"ports below 1024 need elevation, or the configured address may be invalid — check bridge.yaml"), false
	}
	return Check{}, true
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
	if d.Managed {
		return ok(checkNameServiceManager, "lifecycle managed by the host — check skipped")
	}
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
	if d.Managed {
		return ok(checkNameBrowserOpener, "console is reached over the network — check skipped")
	}
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
			return ok(checkNameBrowserOpener, c)
		}
	}
	return warn(checkNameBrowserOpener, "no opener found",
		"install missing; bridge will still print the admin URL for you to paste manually")
}

// probeSox / ffmpegAvailable are the test seams for checkAudioToolchain.
// Without them this check is untestable anywhere the host toolchain differs
// from the case under test — and CI runners have neither binary, so the sox
// probe would fail first and the branches below would never be reached.
// Production MUST NOT mutate them (the soxLookPath / renameFunc convention).
var (
	probeSox      = transcode.ProbeSox
	missingFFmpeg = transcode.MissingFFmpegBinaries
)

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
	info, err := probeSox(ctx)
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
	// ALAC is the one LOSSLESS format no stock sox build can read, and it
	// clears every upstream eligibility gate — so an operator with ALAC in
	// the library and no ffmpeg gets a pipeline that refuses those tracks
	// with no indication that installing one binary would fix it. Only warn
	// when there is something to fix: the library actually holds ALAC, and
	// upscaling (not merely analysis, which never touches ALAC) is on.
	if missing := missingFFmpeg(); d.UpscaleEnabled && len(missing) > 0 {
		if has, err := libraryHasALAC(ctx, d); err == nil && has {
			// Name the binary that is actually absent. BOTH are required —
			// ffprobe supplies the geometry the headerless pipe is described
			// with and the duration the completeness guard compares against —
			// and some distros package them separately, so a host with ffmpeg
			// and no ffprobe is a real state. Telling that operator to
			// "install ffmpeg" sends them to look at a binary they have.
			return warn(checkNameAudioToolchain,
				"sox present; ALAC in library but "+strings.Join(missing, " + ")+" missing",
				"the library contains ALAC (.m4a) tracks, which no stock sox build can decode; "+
					"the bridge decodes them with ffmpeg, which needs BOTH `ffmpeg` and `ffprobe` "+
					"(missing here: "+strings.Join(missing, ", ")+"). Install the ffmpeg package "+
					"(macOS: `brew install ffmpeg`; Debian/Ubuntu: `sudo apt install ffmpeg`; "+
					"Windows: `choco install ffmpeg`) — it ships both. Everything else keeps "+
					"working without it.")
		}
	}
	if info.Version != "" {
		return ok(checkNameAudioToolchain, fmt.Sprintf("sox %s, FLAC supported", info.Version))
	}
	return ok(checkNameAudioToolchain, "sox present, FLAC supported")
}

// libraryHasALAC asks the manifest whether any indexed track is ALAC.
//
// Returns (false, error) when the caller supplied no probe, so the ALAC
// warning is SKIPPED rather than fired on a guess — a fresh install with no
// scan yet must not be told to install ffmpeg for a library it hasn't read.
func libraryHasALAC(ctx context.Context, d Deps) (bool, error) {
	if d.LibraryHasCodec == nil {
		return false, errUnknownLibraryCodecs
	}
	return d.LibraryHasCodec(ctx, "ALAC")
}

// errUnknownLibraryCodecs marks "no probe was supplied", which every caller
// treats as "don't know" — never as "no".
var errUnknownLibraryCodecs = errors.New("library codec probe not available")

// probeFFmpeg is checkDSDRenderToolchain's test seam — the same
// convention as probeSox / missingFFmpeg: CI runners have no ffmpeg, so
// without it none of the branches below are reachable. Production MUST
// NOT mutate it.
var probeFFmpeg = transcode.ProbeFFmpeg

// checkDSDRenderToolchain verifies the ffmpeg decoders the DSD → PCM
// renditions need: ALL FOUR dsd_* decoders (a build with only some of
// them refuses every source, so it fails rather than half-works) and the
// dst decoder for DST-compressed DSDIFF (absence is a warning — the
// uncompressed majority still renders; DST rows stay ineligible until it
// is present). Mirrors checkAudioToolchain's shape, including the no-op
// when the feature is off — with one courtesy: a library that already
// holds DSF / DFF gets a tip, since the feature exists for that library
// and nothing else will ever mention it.
func checkDSDRenderToolchain(ctx context.Context, d Deps) Check {
	if !d.DSDRenderEnabled {
		if has, err := libraryHasDSD(ctx, d); err == nil && has {
			return warn(checkNameDSDRenderToolchain, "not enabled; the library holds DSD (DSF / DFF)",
				"DSD plays only on a DoP-capable wired DAC. `upscale.dsdRender.enabled: true` renders each "+
					"DSD track once to PCM on the bridge (needs ffmpeg with the dsd_* decoders — every stock "+
					"ffmpeg package ships them) so it also plays on CarPlay, wireless outputs, the speaker "+
					"and DACs that cannot take the file's DSD rate. Opt-in: the sweep reads the whole DSD "+
					"library once.")
		}
		return ok(checkNameDSDRenderToolchain, "not enabled (ffmpeg DSD decoders not required)")
	}
	info, err := probeFFmpeg(ctx)
	if err != nil {
		if errors.Is(err, transcode.ErrFFmpegMissing) {
			return fail(checkNameDSDRenderToolchain, "ffmpeg not found",
				"upscale.dsdRender.enabled is on but "+strings.Join(info.MissingBinaries, " + ")+" isn't on PATH; "+
					"install the ffmpeg package (macOS: `brew install ffmpeg`; Debian/Ubuntu: `sudo apt install ffmpeg`; "+
					"Windows: `choco install ffmpeg`) — it ships both binaries and the dsd_* / dst decoders — "+
					"or disable the feature in bridge.yaml")
		}
		return fail(checkNameDSDRenderToolchain, "ffmpeg not runnable",
			"ffmpeg is on PATH but its decoder listing could not be read: "+err.Error())
	}
	if !info.HasDSD {
		return fail(checkNameDSDRenderToolchain, "ffmpeg lacks the DSD decoders",
			"this ffmpeg build does not carry all four of dsd_lsbf, dsd_lsbf_planar, dsd_msbf, dsd_msbf_planar "+
				"(`ffmpeg -hide_banner -decoders | grep dsd_`); no DSD track can be rendered with it. Reinstall a "+
				"stock ffmpeg package (brew / apt / choco all ship them) or disable upscale.dsdRender in bridge.yaml")
	}
	if !info.HasDST {
		return warn(checkNameDSDRenderToolchain, "ffmpeg decodes DSD but not DST",
			"DST-compressed DSDIFF (.dff files carrying a DST chunk) stays unrendered until the `dst` decoder "+
				"is present (`ffmpeg -hide_banner -decoders | grep ' dst '`); plain DSF / DFF renders fine. "+
				"Stock ffmpeg ≥ 3.0 ships it.")
	}
	return ok(checkNameDSDRenderToolchain, "ffmpeg decodes DSD and DST")
}

// libraryHasDSD asks the manifest whether any indexed track is DSF or
// DFF, with libraryHasALAC's "no probe → don't know" rule.
func libraryHasDSD(ctx context.Context, d Deps) (bool, error) {
	if d.LibraryHasCodec == nil {
		return false, errUnknownLibraryCodecs
	}
	for _, codec := range []string{"DSF", "DFF"} {
		has, err := d.LibraryHasCodec(ctx, codec)
		if err != nil {
			return false, err
		}
		if has {
			return true, nil
		}
	}
	return false, nil
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

// isPIDListeningOnPort is platform-provided — the lsof-backed unix
// implementation lives in doctor_notwindows.go and the native iphlpapi.dll
// implementation in doctor_windows.go. The "is it us?" branch of checkPort
// calls it, through ownerProbeFunc; see the per-platform docs for the
// (found, sighting, error) contract, and ownerSighting for what a miss says
// about itself.
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
// Same seam convention as listenFunc above. Production code MUST NOT
// mutate them.
var (
	pidAliveFunc  = pidAlive
	portOwnerFunc = portOwnedByThisUser
	// ownerProbeFunc is the owner probe itself (isPIDListeningOnPort),
	// indirected so a test can hand both port ladders every kind of
	// account, a ruled-out miss included, on every platform. Only
	// Windows' table and Linux's /proc produce one for real, so without
	// it nothing on a Mac could show that no verdict turns on the account.
	ownerProbeFunc = isPIDListeningOnPort
)

// ErrHasFail is returned by Run when the caller passes StopOnFail.
var ErrHasFail = errors.New("doctor reports one or more failing checks")
