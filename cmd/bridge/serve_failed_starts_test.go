package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/supervision"
	"github.com/acoseac/1-bit-bridge/internal/updater"
	"github.com/acoseac/1-bit-bridge/internal/version"
)

// Backlog B660: each start of an install's version counts itself at its
// admission (State.FailedStarts), and takes its count back only where it ends
// cleanly: serve's stop, a restart request, a bind the host refused (an
// address another process holds, a permission), a tailnet listener that
// failed. A panic, an error return and a kill stay counted, and
// updater.ProbationFailedStarts of them roll the install back at the next
// start.

// requireCount requires the marker in r's data dir to hold count starts that
// did not end cleanly, the install still on probation.
func (r *refusedInstall) requireCount(t *testing.T, count int, when string) {
	t.Helper()
	if st := markerOf(t, r.dataDir); st.FailedStarts != count || st.Status != "installing" {
		t.Errorf("the marker %s = %+v, want %d starts counted and the install on probation", when, st, count)
	}
}

// serveStoppedAtItsPreflight runs serve on r's install with its context
// cancelled where it runs its player's state preflight, right after its
// admission, as a stop that lands while serve starts, and returns its code
// and what it said.
func (r *refusedInstall) serveStoppedAtItsPreflight(t *testing.T) (int, string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	stderr := &safeBuffer{}
	codes, done := make(chan int, 1), make(chan struct{})
	go func() {
		defer close(done)
		codes <- runServe(ctx, serveOpts{configPath: r.cfg, binaryPath: r.bin,
			playerStatePreflight: func(string, bool) error { cancel(); return nil }}, &safeBuffer{}, stderr)
	}()
	drainLoopOnCleanup(t, cancel, done, "serve")
	select {
	case code := <-codes:
		return code, stderr.String()
	case <-serveGiveUp(t):
		t.Fatalf("serve stopped as it started did not return; stderr=%s\nserve's goroutines:\n%s", stderr.String(), serveStacks())
	}
	return 0, ""
}

// waitForExit waits for the serve sb runs to exit, and requires want.
func waitForExit(t *testing.T, sb servedBridge, want int) {
	t.Helper()
	select {
	case code := <-sb.done:
		if code != want {
			t.Fatalf("serve exited %d, want %d; stderr=%s", code, want, sb.stderr.String())
		}
	case <-serveGiveUp(t):
		t.Fatalf("serve did not exit; stderr=%s\nserve's goroutines:\n%s", sb.stderr.String(), serveStacks())
	}
}

// requestRestart asks the console at admin to restart the bridge, as its
// Restart button does.
func requestRestart(t *testing.T, admin string) {
	t.Helper()
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Post(admin+"/api/restart", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST /api/restart = %d, want 202", resp.StatusCode)
	}
}

// TestAStopIsNotAFailedStart: serve's stop, once it serves or while it is
// still starting, takes back the count its admission added, so a healthy
// install stopped and started by its operator never nears its rollback.
func TestAStopIsNotAFailedStart(t *testing.T) {
	t.Run("once it serves", func(t *testing.T) {
		r := newRefusedInstall(t, "")
		_, seams := newProbationWatch(longProbation)
		sb := r.bootWith(t, serveOpts{probation: seams})
		r.requireCount(t, 1, "while serve runs")
		stopServed(t, sb)
		r.requireCount(t, 0, "after its stop")
	})
	t.Run("while it starts", func(t *testing.T) {
		r := newRefusedInstall(t, "")
		if code, stderr := r.serveStoppedAtItsPreflight(t); code != 0 {
			t.Fatalf("serve stopped while it started = %d, want 0; stderr=%s", code, stderr)
		}
		r.requireCount(t, 0, "after a stop while it started")
	})
}

// TestARestartRequestIsNotAFailedStart: the console's restart exits 75 for
// the service manager to start serve again, and takes back the count, as a
// stop does.
func TestARestartRequestIsNotAFailedStart(t *testing.T) {
	r := newRefusedInstall(t, "")
	_, seams := newProbationWatch(longProbation)
	sb, admin := r.bootConsole(t, serveOpts{probation: seams})
	r.requireCount(t, 1, "while serve runs")
	requestRestart(t, admin)
	waitForExit(t, sb, supervision.RestartExitCode)
	r.requireCount(t, 0, "after a restart request")
}

