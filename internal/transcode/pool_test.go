package transcode

import (
	"context"
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

// TestPoolFiresOnStateChangeAfterEnqueueAndCompletion is the headline
// contract for the SSE upstream-publisher wiring (PR following #135):
// every observable pool state transition fires the registered
// onStateChange callback. Cmd/bridge wires this to publish a fresh
// `/v1/upscale/stats` snapshot to the SSE broker.
//
// Test flow:
//  1. Wire SetOnStateChange to a counting recorder.
//  2. Enqueue a JobSpec whose RunSox will fail (source missing) so the
//     worker takes the failure path quickly.
//  3. Wait for the failure to land (failedCnt to bump). The recorder
//     should have observed: 1× enqueue + 1× sox-failure = 2 fires.
//  4. A nil-callback Pool (the back-compat path) must not panic on
//     the same flow.
func TestPoolFiresOnStateChangeAfterEnqueueAndCompletion(t *testing.T) {
	store := openTempStoreForPool(t)
	t.Cleanup(func() { _ = store.Close() })

	p := NewPool(store, 1, 4)
	t.Cleanup(p.Stop)

	var fires atomic.Int64
	p.SetOnStateChange(func() {
		fires.Add(1)
	})

	spec := JobSpec{
		SourceLibraryRel: "Music/Album/01.flac",
		SourceAbsPath:    "/dev/null/missing", // RunSox fails fast
		TargetSampleRate: 176400,
		TargetBits:       24,
		Quality:          QualityVeryHigh,
		OutputDir:        t.TempDir(),
	}
	if err := p.Enqueue(spec); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	// Wait up to 2s for the worker to dispatch the job, run sox
	// (fail fast), and bump failedCnt. Bounded poll instead of a
	// fixed sleep avoids flakiness in slow CI.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if p.Stats().Failed >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	got := fires.Load()
	if got < 2 {
		t.Errorf("onStateChange fires = %d, want ≥ 2 (enqueue + completion)", got)
	}
}

// TestPoolNilOnStateChangeDoesNotPanic locks in the back-compat
// shape: a Pool constructed without SetOnStateChange must run every
// path without panicking on the nil callback.
func TestPoolNilOnStateChangeDoesNotPanic(t *testing.T) {
	store := openTempStoreForPool(t)
	t.Cleanup(func() { _ = store.Close() })

	p := NewPool(store, 1, 4)
	t.Cleanup(p.Stop)
	// Deliberately do NOT call SetOnStateChange.

	spec := JobSpec{
		SourceLibraryRel: "Music/Album/02.flac",
		SourceAbsPath:    "/dev/null/missing",
		TargetSampleRate: 176400,
		TargetBits:       24,
		Quality:          QualityVeryHigh,
		OutputDir:        t.TempDir(),
	}
	if err := p.Enqueue(spec); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	// Wait for completion path to run.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if p.Stats().Failed >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	// Test passes if no panic. No assertion needed beyond the
	// implicit "no goroutine fault under -race".
}

// TestPoolSetOnStateChangeIsRaceSafe drives Set + fire concurrently to
// surface any unsynchronised access on the callback slot. CodeRabbit-
// style protection — the stateChangeMu RWMutex guards the swap.
func TestPoolSetOnStateChangeIsRaceSafe(t *testing.T) {
	store := openTempStoreForPool(t)
	t.Cleanup(func() { _ = store.Close() })

	p := NewPool(store, 2, 8)
	t.Cleanup(p.Stop)

	// Initial callback in place from t=0.
	p.SetOnStateChange(func() {})

	var wg sync.WaitGroup
	// One goroutine swaps the callback every microsecond.
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				p.SetOnStateChange(func() {})
			}
		}
	}()

	// Drive a burst of enqueues so workers fire the callback.
	for i := 0; i < 8; i++ {
		_ = p.Enqueue(JobSpec{
			SourceLibraryRel: "Music/" + strconv.Itoa(i) + ".flac",
			SourceAbsPath:    "/dev/null/missing",
			TargetSampleRate: 176400,
			TargetBits:       24,
			Quality:          QualityVeryHigh,
			OutputDir:        t.TempDir(),
		})
	}

	// Let the workers churn briefly.
	time.Sleep(100 * time.Millisecond)
	close(stop)
	wg.Wait()

	// Test passes if no race detected and no panic.
}

