package updater

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

// configCheckTimeout bounds one settings check, and versionProbeTimeout one
// `version` of a backup: a candidate that never answers must not park an
// install, nor a backup the early rollback. Each is generous next to the
// load it bounds (milliseconds), for a cold start of a 30 MiB binary from an
// SD card. Vars, not consts, solely so a test can shorten them; production
// code MUST NOT mutate them.
var (
	configCheckTimeout  = 30 * time.Second
	versionProbeTimeout = 10 * time.Second
)

// The settings check a candidate answers before it is installed (backlog
// B613): `<candidate> serve --validate-config --config <path>` loads the
// settings exactly as that version's serve would at its start, and writes
// nothing, opens no store and binds nothing (cmd/bridge, checkServeConfig).
//
// The flag, the exit codes and the refusal lines are a contract between this
// updater and every later release, each of which it runs as a candidate, so
// none of them may change.
const (
	// ConfigCheckFlag is serve's flag for the check.
	ConfigCheckFlag = "validate-config"
	// ConfigCheckValidExit says the candidate loads the settings.
	ConfigCheckValidExit = 0
	// ConfigCheckRefusedExit says it refuses them: sysexits.h's EX_CONFIG.
	ConfigCheckRefusedExit = 78
	// ConfigCheckFailedExit says it could not check them, a settings file
	// it could not read. Any exit but the two above reads the same way.
	ConfigCheckFailedExit = 1
	// ConfigRefusedPrefix begins each line of a refusal on the check's
	// standard error: one reason a line, naming no path and no value.
	ConfigRefusedPrefix = "config refused: "
)

// ConfigVerdict is what a candidate's settings check answered.
type ConfigVerdict int

const (
	// ConfigValid: the candidate loads the settings.
	ConfigValid ConfigVerdict = iota
	// ConfigRefused: the candidate refuses them, for the reasons it gave.
	ConfigRefused
	// ConfigCheckUnsupported: the candidate predates the check (its serve
	// does not define the flag), so it cannot say.
	ConfigCheckUnsupported
	// ConfigCheckFailed: the check did not answer: the candidate could not
	// be run, was stopped, ran past its time, or exited another way.
	ConfigCheckFailed
)

// ConfigCheck is a candidate's answer to the settings check.
type ConfigCheck struct {
	Verdict ConfigVerdict
	// Reasons are a refusal's lines, without their prefix, bounded and
	// made printable: what the console and `bridge update` show.
	Reasons []string
	// Detail says what failed, for a failed check, in words that name no
	// path: what Install's error carries.
	Detail string
	// Output is the start of what the candidate wrote to its standard
	// error, for the bridge log alone.
	Output string
	// Err is the context's error when the check was stopped by it.
	Err error
}

// CheckConfig runs candidate's settings check against configPath and says
// what it answered. The candidate's exit code decides, and nothing else: 0
// valid, 78 refused (its "config refused: " lines the reasons), and 2 with
// the flag package's own refusal of the flag a candidate that predates the
// check. Every other answer, a timeout, a cancellation and a candidate that
// would not start included, is a failed check, which a later attempt asks
// again: it is never taken for a refusal.
func CheckConfig(ctx context.Context, candidate, configPath string) ConfigCheck {
	run := runCandidate(ctx, configCheckTimeout, candidate, "serve", "--"+ConfigCheckFlag, "--config", configPath)
	output := printableExcerpt(run.stderr)
	switch {
	case run.cancelled != nil:
		return ConfigCheck{Verdict: ConfigCheckFailed, Detail: "the check was stopped", Output: output, Err: run.cancelled}
	case run.timedOut:
		return ConfigCheck{Verdict: ConfigCheckFailed, Detail: fmt.Sprintf("it did not answer within %s", configCheckTimeout), Output: output}
	case run.startErr != nil:
		return ConfigCheck{Verdict: ConfigCheckFailed, Detail: "it could not be run: " + startErrorWithoutPath(run.startErr), Output: output}
	case run.exit == ConfigCheckValidExit:
		return ConfigCheck{Verdict: ConfigValid, Output: output}
	case run.exit == ConfigCheckRefusedExit:
		return ConfigCheck{Verdict: ConfigRefused, Reasons: refusalLines(run.stderr), Output: output}
	case run.exit == 2 && bytes.Contains(run.stderr, []byte("flag provided but not defined: -"+ConfigCheckFlag)):
		return ConfigCheck{Verdict: ConfigCheckUnsupported, Output: output}
	case run.exit < 0:
		return ConfigCheck{Verdict: ConfigCheckFailed, Detail: "it did not exit on its own", Output: output}
	}
	return ConfigCheck{Verdict: ConfigCheckFailed, Detail: fmt.Sprintf("it exited with status %d", run.exit), Output: output}
}

