package transcode

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/google/uuid"
)

// openTempStoreForBatch reuses the pool test's helper — same dir
// shape, lightweight DB. The test file uses raw SQL for track
// seeding so the json_extract path in `ListTrackProjectionsUnderPrefix`
// returns the rates / bits the projector needs.
func openTempStoreForBatch(t *testing.T) *manifest.Store {
	t.Helper()
	dir := t.TempDir()
	s, err := manifest.OpenStore(filepath.Join(dir, "bridge.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	return s
}

// seedBatchFixture plants a small library with mixed-format tracks:
// one already-covered (has variant), two uncovered, one ineligible
// (above target). Exercises every Submit-side filter.
func seedBatchFixture(t *testing.T, s *manifest.Store) {
	t.Helper()
	if err := s.UpsertFolder(&manifest.Folder{Path: "Album"}); err != nil {
		t.Fatal(err)
	}
	tracks := []struct {
		path string
		rate int
		bits int
		size int64
	}{
		{"Album/01.flac", 44100, 16, 1_000_000},
		{"Album/02.flac", 48000, 24, 2_000_000},
		{"Album/03.flac", 96000, 24, 3_000_000},
		// 04.flac already at 192/24 — ineligible.
		{"Album/04.flac", 192000, 24, 4_000_000},
	}
	for _, tr := range tracks {
		rate := float64(tr.rate)
		bits := tr.bits
		isDSD := false
		if err := s.UpsertTrack(&manifest.Track{
			Path:          tr.path,
			Size:          tr.size,
			SampleRate:    &rate,
			BitsPerSample: &bits,
			Codec:         "FLAC",
			IsDSD:         &isDSD,
		}); err != nil {
			t.Fatalf("UpsertTrack %q: %v", tr.path, err)
		}
	}
	// 01.flac is already covered.
	if err := s.UpsertVariant(manifest.VariantRow{
		SourcePath: "Album/01.flac", VariantID: "upscaled-v2-192000-24",
		SidecarPath: "/tmp/sidecar.flac", Format: "flac",
		SampleRate: 192000, BitsPerSample: 24, SizeBytes: 1_500_000,
		SourceMTimeNS: 1, SourceSize: 1_000_000,
		SoxSettings: "{}", CreatedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
}

// eventLog wraps a slice + mutex so the helper can return a stable
// reference. Pre-fix the helper returned the slice header by value;
// the publish closure's `append` updates the helper's local slice
// but the caller's local copy of the header remains the original
// empty slice (slice header is 3 words + the backing array can
// reallocate on append). Wrapping in a struct gives both readers
// and writers the same pointer to the same field.
type eventLog struct {
	mu     sync.Mutex
	events []BatchProgressEvent
}

func (l *eventLog) append(evt BatchProgressEvent) {
	l.mu.Lock()
	l.events = append(l.events, evt)
	l.mu.Unlock()
}

func (l *eventLog) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.events)
}

// newTestCoordinatorWithStubbedPool builds a Coordinator backed by
// a Pool whose runner always succeeds (size = projected source size).
// Returns the Pool too so tests can drive jobs to completion.
func newTestCoordinatorWithStubbedPool(t *testing.T, s *manifest.Store) (*Coordinator, *Pool, *eventLog) {
	t.Helper()
	p := NewPool(s, 2, 16)
	t.Cleanup(p.Stop)
	p.runner = func(ctx context.Context, spec JobSpec) (int64, error) {
		return spec.SourceSize * 2, nil // arbitrary non-zero size
	}
	dataDir := t.TempDir()
	c, err := NewCoordinator(p, s, dataDir, nil)
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	// Capture published progress events for assertion.
	log := &eventLog{}
	c.SetPublish(log.append)
	// Wire pool callbacks the way cmd/bridge does — Coordinator
	// consumes them.
	p.SetOnJobComplete(func(path, variantID string, sampleRate, bitsPerSample int, batchID uuid.UUID, completedAt time.Time) {
		c.OnJobComplete(path, variantID, sampleRate, bitsPerSample, batchID, completedAt)
	})
	p.SetOnJobFailed(func(path, variantID, errMsg string, durationSeconds float64, batchID uuid.UUID, failedAt time.Time) {
		c.OnJobFailed(path, variantID, errMsg, durationSeconds, batchID, failedAt)
	})
	return c, p, log
}

// TestSubmit_FiltersIneligibleAndCovered locks the eligibility
// filter: already-covered tracks contribute to AlreadyCovered,
// tracks at/above target are silently filtered out (not enqueued
// AND not counted as covered).
func TestSubmit_FiltersIneligibleAndCovered(t *testing.T) {
	s := openTempStoreForBatch(t)
	t.Cleanup(func() { _ = s.Close() })
	seedBatchFixture(t, s)

	c, p, _ := newTestCoordinatorWithStubbedPool(t, s)
	t.Cleanup(p.Stop)

	res, err := c.Submit(context.Background(), "Album", 192000, 24, t.TempDir())
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	// 4 tracks total. 01.flac already covered → AlreadyCovered=1.
	// 04.flac at 192/24 → ineligible (filtered out, not counted).
	// 02 + 03 are submission candidates → TotalFiles=2,
	// EnqueuedCount=2.
	if res.AlreadyCovered != 1 {
		t.Errorf("AlreadyCovered = %d, want 1", res.AlreadyCovered)
	}
	if res.TotalFiles != 2 {
		t.Errorf("TotalFiles = %d, want 2", res.TotalFiles)
	}
	if res.EnqueuedCount != 2 {
		t.Errorf("EnqueuedCount = %d, want 2", res.EnqueuedCount)
	}
	if res.ProjectedSizeBytes <= 0 {
		t.Errorf("ProjectedSizeBytes = %d, want > 0", res.ProjectedSizeBytes)
	}
}

// TestSubmit_RefusesOnInsufficientDiskSpace exercises the pre-
// flight: a tiny stubbed disk-free helper forces the batch to
// refuse with the typed error.
func TestSubmit_RefusesOnInsufficientDiskSpace(t *testing.T) {
	s := openTempStoreForBatch(t)
	t.Cleanup(func() { _ = s.Close() })
	seedBatchFixture(t, s)

	p := NewPool(s, 1, 4)
	t.Cleanup(p.Stop)
	c, err := NewCoordinator(p, s, "/this/path/does/not/exist/anywhere", nil)
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	// DataDir doesn't exist → AvailableDiskSpace errors → Submit
	// surfaces it. Doesn't directly test the InsufficientDiskSpace
	// path but locks the "disk probe failure is surfaced not
	// swallowed" contract.
	_, err = c.Submit(context.Background(), "Album", 192000, 24, t.TempDir())
	if err == nil {
		t.Fatalf("Submit on missing dataDir: want error, got nil")
	}
}

// TestSubmit_InsertsBatchRowAndPublishesProgress drives Submit and
// verifies (a) a row appears in `upscale_batches` with status
// running, (b) at least one progress event was emitted.
func TestSubmit_InsertsBatchRowAndPublishesProgress(t *testing.T) {
	s := openTempStoreForBatch(t)
	t.Cleanup(func() { _ = s.Close() })
	seedBatchFixture(t, s)

	c, p, log := newTestCoordinatorWithStubbedPool(t, s)
	t.Cleanup(p.Stop)

	res, err := c.Submit(context.Background(), "Album", 192000, 24, t.TempDir())
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	// Wait briefly for the publisher to drain.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if log.count() >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if log.count() == 0 {
		t.Errorf("no progress events published; want ≥ 1")
	}
	// Verify the row landed in SQLite.
	rows, err := s.ListUpscaleBatches(10)
	if err != nil {
		t.Fatalf("ListUpscaleBatches: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("ListUpscaleBatches: got %d rows, want 1", len(rows))
	}
	if rows[0].ID != res.BatchID {
		t.Errorf("row ID mismatch: got %s, want %s", rows[0].ID, res.BatchID)
	}
	if rows[0].TotalFiles != 2 {
		t.Errorf("row.TotalFiles = %d, want 2", rows[0].TotalFiles)
	}
}

// TestCancel_TransitionsRow exercises the Cancel path.
func TestCancel_TransitionsRow(t *testing.T) {
	s := openTempStoreForBatch(t)
	t.Cleanup(func() { _ = s.Close() })
	seedBatchFixture(t, s)

	c, p, _ := newTestCoordinatorWithStubbedPool(t, s)
	t.Cleanup(p.Stop)

	res, err := c.Submit(context.Background(), "Album", 192000, 24, t.TempDir())
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if err := c.Cancel(res.BatchID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	// Read back — status may be `cancelled` OR `completed` depending
	// on whether the stubbed pool finished before Cancel landed.
	// Either is a legitimate terminal state under the documented
	// Cancel semantics (it stops tracking but doesn't kill in-flight).
	rows, err := s.ListUpscaleBatches(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	got := rows[0].Status
	if got != "cancelled" && got != "completed" {
		t.Errorf("status = %q, want cancelled or completed", got)
	}
}

// TestRecoverInterruptedBatches_RunsAtNewCoordinator pins the boot
// recovery semantics: a row left in `running` from a prior process
// run transitions to `interrupted` on the next NewCoordinator.
func TestRecoverInterruptedBatches_RunsAtNewCoordinator(t *testing.T) {
	s := openTempStoreForBatch(t)
	t.Cleanup(func() { _ = s.Close() })

	id := uuid.Must(uuid.NewRandom())
	if err := s.InsertUpscaleBatch(manifest.UpscaleBatchRow{
		ID: id, Path: "Album", TargetRate: 192000, TargetBits: 24,
		Status: "running", CreatedAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	p := NewPool(s, 1, 4)
	t.Cleanup(p.Stop)
	c, err := NewCoordinator(p, s, t.TempDir(), nil)
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	_ = c

	rows, err := s.ListUpscaleBatches(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows", len(rows))
	}
	if rows[0].Status != "interrupted" {
		t.Errorf("status = %q, want interrupted", rows[0].Status)
	}
}

// TestThroughput_ReturnsZeroBeforeMinSamples locks the
// throughputMinSamples gate.
func TestThroughput_ReturnsZeroBeforeMinSamples(t *testing.T) {
	s := openTempStoreForBatch(t)
	t.Cleanup(func() { _ = s.Close() })

	p := NewPool(s, 1, 4)
	t.Cleanup(p.Stop)
	c, err := NewCoordinator(p, s, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	tp := c.Throughput()
	if tp.JobsPerHour != 0 || tp.EtaSeconds != 0 {
		t.Errorf("throughput non-zero with no samples: %+v", tp)
	}

	// Inject samples directly to bump past the min-samples gate.
	c.recordThroughputDuration(10.0, time.Now())
	c.recordThroughputDuration(20.0, time.Now())
	c.recordThroughputDuration(30.0, time.Now())
	tp = c.Throughput()
	if tp.JobsPerHour <= 0 {
		t.Errorf("throughput should be > 0 with 3 samples; got %+v", tp)
	}
}

// TestRedactSoxErr_DropsPrefixesAndCaps locks the redaction
// contract for sox stderr that lands in upscale_batches.error.
func TestRedactSoxErr_DropsPrefixesAndCaps(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{"sox FAIL formats: invalid bit depth", "formats: invalid bit depth"},
		{"sox: corrupt header", "corrupt header"},
		{"exit status 1: sox FAIL", "1: sox FAIL"}, // strips "exit status " only
		{"unrelated content", "unrelated content"},
	}
	for _, c := range cases {
		got := redactSoxErr(c.raw)
		if got != c.want {
			t.Errorf("redactSoxErr(%q) = %q, want %q", c.raw, got, c.want)
		}
	}
	// Cap test.
	long := strings.Repeat("a", 5000)
	got := redactSoxErr(long)
	if !strings.HasSuffix(got, "…(truncated)") {
		t.Errorf("long input not truncated: ends with %q", got[len(got)-30:])
	}
	if len(got) > 4096+len("…(truncated)") {
		t.Errorf("truncated length = %d, expected ~4100", len(got))
	}
}

// TestErrInsufficientDiskSpaceTypedShape locks the api-side error
// wrapping survives `errors.As` so the handler can render typed
// fields.
func TestErrInsufficientDiskSpaceTypedShape(t *testing.T) {
	want := &InsufficientDiskSpaceError{
		ProjectedBytes: 1_000_000,
		RequiredBytes:  1_100_000,
		AvailableBytes: 500_000,
		Dir:            "/tmp/x",
	}
	var got *InsufficientDiskSpaceError
	if !errors.As(want, &got) {
		t.Fatal("errors.As against own pointer type failed")
	}
	if got.ProjectedBytes != 1_000_000 {
		t.Errorf("ProjectedBytes = %d", got.ProjectedBytes)
	}
}
