package main

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeRestamper drives both branches of runDuplicatesSweeper's
// scan-in-flight check deterministically. manifest.Scanner's scanning
// flag is an unexported atomic with no setter, which is why the sweeper
// takes the duplicatesRestamper interface.
type fakeRestamper struct {
	scanning  atomic.Bool
	scanCalls atomic.Int64 // IsScanning invocations — the spin detector

	mu       sync.Mutex
	restamps int
	fired    chan struct{} // one send per RestampDuplicates call
}

func newFakeRestamper() *fakeRestamper {
	return &fakeRestamper{fired: make(chan struct{}, 8)}
}

func (f *fakeRestamper) IsScanning() bool {
	f.scanCalls.Add(1)
	return f.scanning.Load()
}

func (f *fakeRestamper) RestampDuplicates(context.Context) (int, error) {
	f.mu.Lock()
	f.restamps++
	f.mu.Unlock()
	select {
	case f.fired <- struct{}{}:
	default:
	}
	return 0, nil
}

func (f *fakeRestamper) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.restamps
}

// A policy change that lands while a scan is in flight must still be
// applied — the sweeper RE-ARMS the nudge and retries, it does not drop
// it.
//
// The dropped-nudge version rested on "the running scan's tail already
// applies the new value". The sweeper cannot know that:
// RestampDuplicates snapshots the policy ONCE at the top of the pass and
// then makes two full-library streaming walks, so a PATCH committing
// inside that window is lost outright; and Scan's tail may never run at
// all (it early-returns when the routed-exclusion fetch fails). Either
// way nothing re-stamps until the next periodic scan — 6h by default —
// while the admin UI says the new policy is being applied.
//
// Shape: nudge while "scanning", confirm nothing ran, clear the flag,
// confirm the deferred intent surfaces as exactly one pass.
func TestDuplicatesSweeperReArmsNudgeDeferredBehindAScan(t *testing.T) {
	r := newFakeRestamper()
	r.scanning.Store(true)

	nudge := make(chan struct{}, 1)
	nudge <- struct{}{} // the settings PATCH, landing mid-scan

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		// 1ms retry: the production constant is 5s, and the loop's
		// cadence is not what this test pins.
		runDuplicatesSweeper(ctx, r, nudge, nil, time.Millisecond)
	}()

	// While the scan is in flight the pass must not run — the deferral
	// itself is still correct behaviour, only the dropping was not.
	time.Sleep(50 * time.Millisecond)
	if n := r.count(); n != 0 {
		t.Fatalf("restamped %d times while a scan was in flight, want 0 "+
			"(the pass must defer behind the scanner, not race it)", n)
	}

	// Scan ends. The re-armed nudge must now produce a pass.
	r.scanning.Store(false)
	select {
	case <-r.fired:
	case <-time.After(3 * time.Second):
		t.Fatalf("no restamp after the scan cleared: the nudge that arrived " +
			"mid-scan was DROPPED, so the operator's duplicates.filter change " +
			"is never applied until the next periodic scan")
	}

	// And exactly one — a re-arm that outlives its own deferral would
	// spin the pass forever off a single operator action.
	time.Sleep(100 * time.Millisecond)
	if n := r.count(); n != 1 {
		t.Fatalf("restamped %d times for ONE nudge, want exactly 1 "+
			"(the re-armed nudge must be consumed by the pass, not perpetuate)", n)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("sweeper did not exit on ctx cancellation")
	}
}

// A non-positive deferRetry must NOT turn the re-arm branch into a spin.
//
// time.After(0) fires immediately, so an unclamped zero makes the loop
// put the nudge back and receive it again with no wait — a tight cycle
// that burns a core for the entire duration of a scan while logging at
// Info every iteration. No production caller passes zero today, which is
// precisely why a regression here would go unnoticed: the only signal is
// CPU.
//
// Asserting "the clamp returns the default" would pin almost nothing —
// this counts actual loop iterations instead, by watching IsScanning
// calls over a window. Clamped, the loop parks for 5s and checks once.
func TestDuplicatesSweeperZeroDeferRetryDoesNotSpin(t *testing.T) {
	r := newFakeRestamper()
	r.scanning.Store(true) // never clears: the sweeper stays in the defer branch

	nudge := make(chan struct{}, 1)
	nudge <- struct{}{}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runDuplicatesSweeper(ctx, r, nudge, nil, 0) // the clamp's input
	}()

	const window = 300 * time.Millisecond
	time.Sleep(window)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("sweeper did not exit on cancel")
	}

	// Clamped to 5s, exactly one iteration fits in the window (the
	// initial nudge). The bound is generous — a real spin runs this into
	// the tens of thousands.
	const maxIterations = 5
	if n := r.scanCalls.Load(); n > maxIterations {
		t.Fatalf("IsScanning called %d times in %v: a non-positive deferRetry is "+
			"not clamped, so the scan-in-flight re-arm branch spins at full CPU "+
			"for as long as the scan runs", n, window)
	}
	if r.count() != 0 {
		t.Fatalf("restamped %d times while scanning", r.count())
	}
}

// The deferral back-off must observe ctx cancellation: the sweeper is
// joined on bgWriters, so a goroutine parked in the retry wait during a
// scan would hold shutdown open for the whole back-off window.
func TestDuplicatesSweeperDeferralExitsOnCancel(t *testing.T) {
	r := newFakeRestamper()
	r.scanning.Store(true) // never clears — the sweeper stays in the defer loop

	nudge := make(chan struct{}, 1)
	nudge <- struct{}{}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		// A back-off far longer than the test's patience: only ctx
		// cancellation can end the wait.
		runDuplicatesSweeper(ctx, r, nudge, nil, time.Hour)
	}()

	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("sweeper stayed parked in the deferral back-off after cancel — " +
			"it is bgWriters-joined, so this holds up shutdown")
	}
}
