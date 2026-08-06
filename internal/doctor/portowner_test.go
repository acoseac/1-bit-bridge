package doctor

import (
	"math"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
)

// withPortOwner forces portOwnerFunc for a test. A host's real answer
// depends on whether THIS user happens to own a listener on the probed
// port, which flips between platforms and between "bound by the test" and
// "bound by something else" — so the branch has to be driven explicitly or
// the assertion is about the host, not the code.
func withPortOwner(t *testing.T, owned bool, err error) {
	t.Helper()
	orig := portOwnerFunc
	t.Cleanup(func() { portOwnerFunc = orig })
	portOwnerFunc = func(int) (bool, error) { return owned, err }
}

// withPIDAlive forces pidAliveFunc for a test.
func withPIDAlive(t *testing.T, alive bool) {
	t.Helper()
	orig := pidAliveFunc
	t.Cleanup(func() { pidAliveFunc = orig })
	pidAliveFunc = func(int) bool { return alive }
}

// writePIDFile drops a pidfile holding `pid` and returns its path.
func writePIDFile(t *testing.T, pid int) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "server.pid")
	if err := os.WriteFile(p, []byte(strconv.Itoa(pid)), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// bindPort binds a port for the duration of the test and returns it.
func bindPort(t *testing.T) int {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lis.Close() })
	return lis.Addr().(*net.TCPAddr).Port
}

// TestPortCheck_LivePIDUnattributableWarns is the fix for the production
// false-FAIL on bridge.ars.md.
//
// The runbook prescribes `setcap cap_net_bind_service=+ep` so a non-root
// service can bind :443. That gives the process dumpable=0, so no
// unprivileged observer can attribute the port to a pid — lsof exits 1
// with no output, which isPIDListeningOnPort correctly reads as "ran,
// matched nothing" → (false, nil). Pre-fix that fell straight through to
// "another process owns this port" against the operator's own healthy
// bridge.
func TestPortCheck_LivePIDUnattributableWarns(t *testing.T) {
	withPortProbe(t, true)
	withPIDAlive(t, true)
	withPortOwner(t, false, nil) // can't tell who owns it
	port := bindPort(t)

	c := checkPort(t.Context(), "port-test", port, writePIDFile(t, 4242))
	if c.Status != Warn {
		t.Errorf("bound port, recorded pid alive, owner unattributable: got %v (%s / %s), want warn",
			c.Status, c.Summary, c.Hint)
	}
}

// TestPortCheck_DeadPIDStillFails is the negative control for the test
// above, and the more important of the two: the Warn must be earned by the
// recorded PID being ALIVE, not handed out to every unattributable port.
// A stale pidfile left by a crashed bridge names a dead PID, and a genuine
// conflict on that port has to stay a Fail.
func TestPortCheck_DeadPIDStillFails(t *testing.T) {
	withPortProbe(t, true)
	withPIDAlive(t, false)
	withPortOwner(t, false, nil)
	port := bindPort(t)

	c := checkPort(t.Context(), "port-test", port, writePIDFile(t, 4242))
	if c.Status != Fail {
		t.Errorf("bound port with a STALE pidfile: got %v (%s), want fail — "+
			"the liveness check is what separates 'probably ours' from a real conflict",
			c.Status, c.Summary)
	}
}

// TestPortCheck_LivePIDOwnedByThisUserIsOK pins the upgrade: when the
// last-resort probe CAN say the listener belongs to our uid, that is a
// better answer than the Warn and doctor reports ok.
func TestPortCheck_LivePIDOwnedByThisUserIsOK(t *testing.T) {
	withPortProbe(t, true)
	withPIDAlive(t, true)
	withPortOwner(t, true, nil)
	port := bindPort(t)

	c := checkPort(t.Context(), "port-test", port, writePIDFile(t, 4242))
	if c.Status != OK {
		t.Errorf("bound port owned by this user: got %v (%s / %s), want ok", c.Status, c.Summary, c.Hint)
	}
}

// TestPortCheck_OwnerProbeErrorFallsBackToWarn — a mechanism failure in the
// last-resort probe must not be read as a positive match. It lands on the
// same Warn as "asked and got no match".
func TestPortCheck_OwnerProbeErrorFallsBackToWarn(t *testing.T) {
	withPortProbe(t, true)
	withPIDAlive(t, true)
	withPortOwner(t, true, os.ErrPermission) // owned=true but errored
	port := bindPort(t)

	c := checkPort(t.Context(), "port-test", port, writePIDFile(t, 4242))
	if c.Status != Warn {
		t.Errorf("owner probe errored: got %v (%s), want warn — an errored probe is not a match",
			c.Status, c.Summary)
	}
}

// TestPIDAlive_SelfAndReaped exercises the REAL platform helper rather than
// the seam. Our own PID is alive by construction; a child we have waited
// on is definitively gone.
//
// PID recycling could in principle reassign the reaped PID between Wait
// and the check, which would make this flake — vanishingly unlikely inside
// one test, and the alternative (asserting against a hardcoded PID) tests
// nothing.
func TestPIDAlive_SelfAndReaped(t *testing.T) {
	if !pidAlive(os.Getpid()) {
		t.Error("pidAlive(self) = false; our own process is alive by construction")
	}
	if pidAlive(0) || pidAlive(-1) {
		t.Error("pidAlive must reject non-positive pids rather than asking the OS about them")
	}

	// Out of range for a pid on any supported platform. This matters most
	// on Windows, where the pid is cast to a DWORD: 4294967297 truncates
	// to 1, a pid that very much exists, so an unbounded cast turns a
	// corrupt pidfile into "alive". readPID parses with strconv.Atoi into
	// an int, so a 64-bit host really can carry this value here.
	if pidAlive(math.MaxUint32 + 1) {
		t.Errorf("pidAlive(%d) = true; a pid past the platform's range must not be truncated into a live one",
			uint64(math.MaxUint32)+1)
	}

	// The EPERM-means-alive branch, which is the one the whole fix leans
	// on: a capability-bound bridge is exactly a live process we may be
	// unable to interrogate. pid 1 (init / launchd) is that case for an
	// unprivileged run — signal 0 returns EPERM while the process plainly
	// exists. Verified reachable here rather than assumed: on this host
	// `p.Signal(syscall.Signal(0))` to pid 1 does return EPERM. Running as
	// root the signal simply succeeds, so the assertion holds either way
	// and needs no skip.
	if runtime.GOOS != "windows" && !pidAlive(1) {
		t.Error("pidAlive(1) = false; a process that exists but cannot be signalled must read as ALIVE, " +
			"or a capability-bound bridge is reported dead and its own port reads as a conflict")
	}

	// A trivially short-lived child, reaped so the PID is genuinely free.
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("cmd", "/c", "exit")
	} else {
		cmd = exec.Command("true")
	}
	if err := cmd.Start(); err != nil {
		t.Skipf("could not spawn a probe process on this host: %v", err)
	}
	pid := cmd.Process.Pid
	_ = cmd.Wait()
	if pidAlive(pid) {
		t.Errorf("pidAlive(%d) = true for a reaped child; a stale pidfile must not read as live", pid)
	}
}