// versionLine is the line `bridge version` prints: the version, then the
// wire protocol's.
var versionLine = regexp.MustCompile(`^1-bit-bridge (\S+) \(protocol v[0-9]+\)$`)

// ProbeVersion runs `binary version`, bounded, its standard input empty, and
// returns the version it names. The early rollback asks it of a backup, to
// verify the backup is the version the install marker says it replaced: a
// version check of the file, not a proof of its bytes.
func ProbeVersion(ctx context.Context, binary string) (string, error) {
	run := runCandidate(ctx, versionProbeTimeout, binary, "version")
	switch {
	case run.cancelled != nil:
		return "", run.cancelled
	case run.timedOut:
		return "", fmt.Errorf("it did not answer within %s", versionProbeTimeout)
	case run.startErr != nil:
		return "", fmt.Errorf("it could not be run: %s", startErrorWithoutPath(run.startErr))
	case run.exit != 0:
		return "", fmt.Errorf("it exited with status %d", run.exit)
	}
	first, _, _ := strings.Cut(string(run.stdout), "\n")
	m := versionLine.FindStringSubmatch(strings.TrimSpace(first))
	if m == nil {
		return "", errors.New("it did not name its version")
	}
	return m[1], nil
}

// outputBound is how much of each of a candidate's outputs is kept; the rest
// is read and dropped, so the candidate never blocks on a full pipe.
const outputBound = 64 << 10

// waitDelay is how long a run waits, once its candidate is killed, for the
// pipes to close: a child the candidate left holding them is abandoned
// there. Unlike the Tailscale CLI's calls, whose join must outlast a writer
// (under The CLI and the serve wiring), a candidate's check and its version
// write nothing, so there is nothing to outlast, and the bound is what keeps
// a broken candidate from parking an install.
const waitDelay = 2 * time.Second

// textBusyRetries is how many times a run is started again when the kernel
// answers ETXTBSY: a candidate written moments ago can still be open for
// writing in a child another goroutine forked before its descriptor closed
// (golang/go#22315), for the instant until that child execs.
const textBusyRetries = 5

// candidateRun is what one run of a candidate came to.
type candidateRun struct {
	stdout, stderr []byte
	// exit is the process's exit code, -1 when a signal ended it.
	exit int
	// cancelled is the caller's context's error when it stopped the run;
	// timedOut says the run's own bound did.
	cancelled error
	timedOut  bool
	// startErr is why the process did not start.
	startErr error
}