// TestARestartIntoANewerInstallLeavesItsMarker: an install of a newer version
// made while this start runs replaces the marker and asks for the restart;
// the take-back at the end of this start finds no install of its version to
// take its count from, and writes nothing.
func TestARestartIntoANewerInstallLeavesItsMarker(t *testing.T) {
	r := newRefusedInstall(t, "")
	_, seams := newProbationWatch(longProbation)
	sb, admin := r.bootConsole(t, serveOpts{probation: seams})
	// What an install of a newer version records before its restart.
	newer := updater.State{Status: "installing", TargetVersion: "9.9.9", PreviousVersion: version.ServerVersion,
		AttemptedAt: time.Now().UTC().Truncate(time.Second), SwapStarted: true}
	if err := updater.SaveState(r.dataDir, newer); err != nil {
		t.Fatal(err)
	}
	marker := watch(t, updater.StatePath(r.dataDir))
	requestRestart(t, admin)
	waitForExit(t, sb, supervision.RestartExitCode)
	marker.requireUnchanged(t)
}

// TestAStopAfterConfirmationWritesNothing: a start that confirmed its
// install, which cleared the count, has no count left to take back, so its
// stop leaves the marker byte for byte as the confirmation wrote it: the
// take-back reads the marker again and writes only an install of its version
// that awaits its confirmation and holds the count this start wrote.
func TestAStopAfterConfirmationWritesNothing(t *testing.T) {
	r := newRefusedInstall(t, "")
	events, seams := newProbationWatch(shortProbation)
	sb := r.bootWith(t, serveOpts{probation: seams})
	events.waitFor(t, probationConfirmed)
	if st := markerOf(t, r.dataDir); st.Status != "installed" || st.FailedStarts != 0 {
		t.Fatalf("the marker after the confirmation = %+v, want the install confirmed, its count cleared", st)
	}
	marker := watch(t, updater.StatePath(r.dataDir))
	stopServed(t, sb)
	marker.requireUnchanged(t)
}

// TestAPortAnotherProcessHoldsIsNotAFailedStart: a listener's address that
// another process holds is the host's trouble, which the backup would meet
// too, so the start that cannot bind it, the API's or the console's, takes
// back its count; so is an address the host does not have, a static address
// not up yet after a reboot (B660's second round: four such starts in a row
// counted, and the fourth put back a backup that met the same refusal).
func TestAPortAnotherProcessHoldsIsNotAFailedStart(t *testing.T) {
	t.Run("the API's", func(t *testing.T) {
		r := newRefusedInstall(t, "")
		held := holdLoopback(t)
		r.writeListeners(t, held.addr, "127.0.0.1:0")
		code, stderr := r.serve(t, serveOpts{})
		if code != 1 || !strings.Contains(stderr, "listen "+held.addr) {
			t.Fatalf("serve over an API address another process holds = %d, want 1 at its listen; stderr=%s", code, stderr)
		}
		r.requireCount(t, 0, "after a bind another process refused")
	})
	t.Run("the console's", func(t *testing.T) {
		r := newRefusedInstall(t, "")
		held, lan := holdLoopback(t), holdLoopback(t)
		r.writeListeners(t, lan.addr, held.addr)
		code, stderr := r.serve(t, serveOpts{addrOverride: lan.addr, lanListener: lan.ln})
		if code != 1 || !strings.Contains(stderr, "admin server:") {
			t.Fatalf("serve over a console address another process holds = %d, want 1 at the console's bind; stderr=%s", code, stderr)
		}
		r.requireCount(t, 0, "after a bind another process refused")
	})
	t.Run("an address the host does not have", func(t *testing.T) {
		// TEST-NET-1 (RFC 5737): no host holds it, so a bind is refused
		// with EADDRNOTAVAIL, unless the host binds addresses it lacks.
		const absent = "192.0.2.1:0"
		if ln, err := net.Listen("tcp", absent); err == nil {
			_ = ln.Close()
			t.Skipf("this host binds %s, an address it does not hold, so the refusal cannot be made here", absent)
		}
		r := newRefusedInstall(t, "")
		r.writeListeners(t, absent, "127.0.0.1:0")
		binaries := watch(t, r.bin, r.bak)
		// Four starts in a row, as Restart=always makes of an address that
		// comes up only later: none counts, so the fourth puts nothing back.
		for start := 1; start <= updater.ProbationFailedStarts+1; start++ {
			code, stderr, served := serveToItsEnd(t, serveOpts{configPath: r.cfg, binaryPath: r.bin})
			if served || code != 1 || !strings.Contains(stderr, "listen "+absent) {
				t.Fatalf("start %d over an address the host does not have = %d (served %v), want 1 at its listen; stderr=%s",
					start, code, served, stderr)
			}
			r.requireCount(t, 0, fmt.Sprintf("after start %d's bind refused for an address the host does not have", start))
		}
		binaries.requireUnchanged(t)
	})
}

