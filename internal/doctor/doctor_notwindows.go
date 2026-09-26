//go:build !windows

package doctor

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// lsofCommand is the test seam for the lsof spawn — production points it at
// exec.CommandContext. Tests substitute a long-running stub so the
// cancellation contract can be driven deterministically (a real lsof on a
// wedged mount is not something a test can conjure). Same convention as
// transcode.soxProbeCommand / acoustid.fpcalcCommand / tailscale.commandContext.
// Production code MUST NOT mutate it.
var lsofCommand = exec.CommandContext

// isPIDListeningOnPort reports whether targetPID is among the processes
// listening on the given local TCP port. The bool answers "is it us?" for
// checkPort's own-bridge branch; the error reports a probe-MECHANISM
// failure (lsof is resolved but the invocation couldn't even start, or was
// cut short) so checkPort can degrade to Warn rather than a hard Fail — a
// broken probe must never break a healthy install whose config names the
// port. (checkChosenPort, for a port no config names, refuses instead.)
// When it answers false with no error, the sighting says what the probe
// saw, which is how the arms that explain a miss explain it (ownerSighting):
// lsof listing nothing, lsof naming other pids, or the /proc account.
//
// Where no usable lsof resolved, Linux answers from /proc itself
// (pidListensOnPort, the same kernel tables lsof reads), so attribution
// there does not depend on lsof being installed; its error means neither
// socket table could be read. Elsewhere it returns (false, nil): the port
// cannot be attributed, and checkPort asks next whether the recorded pid
// is alive at all. Either way the sighting opens with the missing lsof.
//
// And where lsof ran cleanly and did not name the pid, /proc is asked too
// (procSecondOpinion). lsof lists only the processes this user may inspect,
// so its miss cannot rule the recorded bridge out, and /proc, reading that
// pid's own descriptors, can: without it, a verdict that turns on the
// ruling-out would depend on whether lsof is installed. Off Linux the
// /proc stub adds nothing. A failed or cut-short lsof asks nothing more,
// and stays the probe failure it always was.
//
// Implementation: `lsof -nP -iTCP:<port> -sTCP:LISTEN -t` prints one PID
// per line. We check membership across ALL of them (not just the first)
// so a dual-stack / multi-interface listener set can't mask our own PID
// behind another process's row. Exec'ing the resolved ABSOLUTE path (not
// the bare "lsof" name) defends against a PATH-injected binary.
//
// **Bounded by probeTimeout wrapped around the INCOMING ctx.** lsof stat()s
// mount points while building its device cache, so a wedged network mount —
// the rclone FUSE mount the production VPS runs its library on is exactly
// that shape — used to hang `bridge doctor` indefinitely, and hold the admin
// console's `GET /api/doctor` goroutine past client disconnect.
//
// **Deliberately NOT invoked with `-b`.** That flag is lsof's own "avoid
// kernel calls that might block" mode and looks like the targeted fix, but
// it is the narrower one: the deadline bounds EVERY blocking cause, `-b`
// only the subset it knows about. Against that it carries a real risk —
// under `-b` lsof skips what it cannot identify without blocking, and a
// skipped listener flips a correct "ok, that's our own bridge" into a Warn
// on a healthy install. Confirming it doesn't is not something this repo
// can do without a wedged mount to test against, so the deadline stands
// alone until someone can.
func isPIDListeningOnPort(ctx context.Context, port, targetPID int) (bool, ownerSighting, error) {
	if lsofPath == "" {
		found, seen, err := procOwnerFunc(port, targetPID)
		if found || err != nil {
			return found, ownerSighting{}, err
		}
		seen.saw = "this host has no lsof, and " + seen.account()
		return false, seen, nil
	}
	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	out, err := lsofCommand(probeCtx, lsofPath, "-nP", "-iTCP:"+strconv.Itoa(port), "-sTCP:LISTEN", "-t").Output()
	if err != nil {
		// Check the context FIRST. A killed process can surface as any
		// exit status, including 1, and misreading a cut-short probe as
		// lsof's "ran, matched nothing" would silently claim the port
		// isn't ours — the one answer we have no evidence for.
		//
		// The INCOMING ctx is checked before the timeout child, because
		// the child's Err() is non-nil in both cases: keying off it alone
		// reports "timed out after 2s" for a caller that cancelled at
		// 50ms, which sends whoever reads the hint hunting a wedged mount
		// that was never involved.
		if callerErr := ctx.Err(); callerErr != nil {
			return false, ownerSighting{}, fmt.Errorf("lsof port probe aborted by caller: %w", callerErr)
		}
		if ctxErr := probeCtx.Err(); ctxErr != nil {
			return false, ownerSighting{}, fmt.Errorf("lsof port probe timed out after %s: %w", probeTimeout, ctxErr)
		}
		// lsof exit code 1 means it ran but matched nothing (the common
		// case when the port's owner isn't visible to us) — that's "not
		// us", not a mechanism failure. ANY OTHER outcome (a different
		// non-zero exit, or a genuine can't-start error like the binary
		// vanishing / permission) surfaces as an error so checkPort
		// degrades to Warn rather than a false Fail.
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
			return false, ownerSighting{}, err
		}
		out = nil
	}
	for _, f := range strings.Fields(string(out)) {
		if n, convErr := strconv.Atoi(f); convErr == nil && n == targetPID {
			return true, ownerSighting{}, nil
		}
	}
	// Not found. lsof's account is read from the same output: nothing
	// listed, other pids, or something that is not `lsof -t`'s answer
	// (lsofSighting). Then /proc's (procSecondOpinion).
	procFound, procSeen, procErr := procOwnerFunc(port, targetPID)
	found, seen := procSecondOpinion(lsofSighting(out, targetPID, blindSpot()), procFound, procSeen, procErr)
	return found, seen, nil
}

