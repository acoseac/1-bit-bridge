package transcode

import (
	"context"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/google/uuid"
)

// batchTotalFiles reads total_files back from the DATABASE — the whole
// point of these tests. The coordinator's in-memory row was always
// correct; it was the persisted row that stayed stale, so an assertion
// against liveBatches (or against SubmitResult, which is built from the
// same in-memory value) passes on the broken code and pins nothing.
func batchTotalFiles(t *testing.T, s *manifest.Store, id uuid.UUID) int {
	t.Helper()
	rows, err := s.ListUpscaleBatches(context.Background(), 200)
	if err != nil {
		t.Fatalf("ListUpscaleBatches: %v", err)
	}
	for _, r := range rows {
		if r.ID == id {
			return r.TotalFiles
		}
	}
	t.Fatalf("batch %s not found", id)
	return 0
}

// blockingBatchCoordinator wires a coordinator whose runner parks every
// job, so a follow-up Submit dedups against the still-in-flight work.
// Returns the coordinator and the release channel.
func blockingBatchCoordinator(t *testing.T, s *manifest.Store) (*Coordinator, chan struct{}) {
	t.Helper()
	release := make(chan struct{})
	p := NewPool(s, 4, 16)
	t.Cleanup(p.Stop)
	p.fsyncFn = noopFsync
	p.runner = func(ctx context.Context, spec JobSpec) (int64, error) {
		<-release
		return spec.SourceSize * 2, nil
	}
	c, err := NewCoordinator(p, s, t.TempDir(), nil,
		func(rel string) (string, error) { return "/tmp/abs/" + rel, nil })
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
	return c, release
}

// A FULLY-deduped batch must persist total_files = 0.
//
// This batch reaches its terminal status through transitionStatus →
// UpdateUpscaleBatchStatus, which pre-fix wrote only status/error/
// updated_at. The coordinator shrank the live row to 0 via
// dropDedupedPath, but the DB kept the original candidate count — so
// the admin Jobs page rendered "0/2 completed" forever for a batch
// where everything was correctly deduped against an overlapping run.
func TestSubmit_FullyDedupedBatchPersistsTotalFiles(t *testing.T) {
	s := openTempStoreForBatch(t)
	t.Cleanup(func() { _ = s.Close() })
	seedBatchFixture(t, s) // Album/02 + Album/03 eligible

	c, release := blockingBatchCoordinator(t, s)
	defer close(release)

	if _, err := c.Submit(context.Background(), "Album", 192000, 24, t.TempDir()); err != nil {
		t.Fatalf("Submit 1: %v", err)
	}
	r2, err := c.Submit(context.Background(), "Album", 192000, 24, t.TempDir())
	if err != nil {
		t.Fatalf("Submit 2: %v", err)
	}
	if r2.EnqueuedCount != 0 {
		t.Fatalf("precondition: batch 2 enqueued %d, want 0 (all deduped)", r2.EnqueuedCount)
	}
	if got := batchTotalFiles(t, s, r2.BatchID); got != 0 {
		t.Fatalf("persisted total_files = %d, want 0 — the deduped count "+
			"never reached the DB, so the Jobs page shows a stale denominator", got)
	}
}

