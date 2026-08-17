package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	bridgefs "github.com/acoseac/1-bit-bridge/internal/fs"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/transcode"
)

// recordingEnqueuer stands in for transcode.Pool.Enqueue and records what
// the sweep submitted.
//
// MUTEX-GUARDED, not a bare slice: runAutoOptimizeSweeper calls enqueue on
// its own goroutine while the test asserts from the test goroutine, so an
// unguarded slice is a genuine data race the -race suite fails on (the
// same lesson as the atomic test counters in internal/enrich). Production
// is unaffected — Pool.Enqueue takes p.mu — but a racy fixture makes the
// whole package's race output untrustworthy.
type recordingEnqueuer struct {
	mu    sync.Mutex
	specs []transcode.JobSpec
	err   error
}

func (r *recordingEnqueuer) enqueue(spec transcode.JobSpec) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return r.err
	}
	r.specs = append(r.specs, spec)
	return nil
}

// failWith makes every subsequent enqueue return err.
func (r *recordingEnqueuer) failWith(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.err = err
}

// snapshot returns a copy so callers can index it without holding the lock.
func (r *recordingEnqueuer) snapshot() []transcode.JobSpec {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]transcode.JobSpec(nil), r.specs...)
}

func (r *recordingEnqueuer) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.specs)
}

// autoOptimizeFixture wires a real store + resolver over real temp files
// with a recording enqueue func, so the sweep's decisions are observed
// through what it actually submits.
type autoOptimizeFixture struct {
	sweeper   *autoOptimizeSweeper
	store     *manifest.Store
	libDir    string
	submitted *recordingEnqueuer
}

