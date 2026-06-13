package analyze

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

func newStore(t *testing.T) *manifest.Store {
	t.Helper()
	s, err := manifest.OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func putTrack(t *testing.T, s *manifest.Store, path string) {
	t.Helper()
	if err := s.UpsertTrack(context.Background(), &manifest.Track{
		Path: path, Size: 100, ModTime: time.Unix(1, 0),
	}); err != nil {
		t.Fatalf("UpsertTrack(%q): %v", path, err)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met within deadline")
}

func noFsync(string) error { return nil }

func TestPoolProcessesJobAndStoresRow(t *testing.T) {
	s := newStore(t)
	putTrack(t, s, "A/B/01.flac")

	var gotSpec AnalyzeSpec
	runner := func(_ context.Context, spec AnalyzeSpec) (Result, error) {
		gotSpec = spec
		return Result{
			WaveformPath: "/w/x.waveform.bin", WaveformTag: "deadbeef",
			WaveformSize: 42, SchemaVersion: WaveformSchemaVersion,
		}, nil
	}
	p := NewPool(s, 1, 4, WithRunner(runner), WithFsync(noFsync))
	defer p.Stop()

	if err := p.Enqueue(AnalyzeSpec{
		SourceAbsPath: "/lib/A/B/01.flac", SourceLibraryRel: "A/B/01.flac",
		SourceMTimeNS: 123, SourceSize: 456,
	}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	waitFor(t, func() bool { return p.Stats().Done == 1 })

	row, err := s.GetAnalysis(context.Background(), "A/B/01.flac")
	if err != nil || row == nil {
		t.Fatalf("GetAnalysis: err=%v row=%v", err, row)
	}
	if row.WaveformTag != "deadbeef" || row.WaveformSize != 42 {
		t.Fatalf("row mismatch: %+v", *row)
	}
	if gotSpec.SourceLibraryRel != "A/B/01.flac" || gotSpec.SourceMTimeNS != 123 {
		t.Fatalf("runner spec mismatch: %+v", gotSpec)
	}
}

func TestPoolFsyncFailureCountsAndWritesNoRow(t *testing.T) {
	s := newStore(t)
	putTrack(t, s, "A/01.flac")
	runner := func(_ context.Context, _ AnalyzeSpec) (Result, error) {
		return Result{WaveformPath: "/w/x", WaveformTag: "t", SchemaVersion: "wf1"}, nil
	}
	p := NewPool(s, 1, 4, WithRunner(runner), WithFsync(func(string) error {
		return errors.New("boom")
	}))
	defer p.Stop()

	if err := p.Enqueue(AnalyzeSpec{SourceLibraryRel: "A/01.flac"}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	waitFor(t, func() bool { return p.Stats().Failed == 1 })
	if row, _ := s.GetAnalysis(context.Background(), "A/01.flac"); row != nil {
		t.Fatalf("expected no row after fsync failure, got %+v", *row)
	}
}

func TestPoolDedupAndQueueFull(t *testing.T) {
	s := newStore(t)
	for _, p := range []string{"A/01.flac", "B/02.flac", "C/03.flac"} {
		putTrack(t, s, p)
	}
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	runner := func(ctx context.Context, _ AnalyzeSpec) (Result, error) {
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
		return Result{WaveformPath: "/w/x", WaveformTag: "t", SchemaVersion: "wf1"}, nil
	}
	p := NewPool(s, 1, 1, WithRunner(runner), WithFsync(noFsync))
	defer func() { close(release); p.Stop() }()

	// First job: the single worker picks it up and blocks in runner.
	if err := p.Enqueue(AnalyzeSpec{SourceLibraryRel: "A/01.flac"}); err != nil {
		t.Fatalf("first Enqueue: %v", err)
	}
	<-started // worker now blocked; its dedup slot is held

	// Dedup: same path returns nil without taking a slot.
	if err := p.Enqueue(AnalyzeSpec{SourceLibraryRel: "A/01.flac"}); err != nil {
		t.Fatalf("dedup Enqueue should be nil: %v", err)
	}
	// Fill the cap-1 queue with a different path.
	if err := p.Enqueue(AnalyzeSpec{SourceLibraryRel: "B/02.flac"}); err != nil {
		t.Fatalf("queue-fill Enqueue: %v", err)
	}
	// Worker busy + queue full → ErrQueueFull.
	if err := p.Enqueue(AnalyzeSpec{SourceLibraryRel: "C/03.flac"}); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("want ErrQueueFull, got %v", err)
	}
}

func TestPoolEnqueueAfterStopRejected(t *testing.T) {
	s := newStore(t)
	runner := func(_ context.Context, _ AnalyzeSpec) (Result, error) {
		return Result{}, nil
	}
	p := NewPool(s, 1, 1, WithRunner(runner), WithFsync(noFsync))
	p.Stop()
	if err := p.Enqueue(AnalyzeSpec{SourceLibraryRel: "x"}); !errors.Is(err, ErrPoolClosed) {
		t.Fatalf("want ErrPoolClosed, got %v", err)
	}
}

func TestPoolTimeoutCountsAsFailureAndReclaimsSlot(t *testing.T) {
	s := newStore(t)
	putTrack(t, s, "A/slow.flac")
	putTrack(t, s, "A/quick.flac")
	hang := func(ctx context.Context, _ AnalyzeSpec) (Result, error) {
		<-ctx.Done() // never completes until the per-job timeout fires
		return Result{}, ctx.Err()
	}
	p := NewPool(s, 1, 4, WithRunner(hang), WithFsync(noFsync),
		WithJobTimeout(50*time.Millisecond))
	defer p.Stop()

	if err := p.Enqueue(AnalyzeSpec{SourceLibraryRel: "A/slow.flac"}); err != nil {
		t.Fatalf("Enqueue slow: %v", err)
	}
	waitFor(t, func() bool { return p.Stats().Failed == 1 })

	// Slot + worker reclaimed: a second job is accepted and also runs
	// (it times out too). Reaching Failed == 2 proves the worker
	// survived the first timeout and the dedup slot was freed.
	if err := p.Enqueue(AnalyzeSpec{SourceLibraryRel: "A/quick.flac"}); err != nil {
		t.Fatalf("second Enqueue after timeout should be accepted: %v", err)
	}
	waitFor(t, func() bool { return p.Stats().Failed == 2 })
}