// procOwnerFunc is the /proc attribution (pidListensOnPort) as
// isPIDListeningOnPort asks it: in lsof's place where no lsof resolved, and
// after a clean lsof miss for a second opinion. A package var so a test
// that records a pid of its own choosing can say what /proc answers about
// that pid rather than ask the host: on Linux, pid 4242 may be a process of
// the test's own user, which /proc can read and rule out. The same
// convention as ownerProbeFunc. Production code MUST NOT mutate it.
var procOwnerFunc = pidListensOnPort

// lsofPath is the absolute path to lsof, resolved ONCE at package init.
// "" means lsof is unavailable on this host: not installed, or — defending
// against PATH injection — the PATH lookup returned a relative path.
// isPIDListeningOnPort keys off it, so the bridge never execs an
// attacker-staged lsof off a writable CWD / PATH entry.
// Mirrors the exec.LookPath + filepath.IsAbs hardening used for the
// Tailscale CLI (PR #95). (CodeRabbit MAJOR on PR #429.)
var lsofPath = resolveLsof()

func resolveLsof() string {
	if p, err := exec.LookPath("lsof"); err == nil && filepath.IsAbs(p) {
		return p
	}
	// PATH lookup failed or returned a relative (injection-unsafe) path —
	// fall back to the canonical absolute locations before giving up.
	for _, p := range []string{"/usr/sbin/lsof", "/usr/bin/lsof", "/bin/lsof"} {
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p
		}
	}
	return ""
}

// isAddrInUse reports whether a listen error is "address already in use".
// On POSIX the stdlib errno is authoritative; the Windows twin additionally
// matches WSAEADDRINUSE, which is what the OS actually returns there. Split
// by build tag rather than checking both constants in one place so neither
// platform carries a matcher that can never fire on it.
func isAddrInUse(err error) bool {
	return errors.Is(err, syscall.EADDRINUSE)
}

// pidAlive reports whether a process with this PID currently exists.
//
// os.FindProcess NEVER returns an error on unix — it just wraps the
// integer — so liveness has to be asked with signal 0, which runs the
// kernel's existence and permission checks without delivering anything.
//
// EPERM means ALIVE: the process exists, we merely aren't allowed to
// signal it. Reading that as dead is precisely the failure this helper
// exists to avoid, because checkPort uses "our recorded PID is alive" to
// decide a bound port is probably ours rather than a conflict.
//
// The Windows twin has the opposite shape and its own trap — see
// doctor_windows.go.
func pidAlive(pid int) bool {
	// pid_t is int32, and an out-of-range value TRUNCATES into it — which
	// is not merely wrong here, it is dangerous. 4294967296 truncates to
	// 0, and kill(0, 0) does not mean "pid 0": it signals EVERY process in
	// the caller's process group, succeeds, and reports the bogus pid as
	// ALIVE. (Observed on darwin while pinning the Windows twin of this
	// bound; a review had flagged only the Windows cast.) readPID parses
	// with strconv.Atoi into an int, so a corrupt or hand-edited pidfile
	// reaches here with exactly such a value on any 64-bit host.
	if pid <= 0 || pid > math.MaxInt32 {
		return false
	}
	p, err := os.FindProcess(pid)
	// Unreachable today: every branch of os/exec_unix.go's findProcess
	// returns a nil error, including the already-reaped case (which comes
	// back as a "done" Process, so the verdict falls out of Signal below).
	// Kept anyway because the DOCUMENTED contract is "a Process or an
	// error", and the failure mode of trusting it is a nil dereference
	// inside a liveness probe. A review suggested `p, _ :=` — that is the
	// version with the nil-deref.
	if err != nil || p == nil {
		return false
	}
	return signal0Alive(p.Signal(syscall.Signal(0)))
}

// signal0Alive classifies the result of a signal-0 liveness probe.
//
// Split out from pidAlive so the classification can be tested directly.
// Asserting it through pidAlive(1) instead would be permission-dependent:
// unprivileged, signal 0 to init returns EPERM and exercises this branch,
// but AS ROOT the signal simply succeeds — so on a root CI runner (the
// common container case) that test passes without ever reaching the EPERM
// path it exists to pin.
//
// EPERM means ALIVE: the process exists, we merely aren't allowed to
// signal it. Everything else — ESRCH for a dead pid, os.ErrProcessDone
// for a reaped child — means dead.
func signal0Alive(err error) bool {
	if err == nil {
		return true
	}
	return errors.Is(err, syscall.EPERM)
}

// inodeOf returns the inode number of the file fi describes, which on a
// cgroup2 mount is the cgroup's id (cgroupID).
func inodeOf(fi fs.FileInfo) (uint64, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return uint64(st.Ino), true
}
