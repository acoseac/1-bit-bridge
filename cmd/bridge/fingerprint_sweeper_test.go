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
