package transcode

import (
	"context"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/google/uuid"
)

// claimInflight simulates an overlapping batch holding `path` in the pool's
// dedup map, so this batch's Enqueue for it returns ErrDuplicateInflight.
//
// Poked directly rather than staged via a second real Submit because the wedge
// needs a PARTIAL dedup (one fresh candidate + one claimed), and a real
// overlapping Submit over the same prefix claims BOTH — which is the
// fully-deduped case the old gate already handled. Downstream the two are
// indistinguishable: Enqueue's only dedup input is `p.inflight`.
func claimInflight(t *testing.T, p *Pool, path string, spec JobSpec) {
	t.Helper()
	spec.SourceLibraryRel = path
	key := path + "|" + spec.VariantID()
	p.mu.Lock()
	p.claimSeq++
	p.inflight[key] = p.claimSeq
	p.mu.Unlock()
}

// wedgeCoordinator wires a coordinator whose runner completes immediately for
// `fastPath` and parks every other job. Returns the coordinator, the pool, and
// a channel closed once `fastPath`'s OnJobComplete callback has RETURNED (i.e.
// after it has removed the path from RemainingIDs under c.mu).
func wedgeCoordinator(t *testing.T, s *manifest.Store, fastPath string) (*Coordinator, *Pool, chan struct{}) {
	t.Helper()
	done := make(chan struct{})

	p := NewPool(s, 4, 16)
	t.Cleanup(p.Stop)
	p.fsyncFn = noopFsync
	// Non-fastPath jobs park until the pool shuts down. Parking on ctx rather
	// than a test-owned channel means the deferred p.Stop can never wedge on
	// wg.Wait() — Stop cancels stopCtx, which is exactly the signal a real
	// runner (exec.CommandContext) honours.
	p.runner = func(ctx context.Context, spec JobSpec) (int64, string, error) {
		if spec.SourceLibraryRel != fastPath {
			<-ctx.Done()
			return 0, "", ctx.Err()
		}
		return spec.SourceSize * 2, "", nil
	}
	c, err := NewCoordinator(p, s, t.TempDir(), nil,
		func(rel string) (string, error) { return "/tmp/abs/" + rel, nil })
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	c.SetPublish(func(BatchProgressEvent) {})
	p.SetOnJobComplete(func(path, variantID string, sr, bps int, d float64, id uuid.UUID, at time.Time) {
		c.OnJobComplete(path, variantID, sr, bps, d, id, at)
		if path == fastPath {
			select {
			case <-done:
			default:
				close(done)
			}
		}
	})
	p.SetOnJobFailed(func(path, variantID, msg string, d float64, id uuid.UUID, at time.Time) {
		c.OnJobFailed(path, variantID, msg, d, id, at)
	})
	return c, p, done
}

// TestSubmit_DedupDropAfterMidLoopCompletionDoesNotWedgeBatch is the F1
// regression guard.
//
// `RemainingIDs` shrinks from two places. The job callbacks (OnJobComplete /
// OnJobFailed) each evaluate `allDone` and terminate the batch themselves;
// `dropDedupedPath` does not. So a callback landing WHILE the enqueue loop is
// still running removes its path without terminating (later candidates are
// still in the set), and a following dedup drop can empty the set with nobody
// left to notice. The old post-loop gate was `deduped == len(cands)`, which is
// false for a PARTIAL dedup — so the row sat `running` forever with no callback
// left to arrive (the deduped job carries the OTHER batch's ID) and the
// `liveBatches` entry leaked for the process lifetime, inflating
// `Throughput().EtaSeconds`.
//
// The interleave is forced structurally via `afterEnqueueHookForTests` rather
// than by timing: the existing `blockingBatchCoordinator` parks every runner so
// no callback can land mid-loop at all, and a wall-clock race would go green on
// broken code whenever the runner is loaded.
func TestSubmit_DedupDropAfterMidLoopCompletionDoesNotWedgeBatch(t *testing.T) {
	s := openTempStoreForBatch(t)
	t.Cleanup(func() { _ = s.Close() })
	seedBatchFixture(t, s) // Album/02 + Album/03 are the eligible candidates.

	// 02 runs and completes; 03 is claimed by a (simulated) overlapping batch.
	const freshPath = "Album/02.flac"
	const dedupedPath = "Album/03.flac"
	c, p, jobDone := wedgeCoordinator(t, s, freshPath)
	claimInflight(t, p, dedupedPath, JobSpec{TargetSampleRate: 192000, TargetBits: 24})

	// Park the enqueue loop right after 02 is enqueued, until 02's
	// OnJobComplete has fully returned. The loop then reaches 03, dedups it,
	// and empties RemainingIDs with the batch still `running`.
	hookFired := false
	afterEnqueueHookForTests = func(path string) {
		if path != freshPath {
			return
		}
		hookFired = true
		select {
		case <-jobDone:
		case <-time.After(15 * time.Second):
			t.Error("timed out waiting for the fresh job's completion callback")
		}
	}
	t.Cleanup(func() { afterEnqueueHookForTests = nil })

	res, err := c.Submit(context.Background(), "Album", 192000, 24, t.TempDir())
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if !hookFired {
		t.Fatalf("enqueue hook never fired for %q — candidate ordering changed, "+
			"the interleave under test was not exercised", freshPath)
	}
	if res.EnqueuedCount != 1 {
		t.Fatalf("EnqueuedCount = %d, want 1 (02 fresh, 03 deduped)", res.EnqueuedCount)
	}

	// The bug: status stays `running` with nothing left to report back.
	if st := batchStatus(t, s, res.BatchID); st != "completed" {
		t.Errorf("batch status = %q, want %q (this is the wedged-forever bug: "+
			"a mid-loop completion plus a later dedup drop empties RemainingIDs "+
			"without any terminal transition)", st, "completed")
	}

	// The leak half: a wedged batch also stays in liveBatches forever, so its
	// remaining files keep feeding Throughput().EtaSeconds.
	c.mu.Lock()
	_, stillLive := c.liveBatches[res.BatchID]
	c.mu.Unlock()
	if stillLive {
		t.Errorf("batch %s still in liveBatches after Submit returned — leaked for "+
			"the process lifetime (no stale-batch reaper exists)", res.BatchID)
	}
}

// TestSubmit_QueueFullTerminalStatusSurvivesDrainedCheck pins the concern the
// old `deduped == len(cands)` gate's comment cited: a queue-full break that
// already set a terminal status must NOT be overwritten with "completed".
//
// The new predicate is strictly safer here — the truncation path deletes the
// batch from liveBatches when it goes terminal, so the liveness half makes
// completeIfDrained a no-op rather than a clobber.
func TestSubmit_QueueFullTerminalStatusSurvivesDrainedCheck(t *testing.T) {
	s := openTempStoreForBatch(t)
	t.Cleanup(func() { _ = s.Close() })
	seedBatchFixture(t, s)

	c, p, _ := wedgeCoordinator(t, s, "")
	// Closed pool → every Enqueue fails on the FIRST candidate, so the
	// truncation path drops the whole tail and sets a terminal "failed".
	p.Stop()

	res, err := c.Submit(context.Background(), "Album", 192000, 24, t.TempDir())
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if res.EnqueuedCount != 0 {
		t.Fatalf("EnqueuedCount = %d, want 0 (pool closed)", res.EnqueuedCount)
	}
	if st := batchStatus(t, s, res.BatchID); st != "failed" {
		t.Errorf("batch status = %q, want %q — the drained check must not clobber "+
			"a terminal status the truncation path already set", st, "failed")
	}
}
