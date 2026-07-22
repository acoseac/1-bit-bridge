//go:build !windows

package doctor

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// isPIDListeningOnPort reports whether targetPID is among the processes
// listening on the given local TCP port. The bool answers "is it us?" for
// checkPort's own-bridge branch; the error reports a probe-MECHANISM
// failure (lsof is resolved but the invocation couldn't even start) so
// checkPort can degrade to Warn rather than a hard Fail — a broken probe
// must never break a healthy install.
//
// Returns (false, nil) when the probe is simply unavailable (no usable
// lsof on this host): portProbeAvailable() already gates that case in
// checkPort, so the bound-port verdict there is Warn, not Fail.
//
// Implementation: `lsof -nP -iTCP:<port> -sTCP:LISTEN -t` prints one PID
// per line. We check membership across ALL of them (not just the first)
// so a dual-stack / multi-interface listener set can't mask our own PID
// behind another process's row. Exec'ing the resolved ABSOLUTE path (not
// the bare "lsof" name) defends against a PATH-injected binary.
func isPIDListeningOnPort(port, targetPID int) (bool, error) {
	if lsofPath == "" {
		return false, nil
	}
	out, err := exec.Command(lsofPath, "-nP", "-iTCP:"+strconv.Itoa(port), "-sTCP:LISTEN", "-t").Output()
	if err != nil {
		// lsof exit code 1 means it ran but matched nothing (the common
		// case when the port's owner isn't visible to us) — that's "not
		// us", not a mechanism failure. ANY OTHER outcome (a different
		// non-zero exit, or a genuine can't-start error like the binary
		// vanishing / permission) surfaces as an error so checkPort
		// degrades to Warn rather than a false Fail.
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return false, nil
		}
		return false, err
	}
	for _, f := range strings.Fields(string(out)) {
		if n, convErr := strconv.Atoi(f); convErr == nil && n == targetPID {
			return true, nil
		}
	}
	return false, nil
}

// lsofPath is the absolute path to lsof, resolved ONCE at package init.
// "" means lsof is unavailable on this host: not installed, or — defending
// against PATH injection — the PATH lookup returned a relative path. Both
// portProbeAvailable and isPIDListeningOnPort key off it, so the bridge
// never execs an attacker-staged lsof off a writable CWD / PATH entry.
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

// portProbeAvailable reports whether isPIDListeningOnPort can identify the
// owner of a bound port on THIS host — i.e. lsof resolved to a usable
// absolute path. When false, checkPort can't tell a port bound by our own
// bridge apart from a real conflict, so it degrades to Warn rather than a
// hard Fail. Package var so tests can stub it.
var portProbeAvailable = func() bool { return lsofPath != "" }
