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

// TestSweepLoopRereadsIntervalEveryIteration is the core conversion.
//
// Pre-change the loop built one time.NewTicker before the loop body and
// never re-evaluated it, which is precisely why scanIntervalSec needed a
// restart. Here the provider hands out a long interval first and a short
// one afterwards; if the loop cached the first value the second sweep
// never arrives inside the deadline.
func TestSweepLoopRereadsIntervalEveryIteration(t *testing.T) {
	var reads atomic.Int64
	interval := func() time.Duration {
		// First read (the pre-sweep scheduleNext) and second (the first
		// wait) are long; everything after is short. A cached provider
		// parks on the long one forever.
		if reads.Add(1) <= 2 {
			return time.Hour
		}
		return 5 * time.Millisecond
	}

	sweeps := make(chan struct{}, 8)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rearm := make(chan struct{}, 1)
	go runSweepLoop(ctx, &sweepStatus[struct{}]{}, 0, interval, nil, rearm, func() {
		select {
		case sweeps <- struct{}{}:
		default:
		}
	})

	// The settle-delay sweep.
	waitSweep(t, sweeps, "initial")
	// The loop is now parked on the 1 h wait it read. Rearm it: the next
	// read returns 5 ms, so a periodic sweep must follow shortly.
	rearm <- struct{}{}
	waitSweep(t, sweeps, "after the interval shortened")
}

// TestSweepLoopRearmDoesNotSweep pins the distinction between the two
// channels. A rearm asks the loop to re-read its SCHEDULE; a nudge asks
// it to do the WORK. Collapsing them would turn "I changed the backup
// cadence" into "run a backup now", which on a large library is a
// materially different thing to have asked for.
func TestSweepLoopRearmDoesNotSweep(t *testing.T) {
	var sweeps atomic.Int64
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rearm := make(chan struct{}, 1)
	go runSweepLoop(ctx, &sweepStatus[struct{}]{}, 0, staticInterval(time.Hour), nil, rearm,
		func() { sweeps.Add(1) })

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
func TestSweepLoopDormantIntervalIsResumable(t *testing.T) {
	var d atomic.Int64
	d.Store(0) // dormant
	var sweeps atomic.Int64

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rearm := make(chan struct{}, 1)
	go runSweepLoop(ctx, &sweepStatus[struct{}]{}, 0,
		func() time.Duration { return time.Duration(d.Load()) }, nil, rearm,
		func() { sweeps.Add(1) })

	waitFor(t, func() bool { return sweeps.Load() == 1 }, "initial sweep")
	time.Sleep(60 * time.Millisecond)
	if got := sweeps.Load(); got != 1 {
		t.Fatalf("sweeps = %d while dormant, want 1 (the initial one only)", got)
	}

	// Re-enable. Without the parked loop there is nothing here to wake.
	d.Store(int64(5 * time.Millisecond))
	rearm <- struct{}{}
	waitFor(t, func() bool { return sweeps.Load() >= 2 }, "sweep after re-enabling")
}

// TestSweepLoopDormantClearsScheduledNext — a stale "next run at 14:00"
// on the Jobs card, after the operator disabled the cadence, is a promise
// the loop will not keep.
func TestSweepLoopDormantClearsScheduledNext(t *testing.T) {
	var d atomic.Int64
	d.Store(int64(time.Hour))
	status := &sweepStatus[struct{}]{}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rearm := make(chan struct{}, 1)
	go runSweepLoop(ctx, status, 0,
		func() time.Duration { return time.Duration(d.Load()) }, nil, rearm, func() {})

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

func waitSweep(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
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