// TestPoolJobTimesOutAndCountsAsFailure locks in the per-job timeout
// contract added alongside processJob extraction: a runner that hangs
// past `p.jobTimeout` is killed by the per-job context, the failure
// counter ticks, AND the worker slot is reclaimed so the next queued
// job runs.
//
// Without the per-job context, the worker would inherit only the
// pool-wide stopCtx — a stuck sox would consume the slot until the
// whole pool was Stop()'d, which on a 2–4 worker production deploy
// is the difference between "one bad track failed" and "upscaling
// is dead, restart bridge serve."
//
// Slot-reclaim coverage (CodeRabbit on PR #162): asserting Failed
// ticks alone would let a regression that timed-out the job but
// failed to release the worker slot pass — we explicitly enqueue a
// second job AFTER the timeout and require it to run to prove the
// recovery path is real.
//
// Mechanism:
//  1. Per-instance `p.jobTimeout = 50 * time.Millisecond` so the
//     test runs in a few hundred ms instead of 10 minutes. Field-
//     level override (vs a package-level var) so parallel tests in
//     the same package don't race on global state.
//  2. Inject a `runner` stub keyed by spec — first job hangs on
//     ctx.Done(), second job returns success immediately — exercising
//     both the timeout branch AND the slot-reclaim path the second
//     job depends on.
//  3. Bounded poll on Stats().Failed reaching 1, then on a per-job
//     completion channel for the second job.
func TestPoolJobTimesOutAndCountsAsFailure(t *testing.T) {
	store := openTempStoreForPool(t)
	t.Cleanup(func() { _ = store.Close() })

	p := NewPool(store, 1, 4)
	p.jobTimeout = 50 * time.Millisecond
	t.Cleanup(p.Stop)

	started := make(chan struct{})
	secondRan := make(chan struct{})
	p.runner = func(ctx context.Context, spec JobSpec) (int64, error) {
		switch spec.SourceLibraryRel {
		case "Music/Album/timeout.flac":
			close(started)
			<-ctx.Done()
			return 0, ctx.Err()
		case "Music/Album/recovery.flac":
			close(secondRan)
			return 1, nil
		default:
			t.Errorf("unexpected runner spec: %q", spec.SourceLibraryRel)
			return 0, nil
		}
	}

	stuckSpec := JobSpec{
		SourceLibraryRel: "Music/Album/timeout.flac",
		SourceAbsPath:    "/dev/null/missing",
		TargetSampleRate: 176400,
		TargetBits:       24,
		Quality:          QualityVeryHigh,
		OutputDir:        t.TempDir(),
	}
	if err := p.Enqueue(stuckSpec); err != nil {
		t.Fatalf("Enqueue stuck: %v", err)
	}

	// Confirm the first job actually entered the runner.
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("runner stub never invoked — pool dispatch broken?")
	}

	// Wait for failedCnt to tick — proves the timeout branch fired.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if p.Stats().Failed >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := p.Stats().Failed; got < 1 {
		t.Fatalf("expected Stats.Failed to reach 1 after timeout, got %d", got)
	}

	// Slot-reclaim assertion: enqueue a second job and require it to
	// run within a tight deadline. A regression that times out a job
	// without releasing the worker slot would hang here.
	recoverSpec := JobSpec{
		SourceLibraryRel: "Music/Album/recovery.flac",
		SourceAbsPath:    "/dev/null/missing",
		TargetSampleRate: 176400,
		TargetBits:       24,
		Quality:          QualityVeryHigh,
		OutputDir:        t.TempDir(),
	}
	if err := p.Enqueue(recoverSpec); err != nil {
		t.Fatalf("Enqueue recovery: %v", err)
	}
	select {
	case <-secondRan:
		// Slot was reclaimed — recovery job ran. Pass.
	case <-time.After(2 * time.Second):
		t.Fatal("recovery job never ran — worker slot leaked after timeout")
	}
}

// TestPoolStopDuringJobSuppressesFailure pins the inverse of the
// timeout test: when Stop() cancels the pool while a job is in
// flight, the resulting ctx.Err() must NOT count as a failure.
// This is the existing graceful-shutdown contract that the timeout
// branching had to preserve carefully.
func TestPoolStopDuringJobSuppressesFailure(t *testing.T) {
	store := openTempStoreForPool(t)
	t.Cleanup(func() { _ = store.Close() })

	p := NewPool(store, 1, 4)
	// No t.Cleanup(p.Stop) — we Stop() explicitly mid-test.

	started := make(chan struct{})
	p.runner = func(ctx context.Context, _ JobSpec) (int64, error) {
		close(started)
		<-ctx.Done()
		return 0, ctx.Err()
	}

	spec := JobSpec{
		SourceLibraryRel: "Music/Album/stopduring.flac",
		SourceAbsPath:    "/dev/null/missing",
		TargetSampleRate: 176400,
		TargetBits:       24,
		Quality:          QualityVeryHigh,
		OutputDir:        t.TempDir(),
	}
	if err := p.Enqueue(spec); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("runner stub never invoked — pool dispatch broken?")
	}
	p.Stop() // blocks until the worker drains

	if got := p.Stats().Failed; got != 0 {
		t.Errorf("Stats.Failed during graceful shutdown: got %d, want 0", got)
	}
}

