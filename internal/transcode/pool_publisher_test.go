package transcode

import (
	"context"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Tests for the bounded coalescing publisher pattern (CLAUDE.md:
// "Bounded SSE publisher goroutine for transcode events"). These
// pin the contract that:
//
//  1. Burst Enqueues do NOT spawn an unbounded number of goroutines.
//     The prior `go fire()` shape produced one ephemeral goroutine
//     per state transition; the new shape funnels every signal
//     through a single long-lived publisher.
//  2. Rapid state changes COALESCE through the capacity-1
//     stateChangeChan: when the publisher is busy invoking the
//     wired callback, additional signals collapse to one.
//  3. Per-job completion events do NOT coalesce. Every
//     `upscale.complete` event must reach the wired callback
//     because iOS keys on the path/variantID for its reverse
//     index. The buffered channel + blocking send provides
//     correct backpressure without dropping events.
//  4. Stop() ordering preserves both fidelity (drains buffered
//     job-complete events) and safety (no send-on-closed-channel
//     panic).

// TestPoolPublisherBurstStaysGoroutineBounded enqueues a burst of
// jobs and asserts the goroutine count does not balloon. Pre-fix
// the same burst would have spawned ≥1× state-change goroutine per
// enqueue + ≥1× per completion + ≥1× per state-change-on-completion;
// with the publisher pattern the count stays bounded by `workers + 1`.
func TestPoolPublisherBurstStaysGoroutineBounded(t *testing.T) {
	store := openTempStoreForPool(t)
	t.Cleanup(func() { _ = store.Close() })

	const workers = 4
	const burst = 1000

	p := NewPool(store, workers, burst+8)
	// Stub a fast successful runner so processJob reaches the
	// success branch (which fires BOTH a job-complete event AND
	// a state-change event — the worst case for goroutine fan-out).
	p.runner = func(ctx context.Context, spec JobSpec) (int64, error) {
		return 1, nil
	}

	// Slow callbacks deliberately — the publisher will be busy
	// the whole time, which is the case where the prior `go fire()`
	// shape would have piled up the most goroutines.
	var stateChanges atomic.Int64
	var jobCompletes atomic.Int64
	p.SetOnStateChange(func() {
		stateChanges.Add(1)
		time.Sleep(50 * time.Microsecond)
	})
	p.SetOnJobComplete(func(path, variantID string, sampleRate, bitsPerSample int, batchID uuid.UUID, completedAt time.Time) {
		jobCompletes.Add(1)
		time.Sleep(50 * time.Microsecond)
	})

	// Seed the parent tracks rows so UpsertVariant's FK passes for
	// every job in the burst. Without these the success branch
	// degrades to the store-error branch (still bounded, but
	// changes the test's coverage shape).
	for i := 0; i < burst; i++ {
		path := pathForBurstJob(i)
		seedTrackForPool(t, store, path)
	}

	// Baseline goroutine count AFTER the pool is fully spun up
	// (workers + publisher) but BEFORE any work is enqueued.
	// Allow a brief settle so the scheduler has parked everything.
	time.Sleep(20 * time.Millisecond)
	baseline := runtime.NumGoroutine()

	// Sample the goroutine count throughout the burst — peak is
	// what we care about, not the post-drain count.
	var peak atomic.Int64
	peak.Store(int64(baseline))
	stopSampling := make(chan struct{})
	var samplerWG sync.WaitGroup
	samplerWG.Add(1)
	go func() {
		defer samplerWG.Done()
		ticker := time.NewTicker(500 * time.Microsecond)
		defer ticker.Stop()
		for {
			select {
			case <-stopSampling:
				return
			case <-ticker.C:
				if n := int64(runtime.NumGoroutine()); n > peak.Load() {
					peak.Store(n)
				}
			}
		}
	}()

	for i := 0; i < burst; i++ {
		spec := JobSpec{
			SourceLibraryRel: pathForBurstJob(i),
			SourceAbsPath:    "/dev/null/missing",
			TargetSampleRate: 176400,
			TargetBits:       24,
			Quality:          QualityVeryHigh,
			OutputDir:        t.TempDir(),
		}
		if err := p.Enqueue(spec); err != nil {
			t.Fatalf("Enqueue %d: %v", i, err)
		}
	}

	// Wait for the burst to complete.
	deadline := time.After(30 * time.Second)
	for {
		if jobCompletes.Load() == burst {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("burst did not complete: jobCompletes=%d, want %d", jobCompletes.Load(), burst)
		case <-time.After(50 * time.Millisecond):
		}
	}

	close(stopSampling)
	samplerWG.Wait()

	// Allow generous slack for test infrastructure (timer
	// goroutines, GC sweepers, runtime.gopark goroutines that
	// short-lived Enqueue calls may have parked transiently). The
	// load-bearing assertion is "doesn't grow with burst size":
	// pre-fix this peak would have been baseline + ~burst
	// (≈1004 for a 1000-burst). Post-fix it should stay within
	// a small constant of baseline.
	maxAllowed := baseline + workers + 16
	if got := peak.Load(); got > int64(maxAllowed) {
		t.Errorf("goroutine peak = %d, want ≤ %d (baseline=%d). The publisher pattern should bound goroutine fan-out regardless of burst size.",
			got, maxAllowed, baseline)
	}

	p.Stop()

	// All buffered job-complete events MUST have been delivered
	// after Stop returns — Stop closes the channels AFTER waiting
	// for workers, then waits for the publisher to drain.
	if got := jobCompletes.Load(); got != burst {
		t.Errorf("jobCompletes after Stop = %d, want %d (publisher must drain remaining events before Stop returns)",
			got, burst)
	}
	// State changes are NOT expected to equal burst — they
	// coalesce. We just want at least 1 to confirm the wiring works.
	if stateChanges.Load() < 1 {
		t.Error("stateChanges = 0, want ≥ 1 — publisher never invoked the state-change callback")
	}
}

// TestPoolPublisherCoalescesStateChanges pins the
// capacity-1 + non-blocking-send contract on stateChangeChan.
// Many state-change signals while the publisher is busy MUST
// collapse to fewer callback invocations — the wired callback
// always reads the current snapshot, so missed signals are
// equivalent to delivered ones.
func TestPoolPublisherCoalescesStateChanges(t *testing.T) {
	store := openTempStoreForPool(t)
	t.Cleanup(func() { _ = store.Close() })

	p := NewPool(store, 1, 1)
	t.Cleanup(p.Stop)

	// Park the callback so the publisher is stuck inside the
	// invocation while we fan out signals. release closes when
	// we want the callback to return.
	release := make(chan struct{})
	var callbackInvocations atomic.Int64
	p.SetOnStateChange(func() {
		callbackInvocations.Add(1)
		<-release
	})

	// Fire one to put the publisher inside the callback. Wait
	// for it to land — the publisher needs to have actually
	// pulled the signal off the channel before we start the
	// burst, otherwise the very first burst signal would land
	// in the buffer slot instead of being merged with subsequent
	// drops.
	if !p.fireStateChange() {
		t.Fatal("first fireStateChange returned false unexpectedly")
	}
	for callbackInvocations.Load() == 0 {
		time.Sleep(time.Millisecond)
	}

	// Now publisher is parked inside the callback. Fire a burst
	// of signals — ALL but at most one should be dropped (the
	// first one fills the cap-1 buffer; the rest hit `default`).
	const burst = 1000
	dropped := 0
	for i := 0; i < burst; i++ {
		if !p.fireStateChange() {
			dropped++
		}
	}
	if dropped < burst-1 {
		t.Errorf("dropped %d of %d burst sends, want ≥ %d (cap-1 buffer + busy publisher should drop all but at most 1)",
			dropped, burst, burst-1)
	}

	// Release the callback so the publisher consumes the
	// buffered signal (if any) and the test can finish.
	close(release)

	// Eventually the publisher invokes the callback for the
	// buffered signal. Total invocations across the entire test
	// must be ≤ 2: the initial one we fired, plus at most one
	// from the buffer.
	deadline := time.After(time.Second)
	for {
		if n := callbackInvocations.Load(); n >= 1 && n <= 2 {
			// Settled — confirm by waiting a moment more and
			// re-checking.
			time.Sleep(20 * time.Millisecond)
			if final := callbackInvocations.Load(); final > 2 {
				t.Errorf("callback invocations = %d after settle, want ≤ 2 (1000-signal burst should coalesce)", final)
			}
			break
		}
		select {
		case <-deadline:
			t.Fatalf("callback never settled: invocations=%d", callbackInvocations.Load())
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// TestPoolPublisherJobCompleteFidelity pins the no-drop contract
// on jobCompleteChan: every successfully-processed job MUST
// produce exactly one onJobComplete callback. Pre-fix `go fireJob`
// could in principle race Stop() and lose events; the buffered
// channel + Stop() drain ordering eliminates the window.
func TestPoolPublisherJobCompleteFidelity(t *testing.T) {
	store := openTempStoreForPool(t)
	t.Cleanup(func() { _ = store.Close() })

	const jobs = 100

	// Seed parent rows so UpsertVariant's FK passes.
	for i := 0; i < jobs; i++ {
		seedTrackForPool(t, store, pathForFidelityJob(i))
	}

	p := NewPool(store, 4, jobs)
	t.Cleanup(p.Stop)
	p.runner = func(ctx context.Context, spec JobSpec) (int64, error) {
		return 1, nil
	}

	var mu sync.Mutex
	gotPaths := make(map[string]int)
	doneCh := make(chan struct{}, jobs)
	p.SetOnJobComplete(func(path, variantID string, sampleRate, bitsPerSample int, batchID uuid.UUID, completedAt time.Time) {
		mu.Lock()
		gotPaths[path]++
		mu.Unlock()
		doneCh <- struct{}{}
	})

	for i := 0; i < jobs; i++ {
		spec := JobSpec{
			SourceLibraryRel: pathForFidelityJob(i),
			SourceAbsPath:    "/dev/null/missing",
			TargetSampleRate: 176400,
			TargetBits:       24,
			Quality:          QualityVeryHigh,
			OutputDir:        t.TempDir(),
		}
		if err := p.Enqueue(spec); err != nil {
			t.Fatalf("Enqueue %d: %v", i, err)
		}
	}

	// Wait for all callbacks. The publisher invokes them
	// synchronously one at a time, so this is bounded by
	// (jobs * callback latency) which is ~milliseconds.
	deadline := time.After(10 * time.Second)
	received := 0
	for received < jobs {
		select {
		case <-doneCh:
			received++
		case <-deadline:
			t.Fatalf("only %d/%d job-complete callbacks fired", received, jobs)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(gotPaths) != jobs {
		t.Errorf("got %d unique paths, want %d (some completions never reached the callback)", len(gotPaths), jobs)
	}
	for i := 0; i < jobs; i++ {
		path := pathForFidelityJob(i)
		if gotPaths[path] != 1 {
			t.Errorf("path %q: callback fired %d times, want 1 (no dups, no drops)", path, gotPaths[path])
		}
	}
}

// TestPoolPublisherStopDrainsBufferedEvents pins the Stop-ordering
// contract: when Stop is called after workers have completed every
// job (events buffered in jobCompleteChan but not yet consumed by
// the publisher), every event must reach the wired callback before
// Stop returns.
//
// This is the case where an early publisher exit on stopCtx.Done()
// would lose events. The test forces buffering by:
//
//  1. Slow callback (2 ms per event) so the publisher can't drain
//     in real-time.
//  2. Wait for `doneCnt == jobs` — confirms workers have completed
//     EVERY job and pushed every event onto jobCompleteChan, before
//     calling Stop. (Stop()'s cooperative-stop check would otherwise
//     skip un-started jobs, which is the documented behaviour but
//     defeats this test's premise.)
//  3. Stop() — must drain the buffered events before returning.
func TestPoolPublisherStopDrainsBufferedEvents(t *testing.T) {
	store := openTempStoreForPool(t)
	t.Cleanup(func() { _ = store.Close() })

	const jobs = 50

	for i := 0; i < jobs; i++ {
		seedTrackForPool(t, store, pathForDrainJob(i))
	}

	// Worker count > 1 so multiple events buffer in
	// jobCompleteChan before the slow callback drains them.
	p := NewPool(store, 4, jobs)
	p.runner = func(ctx context.Context, spec JobSpec) (int64, error) {
		return 1, nil
	}

	var jobCallbacks atomic.Int64
	p.SetOnJobComplete(func(path, variantID string, sampleRate, bitsPerSample int, batchID uuid.UUID, completedAt time.Time) {
		jobCallbacks.Add(1)
		// Slow callback so events queue up in the publisher
		// channel.
		time.Sleep(2 * time.Millisecond)
	})

	for i := 0; i < jobs; i++ {
		spec := JobSpec{
			SourceLibraryRel: pathForDrainJob(i),
			SourceAbsPath:    "/dev/null/missing",
			TargetSampleRate: 176400,
			TargetBits:       24,
			Quality:          QualityVeryHigh,
			OutputDir:        t.TempDir(),
		}
		if err := p.Enqueue(spec); err != nil {
			t.Fatalf("Enqueue %d: %v", i, err)
		}
	}

	// Wait for workers to pass UpsertVariant on every job.
	// doneCnt is bumped AFTER UpsertVariant commits and BEFORE
	// the worker's blocking fireJobComplete send — so when
	// doneCnt == jobs, a worker may still be mid-blocking-send
	// with the event not yet in the channel buffer. The
	// load-bearing guarantee that closes the gap is Stop()'s
	// `p.wg.Wait()`: it parks until every worker has fully
	// returned, which means every in-flight blocking send has
	// completed (the event landed in the buffer). Only then does
	// Stop close the publisher channels and wait for the publisher
	// to drain. Greptile on PR #188 caught the prior docstring
	// claiming doneCnt alone provided the buffer-or-consumed
	// guarantee.
	deadline := time.After(10 * time.Second)
	for p.doneCnt.Load() < jobs {
		select {
		case <-deadline:
			t.Fatalf("workers did not complete all jobs: doneCnt=%d, want %d", p.doneCnt.Load(), jobs)
		case <-time.After(time.Millisecond):
		}
	}

	// Now Stop. The publisher likely has events buffered (the
	// slow callback was deliberately slower than worker
	// throughput); Stop's drain step must let them all fire.
	p.Stop()

	if got := jobCallbacks.Load(); got != jobs {
		t.Errorf("jobCallbacks after Stop = %d, want %d (Stop must drain buffered job-complete events)", got, jobs)
	}
}

// pathForBurstJob returns a unique manifest path for the burst
// goroutine-bound test. Each path must be unique so dedup doesn't
// short-circuit the enqueue.
func pathForBurstJob(i int) string {
	return filepath.Join("Burst", "track-"+strconv.Itoa(i)+".flac")
}

// pathForFidelityJob returns a unique manifest path for the
// fidelity test.
func pathForFidelityJob(i int) string {
	return filepath.Join("Fidelity", "track-"+strconv.Itoa(i)+".flac")
}

// pathForDrainJob returns a unique manifest path for the drain
// test.
func pathForDrainJob(i int) string {
	return filepath.Join("Drain", "track-"+strconv.Itoa(i)+".flac")
}
