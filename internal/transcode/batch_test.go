package transcode

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

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
	if err := s.UpsertFolder(context.Background(), &manifest.Folder{Path: "Album"}); err != nil {
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
		if err := s.UpsertTrack(context.Background(), &manifest.Track{
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
	if err := s.UpsertVariant(context.Background(), manifest.VariantRow{
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
	p.fsyncFn = noopFsync
	p.runner = func(ctx context.Context, spec JobSpec) (int64, error) {
		return spec.SourceSize * 2, nil // arbitrary non-zero size
	}
	dataDir := t.TempDir()
	c, err := NewCoordinator(p, s, dataDir, nil, func(rel string) (string, error) { return "/tmp/abs/" + rel, nil })
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	// Capture published progress events for assertion.
	log := &eventLog{}
	c.SetPublish(log.append)
	// Wire pool callbacks the way cmd/bridge does — Coordinator
	// consumes them.
	p.SetOnJobComplete(func(path, variantID string, sampleRate, bitsPerSample int, durationSeconds float64, batchID uuid.UUID, completedAt time.Time) {
		c.OnJobComplete(path, variantID, sampleRate, bitsPerSample, durationSeconds, batchID, completedAt)
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

// seedHugeTrack plants one uncovered, hi-res track whose projected
// variant size exceeds any real volume's free space, so the disk
// pre-flight deterministically refuses. 96k/24 makes it BOTH
// upscale-eligible (below a 192/24 target) AND optimize-eligible
// (above the CarPlay floor), so the same fixture drives both
// Submit paths.
func seedHugeTrack(t *testing.T, s *manifest.Store) {
	t.Helper()
	if err := s.UpsertFolder(context.Background(), &manifest.Folder{Path: "Huge"}); err != nil {
		t.Fatal(err)
	}
	rate := float64(96000)
	bits := 24
	isDSD := false
	if err := s.UpsertTrack(context.Background(), &manifest.Track{
		Path:          "Huge/01.flac",
		Size:          1 << 60, // ~1 EiB source → projection dwarfs any real free space
		SampleRate:    &rate,
		BitsPerSample: &bits,
		Codec:         "FLAC",
		IsDSD:         &isDSD,
	}); err != nil {
		t.Fatal(err)
	}
}

// TestSubmit_RefusesOnInsufficientDiskSpace exercises the pre-flight
// with a projection no real volume can hold, and pins that the check
// grades the OUTPUT dir (the variants write target) — not the
// coordinator's dataDir. The outputDir deliberately doesn't exist
// yet: AvailableDiskSpaceNearest must fall back to its existing
// parent volume instead of erroring (the lazily-created variants
// dir cold-start case).
func TestSubmit_RefusesOnInsufficientDiskSpace(t *testing.T) {
	s := openTempStoreForBatch(t)
	t.Cleanup(func() { _ = s.Close() })
	seedHugeTrack(t, s)

	p := NewPool(s, 1, 4)
	t.Cleanup(p.Stop)
	c, err := NewCoordinator(p, s, t.TempDir(), nil, func(rel string) (string, error) { return "/tmp/abs/" + rel, nil })
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	outDir := filepath.Join(t.TempDir(), "variants", "not-created-yet")
	_, err = c.Submit(context.Background(), "Huge", 192000, 24, outDir)
	var dskErr *InsufficientDiskSpaceError
	if !errors.As(err, &dskErr) {
		t.Fatalf("Submit: want *InsufficientDiskSpaceError, got %v", err)
	}
	if dskErr.Dir != outDir {
		t.Errorf("disk check graded %q, want the outputDir %q", dskErr.Dir, outDir)
	}
}

// TestSubmitOptimize_DiskCheckTargetsOutputDir is the optimize twin:
// the pre-flight for CarPlay-optimize batches must also grade the
// per-call outputDir.
func TestSubmitOptimize_DiskCheckTargetsOutputDir(t *testing.T) {
	s := openTempStoreForBatch(t)
	t.Cleanup(func() { _ = s.Close() })
	seedHugeTrack(t, s)

	p := NewPool(s, 1, 4)
	t.Cleanup(p.Stop)
	c, err := NewCoordinator(p, s, t.TempDir(), nil, func(rel string) (string, error) { return "/tmp/abs/" + rel, nil })
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	outDir := filepath.Join(t.TempDir(), "variants", "not-created-yet")
	_, err = c.SubmitOptimize(context.Background(), "Huge", outDir)
	var dskErr *InsufficientDiskSpaceError
	if !errors.As(err, &dskErr) {
		t.Fatalf("SubmitOptimize: want *InsufficientDiskSpaceError, got %v", err)
	}
	if dskErr.Dir != outDir {
		t.Errorf("disk check graded %q, want the outputDir %q", dskErr.Dir, outDir)
	}
}

// TestSubmit_DiskCheckFallsBackToDataDir pins the outputDir==""
// fallback: legacy/test callers that don't pass a write target get
// the coordinator's dataDir graded, preserving pre-fix behaviour.
func TestSubmit_DiskCheckFallsBackToDataDir(t *testing.T) {
	s := openTempStoreForBatch(t)
	t.Cleanup(func() { _ = s.Close() })
	seedHugeTrack(t, s)

	p := NewPool(s, 1, 4)
	t.Cleanup(p.Stop)
	dataDir := t.TempDir()
	c, err := NewCoordinator(p, s, dataDir, nil, func(rel string) (string, error) { return "/tmp/abs/" + rel, nil })
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	_, err = c.Submit(context.Background(), "Huge", 192000, 24, "")
	var dskErr *InsufficientDiskSpaceError
	if !errors.As(err, &dskErr) {
		t.Fatalf("Submit: want *InsufficientDiskSpaceError, got %v", err)
	}
	if dskErr.Dir != dataDir {
		t.Errorf("disk check graded %q, want the dataDir fallback %q", dskErr.Dir, dataDir)
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
	rows, err := s.ListUpscaleBatches(context.Background(), 10)
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
	rows, err := s.ListUpscaleBatches(context.Background(), 10)
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
	if err := s.InsertUpscaleBatch(context.Background(), manifest.UpscaleBatchRow{
		ID: id, Path: "Album", TargetRate: 192000, TargetBits: 24,
		Status: "running", CreatedAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	p := NewPool(s, 1, 4)
	t.Cleanup(p.Stop)
	c, err := NewCoordinator(p, s, t.TempDir(), nil, func(rel string) (string, error) { return "/tmp/abs/" + rel, nil })
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	_ = c

	rows, err := s.ListUpscaleBatches(context.Background(), 10)
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
	c, err := NewCoordinator(p, s, t.TempDir(), nil, func(rel string) (string, error) { return "/tmp/abs/" + rel, nil })
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
	emptySpec := JobSpec{}
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
		got := redactSoxErr(c.raw, emptySpec)
		if got != c.want {
			t.Errorf("redactSoxErr(%q) = %q, want %q", c.raw, got, c.want)
		}
	}
	// Cap test.
	long := strings.Repeat("a", 5000)
	got := redactSoxErr(long, emptySpec)
	if !strings.HasSuffix(got, "…(truncated)") {
		t.Errorf("long input not truncated: ends with %q", got[len(got)-30:])
	}
	if len(got) > 4096+len("…(truncated)") {
		t.Errorf("truncated length = %d, expected ~4100", len(got))
	}
}

// TestRedactSoxErr_TruncatesAtUTF8Boundary pins the rune-boundary
// trim on the 4 KiB cap: a multi-byte rune straddling the cut must be
// dropped entirely, never persisted as a half-encoded sequence (same
// invariant auth.RecordClientVersion carries, PR #75).
func TestRedactSoxErr_TruncatesAtUTF8Boundary(t *testing.T) {
	// 4095 ASCII bytes + a 3-byte rune whose bytes occupy 4095..4097 —
	// the cut at 4096 lands mid-rune.
	in := strings.Repeat("a", 4095) + "世" + strings.Repeat("b", 100)
	got := redactSoxErr(in, JobSpec{})
	if !utf8.ValidString(got) {
		t.Errorf("truncated output is not valid UTF-8: %q", got[4080:])
	}
	if !strings.HasSuffix(got, "…(truncated)") {
		t.Errorf("long input not marked truncated: %q", got[len(got)-30:])
	}
	if want := strings.Repeat("a", 4095) + "…(truncated)"; got != want {
		t.Errorf("expected the straddling rune dropped to the boundary; got len %d", len(got))
	}
}

// TestRedactSoxErr_ScrubsAbsolutePath locks the path-redaction
// contract: when the JobSpec carries SourceAbsPath, every occurrence
// of that absolute path in stderr is replaced with the library-
// relative form. CodeRabbit security-medium on PR #201 — the
// docstring promised this and the implementation only trimmed
// prefixes.
func TestRedactSoxErr_ScrubsAbsolutePath(t *testing.T) {
	spec := JobSpec{
		SourceAbsPath:    "/Users/alice/Music/Album/01.flac",
		SourceLibraryRel: "Album/01.flac",
		OutputDir:        "/var/lib/bridge/transcoded",
	}
	in := "sox FAIL formats: can't open input file '/Users/alice/Music/Album/01.flac': Not a directory"
	got := redactSoxErr(in, spec)
	if strings.Contains(got, "/Users/alice/Music") {
		t.Errorf("redactSoxErr leaked absolute path: %q", got)
	}
	if !strings.Contains(got, "Album/01.flac") {
		t.Errorf("redactSoxErr dropped library-relative form: %q", got)
	}
}

// TestRedactSoxErr_ScrubsOutputDir locks the OutputDir scrub pass.
func TestRedactSoxErr_ScrubsOutputDir(t *testing.T) {
	spec := JobSpec{OutputDir: "/var/lib/bridge/transcoded"}
	in := "sox FAIL formats: write failed on /var/lib/bridge/transcoded/abc123-upscaled-v2-192000-24.flac.tmp"
	got := redactSoxErr(in, spec)
	if strings.Contains(got, "/var/lib/bridge/transcoded/") {
		t.Errorf("redactSoxErr leaked OutputDir prefix: %q", got)
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

// TestRedactSoxErr_InteriorGarbagePreservesMessage pins the O(1)
// trim posture: an invalid byte EARLY in an over-limit string must not
// discard the rest of the message (a validate-the-whole-string loop
// would trim everything after the first bad byte — Gemini HIGH on
// PR #375).
func TestRedactSoxErr_InteriorGarbagePreservesMessage(t *testing.T) {
	in := "sox FAIL formats: \xff " + strings.Repeat("x", 5000)
	got := redactSoxErr(in, JobSpec{})
	if len(got) < 4000 {
		t.Fatalf("interior invalid byte collapsed the message: len=%d", len(got))
	}
	if !strings.HasSuffix(got, "…(truncated)") {
		t.Errorf("missing truncation marker: %q", got[len(got)-30:])
	}
}
