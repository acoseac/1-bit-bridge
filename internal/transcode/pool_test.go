package transcode

import (
	"errors"
	"path/filepath"
	"strconv"
	"sync"
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

	// Queue cap of 2, 1 worker. Concurrent enqueue fan-out
	// drives the test instead of a serial loop: with N
	// goroutines all racing for 2 channel slots + 1 in-flight
	// at the worker, N-3 of them MUST hit ErrQueueFull by
	// pure pigeonhole regardless of the worker's drain speed.
	// Pre-fix the test used a serial loop and relied on the
	// worker draining slower than the test enqueued; the
	// `Stop()/Enqueue` mutex serialisation made that timing
	// flaky on faster machines (the worker could finish
	// `RunSox` failure-fast and `releaseDedup` between every
	// pair of test-side enqueues, keeping the queue under
	// cap). Concurrent fan-out replaces the timing assumption
	// with a structural guarantee.
	p := NewPool(store, 1, 2)
	t.Cleanup(p.Stop)

	// Each spec must have a unique dedup key; otherwise the
	// second-and-onward calls hit the dedup early-return
	// before ever reaching the channel-send.
	mkSpec := func(i int) JobSpec {
		return JobSpec{
			SourceLibraryRel: filepath.Join("X", filepath.Base(t.Name())+"-"+filepath.Base(t.TempDir()), "track-"+strconv.Itoa(i)+".flac"),
			SourceAbsPath:    "/dev/null/missing",
			TargetSampleRate: 176400,
			TargetBits:       24,
			Quality:          QualityVeryHigh,
			OutputDir:        t.TempDir(),
		}
	}

	const goroutines = 50
	var wg sync.WaitGroup
	wg.Add(goroutines)
	var queueFullCount atomic.Uint32
	for i := 0; i < goroutines; i++ {
		go func(idx int) {
			defer wg.Done()
			// errors.Is rather than `==` so any future
			// wrapping inside Enqueue still passes the
			// sentinel check (CodeRabbit minor on PR #109).
			if errors.Is(p.Enqueue(mkSpec(idx)), ErrQueueFull) {
				queueFullCount.Add(1)
			}
		}(i)
	}
	wg.Wait()

	// At least one must have bounced — pigeonhole guarantees
	// it (50 goroutines, 2 queue slots + 1 worker slot = max
	// 3 in-flight at any instant).
	if queueFullCount.Load() == 0 {
		t.Fatal("expected at least one concurrent Enqueue to return ErrQueueFull at queue cap")
	}
}

func TestPoolStopBlocksUntilWorkersDrain(t *testing.T) {
	store := openTempStoreForPool(t)
	t.Cleanup(func() { _ = store.Close() })

	p := NewPool(store, 2, 8)
	for i := 0; i < 4; i++ {
		_ = p.Enqueue(JobSpec{
			SourceLibraryRel: "Music/" + strconv.Itoa(i) + ".flac",
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
	if !errors.Is(err, ErrPoolClosed) {
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