// A PARTIALLY-deduped batch must persist the reduced total once a job
// callback lands. Pre-fix UpdateUpscaleBatchProgress omitted the column
// entirely, so no amount of progress churn ever corrected it — and the
// batch is dropped from liveBatches on completion, so it never
// self-heals.
func TestSubmit_PartiallyDedupedBatchPersistsTotalFiles(t *testing.T) {
	s := openTempStoreForBatch(t)
	t.Cleanup(func() { _ = s.Close() })
	seedBatchFixture(t, s)

	// A track in a sub-folder so batch 1 can claim exactly one of the
	// three candidates batch 2 will see.
	rate, bits, isDSD := float64(96000), 24, false
	if err := s.UpsertTrack(context.Background(), &manifest.Track{
		Path: "Album/Disc1/05.flac", Size: 5_000_000,
		SampleRate: &rate, BitsPerSample: &bits, Codec: "FLAC", IsDSD: &isDSD,
	}); err != nil {
		t.Fatalf("UpsertTrack: %v", err)
	}

	c, release := blockingBatchCoordinator(t, s)

	// Batch 1 claims Disc1/05 and parks it in-flight.
	r1, err := c.Submit(context.Background(), "Album/Disc1", 192000, 24, t.TempDir())
	if err != nil {
		t.Fatalf("Submit 1: %v", err)
	}
	if r1.EnqueuedCount != 1 {
		t.Fatalf("precondition: batch 1 enqueued %d, want 1", r1.EnqueuedCount)
	}

	// Batch 2 sees 02 + 03 + Disc1/05; the last dedups against batch 1.
	r2, err := c.Submit(context.Background(), "Album", 192000, 24, t.TempDir())
	if err != nil {
		t.Fatalf("Submit 2: %v", err)
	}
	if r2.EnqueuedCount != 2 {
		t.Fatalf("precondition: batch 2 enqueued %d, want 2 (1 of 3 deduped)", r2.EnqueuedCount)
	}

	// Let the jobs run so batch 2's completion callbacks persist.
	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if st := batchStatus(t, s, r2.BatchID); st != "running" && st != "pending" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if got := batchTotalFiles(t, s, r2.BatchID); got != 2 {
		t.Fatalf("persisted total_files = %d, want 2 — the dedup decrement "+
			"lived only in liveBatches, so the DB kept the pre-dedup count", got)
	}
}

// The monotonic guard: total_files only ever shrinks after INSERT (the
// dedup decrement and the queue-full truncation are the sole writers),
// so a stale row snapshot landing out of order must not raise it back.
func TestUpdateUpscaleBatchProgress_TotalFilesIsMonotonic(t *testing.T) {
	s := openTempStoreForBatch(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()

	id := uuid.New()
	row := manifest.UpscaleBatchRow{
		ID: id, Path: "Album", TargetRate: 192000, TargetBits: 24,
		Status: "running", TotalFiles: 10, CreatedAt: 1, UpdatedAt: 1,
	}
	if err := s.InsertUpscaleBatch(ctx, row); err != nil {
		t.Fatalf("InsertUpscaleBatch: %v", err)
	}

	row.TotalFiles = 6
	row.UpdatedAt = 2
	if err := s.UpdateUpscaleBatchProgress(ctx, row); err != nil {
		t.Fatalf("shrink: %v", err)
	}
	if got := batchTotalFiles(t, s, id); got != 6 {
		t.Fatalf("after shrink total_files = %d, want 6", got)
	}

	// A stale snapshot still carrying the pre-dedup count.
	row.TotalFiles = 10
	row.UpdatedAt = 3
	if err := s.UpdateUpscaleBatchProgress(ctx, row); err != nil {
		t.Fatalf("stale write: %v", err)
	}
	if got := batchTotalFiles(t, s, id); got != 6 {
		t.Fatalf("stale snapshot raised total_files back to %d, want 6", got)
	}
}

// A terminal status must not be clobbered by a later non-terminal
// write.
//
// transitionStatus builds its row copy under Coordinator.mu but
// persists OUTSIDE it, so a pending→running promotion racing a Cancel
// can land in the opposite order. Without a guard the `running` write
// wins, and nothing corrects it — the terminal transition already
// removed the batch from liveBatches, so no later callback revisits the
// row and it sits `running` forever.
func TestUpdateUpscaleBatchStatus_DoesNotResurrectTerminal(t *testing.T) {
	s := openTempStoreForBatch(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()

	id := uuid.New()
	row := manifest.UpscaleBatchRow{
		ID: id, Path: "Album", TargetRate: 192000, TargetBits: 24,
		Status: "running", TotalFiles: 4, CreatedAt: 1, UpdatedAt: 1,
	}
	if err := s.InsertUpscaleBatch(ctx, row); err != nil {
		t.Fatalf("InsertUpscaleBatch: %v", err)
	}

	row.Status = "cancelled"
	row.UpdatedAt = 2
	if err := s.UpdateUpscaleBatchStatus(ctx, row); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	// The stale in-flight promotion, landing after the cancel.
	row.Status = "running"
	row.UpdatedAt = 3
	if err := s.UpdateUpscaleBatchStatus(ctx, row); err != nil {
		t.Fatalf("stale promotion: %v", err)
	}

	if got := batchStatus(t, s, id); got != "cancelled" {
		t.Fatalf("status = %q, want cancelled — a stale non-terminal write "+
			"resurrected a terminal batch that nothing will ever finish", got)
	}
}