func newAutoOptimizeFixture(t *testing.T) *autoOptimizeFixture {
	t.Helper()
	dir := t.TempDir()
	libDir := filepath.Join(dir, "library")
	if err := os.MkdirAll(libDir, 0o700); err != nil {
		t.Fatalf("mkdir library: %v", err)
	}
	store, err := manifest.OpenStore(filepath.Join(dir, "bridge.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	rec := &recordingEnqueuer{}
	f := &autoOptimizeFixture{store: store, libDir: libDir, submitted: rec}
	f.sweeper = &autoOptimizeSweeper{
		store:        store,
		resolver:     bridgefs.New([]string{libDir}),
		enqueue:      rec.enqueue,
		enabled:      func() bool { return true },
		outputDir:    func() string { return filepath.Join(dir, "variants") },
		maxPerSweep:  func() int { return 100 },
		minFreeBytes: func() int64 { return 1 << 30 },
		// Effectively unlimited unless a test narrows it.
		diskFree: func(string) (int64, error) { return 1 << 50, nil },
	}
	return f
}

// seedTrack writes a real file (so ResolveChecked succeeds) and the
// matching hi-res track row.
func (f *autoOptimizeFixture) seedTrack(t *testing.T, rel string, sizeBytes int) {
	t.Helper()
	abs := filepath.Join(f.libDir, rel)
	if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
		t.Fatalf("mkdir for %q: %v", rel, err)
	}
	if err := os.WriteFile(abs, make([]byte, sizeBytes), 0o600); err != nil {
		t.Fatalf("write %q: %v", rel, err)
	}
	rate, bits, dsd := 96000.0, 24, false
	if err := f.store.UpsertTrack(context.Background(), &manifest.Track{
		Path:          rel,
		Size:          int64(sizeBytes),
		ModTime:       time.Unix(1700000000, 0),
		SampleRate:    &rate,
		BitsPerSample: &bits,
		Codec:         "FLAC",
		IsDSD:         &dsd,
	}); err != nil {
		t.Fatalf("UpsertTrack(%q): %v", rel, err)
	}
}

// TestAutoOptimizeSweepEnqueuesBackgroundJobs is the core contract: an
// eligible track produces exactly one job, and that job carries
// Background=true.
//
// The Background assertion is the load-bearing half. Without it the
// sweep's jobs land on the Pool's FOREGROUND lane and head-of-line block
// the on-demand CarPlay request the two-channel queue exists to protect
// — a regression with no visible symptom on this side of the wire.
func TestAutoOptimizeSweepEnqueuesBackgroundJobs(t *testing.T) {
	f := newAutoOptimizeFixture(t)
	f.seedTrack(t, "Artist/Album/01.flac", 4096)

	counts := f.sweeper.sweepOnce(context.Background())
	if counts == nil {
		t.Fatal("sweepOnce returned nil (treated as failure)")
	}
	if counts.Enqueued != 1 {
		t.Fatalf("Enqueued = %d, want 1 (counts=%+v)", counts.Enqueued, counts)
	}
	if f.submitted.count() != 1 {
		t.Fatalf("submitted %d jobs, want 1", f.submitted.count())
	}
	job := f.submitted.snapshot()[0]
	if !job.Background {
		t.Error("JobSpec.Background = false; a swept job MUST take the low-priority lane " +
			"or it head-of-line blocks the on-demand CarPlay path")
	}
	if job.Kind != transcode.JobKindOptimize {
		t.Errorf("JobSpec.Kind = %q, want %q (drives the optimized-* variant id)",
			job.Kind, transcode.JobKindOptimize)
	}
	if job.TargetBits != 16 {
		t.Errorf("TargetBits = %d, want 16 (the CarPlay floor)", job.TargetBits)
	}
	// 96 kHz is 48-family, so the family-preserving target is 48 kHz.
	if job.TargetSampleRate != 48000 {
		t.Errorf("TargetSampleRate = %d, want 48000 (family-preserving from 96 kHz)",
			job.TargetSampleRate)
	}
	if job.SourceAbsPath == "" || !filepath.IsAbs(job.SourceAbsPath) {
		t.Errorf("SourceAbsPath = %q, want an absolute resolved path", job.SourceAbsPath)
	}
	// The variant must record the TRACK ROW's mtime/size, not a live stat —
	// otherwise the staleness predicate re-selects it every sweep.
	if job.SourceSize != 4096 {
		t.Errorf("SourceSize = %d, want the track row's 4096", job.SourceSize)
	}
	if counts.Remaining != 0 {
		// The candidate is enqueued but its variant row doesn't exist yet,
		// so the backlog legitimately still counts it. Assert the honest
		// value rather than 0.
		if counts.Remaining != 1 {
			t.Errorf("Remaining = %d, want 1 (the in-flight candidate)", counts.Remaining)
		}
	}
}

// TestAutoOptimizeSweepDisabledDoesNothing pins the hot-apply off state:
// a disabled sweep enqueues nothing and reports Disabled rather than
// returning nil (nil means "failed, keep the old numbers", which would
// leave the admin card frozen on a stale successful run).
func TestAutoOptimizeSweepDisabledDoesNothing(t *testing.T) {
	f := newAutoOptimizeFixture(t)
	f.seedTrack(t, "Artist/Album/01.flac", 4096)
	f.sweeper.enabled = func() bool { return false }

	counts := f.sweeper.sweepOnce(context.Background())
	if counts == nil {
		t.Fatal("disabled sweep returned nil; want a Disabled-marked counts struct")
	}
	if !counts.Disabled {
		t.Error("counts.Disabled = false, want true")
	}
	if counts.Enqueued != 0 || f.submitted.count() != 0 {
		t.Errorf("disabled sweep enqueued %d jobs (counts.Enqueued=%d), want 0",
			f.submitted.count(), counts.Enqueued)
	}
}

// TestAutoOptimizeSweepStopsAtDiskFloor pins the running budget. The
// on-demand path's per-batch preflight is a point check; a sweeper that
// runs forever needs a cumulative one, or it fills the volume with work
// nobody asked for.
//
// Fixture: free space just above the floor, so the FIRST job fits and the
// second does not.
func TestAutoOptimizeSweepStopsAtDiskFloor(t *testing.T) {
	f := newAutoOptimizeFixture(t)
	// Three sizable candidates. 96/24 → 48/16 projects to roughly a third
	// of the source, so 30 MB sources project to ~10 MB each.
	const srcSize = 30 << 20
	for _, rel := range []string{"A/Album/01.flac", "A/Album/02.flac", "A/Album/03.flac"} {
		f.seedTrack(t, rel, srcSize)
	}
	const floor = 100 << 20
	f.sweeper.minFreeBytes = func() int64 { return floor }
	// Headroom for ~1.5 projected variants above the floor.
	projected := transcode.ProjectedSize(srcSize, 96000, 24, 48000, 16,
		transcode.DefaultCompressionFactor(16))
	if projected <= 0 {
		t.Fatalf("fixture projects %d bytes; expected a positive projection", projected)
	}
	f.sweeper.diskFree = func(string) (int64, error) { return floor + projected + projected/2, nil }

	counts := f.sweeper.sweepOnce(context.Background())
	if counts == nil {
		t.Fatal("sweepOnce returned nil")
	}
	if !counts.DiskFloorReached {
		t.Error("DiskFloorReached = false, want true")
	}
	if counts.Enqueued != 1 {
		t.Errorf("Enqueued = %d, want 1 (only the first projection fits above the floor)", counts.Enqueued)
	}
	if counts.ProjectedBytes != projected {
		t.Errorf("ProjectedBytes = %d, want %d (one variant)", counts.ProjectedBytes, projected)
	}
}

// TestAutoOptimizeSweepFailsClosedOnDiskProbeError: with no free-space
// reading there is no way to honour the floor, so the sweep must skip
// rather than guess. Returning nil keeps the previous counts on the card.
func TestAutoOptimizeSweepFailsClosedOnDiskProbeError(t *testing.T) {
	f := newAutoOptimizeFixture(t)
	f.seedTrack(t, "Artist/Album/01.flac", 4096)
	f.sweeper.diskFree = func(string) (int64, error) { return 0, errors.New("statfs boom") }

	if counts := f.sweeper.sweepOnce(context.Background()); counts != nil {
		t.Errorf("sweepOnce = %+v, want nil (fail closed on an unreadable volume)", counts)
	}
	if f.submitted.count() != 0 {
		t.Errorf("enqueued %d jobs despite an unreadable volume, want 0", f.submitted.count())
	}
}

// TestAutoOptimizeSweepHonoursMaxPerSweep pins the drip cap. It is not
// only a queue guard: every generated variant strict-advances its track's
// indexed_at, so an uncapped first sweep would push one delta row per
// variant to every paired device at once.
func TestAutoOptimizeSweepHonoursMaxPerSweep(t *testing.T) {
	f := newAutoOptimizeFixture(t)
	for _, rel := range []string{"A/Al/01.flac", "A/Al/02.flac", "A/Al/03.flac", "A/Al/04.flac"} {
		f.seedTrack(t, rel, 4096)
	}
	f.sweeper.maxPerSweep = func() int { return 2 }

	counts := f.sweeper.sweepOnce(context.Background())
	if counts == nil {
		t.Fatal("sweepOnce returned nil")
	}
	if counts.Enqueued != 2 {
		t.Errorf("Enqueued = %d, want 2 (the per-sweep cap)", counts.Enqueued)
	}
	if f.submitted.count() != 2 {
		t.Errorf("submitted %d jobs, want 2", f.submitted.count())
	}
	// The uncapped backlog must still report the full set, so the operator
	// sees a draining queue rather than a permanently-tiny one.
	if counts.Remaining != 4 {
		t.Errorf("Remaining = %d, want 4 (the cap bounds the work, not the count)", counts.Remaining)
	}
}

// TestAutoOptimizeSweepSkipsUnresolvable: a row whose file is gone (the
// scanner's missing_count debounce hasn't reaped it yet, or the mount
// dropped) is counted and skipped, and does NOT abort the sweep — the
// remaining candidates still get their turn.
func TestAutoOptimizeSweepSkipsUnresolvable(t *testing.T) {
	f := newAutoOptimizeFixture(t)
	f.seedTrack(t, "A/Al/present.flac", 4096)
	f.seedTrack(t, "A/Al/vanished.flac", 4096)
	if err := os.Remove(filepath.Join(f.libDir, "A/Al/vanished.flac")); err != nil {
		t.Fatalf("remove fixture file: %v", err)
	}

	counts := f.sweeper.sweepOnce(context.Background())
	if counts == nil {
		t.Fatal("sweepOnce returned nil")
	}
	if counts.Unresolvable != 1 {
		t.Errorf("Unresolvable = %d, want 1", counts.Unresolvable)
	}
	if counts.Enqueued != 1 {
		t.Errorf("Enqueued = %d, want 1 (the surviving track must still be swept)", counts.Enqueued)
	}
	if f.submitted.count() != 1 || f.submitted.snapshot()[0].SourceLibraryRel != "A/Al/present.flac" {
		t.Errorf("submitted = %+v, want only the present track", f.submitted.snapshot())
	}
}

// TestAutoOptimizeSweepQueueFullStopsWithoutFailure: a saturated pool is
// a normal condition (an operator batch is running), so the sweep records
// it and defers the rest instead of logging failures per candidate.
func TestAutoOptimizeSweepQueueFullStopsWithoutFailure(t *testing.T) {
	f := newAutoOptimizeFixture(t)
	for _, rel := range []string{"A/Al/01.flac", "A/Al/02.flac", "A/Al/03.flac"} {
		f.seedTrack(t, rel, 4096)
	}
	f.submitted.failWith(transcode.ErrQueueFull)

	counts := f.sweeper.sweepOnce(context.Background())
	if counts == nil {
		t.Fatal("sweepOnce returned nil")
	}
	if !counts.QueueSaturated {
		t.Error("QueueSaturated = false, want true")
	}
	if counts.Enqueued != 0 {
		t.Errorf("Enqueued = %d, want 0", counts.Enqueued)
	}
}

// TestAutoOptimizeSweepCountsDedupAsNotAFailure: an on-demand request for
// the same track already in flight is a dedup, not an error — the sweep
// keeps going and tallies it separately.
func TestAutoOptimizeSweepCountsDedupAsNotAFailure(t *testing.T) {
	f := newAutoOptimizeFixture(t)
	f.seedTrack(t, "A/Al/01.flac", 4096)
	f.submitted.failWith(transcode.ErrDuplicateInflight)

	counts := f.sweeper.sweepOnce(context.Background())
	if counts == nil {
		t.Fatal("sweepOnce returned nil")
	}
	if counts.AlreadyInflight != 1 {
		t.Errorf("AlreadyInflight = %d, want 1", counts.AlreadyInflight)
	}
	if counts.QueueSaturated {
		t.Error("QueueSaturated = true; a dedup must not read as a saturated queue")
	}
}

// TestAutoOptimizeSweepCancelledContextReturnsNil: shutdown must not be
// recorded as a successful sweep with partial numbers.
func TestAutoOptimizeSweepCancelledContextReturnsNil(t *testing.T) {
	f := newAutoOptimizeFixture(t)
	f.seedTrack(t, "A/Al/01.flac", 4096)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if counts := f.sweeper.sweepOnce(ctx); counts != nil {
		t.Errorf("sweepOnce on a cancelled ctx = %+v, want nil", counts)
	}
}

// TestRunAutoOptimizeSweeperSweepsOnNudge pins the loop's nudge path —
// the mechanism that makes "a new track was just scanned" turn into
// pre-generation without waiting out the periodic tick.
func TestRunAutoOptimizeSweeperSweepsOnNudge(t *testing.T) {
	f := newAutoOptimizeFixture(t)
	f.seedTrack(t, "A/Al/01.flac", 4096)

	prev := autoOptimizeSettleDelay
	autoOptimizeSettleDelay = time.Millisecond
	t.Cleanup(func() { autoOptimizeSettleDelay = prev })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	nudge := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		// interval 0 → nudge-only, so the loop parks on the nudge instead
		// of racing a ticker.
		runAutoOptimizeSweeper(ctx, f.sweeper, 0, nudge, nil)
	}()

	// The post-settle sweep already covers the seeded track; wait for it,
	// then seed another and nudge.
	deadline := time.Now().Add(3 * time.Second)
	for f.submitted.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if f.submitted.count() == 0 {
		t.Fatal("initial post-settle sweep enqueued nothing")
	}

	f.seedTrack(t, "A/Al/02.flac", 4096)
	nudge <- struct{}{}
	deadline = time.Now().Add(3 * time.Second)
	for f.submitted.count() < 2 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if f.submitted.count() < 2 {
		t.Fatalf("nudge did not trigger a sweep; submitted %d jobs", f.submitted.count())
	}

	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("runAutoOptimizeSweeper did not return on ctx cancel")
	}
}