// runCandidate runs binary with args under its own bound, its standard
// input empty and each output kept to its first outputBound bytes.
func runCandidate(ctx context.Context, timeout time.Duration, binary string, args ...string) candidateRun {
	abs, err := filepath.Abs(binary)
	if err != nil {
		return candidateRun{startErr: err}
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for attempt := 0; ; attempt++ {
		stdout, stderr := &boundedOutput{max: outputBound}, &boundedOutput{max: outputBound}
		cmd := exec.CommandContext(runCtx, abs, args...)
		cmd.Stdout, cmd.Stderr = stdout, stderr
		cmd.WaitDelay = waitDelay
		err := cmd.Run()
		run := candidateRun{stdout: stdout.buf, stderr: stderr.buf}
		switch {
		case ctx.Err() != nil:
			run.cancelled = ctx.Err()
			return run
		case runCtx.Err() != nil:
			run.timedOut = true
			return run
		case err == nil:
			return run
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			run.exit = exitErr.ExitCode()
			return run
		}
		if errors.Is(err, syscall.ETXTBSY) && attempt < textBusyRetries {
			select {
			case <-runCtx.Done():
			case <-time.After(time.Duration(attempt+1) * 50 * time.Millisecond):
			}
			continue
		}
		run.startErr = err
		return run
	}
}

// startErrorWithoutPath is why a process did not start, without the path it
// was started from: os's *fs.PathError ("fork/exec <path>: …") and, on
// Windows, os/exec's *exec.Error for the lookup of the name, both of which
// name it.
func startErrorWithoutPath(err error) string {
	var ee *exec.Error
	if errors.As(err, &ee) && ee != nil {
		return "exec: " + fmt.Sprint(ee.Err)
	}
	return fsutil.ErrorWithoutPath(err)
}

// boundedOutput keeps the first max bytes written to it and drops the rest,
// answering every write in full so the process writing never blocks on it.
// One writer at a time: exec copies each pipe on a goroutine of its own.
type boundedOutput struct {
	max int
	buf []byte
}

func (b *boundedOutput) Write(p []byte) (int, error) {
	if room := b.max - len(b.buf); room > 0 {
		b.buf = append(b.buf, p[:min(room, len(p))]...)
	}
	return len(p), nil
}

// The bounds of a refusal's reasons: so many lines, each so long.
const (
	maxRefusalLines = 8
	maxRefusalLine  = 300
)

// refusalLines are the reasons a refusing check gave: each line of stderr
// that carries ConfigRefusedPrefix, without it, made printable and bounded.
// A line without the prefix is the candidate's log, never a reason.
func refusalLines(stderr []byte) []string {
	var lines []string
	sc := bufio.NewScanner(bytes.NewReader(stderr))
	sc.Buffer(make([]byte, 0, 4096), outputBound)
	for sc.Scan() {
		reason, ok := strings.CutPrefix(sc.Text(), ConfigRefusedPrefix)
		if !ok {
			continue
		}
		if reason = printableLine(reason, maxRefusalLine); reason == "" {
			continue
		}
		if len(lines) == maxRefusalLines {
			lines = append(lines, "and more")
			break
		}
		lines = append(lines, reason)
	}
	return lines
}

// printableLine is s as one line of valid UTF-8 with no control character,
// cut to at most max bytes on a rune boundary and trimmed.
func printableLine(s string, max int) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == utf8.RuneError {
			return ' '
		}
		return r
	}, strings.ToValidUTF8(s, "�"))
	if len(s) > max {
		s = fsutil.TrimPartialTrailingRune(s[:max])
	}
	return strings.TrimSpace(s)
}

// printableExcerpt is the start of a candidate's output for the bridge log:
// its lines joined, made printable and bounded.
func printableExcerpt(out []byte) string {
	return printableLine(strings.Join(strings.Fields(string(out)), " "), 2048)
}

// configDigestDomain begins every ConfigDigest, so a digest of another kind
// of input can never be taken for one of the settings.
const configDigestDomain = "1-bit-bridge settings digest v1\n"

// ConfigDigest is a digest of the settings the bridge at configPath loads:
// the file's bytes and every BRIDGE_ environment variable with a value,
// which config's overrides read over the file. The automatic install holds
// a refused version back only while the settings have the digest it was
// refused with (State.RejectedConfigDigest), so a change to either lets it
// try again. A variable that is not a setting only makes the digest change
// more often, which costs one more check. No value is kept: the digest is
// all that is recorded.
func ConfigDigest(configPath string) (string, error) {
	return configDigest(configPath, os.Environ(), envNamesFoldCase)
}

// envNamesFoldCase says whether this platform's environment names compare
// without regard to case, as Windows' do, so that config's overrides read a
// variable whatever the case it was set in.
const envNamesFoldCase = runtime.GOOS == "windows"

