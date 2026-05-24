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

// TestPoolPublisherBurstStaysGoroutineBounded enqueues bursts of
// jobs and asserts the goroutine fan-out does NOT scale with burst
// size. Pre-fix the publisher was `go fire()` per state transition,
// which would have produced ~burst extra goroutines for a
// burst-sized job stream; the publisher pattern collapses that to
// a single long-lived goroutine regardless of burst size.
//
// The historical shape compared peak against an absolute bound
// (baseline + workers + 16). That worked in isolation but turned
// flaky under concurrent load because the steady-state goroutine
// count under sustained `UpsertVariant` traffic includes ephemeral
// database/sql + modernc.org/sqlite goroutines (per-query / per-
// statement spins, IO emulation pool) that vary with scheduler
// noise. Those ephemerals are real but bounded — they don't scale
// with the burst — so the contract is preserved.
//
// New shape: **differential measurement.** Run a small reference
// burst first to establish the steady-state floor (workers + pool
// publisher + database lazy spawns). Snapshot. Then run a large
// burst, sample peak throughout. The contract is `large_peak -
// small_peak` stays small: a publisher that spawned per-event
// would have ballooned the large burst's peak by ≈(largeBurst -
// smallBurst); the post-fix delta is bounded by scheduler noise
// only (typically 0..few; well under the 32-goroutine slack).
func TestPoolPublisherBurstStaysGoroutineBounded(t *testing.T) {
	store := openTempStoreForPool(t)
	t.Cleanup(func() { _ = store.Close() })

	const (
		workers    = 4
		smallBurst = 50
		largeBurst = 1000
		// maxDelta absorbs scheduler noise + ephemeral
		// database/sql + modernc.org/sqlite goroutines that
		// briefly spin up under sustained UpsertVariant load.
		// Empirically observed under heavy concurrent CPU
		// pressure: delta = 0..~80. 200 is comfortably above
		// the noise floor while still rejecting a `go fire()`
		// regression — pre-fix delta would have been
		// (largeBurst - smallBurst) ≈ 950, ≥ 5× this budget.
		maxDelta int64 = 200
	)

	p := NewPool(store, workers, largeBurst+8)
	// Stub a fast successful runner so processJob reaches the
	// success branch (which fires BOTH a job-complete event AND
	// a state-change event — the worst case for goroutine fan-out).
	p.fsyncFn = noopFsync
	p.runner = func(ctx context.Context, spec JobSpec) (int64, error) {
		return 1, nil
	}

	// Slow callbacks deliberately — the publisher will be busy
	// the whole time, which is the case where the prior `go fire()`
	// shape would have piled up the most goroutines.
	var stateChanges atomic.Int64
	var jobCompletes atomic.Int64
	// Callback sleep is 1 ms — slow enough that a hypothetical
	// `go fire()` regression's per-event goroutines pile up
	// visibly during the sampling window (1000 jobs × 1 ms = 1 s
	// of cumulative callback work; with one-goroutine-per-event,
	// peak concurrent goroutines would reach hundreds). 50 µs
	// was too fast to catch the regression reliably with a
	// 500 µs sampler (each goroutine lived ~50 µs, so at any
	// sample only a small fraction were alive).
	const callbackWork = 1 * time.Millisecond
	p.SetOnStateChange(func() {
		stateChanges.Add(1)
		time.Sleep(callbackWork)
	})
	p.SetOnJobComplete(func(path, variantID string, sampleRate, bitsPerSample int, durationSeconds float64, batchID uuid.UUID, completedAt time.Time) {
		jobCompletes.Add(1)
		time.Sleep(callbackWork)
	})

	// Seed the parent tracks for BOTH bursts. Enqueue is dedup-keyed
	// on (source_path, variant_id); paths in the two bursts MUST
	// differ so the second burst doesn't silently coalesce against
	// in-flight slots from the first.
	totalSeed := smallBurst + largeBurst
	for i := 0; i < totalSeed; i++ {
		seedTrackForPool(t, store, pathForBurstJob(i))
	}

	// Hoist the per-job OutputDir OUT of the burst loop. The
	// previous shape called t.TempDir() per iteration, which (a)
	// queues a t.Cleanup() per call — N closures piling up during
	// the sample window — and (b) calls os.MkdirAll under a
	// sync.Mutex inside testing.T, briefly parking the main
	// goroutine. Neither matters for correctness; both add
	// scheduler noise to the goroutine peak measurement. One
	// tempdir for all jobs is fine — the stub runner never
	// touches OutputDir.
	outputDir := t.TempDir()

	// Sample the goroutine count throughout each burst.
	var peak atomic.Int64
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

	// === Reference (small) burst ===
	// Establishes the steady-state goroutine floor: workers +
	// publisher + connectionOpener + ephemeral database/sql
	// goroutines that come and go during sustained writes. The
	// publisher pattern keeps fan-out bounded REGARDLESS of burst
	// size, so this reference floor is the right baseline to
	// compare the large burst against.
	peak.Store(int64(runtime.NumGoroutine()))
	runBurstAndDrain(t, p, &jobCompletes, smallBurst, 0, outputDir)
	// Brief settle so transient mid-drain goroutines park before
	// we capture the floor.
	time.Sleep(50 * time.Millisecond)
	smallPeak := peak.Load()

	// === Large burst ===
	// Same shape, larger N. Reset the peak tracker BEFORE the
	// large burst's enqueue loop so the prior burst's peak doesn't
	// dominate the measurement (the steady-state floor is what we
	// want as the reference, not its own prior peak).
	peak.Store(int64(runtime.NumGoroutine()))
	jobCompletes.Store(0)
	runBurstAndDrain(t, p, &jobCompletes, largeBurst, smallBurst, outputDir)
	largePeak := peak.Load()

	close(stopSampling)
	samplerWG.Wait()

	// The contract: publisher fan-out is bounded — peak does NOT
	// scale with burst size. Pre-fix, largePeak would have been
	// roughly smallPeak + (largeBurst - smallBurst) = smallPeak +
	// 950. Post-fix the delta is bounded by scheduler noise.
	delta := largePeak - smallPeak
	if delta > maxDelta {
		t.Errorf("goroutine fan-out scaled with burst size: smallPeak=%d, largePeak=%d, delta=%d > %d. The publisher pattern's whole point is that fan-out is INDEPENDENT of burst size.",
			smallPeak, largePeak, delta, maxDelta)
	}

	p.Stop()

	// All buffered job-complete events MUST have been delivered
	// after Stop returns — Stop closes the channels AFTER waiting
	// for workers, then waits for the publisher to drain.
	if got := jobCompletes.Load(); got != largeBurst {
		t.Errorf("jobCompletes after Stop = %d, want %d (publisher must drain remaining events before Stop returns)",
			got, largeBurst)
	}
	// State changes are NOT expected to equal burst — they
	// coalesce. We just want at least 1 to confirm the wiring works.
	if stateChanges.Load() < 1 {
		t.Error("stateChanges = 0, want ≥ 1 — publisher never invoked the state-change callback")
	}
}