// TestAStoreThatWillNotOpenIsAFailedStart: a start whose store refuses to
// open (a link at bridge.db) returns an error, and stays counted.
func TestAStoreThatWillNotOpenIsAFailedStart(t *testing.T) {
	r := newRefusedInstall(t, "")
	if err := os.Symlink("elsewhere.db", manifest.DefaultDBPath(r.dataDir)); err != nil {
		t.Skipf("no symbolic link can be made here: %v", err)
	}
	if code, stderr, served := serveToItsEnd(t, serveOpts{configPath: r.cfg, binaryPath: r.bin}); served || code != 1 {
		t.Fatalf("serve over a store that will not open = %d (served %v), want 1; stderr=%s", code, served, stderr)
	}
	r.requireCount(t, 1, "after a start whose store would not open")
}

// TestAPanicInServeIsAFailedStart: a start that panics on serve's goroutine
// runs serve's defers with nothing to say it ended cleanly, and stays
// counted.
func TestAPanicInServeIsAFailedStart(t *testing.T) {
	r := newRefusedInstall(t, "")
	r.startThatPanics(t)
	r.requireCount(t, 1, "after a start that panicked")
	r.startThatPanics(t)
	r.requireCount(t, 2, "after two starts that panicked")
}

// TestACountPublishedBeforeItsBarrierFailedIsTakenBack: the marker's write
// publishes before its directory barrier, so a count whose barrier failed may
// stand in the marker all the same; the start said so, went on, and takes it
// back at its stop as it would a count that landed.
func TestACountPublishedBeforeItsBarrierFailedIsTakenBack(t *testing.T) {
	r := newRefusedInstall(t, "")
	// A start of this version opened the data dir before, so this start's
	// count is its admission's only write.
	r.marker.DataDirOpenedBy = version.ServerVersion
	r.saveMarker(t, r.dataDir)
	var writes atomic.Int32
	var armed atomic.Bool
	t.Cleanup(updater.SetBeforeDisarmRecordForTest(func(string) {
		// The admission's count is the first disarm write; the store's
		// record of its migration, after it, must land.
		if writes.Add(1) == 1 {
			armed.Store(true)
		}
	}))
	t.Cleanup(updater.SetMarkerBarrierForTest(func(step string) error {
		if step == "sync" && armed.CompareAndSwap(true, false) {
			return syscall.EIO
		}
		return nil
	}))
	_, seams := newProbationWatch(longProbation)
	sb := r.bootWith(t, serveOpts{probation: seams})
	if !strings.Contains(sb.stderr.String(), "was not counted against its install") {
		t.Errorf("serve did not say its count's write failed; stderr=%s", sb.stderr.String())
	}
	if armed.Load() {
		t.Fatal("the count's barrier was never asked: the test shows nothing")
	}
	r.requireCount(t, 1, "once the count's barrier failed")
	stopServed(t, sb)
	r.requireCount(t, 0, "after the stop")
}
