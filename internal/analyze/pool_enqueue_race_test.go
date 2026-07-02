package analyze

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
)

// TestPoolEnqueueRacingStopNoPanic is the analyze-pool twin of the
// transcode regression guard for the Enqueue-vs-Stop shutdown race
// (bridge02-03 review, finding A). See the transcode test for the full
// rationale: fireStateChange() moved under p.mu so a preempted enqueuer
// can't send on a stateChangeChan that a concurrent Stop() already closed.
//
// A send-on-closed panic crashes the test binary, so surviving many
// Enqueue/Stop races IS the assertion. Run under -race.
func TestPoolEnqueueRacingStopNoPanic(t *testing.T) {
	s := newStore(t)

	// Fails immediately: workers drain + exit fast (no store write, no
	// track seeding) so Stop's wg.Wait returns while enqueuers are mid-window.
	stub := func(context.Context, AnalyzeSpec) (Result, error) {
		return Result{}, errors.New("stub")
	}

	for iter := 0; iter < 200; iter++ {
		p := NewPool(s, 2, 128, WithRunner(stub), WithFsync(noFsync))

		var enq sync.WaitGroup
		for g := 0; g < 8; g++ {
			enq.Add(1)
			go func(g int) {
				defer enq.Done()
				for j := 0; j < 50; j++ {
					// Unique dedup key per enqueue — a shared key hits the
					// p.inflight early-return BEFORE the channel send +
					// fireStateChange, so the race window is never entered.
					rel := fmt.Sprintf("t-%d-%d-%d.flac", iter, g, j)
					_ = p.Enqueue(AnalyzeSpec{
						SourceLibraryRel: rel,
						SourceAbsPath:    "/lib/" + rel,
					})
				}
			}(g)
		}

		stopDone := make(chan struct{})
		go func() { p.Stop(); close(stopDone) }()
		enq.Wait()
		<-stopDone
	}
}
