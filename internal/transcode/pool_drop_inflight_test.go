package transcode

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestDropInflightRemovesMatchingEntries pins the predicate-based
// drop contract. Two enqueued specs, one matching the predicate
// — dropping leaves the non-matching slot intact so a re-enqueue
// of the dropped path proceeds (no dedup no-op) while the
// non-dropped path stays deduplicated.
func TestDropInflightRemovesMatchingEntries(t *testing.T) {
	store := openTempStoreForPool(t)

	p := NewPool(store, 1, 16)
	t.Cleanup(func() { stopPoolWithin(t, p, 30*time.Second) })

	// Block worker forever so jobs stay in the inflight map and
	// are observable. The cleanup's cancel releases the runner block
	// before Stop runs (cleanups run last-registered first). The stub
	// sends to no channel: once released, the single worker runs up to
	// three jobs (A, B and the re-enqueued A) before Stop lands, and a
	// send nothing reads blocks the third for good, with Stop waiting
	// on it (B924).
	runnerCtx, runnerCancel := context.WithCancel(context.Background())
	t.Cleanup(runnerCancel)
	p.fsyncFn = noopFsync
	p.runner = func(ctx context.Context, _ JobSpec) (RunResult, error) {
		<-runnerCtx.Done()
		return RunResult{SizeBytes: 0}, ctx.Err()
	}

	specA := JobSpec{
		SourceLibraryRel: "Diana Krall/Live/01.flac",
		SourceAbsPath:    "/dev/null/missing-a",
		TargetSampleRate: 176400, TargetBits: 24,
		Quality: QualityVeryHigh, OutputDir: t.TempDir(),
	}
	specB := JobSpec{
		SourceLibraryRel: "Other/Album/01.flac",
		SourceAbsPath:    "/dev/null/missing-b",
		TargetSampleRate: 176400, TargetBits: 24,
		Quality: QualityVeryHigh, OutputDir: t.TempDir(),
	}
	if err := p.Enqueue(specA); err != nil {
		t.Fatalf("Enqueue A: %v", err)
	}
	if err := p.Enqueue(specB); err != nil {
		t.Fatalf("Enqueue B: %v", err)
	}

	// Drop only specA's entry — predicate sees source path only.
	dropped := p.DropInflight(func(sourcePath string) bool {
		return strings.HasPrefix(sourcePath, "Diana Krall/")
	})
	if dropped != 1 {
		t.Fatalf("DropInflight: got %d, want 1", dropped)
	}

	// Re-enqueue specA must NOT no-op (slot was dropped).
	if err := p.Enqueue(specA); err != nil {
		t.Fatalf("re-Enqueue A after drop: %v", err)
	}
	// Re-enqueue specB MUST dedup (still in inflight) → ErrDuplicateInflight.
	if err := p.Enqueue(specB); !errors.Is(err, ErrDuplicateInflight) {
		t.Fatalf("re-Enqueue B (dedup): got %v, want ErrDuplicateInflight", err)
	}
	stats := p.Stats()
	// 3 enqueues total: original A, original B, re-A after drop.
	// Re-B was deduplicated and did NOT increment.
	if stats.Enqueued != 3 {
		t.Errorf("Stats.Enqueued: got %d, want 3", stats.Enqueued)
	}
}

// TestDropInflightPredicateReceivesSourcePathOnly pins the
// key-parsing contract — predicate must see `Music/Album/01.flac`,
// NOT `Music/Album/01.flac|upscaled-v2-176400-24`. A naive
// predicate that did a substring match on the raw key could
// silently match on variant_id segments instead.
func TestDropInflightPredicateReceivesSourcePathOnly(t *testing.T) {
	store := openTempStoreForPool(t)

	p := NewPool(store, 1, 16)
	t.Cleanup(func() { stopPoolWithin(t, p, 30*time.Second) })

	runnerCtx, runnerCancel := context.WithCancel(context.Background())
	t.Cleanup(runnerCancel)
	p.fsyncFn = noopFsync
	p.runner = func(ctx context.Context, _ JobSpec) (RunResult, error) {
		<-runnerCtx.Done()
		return RunResult{SizeBytes: 0}, ctx.Err()
	}

	spec := JobSpec{
		SourceLibraryRel: "Music/Album/track.flac",
		SourceAbsPath:    "/dev/null/missing",
		TargetSampleRate: 176400, TargetBits: 24,
		Quality: QualityVeryHigh, OutputDir: t.TempDir(),
	}
	if err := p.Enqueue(spec); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	var seen string
	var mu sync.Mutex
	p.DropInflight(func(sourcePath string) bool {
		mu.Lock()
		seen = sourcePath
		mu.Unlock()
		return false
	})
	mu.Lock()
	defer mu.Unlock()
	if seen != "Music/Album/track.flac" {
		t.Errorf("predicate received %q, want %q (key was split correctly?)", seen, "Music/Album/track.flac")
	}
	if strings.Contains(seen, "|") {
		t.Errorf("predicate saw pipe character in source path: %q", seen)
	}
}

// TestDropInflightNilPredicateNoOps pins the back-compat shape —
// passing nil returns 0 with no panic.
func TestDropInflightNilPredicateNoOps(t *testing.T) {
	store := openTempStoreForPool(t)

	p := NewPool(store, 1, 4)
	t.Cleanup(func() { stopPoolWithin(t, p, 30*time.Second) })

	dropped := p.DropInflight(nil)
	if dropped != 0 {
		t.Errorf("DropInflight(nil): got %d, want 0", dropped)
	}
}

// TestDropInflightEmptyMapReturnsZero pins behavior on an empty
// pool — nothing to drop, return 0, no false-positive predicate
// calls.
func TestDropInflightEmptyMapReturnsZero(t *testing.T) {
	store := openTempStoreForPool(t)

	p := NewPool(store, 1, 4)
	t.Cleanup(func() { stopPoolWithin(t, p, 30*time.Second) })

	called := false
	dropped := p.DropInflight(func(string) bool {
		called = true
		return true
	})
	if dropped != 0 {
		t.Errorf("DropInflight on empty: got %d, want 0", dropped)
	}
	if called {
		t.Errorf("predicate called on empty inflight map")
	}
}

// stopReserve is the time a pool's stop leaves before the test binary's
// deadline, so the cleanups after it still run (as backup's waitWithin does).
const stopReserve = 10 * time.Second

// stopPoolWithin is p.Stop with a bound of its own: limit, capped by the test
// binary's deadline less stopReserve, so a worker that never exits fails the
// test by name instead of holding it to the binary's deadline (B924).
func stopPoolWithin(t *testing.T, p *Pool, limit time.Duration) {
	t.Helper()
	bound := limit
	if deadline, ok := t.Deadline(); ok {
		if left := time.Until(deadline) - stopReserve; left < bound {
			bound = max(left, 0)
		}
	}
	stopped := make(chan struct{})
	go func() { p.Stop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(bound):
		t.Errorf("the pool did not stop within %v: a worker never exited", bound)
	}
}
