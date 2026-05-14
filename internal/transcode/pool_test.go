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
	"github.com/google/uuid"
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
	p.fsyncFn = noopFsync
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
	p.fsyncFn = noopFsync
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

	// Track onStateChange fires so we can assert the panic-recovery
	// path fires the callback like every other terminal branch
	// (otherwise SSE clients wouldn't see the failure tick until the
	// next unrelated event lands).
	var fires atomic.Int64
	p.SetOnStateChange(func() {
		fires.Add(1)
	})

	panicked := make(chan struct{})
	survivorRan := make(chan struct{})
	p.fsyncFn = noopFsync
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

	// Assert the panic-recovery defer fires the onStateChange
	// callback alongside the synchronous error branches. Expected
	// fires: panic-job enqueue + panic-recovery + survivor-job
	// enqueue + survivor runner-error = 4. We loosen to ≥ 4 so a
	// future addition of an extra fire doesn't trip a brittle
	// equality check; the load-bearing assertion is "the panic
	// branch did NOT silently skip the fire" — without the fix
	// fires would land at 3 (one fire missing from the panic
	// branch).
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if fires.Load() >= 4 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := fires.Load(); got < 4 {
		t.Errorf("onStateChange fires = %d, want ≥ 4 (panic-enqueue + panic-recovery + survivor-enqueue + survivor-fail)", got)
	}
}

// TestPoolFiresOnJobCompleteAfterUpsertVariant pins the headline
// contract for the iOS-side event-driven reconciliation path: when
// a job succeeds, the registered onJobComplete callback fires with
// the JobSpec's raw `SourceLibraryRel` AND the corresponding
// `track_variants` row is already visible from inside the callback.
// The latter ordering invariant is what makes the iOS-triggered
// manifest re-sync safe — pre-fix the iOS client would race the DB
// commit and miss the new variant.
func TestPoolFiresOnJobCompleteAfterUpsertVariant(t *testing.T) {
	store := openTempStoreForPool(t)
	t.Cleanup(func() { _ = store.Close() })

	// Seed the parent tracks row so UpsertVariant's FK constraint
	// passes. The pool callback fires AFTER the variant insert
	// commits — the FK FAIL we'd otherwise see (787) is exactly the
	// failure mode the post-commit guarantee protects against.
	seedTrackForPool(t, store, "Music/Album/01.flac")

	p := NewPool(store, 1, 4)
	t.Cleanup(p.Stop)

	// Successful runner stub — RunSox isn't invoked, the worker
	// reaches the UpsertVariant + callback path directly.
	p.fsyncFn = noopFsync
	p.runner = func(ctx context.Context, spec JobSpec) (int64, error) {
		return 42, nil
	}

	type jobEvent struct {
		path, variantID         string
		sampleRate, bitsPerSamp int
		completedAt             time.Time
		variantsAtCallbackTime  int
	}
	gotCh := make(chan jobEvent, 1)
	p.SetOnJobComplete(func(path, variantID string, sampleRate, bitsPerSample int, durationSeconds float64, batchID uuid.UUID, completedAt time.Time) {
		// Critical ordering check: query the store from inside the
		// callback. A regression that fires before UpsertVariant
		// commits would see zero variants here.
		count, _, err := store.CountVariants(context.Background())
		if err != nil {
			t.Errorf("CountVariants inside callback: %v", err)
		}
		_ = batchID // legacy single-track path leaves it zero
		gotCh <- jobEvent{
			path:                   path,
			variantID:              variantID,
			sampleRate:             sampleRate,
			bitsPerSamp:            bitsPerSample,
			completedAt:            completedAt,
			variantsAtCallbackTime: count,
		}
	})

	spec := JobSpec{
		SourceLibraryRel: "Music/Album/01.flac",
		SourceAbsPath:    "/dev/null/missing", // runner stubbed; path irrelevant
		TargetSampleRate: 192000,
		TargetBits:       24,
		Quality:          QualityVeryHigh,
		OutputDir:        t.TempDir(),
	}
	// UTC for parity with the pool's `time.Now().UTC()` capture — avoids
	// the monotonic-clock-vs-wall-clock divergence the bare time.Now()
	// would carry (Gemini MEDIUM on PR #187).
	beforeEnqueue := time.Now().UTC()
	if err := p.Enqueue(spec); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	var got jobEvent
	select {
	case got = <-gotCh:
	case <-time.After(2 * time.Second):
		t.Fatal("onJobComplete never fired on success path")
	}

	if got.path != spec.SourceLibraryRel {
		t.Errorf("path = %q, want %q (raw SourceLibraryRel, no normalisation)", got.path, spec.SourceLibraryRel)
	}
	if got.variantID != spec.VariantID() {
		t.Errorf("variantID = %q, want %q", got.variantID, spec.VariantID())
	}
	if got.sampleRate != 192000 {
		t.Errorf("sampleRate = %d, want 192000", got.sampleRate)
	}
	if got.bitsPerSamp != 24 {
		t.Errorf("bitsPerSample = %d, want 24", got.bitsPerSamp)
	}
	if got.completedAt.Before(beforeEnqueue) {
		t.Errorf("completedAt = %v predates enqueue %v", got.completedAt, beforeEnqueue)
	}
	if got.variantsAtCallbackTime < 1 {
		t.Errorf("CountVariants inside callback = %d, want ≥ 1 — fires-before-commit regression", got.variantsAtCallbackTime)
	}

	// Lock the single-capture timestamp guarantee: the inserted
	// `track_variants.created_at` row column MUST equal
	// `event.completedAt.UnixNano()` exactly — both surfaces derive
	// from the same `completedAt := time.Now().UTC()` capture in
	// processJob. A regression back to two separate `time.Now()`
	// calls would produce slightly different values that this strict
	// equality catches. (CodeRabbit nitpick on PR #187 — pinned so a
	// future maintainer can't drop the shared capture.)
	row, err := store.GetVariant(context.Background(), spec.SourceLibraryRel, spec.VariantID())
	if err != nil {
		t.Fatalf("GetVariant for parity check: %v", err)
	}
	if row.CreatedAt != got.completedAt.UnixNano() {
		t.Errorf("DB row CreatedAt (%d) != event completedAt.UnixNano() (%d) — "+
			"single-capture guarantee broken",
			row.CreatedAt, got.completedAt.UnixNano())
	}
}

