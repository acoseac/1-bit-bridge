package transcode

import (
	"context"
	"strings"
	"sync"
	"testing"
)

// TestDropInflightRemovesMatchingEntries pins the predicate-based
// drop contract. Two enqueued specs, one matching the predicate
// — dropping leaves the non-matching slot intact so a re-enqueue
// of the dropped path proceeds (no dedup no-op) while the
// non-dropped path stays deduplicated.
func TestDropInflightRemovesMatchingEntries(t *testing.T) {
	store := openTempStoreForPool(t)
	t.Cleanup(func() { _ = store.Close() })

	p := NewPool(store, 1, 16)
	t.Cleanup(p.Stop)

	// Block worker forever so jobs stay in the inflight map and
	// are observable. defer cancel() ensures we release the
	// runner block on test cleanup.
	runnerStarted := make(chan struct{}, 2)
	runnerCtx, runnerCancel := context.WithCancel(context.Background())
	t.Cleanup(runnerCancel)
	p.fsyncFn = noopFsync
	p.runner = func(ctx context.Context, _ JobSpec) (int64, error) {
		runnerStarted <- struct{}{}
		<-runnerCtx.Done()
		return 0, ctx.Err()
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
	// Re-enqueue specB MUST no-op (still in inflight).
	if err := p.Enqueue(specB); err != nil {
		t.Fatalf("re-Enqueue B (dedup): %v", err)
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
	t.Cleanup(func() { _ = store.Close() })

	p := NewPool(store, 1, 16)
	t.Cleanup(p.Stop)

	runnerCtx, runnerCancel := context.WithCancel(context.Background())
	t.Cleanup(runnerCancel)
	p.fsyncFn = noopFsync
	p.runner = func(ctx context.Context, _ JobSpec) (int64, error) {
		<-runnerCtx.Done()
		return 0, ctx.Err()
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
	t.Cleanup(func() { _ = store.Close() })

	p := NewPool(store, 1, 4)
	t.Cleanup(p.Stop)

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
	t.Cleanup(func() { _ = store.Close() })

	p := NewPool(store, 1, 4)
	t.Cleanup(p.Stop)

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
