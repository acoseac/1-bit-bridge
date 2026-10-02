package main

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// The cadence tests below all drive runSweepLoop directly. They exist
// because "the interval is a provider now" is only half the change: a
// provider that is read once at the top of the loop is exactly as
// restart-bound as the captured duration it replaced, and nothing about
// the type signature would say so.
//
// Each drains its loop through drainLoopOnCleanup rather than a bare
// `defer cancel()`. On a t.Fatalf the body Goexits, the deferred cancel
// fires, and the test returns with the loop still running — so what
// gets reported afterwards is whatever the leaked goroutine trips over,
// not the assertion that failed. These four are in-memory, which makes
// them the mild end of the class #944/#945 fixed, but they are also
// invisible to TestEveryBackgroundGoroutineDrainsOnCleanup: its shape
// match is `go func(){ defer close(ch) … }()`, and a bare
// `go runSweepLoop(...)` has no channel to wait on at all. Wrapping
// them in that shape is what puts them IN the population the guard
// counts, which is the point — the guard exists so the next one does
// not have to be found by hand.
//
// The `defer cancel()` each used to carry is gone rather than kept
// beside the drain: a defer beats every t.Cleanup, so it would cancel
// the context before the drain's own cancel ran and quietly invert the
// ordering the helper establishes.

// TestSweepLoopRereadsIntervalEveryIteration is the core conversion.
//
// Pre-change the loop built one time.NewTicker before the loop body and
// never re-evaluated it, which is precisely why scanIntervalSec needed a
// restart. Here the provider hands out a long interval first and a short
// one afterwards; if the loop cached the first value the second sweep
// never arrives inside the deadline.
//
// It also pins what the loop tells each pass: the settle-delay sweep is the
// boot pass, and a periodic one is not.
func TestSweepLoopRereadsIntervalEveryIteration(t *testing.T) {
	var reads atomic.Int64
	interval := func() time.Duration {
		// First read (the boot pass's decision) and second (the first
		// wait) are long; everything after is short. A cached provider
		// parks on the long one forever.
		if reads.Add(1) <= 2 {
			return time.Hour
		}
		return 5 * time.Millisecond
	}

	sweeps := make(chan bool, 8)
	ctx, cancel := context.WithCancel(context.Background())
	rearm := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		runSweepLoop(ctx, &sweepStatus[struct{}]{}, 0, interval, nil, rearm, func(boot bool) {
			select {
			case sweeps <- boot:
			default:
			}
		})
	}()
	drainLoopOnCleanup(t, cancel, done, "the cadence sweep loop")

	// The settle-delay sweep.
	if boot := waitSweep(t, sweeps, "initial"); !boot {
		t.Error("the settle-delay sweep was not told it is the boot pass")
	}
	// The loop is now parked on the 1 h wait it read. Rearm it: the next
	// read returns 5 ms, so a periodic sweep must follow shortly.
	rearm <- struct{}{}
	if boot := waitSweep(t, sweeps, "after the interval shortened"); boot {
		t.Error("a periodic sweep was told it is the boot pass")
	}
}

// TestSweepLoopRearmDoesNotSweep pins the distinction between the two
// channels. A rearm asks the loop to re-read its SCHEDULE; a nudge asks
// it to do the WORK. Collapsing them would turn "I changed the backup
// cadence" into "run a backup now", which on a large library is a
// materially different thing to have asked for.
func TestSweepLoopRearmDoesNotSweep(t *testing.T) {
	var sweeps atomic.Int64
	ctx, cancel := context.WithCancel(context.Background())
	rearm := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		runSweepLoop(ctx, &sweepStatus[struct{}]{}, 0, staticInterval(time.Hour), nil, rearm,
			func(bool) { sweeps.Add(1) })
	}()
	drainLoopOnCleanup(t, cancel, done, "the cadence sweep loop")

	// Wait out the initial sweep.
	waitFor(t, func() bool { return sweeps.Load() == 1 }, "initial sweep")
	for i := 0; i < 5; i++ {
		rearm <- struct{}{}
	}
	// Give the loop room to (incorrectly) sweep.
	time.Sleep(120 * time.Millisecond)
	if got := sweeps.Load(); got != 1 {
		t.Errorf("sweeps = %d after 5 rearms, want 1 — a rearm must re-read the "+
			"schedule, never run the work", got)
	}
}