// TestPoolDoesNotFireOnJobCompleteOnFailure pins the success-only
// emission contract for v1. A sox failure / store failure must NOT
// fire onJobComplete — iOS learns about failures via the stats push,
// not the per-job event. If we ever ship `upscale.failed`, that's a
// new callback, not a generalisation of this one.
func TestPoolDoesNotFireOnJobCompleteOnFailure(t *testing.T) {
	store := openTempStoreForPool(t)
	t.Cleanup(func() { _ = store.Close() })

	p := NewPool(store, 1, 4)
	t.Cleanup(p.Stop)

	soxErr := errors.New("sox synthetic failure")
	p.fsyncFn = noopFsync
	p.runner = func(ctx context.Context, spec JobSpec) (int64, error) {
		return 0, soxErr
	}

	var jobFires atomic.Int64
	p.SetOnJobComplete(func(string, string, int, int, float64, uuid.UUID, time.Time) {
		jobFires.Add(1)
	})
	var stateFires atomic.Int64
	p.SetOnStateChange(func() {
		stateFires.Add(1)
	})

	spec := JobSpec{
		SourceLibraryRel: "Music/Album/fail.flac",
		SourceAbsPath:    "/dev/null/missing",
		TargetSampleRate: 176400,
		TargetBits:       24,
		Quality:          QualityVeryHigh,
		OutputDir:        t.TempDir(),
	}
	if err := p.Enqueue(spec); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	// Bounded poll for the deferred `go fire()` goroutine to schedule
	// after failedCnt.Add(1). A fixed sleep (greptile P2 on PR #187)
	// could race on a loaded CI runner where the goroutine takes longer
	// than 50 ms to be picked up by the scheduler. Same deadline shape
	// the test's other poll uses.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if p.Stats().Failed >= 1 && stateFires.Load() >= 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	if got := jobFires.Load(); got != 0 {
		t.Errorf("onJobComplete fires on failure = %d, want 0 (success-only contract)", got)
	}
	if got := stateFires.Load(); got < 2 {
		t.Errorf("onStateChange fires = %d, want ≥ 2 — failure path must still notify state listeners", got)
	}
}

// TestPoolNilOnJobCompleteDoesNotPanic locks the back-compat shape:
// a pool built without SetOnJobComplete runs the success path
// without panicking on the nil slot.
func TestPoolNilOnJobCompleteDoesNotPanic(t *testing.T) {
	store := openTempStoreForPool(t)
	t.Cleanup(func() { _ = store.Close() })

	seedTrackForPool(t, store, "Music/Album/no_cb.flac")

	p := NewPool(store, 1, 4)
	t.Cleanup(p.Stop)

	p.fsyncFn = noopFsync
	p.runner = func(ctx context.Context, spec JobSpec) (int64, error) {
		return 1, nil
	}
	// Deliberately do NOT call SetOnJobComplete.

	spec := JobSpec{
		SourceLibraryRel: "Music/Album/no_cb.flac",
		SourceAbsPath:    "/dev/null/missing",
		TargetSampleRate: 176400,
		TargetBits:       24,
		Quality:          QualityVeryHigh,
		OutputDir:        t.TempDir(),
	}
	if err := p.Enqueue(spec); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	// Wait for success path to land.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if p.Stats().Done >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := p.Stats().Done; got < 1 {
		t.Fatalf("Stats.Done = %d, want ≥ 1", got)
	}
	// Pass condition: no panic under -race.
}

