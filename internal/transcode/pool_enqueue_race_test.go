package transcode

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
)

// TestPoolEnqueueRacingStopNoPanic is the regression guard for the
// Enqueue-vs-Stop shutdown race (bridge02-03 review, finding A).
//
// Before the fix, Enqueue called fireStateChange() AFTER releasing p.mu.
// A preempted enqueuer could resume in that window after a concurrent
// Stop() had already run to completion (wg.Wait returns once workers
// drain the just-enqueued job and exit) and closed stateChangeChan — so
// the send hit a closed channel and panicked (send-on-closed panics even
// inside a select/default). The fix moves fireStateChange() under p.mu,
// which strictly orders it before Stop's close (Stop must take p.mu to
// close the jobs channels, and closes stateChangeChan only afterward).
//
// A send-on-closed panic crashes the test binary, so surviving many
// Enqueue/Stop races IS the assertion. Run under -race for the strongest
// signal. The fast-failing runner keeps workers exiting quickly so Stop's
// wg.Wait returns while enqueuers are still mid-window — the fixed code
// sails through, the buggy code panics within a few iterations.
func TestPoolEnqueueRacingStopNoPanic(t *testing.T) {
	store := openTempStoreForPool(t)
	t.Cleanup(func() { _ = store.Close() })

	// Fails immediately: workers drain + exit fast (no store write, no
	// track seeding needed — processJob's failure path skips UpsertVariant).
	stub := func(context.Context, JobSpec) (int64, string, error) { return 0, "", errors.New("stub") }

	for iter := 0; iter < 200; iter++ {
		p := NewPool(store, 2, 128)
		p.fsyncFn = noopFsync
		p.runner = stub

		var enq sync.WaitGroup
		for g := 0; g < 8; g++ {
			enq.Add(1)
			go func(g int) {
				defer enq.Done()
				for j := 0; j < 50; j++ {
					// Unique dedup key per enqueue. A shared key hits the
					// p.inflight early-return BEFORE the channel send +
					// fireStateChange, so the race window would never be
					// entered and the test would falsely pass on buggy code.
					_ = p.Enqueue(JobSpec{
						SourceLibraryRel: fmt.Sprintf("t-%d-%d-%d.flac", iter, g, j),
					})
				}
			}(g)
		}

		// Race Stop against the in-flight enqueue storm. Wait for the pool
		// to fully stop before the next iteration so goroutines don't pile.
		stopDone := make(chan struct{})
		go func() { p.Stop(); close(stopDone) }()
		enq.Wait()
		<-stopDone
	}
}