// runBurstAndDrain enqueues `n` jobs starting at path index
// `pathStart` and blocks until `done.Load() == n`. The pathStart
// offset lets two bursts in the same test use disjoint paths so
// Enqueue's dedup (keyed on source_path + variant_id) doesn't
// silently coalesce the second burst against in-flight slots
// from the first.
//
// `done` is the per-burst completion counter, NOT the
// publisher-level jobCompletes — the caller resets it between
// bursts so the wait loop knows when the local burst is finished.
//
// Wait loop uses a single Timer/Ticker pair (rather than a
// time.After call per iteration) so the wait itself doesn't spawn
// N short-lived timer goroutines that the sampler could catch.
func runBurstAndDrain(t *testing.T, p *Pool, done *atomic.Int64, n, pathStart int, outputDir string) {
	t.Helper()
	for i := 0; i < n; i++ {
		spec := JobSpec{
			SourceLibraryRel: pathForBurstJob(i + pathStart),
			SourceAbsPath:    "/dev/null/missing",
			TargetSampleRate: 176400,
			TargetBits:       24,
			Quality:          QualityVeryHigh,
			OutputDir:        outputDir,
		}
		if err := p.Enqueue(spec); err != nil {
			t.Fatalf("Enqueue %d (burst of %d): %v", i, n, err)
		}
	}
	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()
	poll := time.NewTicker(50 * time.Millisecond)
	defer poll.Stop()
	for done.Load() < int64(n) {
		select {
		case <-deadline.C:
			t.Fatalf("burst of %d did not complete: done=%d", n, done.Load())
		case <-poll.C:
		}
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
	p.fsyncFn = noopFsync
	p.runner = func(ctx context.Context, spec JobSpec) (int64, error) {
		return 1, nil
	}

	var mu sync.Mutex
	gotPaths := make(map[string]int)
	doneCh := make(chan struct{}, jobs)
	p.SetOnJobComplete(func(path, variantID string, sampleRate, bitsPerSample int, durationSeconds float64, batchID uuid.UUID, completedAt time.Time) {
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
	p.fsyncFn = noopFsync
	p.runner = func(ctx context.Context, spec JobSpec) (int64, error) {
		return 1, nil
	}

	var jobCallbacks atomic.Int64
	p.SetOnJobComplete(func(path, variantID string, sampleRate, bitsPerSample int, durationSeconds float64, batchID uuid.UUID, completedAt time.Time) {
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