// TestSweepLoopDormantIntervalIsResumable pins the 0 → N transition.
//
// The old loop returned outright when the interval was non-positive and
// no nudge was wired, so "disabled" was terminal for the process: an
// operator setting backup.intervalHours back to 24 had no loop alive to
// notice. Parking instead is what makes the field hot in both directions.
//
// And a loop dormant from the start takes no boot pass (backlog B209).
// This test asserted the opposite until 2026-10-02 ("want 1 (the initial
// one only)"), the defect written down as intended: since #769 started the
// backup ticker on every bridge, that initial sweep was a snapshot and a
// prune at every boot of a bridge whose backups were switched off. The
// first pass after re-enabling is a periodic one, never the boot pass.
func TestSweepLoopDormantIntervalIsResumable(t *testing.T) {
	var d, reads atomic.Int64 // d 0: dormant
	sweeps := make(chan bool, 8)

	ctx, cancel := context.WithCancel(context.Background())
	rearm := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		runSweepLoop(ctx, &sweepStatus[struct{}]{}, 0,
			func() time.Duration { reads.Add(1); return time.Duration(d.Load()) }, nil, rearm,
			func(boot bool) { sweeps <- boot })
	}()
	drainLoopOnCleanup(t, cancel, done, "the cadence sweep loop")

	// The loop reads its interval where it decides on the boot pass and
	// again where it parks, so a second read says it has parked.
	waitFor(t, func() bool { return reads.Load() >= 2 }, "the loop to park")
	if got := len(sweeps); got != 0 {
		t.Fatalf("sweeps = %d while dormant from the start, want 0: a cadence that is off takes no boot pass", got)
	}

	// Re-enable. Without the parked loop there is nothing here to wake.
	d.Store(int64(5 * time.Millisecond))
	rearm <- struct{}{}
	if boot := waitSweep(t, sweeps, "a sweep after re-enabling"); boot {
		t.Error("the first sweep after re-enabling was told it is the boot pass")
	}
}

// TestSweepLoopDormantServesANudgeFromTheSettleWindow pins the drain's other
// half. A nudge that lands in the settle window is drained because the boot
// pass covers it; a loop dormant at the end of that window takes no boot
// pass, so the nudge is a request nothing has covered, and it gets a sweep
// of its own. Drained there, as it was before the boot pass learned to
// stand down, it would be lost.
func TestSweepLoopDormantServesANudgeFromTheSettleWindow(t *testing.T) {
	var reads atomic.Int64
	sweeps := make(chan bool, 8)
	nudge := make(chan struct{}, 1)
	nudge <- struct{}{} // lands before the settle window ends

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runSweepLoop(ctx, &sweepStatus[struct{}]{}, 0,
			func() time.Duration { reads.Add(1); return 0 }, nudge, nil,
			func(boot bool) { sweeps <- boot })
	}()
	drainLoopOnCleanup(t, cancel, done, "the cadence sweep loop")

	// The boot decision, the first park and the park after the nudge's
	// sweep: three reads.
	waitFor(t, func() bool { return reads.Load() >= 3 }, "the loop to serve the nudge and park again")
	if got := len(sweeps); got != 1 {
		t.Fatalf("sweeps = %d, want 1: the nudge's", got)
	}
	if boot := <-sweeps; boot {
		t.Error("the nudge's sweep was told it is the boot pass")
	}
}

// TestSweepLoopDormantClearsScheduledNext — a stale "next run at 14:00"
// on the Jobs card, after the operator disabled the cadence, is a promise
// the loop will not keep.
func TestSweepLoopDormantClearsScheduledNext(t *testing.T) {
	var d atomic.Int64
	d.Store(int64(time.Hour))
	status := &sweepStatus[struct{}]{}

	ctx, cancel := context.WithCancel(context.Background())
	rearm := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		runSweepLoop(ctx, status, 0,
			func() time.Duration { return time.Duration(d.Load()) }, nil, rearm, func(bool) {})
	}()
	drainLoopOnCleanup(t, cancel, done, "the cadence sweep loop")

	waitFor(t, func() bool {
		_, _, _, next, _ := status.snapshot()
		return !next.IsZero()
	}, "a scheduled next run")

	d.Store(0)
	rearm <- struct{}{}
	waitFor(t, func() bool {
		_, _, _, next, _ := status.snapshot()
		return next.IsZero()
	}, "the scheduled next run to be cleared")
}

// --- helpers ---

// waitSweep waits for a sweep on ch and gives what the loop told it: whether
// it is the boot pass.
func waitSweep(t *testing.T, ch <-chan bool, what string) bool {
	t.Helper()
	select {
	case boot := <-ch:
		return boot
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		return false
	}
}

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