// TestPoolSetOnJobCompleteIsRaceSafe drives Set + concurrent worker
// fires to surface any unsynchronised access on the callback slot.
// Mirrors TestPoolSetOnStateChangeIsRaceSafe.
func TestPoolSetOnJobCompleteIsRaceSafe(t *testing.T) {
	store := openTempStoreForPool(t)
	t.Cleanup(func() { _ = store.Close() })

	// Seed parents for all 16 race-burst tracks so the success path
	// runs to completion under FK constraints.
	for i := 0; i < 16; i++ {
		seedTrackForPool(t, store, "Music/Race/"+strconv.Itoa(i)+".flac")
	}

	p := NewPool(store, 2, 32)
	t.Cleanup(p.Stop)

	p.fsyncFn = noopFsync
	p.runner = func(ctx context.Context, spec JobSpec) (int64, error) {
		return 1, nil
	}

	var fires atomic.Int64
	p.SetOnJobComplete(func(string, string, int, int, float64, uuid.UUID, time.Time) {
		fires.Add(1)
	})

	// Concurrent swap goroutine — alternates between the counting
	// callback and nil to drive every Set path.
	stopSwap := make(chan struct{})
	var swapDone sync.WaitGroup
	swapDone.Add(1)
	go func() {
		defer swapDone.Done()
		for i := 0; ; i++ {
			select {
			case <-stopSwap:
				return
			default:
			}
			if i%2 == 0 {
				p.SetOnJobComplete(nil)
			} else {
				p.SetOnJobComplete(func(string, string, int, int, float64, uuid.UUID, time.Time) {
					fires.Add(1)
				})
			}
		}
	}()

	// Enqueue a burst of jobs. Fail-fast on a real enqueue error
	// (ErrPoolClosed) — those are bugs and silently swallowing them
	// would let the test pass without exercising the workload.
	// ErrQueueFull is a legitimate transient outcome under bursty
	// load with a fixed queueCap, so it's expected and counted but
	// not fatal. (CodeRabbit Major round-3 on PR #187 — the prior
	// `_ = p.Enqueue(spec)` discarded all errors indiscriminately
	// and the count-shortfall check below couldn't tell a real
	// failure from "no jobs actually ran.")
	enqueued := 0
	for i := 0; i < 16; i++ {
		spec := JobSpec{
			SourceLibraryRel: "Music/Race/" + strconv.Itoa(i) + ".flac",
			SourceAbsPath:    "/dev/null/missing",
			TargetSampleRate: 176400,
			TargetBits:       24,
			Quality:          QualityVeryHigh,
			OutputDir:        t.TempDir(),
		}
		switch err := p.Enqueue(spec); err {
		case nil:
			enqueued++
		case ErrQueueFull:
			// Legitimate under-contention outcome with queueCap=32 +
			// concurrent dedup churn. Don't escalate; the workload
			// floor check below verifies we still ran enough jobs.
		case ErrPoolClosed:
			t.Fatalf("Enqueue %d returned ErrPoolClosed before Stop()", i)
		default:
			t.Fatalf("Enqueue %d unexpected error: %v", i, err)
		}
	}
	// We MUST have enqueued enough to actually exercise concurrent
	// fires. Less than half the burst lands → the race test would
	// pass under -race trivially. queueCap=32 + workers=2 means
	// every Enqueue should accept under normal conditions; this is
	// a regression alarm, not a tight bound.
	if enqueued < 8 {
		t.Fatalf("only %d of 16 jobs enqueued; race coverage too thin to be meaningful", enqueued)
	}

	// Wait for the queue to drain — bounded. The terminal-count
	// check now compares against `enqueued` (what actually
	// landed) rather than the literal 16; with the fail-fast
	// floor above, this guarantees we waited for the actual
	// workload to run, not just for the deadline to elapse.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if s := p.Stats(); int(s.Done+s.Failed) >= enqueued {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if s := p.Stats(); int(s.Done+s.Failed) < enqueued {
		t.Fatalf("workload didn't drain within deadline: enqueued=%d done=%d failed=%d",
			enqueued, s.Done, s.Failed)
	}
	close(stopSwap)
	swapDone.Wait()
	// Pass condition: no data race under -race; fires count is
	// non-deterministic (depends on which slot is set at fire time).
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

// seedTrackForPool inserts a minimal parent tracks row so a
// subsequent UpsertVariant on the same source path satisfies the
// `track_variants → tracks` foreign-key constraint. Tests that
// drive the pool's success path (i.e. exercise UpsertVariant) need
// this; failure-path tests don't.
func seedTrackForPool(t *testing.T, store *manifest.Store, path string) {
	t.Helper()
	tr := &manifest.Track{
		Path:    path,
		Size:    1,
		ModTime: time.Now().UTC(),
	}
	if err := store.UpsertTrack(context.Background(), tr); err != nil {
		t.Fatalf("seedTrackForPool UpsertTrack %q: %v", path, err)
	}
}
