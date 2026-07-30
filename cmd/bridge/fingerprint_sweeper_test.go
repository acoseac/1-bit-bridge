package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/acoustid"
)

// TestSweeperPacerSerialisesAcrossWorkers pins the fix for a bug that made the
// pacer look like it worked while doing nothing.
//
// The earlier version released the mutex before sleeping, so every worker
// acquired it in turn, read the SAME stale timestamp, computed the same delay,
// and they all woke and fired together — a burst of exactly worker-count
// requests, which is precisely what the pacer exists to prevent. Nothing about
// the code's shape gave that away; only the timing does.
//
// Asserting on elapsed time rather than on call ordering is what makes this a
// real test: serialisation that does not actually space the calls out would
// still pass an ordering check.
func TestSweeperPacerSerialisesAcrossWorkers(t *testing.T) {
	// A local base URL resolves to the self-hosted interval, keeping the test
	// quick while still exercising real pacing.
	c := acoustid.NewClient("http://127.0.0.1:1/v2", "k", "ua", nil)
	interval := c.MinInterval()
	s := &fingerprintSweeper{client: c}

	const workers = 4
	var wg sync.WaitGroup
	start := time.Now()
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.wait(context.Background())
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)

	// The first call returns immediately (no prior timestamp); each of the
	// remaining three must wait its own interval.
	min := time.Duration(workers-2) * interval
	if elapsed < min {
		t.Fatalf("%d workers paced in %v, want at least %v — they are not being "+
			"serialised, so they would burst against AcoustID", workers, elapsed, min)
	}
}

// TestSweeperPacerHonoursCancellation — a shutting-down sweep must not sit out
// the full interval.
func TestSweeperPacerHonoursCancellation(t *testing.T) {
	c := acoustid.NewClient("https://api.acoustid.org/v2", "k", "ua", nil) // public: 350ms
	s := &fingerprintSweeper{client: c, last: time.Now()}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	s.wait(ctx)
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("wait took %v on a cancelled context, want a prompt return", elapsed)
	}
}

// TestWaitForWorkersLetsHealthyWorkersFinish pins the distinction that a
// production run exposed.
//
// The first version applied the shutdown grace UNCONDITIONALLY, so every sweep
// was capped at it: on a real host, 500 candidates on one worker hit the cap
// after 60s, the sweep reported a truncated result, and the workers kept
// running past it into the next tick. A healthy sweep and a wedged filesystem
// look alike in the code and are not alike at all — one is normal work that
// takes minutes, the other is a mount that will never answer.
func TestWaitForWorkersLetsHealthyWorkersFinish(t *testing.T) {
	// The worker must OUTLAST the grace, or the test passes under the buggy
	// unconditional form too and pins nothing. A short grace keeps the suite
	// fast; passing it in means no shared state is mutated to get it.
	const grace = 40 * time.Millisecond

	done := make(chan struct{})
	go func() { time.Sleep(200 * time.Millisecond); close(done) }()

	start := time.Now()
	waitForWorkers(context.Background(), grace, done)
	elapsed := time.Since(start)

	if elapsed < 180*time.Millisecond {
		t.Fatalf("returned after %v, before the worker finished at ~200ms — "+
			"the shutdown grace is bounding healthy work, which caps every sweep", elapsed)
	}
	select {
	case <-done:
	default:
		t.Fatal("returned before the workers finished")
	}
}

// TestWaitForWorkersGivesUpOnACancelledSweep — the case the grace exists for.
// A worker wedged in an uninterruptible FUSE syscall will not take SIGKILL, so
// the wait must not be unbounded once shutdown has begun.
func TestWaitForWorkersGivesUpOnACancelledSweep(t *testing.T) {
	never := make(chan struct{}) // a worker that never finishes
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// A short grace rather than sleeping the real one out.
	const grace = 80 * time.Millisecond

	start := time.Now()
	waitForWorkers(ctx, grace, never)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("waited %v on a cancelled sweep — a wedged worker must not hang shutdown", elapsed)
	}
}

// TestSweeperDrainGraceIsAShutdownBound documents what the constant is for, so
// it is not repurposed as a per-sweep budget again. A sweep of 500 candidates
// on one worker legitimately runs for minutes.
func TestSweeperDrainGraceIsAShutdownBound(t *testing.T) {
	if sweeperDrainGrace > time.Minute {
		t.Errorf("sweeperDrainGrace = %v; it bounds SHUTDOWN, so it should stay small — "+
			"a long value here delays process exit on a wedged mount", sweeperDrainGrace)
	}
}