// TestPoolPanicInRunnerReleasesDedup pins the contract that a panic
// inside p.runner doesn't leak the (source, variant) dedup slot —
// pre-fix, the explicit releaseDedup calls in processJob's success/
// error branches were bypassed on panic, blacklisting the variant
// from future scheduling until the bridge process restarted AND
// crashing the worker goroutine. The recover()+deferred-release
// pattern in processJob both contains the panic to one job AND
// ensures the slot is reclaimed.
func TestPoolPanicInRunnerReleasesDedup(t *testing.T) {
	store := openTempStoreForPool(t)
	t.Cleanup(func() { _ = store.Close() })

	p := NewPool(store, 1, 4)
	t.Cleanup(p.Stop)

	panicked := make(chan struct{})
	survivorRan := make(chan struct{})
	p.runner = func(ctx context.Context, spec JobSpec) (int64, error) {
		switch spec.SourceLibraryRel {
		case "Music/Album/panic.flac":
			close(panicked)
			panic("synthetic transcode panic for slot-reclaim test")
		case "Music/Album/survivor.flac":
			close(survivorRan)
			// Return a non-nil error so this job hits the runner-
			// error branch instead of trying to write a foreign-key-
			// invalid track_variants row. We're testing pool behaviour,
			// not the store path.
			return 0, errors.New("expected: not a real source file")
		default:
			t.Errorf("unexpected runner spec: %q", spec.SourceLibraryRel)
			return 0, nil
		}
	}

	panicSpec := JobSpec{
		SourceLibraryRel: "Music/Album/panic.flac",
		SourceAbsPath:    "/dev/null/missing",
		TargetSampleRate: 176400,
		TargetBits:       24,
		Quality:          QualityVeryHigh,
		OutputDir:        t.TempDir(),
	}
	if err := p.Enqueue(panicSpec); err != nil {
		t.Fatalf("Enqueue panic: %v", err)
	}
	select {
	case <-panicked:
	case <-time.After(2 * time.Second):
		t.Fatal("runner stub never invoked — pool dispatch broken?")
	}

	// Wait for failedCnt to tick — proves the recover branch fired
	// AND incremented the failure counter so the panic is observable
	// to operators via /v1/health pool stats.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if p.Stats().Failed >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := p.Stats().Failed; got < 1 {
		t.Fatalf("expected Stats.Failed to reach 1 after panic, got %d", got)
	}

	// Worker-survival + slot-reclaim assertion: enqueue a follow-up
	// job and require it to run within a tight deadline. Without the
	// recover() in processJob, the worker goroutine would have died
	// when the panic propagated up; the second job would never be
	// dispatched. With the fix, the worker survives and picks up
	// the next job from the queue.
	survivorSpec := JobSpec{
		SourceLibraryRel: "Music/Album/survivor.flac",
		SourceAbsPath:    "/dev/null/missing",
		TargetSampleRate: 176400,
		TargetBits:       24,
		Quality:          QualityVeryHigh,
		OutputDir:        t.TempDir(),
	}
	if err := p.Enqueue(survivorSpec); err != nil {
		t.Fatalf("Enqueue survivor: %v", err)
	}
	select {
	case <-survivorRan:
	case <-time.After(2 * time.Second):
		t.Fatal("survivor job never ran — worker dead after panic")
	}

	// Slot-reclaim assertion: poll until both the panicked-job AND
	// the survivor-job slots are released. Polling Inflight directly
	// (rather than gating on Stats.Failed) handles the race where
	// failedCnt is incremented inside the runner-error branch BEFORE
	// the synchronous releaseDedup call — Failed >= 2 doesn't imply
	// the release has fired yet.
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if p.Stats().Inflight == 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := p.Stats().Inflight; got != 0 {
		t.Errorf("expected Stats.Inflight == 0 after panic + survivor, got %d (a job slot leaked)", got)
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
