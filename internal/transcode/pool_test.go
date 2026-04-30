package transcode

import (
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// Pool tests focus on the dedup, queue-cap, and graceful-stop
// contracts. The actual sox invocation is covered by the
// transcode_integration_test build tag elsewhere; here we can
// observe pool behaviour without running real conversions
// because every test case rejects its enqueues at the dedup or
// queue-cap gates BEFORE worker dispatch — or, when we do let a
// job through, we use a JobSpec whose source path doesn't exist
// so RunSox fails fast (the failure path is what the pool's
// dedup-release contract has to handle correctly).

func TestPoolEnqueueDeduplicates(t *testing.T) {
	store := openTempStoreForPool(t)
	t.Cleanup(func() { _ = store.Close() })

	// Use a tiny worker count so jobs queue rather than start
	// immediately — keeps the dedup window observable.
	p := NewPool(store, 1, 16)
	t.Cleanup(p.Stop)

	spec := JobSpec{
		SourceLibraryRel: "Music/Album/01.flac",
		SourceAbsPath:    "/dev/null/missing", // will fail RunSox, that's fine
		TargetSampleRate: 176400,
		TargetBits:       24,
		Quality:          QualityVeryHigh,
		OutputDir:        t.TempDir(),
	}

	// First enqueue lands.
	if err := p.Enqueue(spec); err != nil {
		t.Fatalf("first Enqueue: %v", err)
	}
	// Second enqueue with identical (source, variant) is a
	// silent no-op — returns nil, doesn't take a slot.
	if err := p.Enqueue(spec); err != nil {
		t.Fatalf("dedup Enqueue: %v", err)
	}
	stats := p.Stats()
	if stats.Enqueued != 1 {
		t.Errorf("Stats.Enqueued: got %d, want 1 (dedup should not increment)", stats.Enqueued)
	}
}

func TestPoolEnqueueReturnsErrQueueFullAtCap(t *testing.T) {
	store := openTempStoreForPool(t)
	t.Cleanup(func() { _ = store.Close() })

	// Queue cap of 2, zero workers (well, 1 minimum) — we want
	// the queue to fill before any worker drains it. The block-
	// sender JobSpec has a path that won't resolve, so workers
	// fail fast and recycle, but we slip enqueues in faster
	// than the worker can pull.
	p := NewPool(store, 1, 2)
	t.Cleanup(p.Stop)

	// Each spec must have a unique dedup key; otherwise the
	// second-and-onward calls hit the dedup early-return
	// before ever reaching the channel-send.
	mkSpec := func(i int) JobSpec {
		return JobSpec{
			SourceLibraryRel: filepath.Join("X", filepath.Base(t.Name())+"-"+filepath.Base(t.TempDir()), "track-"+itoa(i)+".flac"),
			SourceAbsPath:    "/dev/null/missing",
			TargetSampleRate: 176400,
			TargetBits:       24,
			Quality:          QualityVeryHigh,
			OutputDir:        t.TempDir(),
		}
	}

	// Submit a flurry — at least one MUST hit ErrQueueFull
	// because the queue cap is 2 and the worker can't drain
	// faster than we enqueue.
	var queueFullSeen atomic.Bool
	for i := 0; i < 20 && !queueFullSeen.Load(); i++ {
		if err := p.Enqueue(mkSpec(i)); err == ErrQueueFull {
			queueFullSeen.Store(true)
		}
	}
	if !queueFullSeen.Load() {
		t.Fatal("expected at least one Enqueue to return ErrQueueFull at queue cap")
	}
}

func TestPoolStopBlocksUntilWorkersDrain(t *testing.T) {
	store := openTempStoreForPool(t)
	t.Cleanup(func() { _ = store.Close() })

	p := NewPool(store, 2, 8)
	for i := 0; i < 4; i++ {
		_ = p.Enqueue(JobSpec{
			SourceLibraryRel: "Music/" + itoa(i) + ".flac",
			SourceAbsPath:    "/dev/null/missing",
			TargetSampleRate: 176400,
			TargetBits:       24,
			Quality:          QualityVeryHigh,
			OutputDir:        t.TempDir(),
		})
	}

	stopDone := make(chan struct{})
	go func() {
		p.Stop()
		close(stopDone)
	}()
	select {
	case <-stopDone:
		// Good — Stop returned within the timeout.
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not return within 5s — workers blocked?")
	}
	// Stop is idempotent.
	p.Stop()
}

func TestPoolEnqueueAfterStopReturnsErrPoolClosed(t *testing.T) {
	store := openTempStoreForPool(t)
	t.Cleanup(func() { _ = store.Close() })

	p := NewPool(store, 1, 1)
	p.Stop()
	err := p.Enqueue(JobSpec{
		SourceLibraryRel: "Music/x.flac",
		TargetSampleRate: 176400,
		TargetBits:       24,
	})
	if err != ErrPoolClosed {
		t.Errorf("Enqueue after Stop: got %v, want ErrPoolClosed", err)
	}
}

// --- helpers ---

func openTempStoreForPool(t *testing.T) *manifest.Store {
	t.Helper()
	dir := t.TempDir()
	s, err := manifest.OpenStore(filepath.Join(dir, "bridge.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	return s
}

// itoa is a tiny stdlib-free integer→string helper so the test
// imports stay short. The negative branch is unused (test calls
// pass non-negative i only) but kept for safety.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var buf [20]byte
	idx := len(buf)
	for i > 0 {
		idx--
		buf[idx] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		idx--
		buf[idx] = '-'
	}
	return string(buf[idx:])
}
