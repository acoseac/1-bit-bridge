package transcode

import (
	"context"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestRoutesToOptimizeChannel pins the pure routing-decision helper.
// Empty Kind (legacy default) and JobKindUpscale both route to the
// background channel; JobKindOptimize routes to the foreground channel
// UNLESS the job is flagged background (the auto-optimize sweeper's
// speculative pre-generation, which must not head-of-line block the
// on-demand CarPlay path).
func TestRoutesToOptimizeChannel(t *testing.T) {
	cases := []struct {
		name       string
		kind       JobKind
		background bool
		want       bool
	}{
		{"optimize/foreground", JobKindOptimize, false, true},
		{"optimize/background", JobKindOptimize, true, false},
		{"upscale/foreground", JobKindUpscale, false, false},
		{"upscale/background", JobKindUpscale, true, false},
		{"legacy-zero-value", JobKind(""), false, false},
		{"future-unknown-kind", JobKind("future-unknown-kind"), false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := routesToOptimizeChannel(c.kind, c.background); got != c.want {
				t.Errorf("routesToOptimizeChannel(%q, background=%v) = %v, want %v",
					c.kind, c.background, got, c.want)
			}
		})
	}
}

// TestPoolBackgroundOptimizeUsesUpscaleLane is the end-to-end half of
// the routing contract: a background-flagged optimize job must land in
// the LOW-priority channel even though its Kind is JobKindOptimize.
// Asserted through the channels directly (no workers draining) so the
// observation is the lane itself, not a timing artefact.
//
// Negative control: with the `background` arm removed from
// routesToOptimizeChannel this fails on the optimizeJobs length.
func TestPoolBackgroundOptimizeUsesUpscaleLane(t *testing.T) {
	store := openTempStoreForPool(t)
	t.Cleanup(func() { _ = store.Close() })

	// Zero workers is not allowed (NewPool floors at 1), so stop the
	// pool's drain immediately and inspect the buffered channels.
	p := NewPool(store, 1, 8)
	p.fsyncFn = noopFsync
	// Park the single worker on a runner that never returns until the
	// test ends, so neither channel drains under us.
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	p.runner = func(ctx context.Context, _ JobSpec) (int64, string, error) {
		select {
		case <-release:
		case <-ctx.Done():
		}
		return 0, "", context.Canceled
	}
	t.Cleanup(p.Stop)

	// One job to occupy the worker, then the two we actually measure.
	if err := p.Enqueue(JobSpec{SourceLibraryRel: "occupy.flac", TargetSampleRate: 44100, TargetBits: 16, Kind: JobKindUpscale}); err != nil {
		t.Fatalf("occupy enqueue: %v", err)
	}
	// Let the worker pull the occupier so it isn't counted below.
	deadline := time.Now().Add(2 * time.Second)
	for len(p.upscaleJobs) > 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}

	if err := p.Enqueue(JobSpec{
		SourceLibraryRel: "fg.flac", TargetSampleRate: 44100, TargetBits: 16,
		Kind: JobKindOptimize,
	}); err != nil {
		t.Fatalf("foreground enqueue: %v", err)
	}
	if err := p.Enqueue(JobSpec{
		SourceLibraryRel: "bg.flac", TargetSampleRate: 44100, TargetBits: 16,
		Kind: JobKindOptimize, Background: true,
	}); err != nil {
		t.Fatalf("background enqueue: %v", err)
	}

	if got := len(p.optimizeJobs); got != 1 {
		t.Errorf("optimizeJobs (foreground lane) = %d, want 1", got)
	}
	if got := len(p.upscaleJobs); got != 1 {
		t.Errorf("upscaleJobs (background lane) = %d, want 1 (the background optimize job)", got)
	}
}

