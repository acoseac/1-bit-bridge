//go:build !windows

package doctor

import (
	"context"
	"net"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// cancelPropagationBudget is how long a cancelled probe is allowed to take
// before the test calls it a failure. Deliberately well under probeTimeout
// (2s): passing within this window proves the CALLER's cancellation did the
// work, not the internal deadline — which would otherwise mask a version
// that ignores the incoming context entirely.
const cancelPropagationBudget = 1500 * time.Millisecond

// stubLsofWithSleep points the lsof spawn at a long-running `sleep` for the
// duration of a test.
//
// Why a real process rather than a fake that returns canned output: the
// property under test is that CANCELLING THE CONTEXT ACTUALLY STOPS THE
// PROBE. Only exec.CommandContext's own kill-on-cancel can demonstrate
// that, and only against a child that would otherwise still be running.
// `sleep 10` stands in for what production hits — lsof stat()ing a wedged
// network mount while building its device cache.
//
// It also sets lsofPath, because isPIDListeningOnPort short-circuits to
// (false, nil) when lsof didn't resolve on this host, and that early return
// would let the test pass on a machine with no lsof without ever reaching
// the code it exists to cover.
func stubLsofWithSleep(t *testing.T, seconds string) {
	t.Helper()
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skipf("no sleep(1) on PATH to stand in for a blocking lsof: %v", err)
	}

	origPath := lsofPath
	origCmd := lsofCommand
	t.Cleanup(func() {
		lsofPath = origPath
		lsofCommand = origCmd
	})
	lsofPath = sleep
	lsofCommand = func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, sleep, seconds)
	}
}

// TestIsPIDListeningOnPortHonoursCallerCancellation pins the propagation
// itself: the context handed to the probe must reach the subprocess, so
// cancelling it stops the probe rather than leaving the caller blocked
// until lsof decides to return.
//
// Pre-fix this function used a bare exec.Command with no context at all,
// and there was no bound of any kind — the observable difference is
// entirely in the elapsed time, so that is what the assertion keys on. The
// error return is asserted alongside it because a cut-short probe must NOT
// be reported as lsof's "ran, matched nothing": checkPort would take that
// as evidence the port isn't ours, which is precisely the claim no
// evidence supports.
func TestIsPIDListeningOnPortHonoursCallerCancellation(t *testing.T) {
	stubLsofWithSleep(t, "10")

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	defer cancel()

	start := time.Now()
	found, err := isPIDListeningOnPort(ctx, 7788, os.Getpid())
	elapsed := time.Since(start)

	if elapsed > cancelPropagationBudget {
		t.Errorf("probe took %s after the caller cancelled at ~50ms; the context is not reaching the subprocess", elapsed)
	}
	if err == nil {
		t.Error("a cancelled probe must return an error (a Warn for checkPort), not a silent 'not us'")
	}
	if found {
		t.Error("a cancelled probe cannot have found anything")
	}
}

// TestCheckPortPropagatesCancellationToOwnerProbe walks the whole chain
// Run → checkPort → isPIDListeningOnPort → exec, which is the path that
// actually hangs in production: the admin console calls doctor from a
// request goroutine, and its http.Server deliberately sets no WriteTimeout
// (a documented invariant — SSE and the multi-minute install/backup
// endpoints need it unset), so nothing else reaps a stuck check.
//
// The bind error is injected rather than bound for real so the test lands
// deterministically on the owner-probe branch, and the pidfile carries our
// own live PID so checkPort actually asks "is it us?".
func TestCheckPortPropagatesCancellationToOwnerProbe(t *testing.T) {
	stubLsofWithSleep(t, "10")
	withPortProbe(t, true)

	origListen := listenFunc
	t.Cleanup(func() { listenFunc = origListen })
	listenFunc = func(network, address string) (net.Listener, error) {
		return nil, &net.OpError{Op: "listen", Net: network, Err: os.NewSyscallError("bind", syscall.EADDRINUSE)}
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	defer cancel()

	start := time.Now()
	c := checkPort(ctx, "port-test", 7788, writePIDFile(t, os.Getpid()))
	elapsed := time.Since(start)

	if elapsed > cancelPropagationBudget {
		t.Errorf("checkPort took %s after the caller cancelled at ~50ms; ctx is not reaching the lsof spawn", elapsed)
	}
	// A probe-mechanism failure is a Warn, never a Fail — a broken or
	// aborted probe must not cry wolf about a healthy install's own port.
	if c.Status != Warn {
		t.Errorf("cancelled owner probe: got %v (%s / %s), want warn", c.Status, c.Summary, c.Hint)
	}
}