// configDigest is ConfigDigest over the environment environ, whose names
// compare without regard to case where foldCase says so.
func configDigest(configPath string, environ []string, foldCase bool) (string, error) {
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return "", fmt.Errorf("read the settings: %w", err)
	}
	env := bridgeEnvironment(environ, foldCase)
	h := sha256.New()
	h.Write([]byte(configDigestDomain))
	fmt.Fprintf(h, "%d\n", len(raw))
	h.Write(raw)
	for _, kv := range env {
		fmt.Fprintf(h, "%d\n%s", len(kv), kv)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// bridgeEnvironment is each BRIDGE_ variable of environ that has a value,
// sorted, as NAME=value.
func bridgeEnvironment(environ []string, foldCase bool) []string {
	var env []string
	for _, kv := range environ {
		if k, v, ok := strings.Cut(kv, "="); ok && strings.HasPrefix(k, "BRIDGE_") && v != "" {
			env = append(env, kv)
		}
	}
	sort.Strings(env)
	return env
}

// Hold is what holds a candidate back from the automatic install on this
// host.
type Hold struct {
	// Version is the rejected version that holds the candidate back; ""
	// when nothing does.
	Version string
	// Reason says why, in words that name no path and no value.
	Reason string
	// Recorded says the reason was recorded with the rejection (a version
	// that refused the settings, or that predates the check), not an
	// operator's rollback, which the auto-installer words itself.
	Recorded bool
}

// HeldBack reports what holds candidate back from the automatic install, read
// from the marker in dataDir: a rejected version that candidate does not
// pass (rejectionBlocks), and for a version refused for its settings, only
// while the settings at configPath still have the digest they were refused
// with. Settings it cannot read now hold the version back, as an automatic
// install must not guess. A marker it cannot read holds nothing back, as
// swapAwaitingRestart's does not.
func HeldBack(dataDir, configPath, candidate string) Hold {
	if dataDir == "" || candidate == "" {
		return Hold{}
	}
	st, err := LoadState(dataDir)
	if err != nil || !rejectionBlocks(candidate, st.RejectedVersion) {
		return Hold{}
	}
	if st.RejectedConfigDigest != "" && configPath != "" {
		if now, err := ConfigDigest(configPath); err == nil && now != st.RejectedConfigDigest {
			return Hold{}
		}
	}
	if st.RejectedReason == "" {
		return Hold{Version: st.RejectedVersion,
			Reason: fmt.Sprintf("%s was rolled back on this host; auto-install resumes at the next newer release", st.RejectedVersion)}
	}
	reason := st.RejectedReason
	if st.RejectedConfigDigest != "" {
		reason += "; it is tried again once the settings change"
	}
	return Hold{Version: st.RejectedVersion, Reason: reason, Recorded: true}
}

// recordRejection records version as held back for reason, against the
// settings digest (empty holds it whatever the settings are), keeping the
// rest of the marker as it is, so a refusal before an install leaves the
// last install's bookkeeping (its "installed" stamp, the cleanup of its
// backup) in place. A marker that cannot be read is replaced.
func recordRejection(dataDir, version, reason, digest string) error {
	st, err := LoadState(dataDir)
	if err != nil {
		st = State{}
	}
	st.RejectedVersion = normalizeTag(version)
	st.RejectedReason = reason
	st.RejectedConfigDigest = digest
	return SaveState(dataDir, st)
}

// futureMarkerSlack is how far ahead of this host's clock an install
// marker's time may be and still be taken for this host's: a clock stepped
// back a little after the install. A marker further ahead is not credible.
const futureMarkerSlack = time.Minute

// EarlyRollback reports whether a serve of version current, whose settings it
// refused before it touched the data dir, may put back the backup the
// install marker st describes (cmd/bridge, the early rollback; backlog
// B613), and why not when it may not. It may only when the marker is an
// install of current that reached its swap, recently, over another version
// it names, and no version but that one has opened the data dir since
// (State.DataDirOpenedBy): a backup restored over a store a newer version
// migrated would be a downgrade past its migrations. why is "" when the
// marker is no install of current, which is nothing to explain.
//
// DecideBootAction's rule is left as it is: its version match counts as the
// install's success, where this asks about an install that could not start.
func EarlyRollback(st State, current string, now time.Time) (ok bool, why string) {
	if st.Status != "installing" || normalizeTag(st.TargetVersion) != normalizeTag(current) {
		return false, ""
	}
	switch {
	case !st.SwapStarted:
		return false, "the install never reached its swap, so the backup may not be the version it replaced"
	case st.PreviousVersion == "" || normalizeTag(st.PreviousVersion) == normalizeTag(current):
		return false, "the install marker names no other version to go back to"
	case st.AttemptedAt.After(now.Add(futureMarkerSlack)):
		return false, "the install marker is dated in the future, so it cannot be told recent (check the clock)"
	case now.Sub(st.AttemptedAt) > recencyWindow:
		return false, fmt.Sprintf("the install is older than %s", recencyWindow)
	case st.DataDirOpenedBy != "":
		return false, fmt.Sprintf("%s has already opened the data dir, so its store may hold changes %s does not know",
			st.DataDirOpenedBy, st.PreviousVersion)
	}
	return true, ""
}

// SameVersion reports whether a and b name one version, a leading "v" and white
// space aside, as the marker's versions are compared everywhere.
func SameVersion(a, b string) bool { return normalizeTag(a) == normalizeTag(b) }

// ArmsDataDirRecord reports whether a serve of version current, about to write
// the data dir, must first record in the marker st that it did
// (State.DataDirOpenedBy): an install is armed, no version has recorded it
// yet, and current is not the version the backup holds, whose writes a
// restore of the backup takes back to its own.
func ArmsDataDirRecord(st State, current string) bool {
	return st.Status == "installing" && st.DataDirOpenedBy == "" &&
		normalizeTag(st.PreviousVersion) != normalizeTag(current)
}