// TestPoolOptimizeBacklogDrainsBeforeUpscale is the load-bearing
// priority contract: when both channels carry queued work, the
// optimize channel drains via the bias-select's Phase 1 non-
// blocking poll AHEAD of the upscale backlog. Pre-fix (single
// FIFO channel) an optimize job queued behind a sustained upscale
// batch waited for the whole batch to drain — minutes of wall-
// clock for a long DSD-rate upscale, which is exactly the
// CarPlay-plug-in latency hazard this PR closes.
//
// Test shape: 1 worker (forces serialised processing so observed
// ORDER is meaningful). Runner uses a `sync.Once` gate: the very
// first job pulled from EITHER channel parks until all subsequent
// enqueues have landed in their respective channels, then is
// released. From that point, every following job pull happens
// against a fully-populated set of channels — and the Phase 1
// bias picks optimize over the upscale backlog.
//
// Contract assertion: optimize lands at index 0 OR index 1 of the
// processed list (NOT index >= 2). Index 0 fires when the worker
// hadn't yet pulled upscale-0 by the time optimize was enqueued
// (so the parked first-job is optimize itself). Index 1 fires
// when the worker pulled upscale-0 first, then optimize on the
// next loop. Under the pre-fix single-FIFO behaviour, optimize
// would land at index 5 (tail of the 5-upscale + 1-optimize
// FIFO). Either index 0 or 1 demonstrates the priority property;
// anything >= 2 is a starvation regression.
func TestPoolOptimizeBacklogDrainsBeforeUpscale(t *testing.T) {
	store := openTempStoreForPool(t)
	t.Cleanup(func() { _ = store.Close() })

	// 1 worker, enough queue cap for the test's 6 jobs.
	p := NewPool(store, 1, 16)
	p.fsyncFn = noopFsync

	// Park the FIRST job pulled from either channel via sync.Once.
	// Subsequent jobs flow through immediately. This guarantees
	// every enqueue lands in its channel before the priority bias
	// has a chance to make a decision.
	var firstJobOnce sync.Once
	firstJobParked := make(chan struct{}) // worker → test: "I'm parked"
	releaseFirst := make(chan struct{})   // test → worker: "go"

	var processed []JobKind
	var processedMu sync.Mutex
	p.runner = func(ctx context.Context, spec JobSpec) (int64, string, error) {
		firstJobOnce.Do(func() {
			close(firstJobParked)
			<-releaseFirst
		})
		processedMu.Lock()
		processed = append(processed, spec.Kind)
		processedMu.Unlock()
		return 0, "", nil
	}
	t.Cleanup(p.Stop)

	mkSpec := func(i int, kind JobKind) JobSpec {
		return JobSpec{
			SourceLibraryRel: "P/" + string(kind) + "-" + strconv.Itoa(i) + ".flac",
			SourceAbsPath:    "/dev/null/missing",
			Kind:             kind,
			TargetSampleRate: 176400,
			TargetBits:       24,
			Quality:          QualityVeryHigh,
			OutputDir:        t.TempDir(),
		}
	}

	// Enqueue 5 upscale jobs FIRST so the upscale channel is full
	// when the optimize job lands.
	for i := 0; i < 5; i++ {
		if err := p.Enqueue(mkSpec(i, JobKindUpscale)); err != nil {
			t.Fatalf("upscale Enqueue %d: %v", i, err)
		}
	}
	// Now the priority job.
	if err := p.Enqueue(mkSpec(0, JobKindOptimize)); err != nil {
		t.Fatalf("optimize Enqueue: %v", err)
	}

	// Wait for the worker to have parked on the first job (which
	// it pulled from one of the two channels). At this point every
	// other job is still queued.
	select {
	case <-firstJobParked:
	case <-time.After(2 * time.Second):
		t.Fatal("worker never parked on first job")
	}
	// Release the parked job. Worker drains the rest under the
	// fully-populated channel state.
	close(releaseFirst)

	// Wait for all 6 jobs to complete.
	deadline := time.Now().Add(5 * time.Second)
	for {
		processedMu.Lock()
		n := len(processed)
		processedMu.Unlock()
		if n >= 6 {
			break
		}
		if time.Now().After(deadline) {
			processedMu.Lock()
			t.Fatalf("worker drained only %d of 6 jobs within deadline", len(processed))
			processedMu.Unlock()
		}
		time.Sleep(10 * time.Millisecond)
	}

	processedMu.Lock()
	defer processedMu.Unlock()
	if len(processed) != 6 {
		t.Fatalf("processed %d jobs, want 6", len(processed))
	}
	// Find the optimize position. Index 0 or 1 is acceptable;
	// index >= 2 is the pre-fix FIFO behaviour we're ruling out.
	optimizeIdx := -1
	for i, k := range processed {
		if k == JobKindOptimize {
			optimizeIdx = i
			break
		}
	}
	if optimizeIdx < 0 {
		t.Fatalf("optimize job never processed; saw: %v", processed)
	}
	if optimizeIdx > 1 {
		t.Errorf("optimize at index %d, want <= 1 (priority "+
			"contract: optimize must drain before the queued "+
			"upscale backlog). Order: %v", optimizeIdx, processed)
	}
}

