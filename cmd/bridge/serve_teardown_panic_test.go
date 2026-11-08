package main

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
)

// Backlog B760's third residual: a panic on serve's goroutine was read as a
// clean end wherever a stop, a restart request or a refusal of the host's had
// come before it, so the start's count went back: a panic in the teardown of
// a stop that came before the start was ready, a panic after a stop and
// before serve's stop branch read it, and a panic in the teardown of a ready
// start whose stop had taken its count back as it arrived.

// teardownPanic and startPanic are what these tests' starts panic with: in
// serve's teardown, and in its start once its stop came.
const (
	teardownPanic = "a crash in serve's teardown"
	startPanic    = "a crash of serve's start once its stop came"
)

// launchServeThatMayPanic boots serve on r's install with opts until it
// listens, as bootWith does, recovering a panic of serve's goroutine around
// runServe as a supervisor's restart follows a crash: panicked gets what serve
// ended in, nil for no panic, once serve has ended.
func (r *refusedInstall) launchServeThatMayPanic(t *testing.T, opts serveOpts) (servedBridge, <-chan any) {
	t.Helper()
	opts.configPath, opts.binaryPath = r.cfg, r.bin
	if opts.backupProbeTimeout == 0 {
		opts.backupProbeTimeout = testBackupProbeTimeout
	}
	panicked := make(chan any, 1)
	sb := launchServe(t, func(ctx context.Context, stdout, stderr io.Writer) (code int) {
		defer func() {
			p := recover()
			if p != nil {
				code = -1
			}
			panicked <- p
		}()
		return runServe(ctx, opts, stdout, stderr)
	})
	return sb, panicked
}

// waitForThePanic waits for the serve sb runs to end, and requires it to have
// ended in want, raised as it was.
func waitForThePanic(t *testing.T, sb servedBridge, panicked <-chan any, want string) {
	t.Helper()
	select {
	case code := <-sb.done:
		if p := <-panicked; p != want {
			t.Fatalf("serve ended %d in %v, want %q raised again as it was; stderr=%s", code, p, want, sb.stderr.String())
		}
	case <-serveGiveUp(t):
		t.Fatalf("serve did not end; stderr=%s\nserve's goroutines:\n%s", sb.stderr.String(), serveStacks())
	}
}

// TestAPanicInTheTeardownOfAStopBeforeReadyStaysCounted: a stop of a start
// that has not answered as ready, whose teardown then panics on serve's
// goroutine, stays counted, its token cleared: the panic is no clean end,
// whatever came before it, and it is raised again as it was. Until B760 the
// stop read the end as clean once the teardown had run, and the count went
// back.
func TestAPanicInTheTeardownOfAStopBeforeReadyStaysCounted(t *testing.T) {
	r := newRefusedInstall(t, "")
	_, seams := newProbationWatch(neverReady)
	sb, panicked := r.launchServeThatMayPanic(t, serveOpts{probation: seams, teardownHold: func() { panic(teardownPanic) }})
	r.requireCount(t, 1, "while serve runs, not ready yet")
	sb.stop()
	waitForThePanic(t, sb, panicked, teardownPanic)
	r.requireCount(t, 1, "after the teardown of its stop panicked")
	if st := markerOf(t, r.dataDir); st.RunningStart != "" {
		t.Errorf("the marker after the panic = %+v, want the start's token cleared", st)
	}
}

// TestAPanicAfterAStopBeforeServesStopBranchStaysCounted: a stop that lands as
// serve starts, its caller's context ended, followed on serve's goroutine by a
// panic before serve's select has read the stop, stays counted, the panic
// raised again as it was. Until B760 the caller's ended context alone made the
// end a stop.
func TestAPanicAfterAStopBeforeServesStopBranchStaysCounted(t *testing.T) {
	r := newRefusedInstall(t, "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var p any
	func() {
		defer func() { p = recover() }()
		var out, errs bytes.Buffer
		runServe(ctx, serveOpts{configPath: r.cfg, binaryPath: r.bin, backupProbeTimeout: testBackupProbeTimeout,
			playerStatePreflight: func(string, bool) error {
				cancel()
				panic(startPanic)
			}}, &out, &errs)
	}()
	if p != startPanic {
		t.Fatalf("serve ended in %v, want %q raised again as it was", p, startPanic)
	}
	r.requireCount(t, 1, "after a panic that came after its stop")
}

// TestAPanicInTheTeardownOfAReadyStartsStopCountsItAgain: a start that has
// answered as ready has its count taken back as its stop arrives (backlog
// B660's second round), so that a supervisor's kill of a slow teardown leaves
// a clean stop uncounted; a panic on serve's goroutine in the teardown after
// that counts the start again, says so, and is raised again as it was. Until
// B760 the count stayed taken back.
func TestAPanicInTheTeardownOfAReadyStartsStopCountsItAgain(t *testing.T) {
	r := newRefusedInstall(t, "")
	teardown := newParkedTeardown()
	events, seams := newProbationWatch(longProbation)
	sb, panicked := r.launchServeThatMayPanic(t, serveOpts{probation: seams, upnpStopping: teardown.seam,
		teardownHold: func() { panic(teardownPanic) }})
	teardown.releaseFirst(t)
	events.waitFor(t, probationWindow)
	r.requireCount(t, 1, "while serve runs, ready")
	sb.stop()
	teardown.waitParked(t)
	if !r.waitForCountWithin(t, 0, withinTheTest) {
		t.Fatalf("the marker while the teardown is held = %+v, want the stop's count taken back as it arrived: the test shows nothing",
			markerOf(t, r.dataDir))
	}
	teardown.release.close()
	waitForThePanic(t, sb, panicked, teardownPanic)
	r.requireCount(t, 1, "after the teardown of its stop panicked")
	if !strings.Contains(sb.stderr.String(), "counted again") {
		t.Errorf("serve did not say it counted the start again; stderr=%s", sb.stderr.String())
	}
}
