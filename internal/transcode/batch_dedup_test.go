package transcode

import (
	"context"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/google/uuid"
)

func batchStatus(t *testing.T, s *manifest.Store, id uuid.UUID) string {
	t.Helper()
	rows, err := s.ListUpscaleBatches(context.Background(), 200)
	if err != nil {
		t.Fatalf("ListUpscaleBatches: %v", err)
	}
	for _, r := range rows {
		if r.ID == id {
			return r.Status
		}
	}
	t.Fatalf("batch %s not found", id)
	return ""
}

// TestSubmit_OverlappingReSubmitDoesNotStickRunning is the B1 regression guard:
// re-submitting the same folder+target while the first batch's jobs are still
// in-flight used to leave the second batch `running` forever, because the
// deduped jobs carry the FIRST batch's ID and never call back into the second.
// The fix drops deduped paths from the batch's remaining set and completes a
// fully-deduped batch immediately.
func TestSubmit_OverlappingReSubmitDoesNotStickRunning(t *testing.T) {
	s := openTempStoreForBatch(t)
	t.Cleanup(func() { _ = s.Close() })
	seedBatchFixture(t, s) // Album/02 + Album/03 are the eligible candidates.

	// Blocking runner: jobs stay claimed in the pool's inflight map (never
	// commit a variant), so an overlapping re-submit dedups against them.
	release := make(chan struct{})
	p := NewPool(s, 2, 16)
	t.Cleanup(p.Stop)
	p.fsyncFn = noopFsync
	p.runner = func(ctx context.Context, spec JobSpec) (int64, error) {
		<-release
		return spec.SourceSize * 2, nil
	}
	dataDir := t.TempDir()
	c, err := NewCoordinator(p, s, dataDir, nil, func(rel string) (string, error) { return "/tmp/abs/" + rel, nil })
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	c.SetPublish(func(BatchProgressEvent) {})
	p.SetOnJobComplete(func(path, variantID string, sr, bps int, d float64, id uuid.UUID, at time.Time) {
		c.OnJobComplete(path, variantID, sr, bps, d, id, at)
	})
	p.SetOnJobFailed(func(path, variantID, msg string, d float64, id uuid.UUID, at time.Time) {
		c.OnJobFailed(path, variantID, msg, d, id, at)
	})

	// Batch 1 enqueues 02 + 03; they block in the runner and stay in-flight.
	r1, err := c.Submit(context.Background(), "Album", 192000, 24, t.TempDir())
	if err != nil {
		t.Fatalf("Submit 1: %v", err)
	}
	if r1.EnqueuedCount != 2 {
		t.Fatalf("batch 1 EnqueuedCount = %d, want 2", r1.EnqueuedCount)
	}
	if r1.TotalFiles != 2 {
		t.Errorf("batch 1 TotalFiles = %d, want 2 (both enqueued)", r1.TotalFiles)
	}

	// Batch 2: same folder + target while batch 1 is in-flight → both dedup.
	r2, err := c.Submit(context.Background(), "Album", 192000, 24, t.TempDir())
	if err != nil {
		t.Fatalf("Submit 2: %v", err)
	}
	if r2.EnqueuedCount != 0 {
		t.Errorf("batch 2 EnqueuedCount = %d, want 0 (all deduped)", r2.EnqueuedCount)
	}
	// TotalFiles must report the enqueued count (0), NOT len(cands) (2). The old
	// code read it back from liveBatches after the fully-deduped transition had
	// already deleted the batch, so it fell back to len(cands) and the 202
	// response disagreed with the persisted row (CodeRabbit PR #515).
	if r2.TotalFiles != 0 {
		t.Errorf("batch 2 TotalFiles = %d, want 0 (all deduped, not len(cands))", r2.TotalFiles)
	}
	// The fix: batch 2 must reach a terminal status, not sit `running`/`pending`.
	if st := batchStatus(t, s, r2.BatchID); st == "running" || st == "pending" {
		t.Errorf("batch 2 status = %q, want terminal (this was the stuck-running bug)", st)
	}

	close(release) // let batch 1's jobs finish so the pool Stops cleanly
}