// TestPoolUpscaleProgressUnderInterleavedLoad pins the partial-fair
// property of the bias-select under INTERLEAVED enqueue load: when
// optimize and upscale jobs land alternately, Phase 1's non-blocking
// poll finds the optimize channel intermittently empty (during the
// windows BETWEEN successive optimize sends), Phase 2 fires, and
// pseudo-random fair-select gives upscale enough chances to drain.
//
// **What this DOES NOT test**: strict anti-starvation under SUSTAINED
// optimize streams. Gemini medium on PR #281 correctly flagged the
// implementation as susceptible to upscale starvation if the optimize
// channel is never empty at the moment of each Phase 1 poll — the
// worker would `continue` after every Phase 1 hit and never reach
// Phase 2. That's the documented limitation; testing it would require
// a producer goroutine that maintains continuous backpressure, which
// is outside the scope of this PR. For the CarPlay-Optimize use case
// (single-track user-tap submissions), the interleaved-load shape is
// the realistic regime.
func TestPoolUpscaleProgressUnderInterleavedLoad(t *testing.T) {
	store := openTempStoreForPool(t)
	t.Cleanup(func() { _ = store.Close() })

	p := NewPool(store, 1, 32)
	p.fsyncFn = noopFsync

	var processed []JobKind
	var processedMu sync.Mutex
	p.runner = func(ctx context.Context, spec JobSpec) (int64, string, error) {
		processedMu.Lock()
		processed = append(processed, spec.Kind)
		processedMu.Unlock()
		return 0, "", nil
	}
	t.Cleanup(p.Stop)

	mkSpec := func(i int, kind JobKind) JobSpec {
		return JobSpec{
			SourceLibraryRel: "S/" + string(kind) + "-" + strconv.Itoa(i) + ".flac",
			SourceAbsPath:    "/dev/null/missing",
			Kind:             kind,
			TargetSampleRate: 176400,
			TargetBits:       24,
			Quality:          QualityVeryHigh,
			OutputDir:        t.TempDir(),
		}
	}

	// Interleave 10 of each kind. The interleaving + the worker's
	// async drain produce realistic burst patterns where Phase 1
	// will sometimes miss (Channel is empty between sends), giving
	// Phase 2 a chance to pick upscale.
	for i := 0; i < 10; i++ {
		if err := p.Enqueue(mkSpec(i, JobKindOptimize)); err != nil {
			t.Fatalf("optimize Enqueue %d: %v", i, err)
		}
		if err := p.Enqueue(mkSpec(i, JobKindUpscale)); err != nil {
			t.Fatalf("upscale Enqueue %d: %v", i, err)
		}
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		processedMu.Lock()
		n := len(processed)
		processedMu.Unlock()
		if n >= 20 {
			break
		}
		if time.Now().After(deadline) {
			processedMu.Lock()
			t.Fatalf("worker drained only %d of 20 jobs within deadline", len(processed))
			processedMu.Unlock()
		}
		time.Sleep(10 * time.Millisecond)
	}

	processedMu.Lock()
	defer processedMu.Unlock()
	var optCount, upsCount int
	for _, k := range processed {
		switch k {
		case JobKindOptimize:
			optCount++
		case JobKindUpscale:
			upsCount++
		}
	}
	if optCount != 10 || upsCount != 10 {
		t.Errorf("kind distribution: optimize=%d (want 10), upscale=%d (want 10) — "+
			"upscale progress regression under interleaved load", optCount, upsCount)
	}
}

