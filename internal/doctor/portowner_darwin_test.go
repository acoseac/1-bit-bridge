//go:build darwin

package doctor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"
)

// TestOwnListenersOnTheKernel drives macOS's second look at a recorded pid
// (pidListensOnPort) against real processes: a bridge stand-in listening on
// a port of its own, one that listens on nothing, and root's launchd. lsof
// lists the stand-in's listener, which is a match on its own port and rules
// it out of any other. It lists nothing for the other two, which is what a
// process with no listener and one this user may not read (#1028's row M1,
// root's Tailscale extension) both look like, so neither is ruled out.
func TestOwnListenersOnTheKernel(t *testing.T) {
	if mode := os.Getenv(listeningChildEnv); mode != "" {
		runListeningChild(mode)
	}
	if !lsofResolved() {
		t.Skip("no lsof on this Mac, and lsof is what reads a pid's listeners here")
	}
	bridge, own := startListeningChild(t, true)
	idle, _ := startListeningChild(t, false)
	held := bindPort(t)

	t.Run("the bridge's own port", func(t *testing.T) {
		found, seen, err := pidListensOnPort(t.Context(), own, bridge)
		if err != nil || !found {
			t.Errorf("got %v, %+v, %v; want found", found, seen, err)
		}
	})
	t.Run("a port another process holds", func(t *testing.T) {
		found, seen, err := pidListensOnPort(t.Context(), held, bridge)
		want := ownerSighting{saw: fmt.Sprintf("lsof lists pid %d listening only on 127.0.0.1:%d", bridge, own), ruledOut: true}
		if err != nil || found || seen != want {
			t.Errorf("got %v, %+v, %v; want not found, %+v", found, seen, err, want)
		}
	})
	t.Run("a process with no listener", func(t *testing.T) {
		requireNotRuledOut(t, held, idle)
	})
	t.Run("root's launchd, to a user", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root reads launchd's descriptors")
		}
		requireNotRuledOut(t, held, 1)
	})
}

// requireNotRuledOut requires the second look to neither find pid on port
// nor rule it out, without an error.
func requireNotRuledOut(t *testing.T, port, pid int) {
	t.Helper()
	found, seen, err := pidListensOnPort(t.Context(), port, pid)
	if err != nil || found || seen.ruledOut {
		t.Errorf("pid %d: got %v, %+v, %v; want neither found nor ruled out", pid, found, seen, err)
	}
}

// TestABlindedLsofRulesNoBridgeOut is why the ruling-out rests on what lsof
// listed rather than on the recorded pid's uid. lsof run as a process's own
// user reads it, so a clean miss about a pid of this user's could rule the
// pid out, but an lsof that a sandbox denies process info (a doctor run from
// a sandboxed terminal, or an agent's) reads no process and exits 1 all the
// same: no output, nothing on stderr, byte for byte a clean miss. A rule on
// the uid would read that as the bridge not holding its OWN port, and FAIL
// it. Asked for the bridge's listeners, the blinded lsof lists none, which
// rules nothing out, so both ports warn.
func TestABlindedLsofRulesNoBridgeOut(t *testing.T) {
	if mode := os.Getenv(listeningChildEnv); mode != "" {
		runListeningChild(mode)
	}
	if !lsofResolved() {
		t.Skip("no lsof on this Mac")
	}
	sandboxExec, err := exec.LookPath("sandbox-exec")
	if err != nil {
		t.Skipf("no sandbox-exec to blind lsof with: %v", err)
	}
	bridge, own := startListeningChild(t, true)
	held := bindPort(t)
	pidFile := writePIDFile(t, bridge)

	const profile = "(version 1)(allow default)(deny process-info-pidfdinfo)"
	origCmd := lsofCommand
	t.Cleanup(func() { lsofCommand = origCmd })
	lsofCommand = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		return exec.CommandContext(ctx, sandboxExec, append([]string{"-p", profile, name}, args...)...)
	}
	// The premise, measured rather than assumed: the sandbox hides the
	// bridge's listener from lsof and says nothing about it.
	out, err := lsofCommand(t.Context(), lsofPath, "-nP", "-iTCP:"+strconv.Itoa(own), "-sTCP:LISTEN", "-t").CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 || len(out) != 0 {
		t.Skipf("this sandbox no longer blinds lsof silently (%v, %q), so it cannot stand in for one that does", err, out)
	}

	withHiddenListener(t, false, nil)
	for _, tc := range []struct {
		name string
		port int
	}{
		{"the bridge's own port", own},
		{"a port another process holds", held},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if c := checkPort(t.Context(), "port-test", tc.port, pidFile); c.Status != Warn {
				t.Errorf("got %v (%s / %s), want warn: a blinded lsof lists nothing, which rules nothing out",
					c.Status, c.Summary, c.Hint)
			}
		})
	}
}

// TestOwnListenersWithoutLsof: lsof is the reader of a pid's listeners here,
// so without it the second look has nothing to look with, and says so in the
// words the missing lsof is put in front of.
func TestOwnListenersWithoutLsof(t *testing.T) {
	withoutLsof(t)
	found, seen, err := pidListensOnPort(t.Context(), 7788, os.Getpid())
	if want := (ownerSighting{saw: nothingElseMatches}); err != nil || found || seen != want {
		t.Errorf("got %v, %+v, %v; want not found, %+v", found, seen, err, want)
	}
}

// TestOwnListenersRunsReadWhatLsofAnswered covers the run's outcomes other
// than a listing, lsof answering through sh(1): exit 1 is lsof's "matched
// nothing", which says nothing; any other failure is an error, which
// procSecondOpinion reads as nothing either; and output of another shape is
// not a listing of pid 4242's listeners.
func TestOwnListenersRunsReadWhatLsofAnswered(t *testing.T) {
	for _, tc := range []struct {
		name    string
		stdout  string
		code    int
		wantErr bool
		want    ownerSighting
	}{
		{"matched nothing", "", 1, false, ownerSighting{saw: "lsof lists no listener of pid 4242"}},
		{"failed", "", 2, true, ownerSighting{}},
		{"another shape", "4242\n", 0, false, ownerSighting{saw: "lsof's output does not list pid 4242's listeners"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withLsofAnswering(t, tc.stdout, tc.code)
			found, seen, err := pidListensOnPort(t.Context(), 7788, 4242)
			if (err != nil) != tc.wantErr || found || seen != tc.want {
				t.Errorf("got %v, %+v, %v; want not found, %+v, error %v", found, seen, err, tc.want, tc.wantErr)
			}
		})
	}
}

// TestOwnListenersRunHonoursTheCallersContext: the second look runs lsof
// again, and lsof stat()s mount points before it reads any process, so a
// wedged network mount holds this run as it holds the first. The caller's
// cancellation must reach it, and a cut-short run is an error, never a
// listing of nothing.
func TestOwnListenersRunHonoursTheCallersContext(t *testing.T) {
	stubLsofWithSleep(t, "10")
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	defer cancel()

	start := time.Now()
	found, seen, err := pidListensOnPort(ctx, 7788, os.Getpid())
	if elapsed := time.Since(start); elapsed > cancelPropagationBudget {
		t.Errorf("the run took %s after the caller cancelled at ~50ms; the context is not reaching lsof", elapsed)
	}
	if err == nil || found || seen.ruledOut {
		t.Errorf("got %v, %+v, %v; want an error, neither found nor ruled out", found, seen, err)
	}
}