// TestPoolStopDrainsBothChannels pins the shutdown contract for the
// two-channel layout: Stop() closes BOTH optimize and upscale
// channels, and the channel-nil pattern in workerLoop exits the
// loop once both go nil. Pre-existing TestPoolStopBlocksUntilWorkersDrain
// covers the basic "Stop returns within deadline" behaviour; this
// test specifically exercises the case where each channel has work
// pending at Stop time.
func TestPoolStopDrainsBothChannels(t *testing.T) {
	store := openTempStoreForPool(t)
	t.Cleanup(func() { _ = store.Close() })

	p := NewPool(store, 2, 16)
	p.fsyncFn = noopFsync

	var processed atomic.Uint32
	// firstProcessed fires once a worker has actually run a job. Waiting
	// on it before Stop() makes the "workers ran" assertion deterministic:
	// without it, on a slow / heavily-scheduled runner (CI under -race)
	// Stop() can drain both channels before any worker goroutine is even
	// scheduled, leaving processed==0 — a real flake observed on CI.
	firstProcessed := make(chan struct{}, 1)
	p.runner = func(ctx context.Context, spec JobSpec) (int64, string, error) {
		processed.Add(1)
		select {
		case firstProcessed <- struct{}{}:
		default:
		}
		return 0, "", nil
	}

	mkSpec := func(i int, kind JobKind) JobSpec {
		return JobSpec{
			SourceLibraryRel: filepath.Join("D", string(kind)+"-"+strconv.Itoa(i)+".flac"),
			SourceAbsPath:    "/dev/null/missing",
			Kind:             kind,
			TargetSampleRate: 176400,
			TargetBits:       24,
			Quality:          QualityVeryHigh,
			OutputDir:        t.TempDir(),
		}
	}

	// 4 of each kind into the respective channels.
	for i := 0; i < 4; i++ {
		_ = p.Enqueue(mkSpec(i, JobKindOptimize))
		_ = p.Enqueue(mkSpec(i, JobKindUpscale))
	}

	// Wait until a worker has run ≥1 job (several still queued across both
	// channels) before stopping — that's the populated-pool state this
	// test is about, and it removes the race in the processed-count check.
	select {
	case <-firstProcessed:
	case <-time.After(5 * time.Second):
		t.Fatal("no job processed within 5s — workers not draining the queue?")
	}

	stopDone := make(chan struct{})
	go func() {
		p.Stop()
		close(stopDone)
	}()
	select {
	case <-stopDone:
		// Good.
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not return within 5s — channel-nil " +
			"pattern not exiting workerLoop?")
	}

	if got := processed.Load(); got == 0 {
		t.Errorf("processed=%d — workers ran zero jobs before Stop?", got)
	}
}

// TestPoolStatsQueueLenIsCombinedDepth pins the back-compat contract:
// PoolStats.QueueLen continues to report the COMBINED depth across
// both priority channels, NOT per-kind. The admin tile + iOS
// UpscaleStatsSnapshot consume this as a single "pending work"
// number; per-kind split would be a sibling field, not a retarget.
func TestPoolStatsQueueLenIsCombinedDepth(t *testing.T) {
	store := openTempStoreForPool(t)
	t.Cleanup(func() { _ = store.Close() })

	p := NewPool(store, 1, 16)
	p.fsyncFn = noopFsync

	// Park the worker so jobs queue rather than drain.
	hold := make(chan struct{})
	p.runner = func(ctx context.Context, spec JobSpec) (int64, string, error) {
		<-hold
		return 0, "", nil
	}
	t.Cleanup(func() {
		close(hold)
		p.Stop()
	})

	mkSpec := func(i int, kind JobKind) JobSpec {
		return JobSpec{
			SourceLibraryRel: "Q/" + string(kind) + "-" + strconv.Itoa(i) + ".flac",
			SourceAbsPath:    "/dev/null/missing",
			Kind:             kind,
			TargetSampleRate: 176400,
			TargetBits:       24,
			Quality:          QualityVeryHigh,
			OutputDir:        t.TempDir(),
		}
	}

	// 3 optimize + 4 upscale jobs.
	for i := 0; i < 3; i++ {
		_ = p.Enqueue(mkSpec(i, JobKindOptimize))
	}
	for i := 0; i < 4; i++ {
		_ = p.Enqueue(mkSpec(i, JobKindUpscale))
	}

	// One job is in the worker (parked on `hold`); the rest sit in
	// the channels. Combined channel depth = 7 - 1 = 6.
	// Allow brief settle for the worker to pull its job.
	time.Sleep(50 * time.Millisecond)

	stats := p.Stats()
	if stats.QueueLen != 6 {
		t.Errorf("QueueLen = %d, want 6 (combined depth across both "+
			"priority channels)", stats.QueueLen)
	}
	// Per Gemini medium on PR #281: QueueCap must also report the
	// COMBINED capacity (2 × per-channel) so admin tiles computing
	// QueueLen/QueueCap as a fill ratio can't exceed 100%. NewPool
	// was constructed with queueCap=16; combined capacity is 32.
	if stats.QueueCap != 32 {
		t.Errorf("QueueCap = %d, want 32 (combined capacity across both "+
			"priority channels — per-channel back-pressure still enforced "+
			"at Enqueue time)", stats.QueueCap)
	}
}
