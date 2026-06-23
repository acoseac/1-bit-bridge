package transcode

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/fsutil"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/metrics"
	"github.com/google/uuid"
)

// defaultJobTimeout caps a single transcode invocation. A typical
// 4-min FLAC upscale to 192/24 finishes in 10–30 s on commodity
// hardware; long classical movements top out around 3–4 min. 10
// minutes leaves generous headroom for legitimate long files while
// structurally bounding the slot leak from a corrupt header that
// puts sox into an indefinite spin. Files that genuinely need
// longer surface as failed jobs — operator-actionable, vs the
// prior silent deadlock.
//
// Per-pool override lives on `Pool.jobTimeout` so tests can shrink
// the deadline without mutating package-level global state (which
// would race with `t.Parallel()`). Matches the project's existing
// per-instance DI shape — see `Pool.runner` and `manifest.Store.now`.
const defaultJobTimeout = 10 * time.Minute

// Pool is the long-lived worker pool that hosts SoX conversions
// for the v1.2 PCM upscaling feature. Two consumers in production:
//
//  1. `bridge serve`: instantiates one Pool at startup (only when
//     `cfg.Upscale.Enabled == true` AND the sox-on-PATH probe
//     passes), attaches it to the api.Server, and feeds it with
//     `POST /v1/upscale` requests.
//  2. `bridge upscale` CLI: instantiates its own per-invocation
//     Pool with worker count from `--workers`. Same primitive,
//     different lifetime.
//
// The Pool's pending-job channel is bounded (`QueueCap`); enqueues
// are non-blocking via `select` + default → return ErrQueueFull
// instead of waiting. The HTTP handler maps that to a `503
// queue_full` response so a user spamming "Generate" on a 50k-
// track library bounces against a clean rejection rather than
// exhausting memory.
//
// **Dedup keyed on (source_path, variant_id)**: a duplicate
// enqueue while a job is already queued or running is a silent
// no-op. The hash set is mutex-guarded; reads happen on the HTTP
// handler goroutine, writes on the worker goroutines and the
// enqueue site. Same lock covers both — contention is bounded by
// the worker count.
type Pool struct {
	store   *manifest.Store
	workers int
	// Two-channel priority queue (PR-pending). `optimizeJobs` carries
	// foreground/latency-sensitive `JobKindOptimize` jobs (CarPlay
	// downsample variants generated on-demand from the iOS handheld
	// plug-in path); `upscaleJobs` carries background `JobKindUpscale`
	// + empty-Kind (legacy default) jobs. Worker drain biases toward
	// optimize via a select-default phase + fair fallback select so
	// neither channel can starve the other.
	//
	// Pre-fix this was a single `jobs chan poolJob` FIFO; a 100-job
	// batch upscale could head-of-line block a CarPlay request that
	// landed mid-batch. Channel-level priority eliminates that HOL
	// blocking; it does NOT preempt an in-flight `exec.CommandContext`
	// sox subprocess once spawned — that mitigation belongs in a
	// future PR layering OS-level `nice` / `ionice` / `SetPriorityClass`
	// on the subprocess.
	//
	// `queueCap` is applied per-channel: each holds up to `queueCap`
	// jobs before the non-blocking enqueue path returns ErrQueueFull.
	// The 503 HTTP semantics carry over independently for each kind.
	optimizeJobs chan poolJob
	upscaleJobs  chan poolJob
	queueCap     int

	mu       sync.Mutex
	inflight map[string]struct{} // key = source_path + "|" + variant_id

	wg          sync.WaitGroup
	stopCtx     context.Context
	stopCancel  context.CancelFunc
	closed      atomic.Bool
	enqueuedCnt atomic.Uint64
	doneCnt     atomic.Uint64
	failedCnt   atomic.Uint64

	// activeJobs[workerID] holds the job worker `workerID` is currently
	// processing (nil = idle). One atomic.Pointer slot per worker, sized
	// once at NewPool. Single-writer-per-slot (the owning worker) +
	// lock-free reads via ActiveWorkers() for the live SSE worker grid.
	// The stored *ActiveJob is IMMUTABLE after Store — a reader never sees
	// a torn struct, and the JSON stays byte-stable while a job runs (it
	// carries StartedAtUnixMs, NOT a ticking elapsed) so the upscale SSE
	// frame diff-suppresses between real job transitions.
	activeJobs []atomic.Pointer[ActiveJob]

	// onStateChange fires after every observable state transition
	// (job enqueued, completed, sox-failed, store-failed). Nil when
	// not wired — silently dropped, same back-compat shape as every
	// other optional Pool integration. cmd/bridge wires this to
	// publish a fresh `/v1/upscale/stats` snapshot to the SSE
	// broker so iOS clients see push-delivered updates instead of
	// polling.
	//
	// **Threading model**: invoked synchronously, serially, on the
	// SINGLE long-lived publisher goroutine (`runPublisher`) when
	// it drains `stateChangeChan`. Workers send signals via the
	// non-blocking `fireStateChange()` helper (capacity-1 buffer +
	// select-default = signals coalesce); the publisher then calls
	// this callback at most once per drained signal. Implications
	// for the wired callback: (a) keep it bounded — a slow callback
	// stalls subsequent state-change AND job-complete deliveries
	// because the same publisher serves both channels; (b) MUST
	// NOT re-acquire `p.mu` (publisher already holds no Pool locks
	// but `Stats() / UpscaleStatsSnapshot` reads p.mu, and a
	// callback that takes another mutex layered above p.mu could
	// reintroduce the cross-mutex coupling the publisher pattern
	// was designed to eliminate); (c) MUST tolerate Stop() ordering
	// — see Stop()'s docstring.
	stateChangeMu sync.RWMutex
	onStateChange func()

	// onJobComplete fires exactly once per successful job, AFTER
	// `manifest.Store.UpsertVariant` returns nil — i.e. AFTER the
	// SQLite transaction commits, so any consumer-triggered manifest
	// re-sync will observe the new `track_variants` row and the
	// bumped `tracks.indexed_at`. Nil when not wired. cmd/bridge
	// builds an `api.UpscaleCompleteEvent` from these primitives and
	// publishes it to the SSE broker on topic `"upscale.complete"`.
	//
	// **Threading model**: invoked synchronously, one-at-a-time, on
	// the SINGLE long-lived publisher goroutine (`runPublisher`) as
	// it drains `jobCompleteChan`. Workers send via the BLOCKING
	// `fireJobComplete()` helper (buffered cap=2×workers, no drops)
	// so every `upscale.complete` event reaches this callback —
	// load-bearing for the iOS reverse-index path-promotion
	// machinery that keys on per-event `path`/`variantID`. A full
	// buffer briefly stalls the next worker send (correct
	// backpressure: delaying the next sox job is better than losing
	// an event). Same callback-side rules as `onStateChange` —
	// don't block forever, don't re-acquire `p.mu`, respect Stop()
	// ordering. `publisherWG` ensures every buffered event is
	// drained before Stop returns.
	jobCompleteMu sync.RWMutex
	onJobComplete func(path, variantID string, sampleRate, bitsPerSample int, durationSeconds float64, batchID uuid.UUID, completedAt time.Time)

	// onJobFailed fires once per job that errored (sox failure, store
	// write failure, per-job timeout, panic-recovery). The Coordinator
	// (v1.3 batch.go) consumes this to bump `failed_files` on the
	// `upscale_batches` row keyed by batchID. Pre-existing
	// `onStateChange` carries only counter snapshots — without
	// per-job attribution the Coordinator cannot tell which batch
	// owned a failure.
	//
	// Threading model mirrors `onJobComplete`: invoked on the SINGLE
	// long-lived publisher goroutine (`runPublisher`) as it drains
	// `jobFailedChan`. Workers send via the BLOCKING `fireJobFailed`
	// helper (buffered cap=2×workers, no drops) — failure events
	// are NOT coalesce-able (each carries a unique path / errMsg the
	// admin Jobs page needs). A full buffer briefly stalls the next
	// worker send (correct backpressure: delaying the next sox job
	// is better than losing a failure event). Same callback-side
	// rules as onJobComplete — don't block forever, don't re-acquire
	// p.mu, respect Stop() ordering.
	jobFailedMu sync.RWMutex
	onJobFailed func(path, variantID, errMsg string, durationSeconds float64, batchID uuid.UUID, failedAt time.Time)

	// runner executes one transcode job under the supplied context.
	// Defaults to RunSox in NewPool; tests inject a hang-until-ctx-
	// cancelled stub to drive the per-job timeout branch without a
	// real sox process. Same DI shape `manifest.Store.now` uses for
	// the clock.
	runner func(ctx context.Context, spec JobSpec) (int64, error)

	// jobTimeout is the per-job deadline applied via context.WithTimeout
	// inside processJob. Defaults to defaultJobTimeout in NewPool;
	// tests override per-instance to drive the timeout branch in
	// milliseconds without racing other tests on a package-level var.
	jobTimeout time.Duration

	// fsyncFn flushes a freshly-written variant's file (and on POSIX
	// systems its parent directory) to stable storage before the
	// `UpsertVariant` DB row commits. Defaults to
	// `fsyncFileAndParent`; tests inject a stub to drive the
	// fsync-failure branch without touching the real filesystem.
	// Same DI shape `runner` uses for the sox subprocess.
	fsyncFn func(path string) error

	// Coalescing publisher (CLAUDE.md: "Bounded SSE publisher
	// goroutine for transcode events"). Replaces the prior pattern
	// of spawning a fresh `go fire()` goroutine per state transition,
	// which was unbounded under burst (a 500-clip enqueue storm
	// fanned ≥3000 ephemeral goroutines if the broker briefly
	// stalled). One long-lived publisher consumes both channels and
	// invokes the wired callbacks synchronously, one at a time.
	//
	// `stateChangeChan` cap=1: state-change events are coalesce-able
	// (the wired callback always reads a fresh snapshot, so any
	// missed signal is equivalent to one that arrived). Workers
	// non-blocking-send via select+default — drops are correct.
	//
	// `jobCompleteChan` cap=2×workers: `upscale.complete` events
	// are NOT coalesce-able (each carries a unique path/variantID
	// the iOS reverse index needs). Workers blocking-send for
	// fidelity; a full buffer briefly stalls the next send, which
	// is the correct backpressure (delaying the next sox job is
	// better than losing an event the iOS path-promotion depends
	// on).
	//
	// Deadlock-avoidance reasoning (preserves the invariant the
	// pre-fix `go fire()` was protecting): the publisher is a
	// SEPARATE goroutine; workers never hold p.mu while sending;
	// the publisher invokes the broker callback synchronously but
	// the broker takes its OWN mutex, not p.mu — so the prior
	// re-entrancy hazard (callback → UpscaleStatsSnapshot → Stats →
	// p.mu) cannot deadlock the worker. Stop() ordering is load-
	// bearing — see Stop's docstring.
	stateChangeChan chan struct{}
	jobCompleteChan chan jobCompleteEvent
	// jobFailedChan carries per-job failure events to the publisher.
	// Same cap=2×workers fidelity buffer as jobCompleteChan; same
	// blocking-send semantics. Pre-PR-3 the failure path only fired
	// `stateChange` (counter snapshot, no per-job attribution); the
	// v1.3 Coordinator needs the path + errMsg + batchID to mark
	// the right `upscale_batches.failed_files` slot.
	jobFailedChan chan jobFailedEvent
	publisherWG   sync.WaitGroup
}

// poolJob is one transcode unit on the Pool's queue. Carries the
// JobSpec the worker will execute plus the dedup key so the
// completion path can drop the slot.
type poolJob struct {
	spec  JobSpec
	dedup string
}

// jobCompleteEvent is the payload pushed onto Pool.jobCompleteChan
// by a worker that just successfully wrote a new variant row. The
// publisher consumes these and invokes the wired onJobComplete
// callback (which builds an api.UpscaleCompleteEvent and publishes
// it to the SSE broker on topic `"upscale.complete"`).
//
// Fields mirror the onJobComplete signature exactly so the publisher
// is a straight unwrap-and-call. `path` is byte-identical to the
// manifest's Track.path (what iOS keys on for its reverse index);
// don't reformat or normalise.
type jobCompleteEvent struct {
	path          string
	variantID     string
	sampleRate    int
	bitsPerSample int
	completedAt   time.Time
	// durationSeconds is the wall-clock cost of the successful sox
	// run (captured at processJob entry vs completion). Feeds the
	// Coordinator's rolling-throughput average; 0 on a graceful-
	// shutdown / closed-pool short-circuit where the job didn't
	// actually run. Per CodeRabbit high on PR #201: previously this
	// path passed 0 to `Coordinator.recordThroughput` and the
	// throughput window stayed empty even on a busy bridge.
	durationSeconds float64
	// batchID attributes this completion to one operator-initiated
	// batch (v1.3). Zero-value uuid.UUID for jobs submitted outside
	// the batch path — the Coordinator filters those out before
	// updating counters.
	batchID uuid.UUID
}

// jobFailedEvent is the per-batch failure-attribution payload. Fires
// once per job that failed sox / store-write / per-job timeout /
// panic-recovery. Mirrors jobCompleteEvent's batch-aware shape so
// the Coordinator can bump `failed_files` on the right row.
//
// `errMsg` is the redacted operator-facing reason (sox stderr, the
// store-write error string, or `"sox timed out after Ns"`). Multi-
// line tolerated; the Coordinator caps at ~4 KiB before writing to
// `upscale_batches.error`. `durationSeconds` feeds the rolling
// throughput average; non-zero for legitimate runs, ~0 for fast-
// fail paths (panic-recovery, closed-pool short-circuit).
type jobFailedEvent struct {
	path            string
	variantID       string
	errMsg          string
	durationSeconds float64
	failedAt        time.Time
	batchID         uuid.UUID
}

// ErrQueueFull is returned by Enqueue when the pending-job channel
// is at capacity. HTTP callers map this to a 503 `queue_full`
// response so the iOS toast can say "Queue full — wait for current
// conversions to finish, then try again."
var ErrQueueFull = errors.New("transcode pool queue is full")

// ErrPoolClosed is returned when Enqueue is called after Stop.
// Production consumers shouldn't see this — `bridge serve` only
// stops the pool during graceful shutdown, when no new requests
// are accepted. Tests use it.
var ErrPoolClosed = errors.New("transcode pool is closed")

// NewPool constructs a Pool sized at `workers` parallel sox
// processes with a `queueCap`-bounded pending-job channel. Starts
// `workers` goroutines immediately; they live until Stop is
// called. `store` is the SQLite-backed manifest store the
// completion path writes the row into.
//
// Caller is expected to have already verified sox is on PATH via
// PrecheckSox — the worker doesn't repeat the probe per job.
func NewPool(store *manifest.Store, workers, queueCap int) *Pool {
	if workers < 1 {
		workers = 1
	}
	if queueCap < 1 {
		queueCap = 1
	}
	stopCtx, stopCancel := context.WithCancel(context.Background())
	p := &Pool{
		store:        store,
		workers:      workers,
		optimizeJobs: make(chan poolJob, queueCap),
		upscaleJobs:  make(chan poolJob, queueCap),
		queueCap:     queueCap,
		inflight:     make(map[string]struct{}),
		activeJobs:   make([]atomic.Pointer[ActiveJob], workers),
		stopCtx:      stopCtx,
		stopCancel:   stopCancel,
		runner:       RunSox,
		jobTimeout:   defaultJobTimeout,
		fsyncFn:      fsyncFileAndParent,
		// stateChange capacity 1 — coalesce. jobComplete capacity
		// 2×workers — fidelity buffer per the docstring on the
		// channel fields.
		stateChangeChan: make(chan struct{}, 1),
		jobCompleteChan: make(chan jobCompleteEvent, 2*workers),
		jobFailedChan:   make(chan jobFailedEvent, 2*workers),
	}
	for i := 0; i < workers; i++ {
		p.wg.Add(1)
		go p.workerLoop(i) // stable worker ID for the activeJobs slot
	}
	p.publisherWG.Add(1)
	go p.runPublisher()
	return p
}

// Enqueue submits a JobSpec to the worker pool. Non-blocking: if
// the queue is full, returns ErrQueueFull immediately. Dedup is
// silent — a duplicate (same source_path + variant_id) returns
// nil without taking a slot. After Stop, returns ErrPoolClosed.
//
// **Race-safe vs Stop**: pre-fix, Stop() did `close(p.optimizeJobs)
// / close(p.upscaleJobs)` concurrently with an Enqueue holding
// `inflight[dedup]` but not yet at the channel send — sending on a
// closed channel panics
// (Gemini high + Qodo bug 1 + CodeRabbit echo on PR #109). Fix:
// the channel-send branch runs INSIDE `p.mu` and Stop() takes
// the same mutex before close. The mutex serialises the two
// operations cheaply (the send is non-blocking via the select +
// default, so the lock window stays in microseconds), and the
// re-checked `closed` flag inside the lock catches a Stop that
// won the race for the mutex but hadn't yet closed the channel.
func (p *Pool) Enqueue(spec JobSpec) error {
	if p.closed.Load() {
		return ErrPoolClosed
	}
	dedup := spec.SourceLibraryRel + "|" + spec.VariantID()
	// `defer` is intentionally NOT used here for unlock — pre-fix
	// (PR #136 first revision) the success path used
	// `defer p.mu.Unlock()` AND `defer fire()`. Go's LIFO defer
	// order made `fire()` run BEFORE the unlock, deadlocking when
	// the wired callback called UpscaleStatsSnapshot which takes
	// p.mu.Lock() inside Stats(). CodeRabbit + Gemini caught this
	// at critical severity. Now: explicit unlock per branch, then
	// fireStateChange() — the publisher consumes the signal
	// asynchronously on its own goroutine, so the wired broker
	// callback (with its own mutex) can't cross-mutex couple with
	// p.mu via the publish path.
	p.mu.Lock()
	if p.closed.Load() {
		p.mu.Unlock()
		return ErrPoolClosed
	}
	if _, ok := p.inflight[dedup]; ok {
		p.mu.Unlock()
		return nil // already queued or running
	}
	// Optimistic insert: claim the slot before trying the channel
	// send so two concurrent enqueues for the same job can't both
	// pass the dedup check. If the channel send fails, we roll
	// back below.
	p.inflight[dedup] = struct{}{}
	// Route per JobKind. `JobKindOptimize` → optimizeJobs (foreground);
	// every other kind (`JobKindUpscale` AND empty-Kind legacy default)
	// → upscaleJobs (background). Routing is pinned by a pure helper
	// so the test suite can assert the routing contract without
	// spinning a Pool.
	jobsChan := p.upscaleJobs
	if routesToOptimizeChannel(spec.Kind) {
		jobsChan = p.optimizeJobs
	}
	select {
	case jobsChan <- poolJob{spec: spec, dedup: dedup}:
		p.enqueuedCnt.Add(1)
		p.mu.Unlock()
		// Non-blocking send to the publisher; safe to invoke after
		// unlock OR (in principle) under the lock since the send
		// can't park. Kept after unlock to minimise lock window.
		p.fireStateChange()
		return nil
	default:
		// Roll back the optimistic claim — couldn't fit the job
		// after all.
		delete(p.inflight, dedup)
		p.mu.Unlock()
		return ErrQueueFull
	}
}

// routesToOptimizeChannel is the pure routing decision used by Enqueue
// to pick the destination channel for a given JobKind. Pulled out as a
// static helper so the test suite can pin the routing contract without
// the rest of the Pool machinery, mirroring the friendlyErrorMessage /
// isRenderGap test-affordance convention used elsewhere in the project.
//
// **Contract**: `JobKindOptimize` is the only kind that routes to the
// optimize/foreground channel. Every other kind (`JobKindUpscale` AND
// the empty-Kind zero value) routes to upscale/background. The empty
// default exists because many legacy test fixtures + the
// pre-batch-feature `bridge upscale` CLI invoke `JobSpec{...}` without
// setting Kind explicitly.
func routesToOptimizeChannel(kind JobKind) bool {
	return kind == JobKindOptimize
}

// notifyStateChangeFn returns the current onStateChange callback
// under the stateChangeMu read lock so a concurrent
// SetOnStateChange can swap it without racing. Returns nil when
// not wired — caller checks before invoking.
func (p *Pool) notifyStateChangeFn() func() {
	p.stateChangeMu.RLock()
	defer p.stateChangeMu.RUnlock()
	return p.onStateChange
}

// SetOnStateChange wires (or rewires) the callback fired after every
// observable state transition. nil disables notification (back-compat
// for tests / non-broker deployments). Called once during cmd/bridge
// wiring after the broker is up; subsequent calls are race-safe but
// in practice this is set-once.
func (p *Pool) SetOnStateChange(fn func()) {
	p.stateChangeMu.Lock()
	p.onStateChange = fn
	p.stateChangeMu.Unlock()
}

// notifyJobCompleteFn returns the current onJobComplete callback
// under the jobCompleteMu read lock so a concurrent SetOnJobComplete
// can swap it without racing. Returns nil when not wired — caller
// checks before invoking. Mirrors notifyStateChangeFn.
//
// PR 3 (v1.3) extended the signature with `batchID` so the publisher
// can attribute the completion to the right `upscale_batches` row.
// Pre-batch callers (legacy `POST /v1/upscale`, `bridge upscale`
// CLI) leave the field at zero-value uuid.UUID; the Coordinator
// filters those out before updating counters.
func (p *Pool) notifyJobCompleteFn() func(string, string, int, int, float64, uuid.UUID, time.Time) {
	p.jobCompleteMu.RLock()
	defer p.jobCompleteMu.RUnlock()
	return p.onJobComplete
}

// SetOnJobComplete wires (or rewires) the per-job completion callback
// (see Pool.onJobComplete docstring for ordering invariants). nil
// disables notification. Set-once at cmd/bridge wiring time; race-
// safe vs concurrent calls.
//
// PR 3 (v1.3) extended the signature with `batchID`. Existing
// consumers in cmd/bridge/main.go pass it through to the
// Coordinator's job-complete handler; pre-v1.3 callers that don't
// care about batch attribution can ignore the new parameter.
func (p *Pool) SetOnJobComplete(fn func(path, variantID string, sampleRate, bitsPerSample int, durationSeconds float64, batchID uuid.UUID, completedAt time.Time)) {
	p.jobCompleteMu.Lock()
	p.onJobComplete = fn
	p.jobCompleteMu.Unlock()
}

// notifyJobFailedFn mirrors notifyJobCompleteFn for the failure side.
// Returns nil when no callback is wired.
func (p *Pool) notifyJobFailedFn() func(string, string, string, float64, uuid.UUID, time.Time) {
	p.jobFailedMu.RLock()
	defer p.jobFailedMu.RUnlock()
	return p.onJobFailed
}

// SetOnJobFailed wires the per-job failure callback. nil disables.
// See Pool.onJobFailed docstring for threading invariants. Set-once
// at cmd/bridge wiring time alongside SetOnJobComplete.
func (p *Pool) SetOnJobFailed(fn func(path, variantID, errMsg string, durationSeconds float64, batchID uuid.UUID, failedAt time.Time)) {
	p.jobFailedMu.Lock()
	p.onJobFailed = fn
	p.jobFailedMu.Unlock()
}

// Stop signals the workers to drain the queue and exit. Blocks
// until every in-flight conversion completes (either through
// success, failure, or sox-process kill on the cancelled
// context) AND the publisher has drained its remaining buffered
// events. The Pool can't be reused after Stop.
//
// Idempotent: calling Stop twice is safe (the underlying CancelFunc
// is itself idempotent).
//
// **Shutdown ordering is load-bearing** — the sequence below
// prevents BOTH "send on closed channel" panics AND publisher
// deadlocks:
//
//  1. close(p.optimizeJobs) + close(p.upscaleJobs) under p.mu so any
//     in-flight Enqueue completes its channel send (or its dedup-
//     rollback) before either close() lands. See Enqueue's docstring
//     for the full race trace. Both channels share the same p.mu so
//     ordering between them inside the lock is irrelevant.
//  2. stopCancel() so any sox subprocess waiting on the per-job
//     context dies promptly.
//  3. p.wg.Wait() — block until every worker goroutine has
//     returned. After this point, no worker can send to
//     stateChangeChan or jobCompleteChan.
//  4. close() both publisher channels — safe ONLY because step 3
//     guaranteed no more sends are possible.
//  5. p.publisherWG.Wait() — let the publisher drain its remaining
//     buffered events (especially `upscale.complete` events the
//     iOS path-promotion path depends on), observe both channels
//     closed, and return.
//
// The publisher does NOT exit on stopCtx.Done() — workers blocking-
// send to jobCompleteChan, and an early publisher exit would
// deadlock the worker on a buffer-full send.
func (p *Pool) Stop() {
	if p.closed.Swap(true) {
		return
	}
	p.mu.Lock()
	close(p.optimizeJobs)
	close(p.upscaleJobs)
	p.mu.Unlock()
	p.stopCancel()
	p.wg.Wait()
	// Workers are guaranteed exited; safe to close publisher inputs.
	close(p.stateChangeChan)
	close(p.jobCompleteChan)
	close(p.jobFailedChan)
	p.publisherWG.Wait()
}

// fireStateChange enqueues a state-change signal for the publisher.
// Non-blocking: the channel is capacity 1, and a full buffer means a
// signal is already pending — dropping the new one is correct because
// the wired callback always reads a fresh snapshot.
//
// Safe to call from any goroutine including under p.mu (the send is
// non-blocking, so it can't park while holding the lock — preserves
// the "no callbacks under p.mu" invariant the prior `go fire()`
// pattern was protecting).
//
// Returns false only when the buffer was full and the signal was
// dropped. Useful for tests that want to assert coalescing
// behaviour; production callers ignore the return.
func (p *Pool) fireStateChange() bool {
	select {
	case p.stateChangeChan <- struct{}{}:
		return true
	default:
		return false
	}
}

// fireJobComplete enqueues a per-job completion event for the
// publisher. Blocking send — `upscale.complete` events are NOT
// coalesce-able (each carries a unique path/variantID the iOS
// reverse index needs); a full buffer briefly stalls the next
// send, which is the correct backpressure (delaying the next
// sox job is better than losing an event).
//
// MUST NOT be called under p.mu — a buffer-full stall while
// holding p.mu would block any Stats() / Enqueue() that needs
// the lock, which in turn could deadlock the broker callback if
// it fans out via UpscaleStatsSnapshot. Workers call this from
// processJob, never holding p.mu (releaseDedup runs first).
func (p *Pool) fireJobComplete(evt jobCompleteEvent) {
	p.jobCompleteChan <- evt
}

// fireJobFailed mirrors fireJobComplete for the failure side. Same
// blocking-send fidelity contract — failure events carry unique
// path / errMsg / batchID that the Coordinator needs to mark the
// right `upscale_batches.failed_files` slot AND surface the error
// reason to the admin Jobs page.
//
// MUST NOT be called under p.mu (same reasoning as fireJobComplete).
// Workers call this from processJob's failure branches after
// releaseDedup.
func (p *Pool) fireJobFailed(evt jobFailedEvent) {
	p.jobFailedChan <- evt
}

// fireJobFailedFor builds + emits a jobFailedEvent for job with errMsg,
// stamping failedAt ONCE and deriving the duration from that single
// clock read (timestamp/duration parity — Gemini r4 F28). Centralises
// the event construction the sox / fsync / store / panic-recovery
// branches share so it isn't copy-pasted four times (Sonar new-code
// duplication). Each branch's differing surrounding logic (dedup
// release, state-change fire, logging, return) stays at the call site.
func (p *Pool) fireJobFailedFor(job poolJob, errMsg string, startedAt time.Time) {
	failedAt := time.Now().UTC()
	p.fireJobFailed(jobFailedEvent{
		path:            job.spec.SourceLibraryRel,
		variantID:       job.spec.VariantID(),
		errMsg:          errMsg,
		durationSeconds: failedAt.Sub(startedAt).Seconds(),
		failedAt:        failedAt,
		batchID:         job.spec.BatchID,
	})
}

// runPublisher is the single long-lived goroutine that consumes
// stateChangeChan + jobCompleteChan and invokes the wired callbacks
// synchronously. Replaces the prior pattern of one ephemeral
// goroutine per state transition (unbounded under burst).
//
// Exits ONLY when both input channels are closed AND drained — NOT
// on stopCtx.Done(). Workers blocking-send to jobCompleteChan; if
// the publisher exited on stopCtx cancellation while a worker was
// mid-send, the worker would hang forever and p.wg.Wait() in Stop
// would deadlock. Stop() closes the channels AFTER waiting for
// workers, which is the correct synchronization point.
//
// Per-iteration channel-nil pattern: when one channel is observed
// closed (`ok == false`), it's nil'd out in the local copy so
// subsequent select iterations can't hot-spin on the always-ready
// closed-channel case. Once both channels are nil, the for-loop
// guard exits.
func (p *Pool) runPublisher() {
	defer p.publisherWG.Done()
	stateCh := (<-chan struct{})(p.stateChangeChan)
	jobCh := (<-chan jobCompleteEvent)(p.jobCompleteChan)
	failCh := (<-chan jobFailedEvent)(p.jobFailedChan)
	for stateCh != nil || jobCh != nil || failCh != nil {
		select {
		case _, ok := <-stateCh:
			if !ok {
				stateCh = nil
				continue
			}
			if fn := p.notifyStateChangeFn(); fn != nil {
				fn()
			}
		case evt, ok := <-jobCh:
			if !ok {
				jobCh = nil
				continue
			}
			if fn := p.notifyJobCompleteFn(); fn != nil {
				fn(evt.path, evt.variantID, evt.sampleRate, evt.bitsPerSample, evt.durationSeconds, evt.batchID, evt.completedAt)
			}
		case evt, ok := <-failCh:
			if !ok {
				failCh = nil
				continue
			}
			if fn := p.notifyJobFailedFn(); fn != nil {
				fn(evt.path, evt.variantID, evt.errMsg, evt.durationSeconds, evt.batchID, evt.failedAt)
			}
		}
	}
}

// Stats returns a snapshot of pool counters for /v1/health
// observability or tests. Numbers are monotonic per process
// lifetime; restarting bridge serve resets them.
type PoolStats struct {
	Workers  int
	QueueCap int
	QueueLen int
	Inflight int
	Enqueued uint64
	Done     uint64
	Failed   uint64
}

// ActiveJob is the immutable snapshot of the job a worker is currently
// running, stored in Pool.activeJobs[workerID]. Built once at job start
// and never mutated (a future phase field would allocate + Store a fresh
// one), so concurrent ActiveWorkers() reads are torn-state-free without a
// lock. Carries StartedAtUnixMs (not a ticking elapsed) so the worker
// grid's wire shape stays stable while a job runs — the client ticks the
// elapsed display locally.
type ActiveJob struct {
	SourceRel        string  // SourceLibraryRel — the display path
	SourceSampleRate int     // Hz, 0 if unknown
	SourceBits       int     // bit depth, 0 if unknown
	TargetSampleRate int     // Hz
	TargetBits       int     // 16/24/32
	Quality          Quality // resampler quality (very-high / high / medium)
	Kind             JobKind // upscale / optimize
	StartedAtUnixMs  int64
}

// ActiveJobView is the per-worker value snapshot ActiveWorkers returns —
// one entry per worker slot, Busy=false for idle workers. Plain value
// type safe to hand to the admin / SSE layer.
type ActiveJobView struct {
	WorkerID         int    `json:"workerId"`
	Busy             bool   `json:"busy"`
	SourceRel        string `json:"sourceRel,omitempty"`
	SourceSampleRate int    `json:"sourceSampleRate,omitempty"`
	SourceBits       int    `json:"sourceBits,omitempty"`
	TargetSampleRate int    `json:"targetSampleRate,omitempty"`
	TargetBits       int    `json:"targetBits,omitempty"`
	Quality          string `json:"quality,omitempty"`
	Kind             string `json:"kind,omitempty"`
	StartedAtUnixMs  int64  `json:"startedAtUnixMs,omitempty"`
}

// ActiveWorkers returns one snapshot per worker slot (WorkerID 0..N-1),
// Busy=false for idle workers. Lock-free: each slot is an atomic.Pointer
// read; the *ActiveJob behind it is immutable. Order is stable (slot
// index), so the SSE worker grid renders deterministically.
func (p *Pool) ActiveWorkers() []ActiveJobView {
	out := make([]ActiveJobView, len(p.activeJobs))
	for i := range p.activeJobs {
		v := ActiveJobView{WorkerID: i}
		if aj := p.activeJobs[i].Load(); aj != nil {
			v.Busy = true
			v.SourceRel = aj.SourceRel
			v.SourceSampleRate = aj.SourceSampleRate
			v.SourceBits = aj.SourceBits
			v.TargetSampleRate = aj.TargetSampleRate
			v.TargetBits = aj.TargetBits
			v.Quality = string(aj.Quality)
			v.Kind = string(aj.Kind)
			v.StartedAtUnixMs = aj.StartedAtUnixMs
		}
		out[i] = v
	}
	return out
}

// DropInflight removes entries from the dedup `inflight` map whose
// source_path satisfies the supplied predicate. Returns the count
// dropped.
//
// Used by the variant-delete handler (DELETE /v1/upscale/variants)
// and the integrity watcher to make sure a re-submission of an
// upscale request for a deleted path doesn't no-op against the
// stale dedup slot — without dropping the entry, a follow-up
// Enqueue would silently coalesce against an in-flight worker
// that's about to write a sidecar the caller intends to delete.
//
// Does NOT cancel in-flight workers — there is no per-job
// cancellation primitive (workers run under the pool's stopCtx
// only). The unlink race is the caller's problem; the caller
// removes the sidecar file BEFORE the worker can write it, and if
// a worker beats the unlink the next `--gc` reverse pass / the
// integrity watcher reaps the orphan within ≤1 h. Document the
// race shape at the call site, not here.
//
// Lock discipline: `p.mu` held only for the iteration window.
// Predicate is called synchronously under the lock; predicate
// authors keep predicates allocation-light (no DB calls, no map
// lookups on shared state). Caller proceeds to any I/O / SSE
// publishes AFTER this method returns, not under p.mu.
//
// Map key parsing: keys are `source_path + "|" + variant_id` per
// the Enqueue contract. Splits on the FIRST `|` via
// strings.IndexByte (no SplitN allocation per iteration) and
// passes only the source_path segment to the predicate. The
// `|` byte is reserved (paths never contain it in practice, and
// the manifest scanner rejects upserts with malformed paths at
// the boundary); a defensive missing-pipe entry is left untouched
// rather than logged — it's never legitimately present.
func (p *Pool) DropInflight(matches func(sourcePath string) bool) int {
	if matches == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	dropped := 0
	for key := range p.inflight {
		pipe := strings.IndexByte(key, '|')
		if pipe < 0 {
			continue
		}
		if matches(key[:pipe]) {
			delete(p.inflight, key)
			dropped++
		}
	}
	return dropped
}

// Stats returns the current snapshot. Safe to call concurrently
// with Enqueue / worker activity.
//
// **QueueLen AND QueueCap are both COMBINED across the two priority
// channels** so the ratio `QueueLen / QueueCap` stays bounded by 1.0
// for monitoring tools that compute a fill-percentage. Pre-fix the
// snapshot reported a per-channel QueueCap alongside a combined
// QueueLen, which produced ratios >100% whenever both channels were
// partially full — caught by Gemini medium on PR #281.
//
// **Per-channel back-pressure is still enforced** at Enqueue time:
// each channel independently has `p.queueCap` slots, and each
// independently triggers `ErrQueueFull` when full. The doubling
// here is presentational — it gives consumers a single combined
// capacity number to divide against the combined depth.
//
// If a future surface needs per-channel split (e.g. an admin
// "optimize queue depth" indicator), extend PoolStats with sibling
// fields rather than retargeting either of the existing combined
// values — back-compat consumers depend on QueueLen + QueueCap
// staying ratio-coherent.
func (p *Pool) Stats() PoolStats {
	p.mu.Lock()
	inflight := len(p.inflight)
	p.mu.Unlock()
	return PoolStats{
		Workers:  p.workers,
		QueueCap: 2 * p.queueCap,
		QueueLen: len(p.optimizeJobs) + len(p.upscaleJobs),
		Inflight: inflight,
		Enqueued: p.enqueuedCnt.Load(),
		Done:     p.doneCnt.Load(),
		Failed:   p.failedCnt.Load(),
	}
}

// workerLoop is the body of each pool worker goroutine. Pulls
// jobs off the two-channel priority queue and dispatches each
// through processJob.
//
// **Bias-select pattern** (PR-pending): Phase 1 polls the optimize
// channel with `select { ... default: }` to drain any pending
// foreground/CarPlay backlog FIRST and `continue` back to the top
// of the loop if anything was there. Phase 2 fires only when
// optimize was empty at the moment of the Phase 1 poll, and fairly
// selects across whichever channels are still active.
//
// **Anti-starvation is partial, NOT strict** — caught by Gemini
// medium on PR #281. Under a SUSTAINED stream of optimize jobs
// (one always pending at the moment of each Phase 1 poll), the
// worker will continuously hit the `continue` after the Phase 1
// case and never reach Phase 2 — upscale CAN starve in that regime.
// Phase 2's fair select only buys progress for the upscale channel
// during the BRIEF windows when the optimize channel happens to
// drain to empty between worker iterations.
//
// For the CarPlay-Optimize use case this is acceptable: optimize
// jobs are user-driven (one iOS tap per request, single-track at
// a time), so a sustained-stream regime that fully starves upscale
// is not a real scenario. If a future workload (e.g. an automated
// CarPlay-prep batch from an iOS device) does produce sustained
// optimize streams, the next iteration is a weighted scheduler
// (e.g. "every Nth iteration, force a Phase 2 path") rather than
// trying to fix this within the current bias-select shape.
//
// **Channel-nil pattern** for clean shutdown: when Stop() closes a
// channel, the local copy (optCh or upsCh) is set to nil. A nil
// channel's `select` case blocks forever — Go-idiomatic — so the
// surviving channel's case stays live and the loop exits cleanly
// once both are nil. Mirrors the same pattern `runPublisher` uses
// for stateChangeChan / jobCompleteChan / jobFailedChan.
//
// The pool's stopCtx is plumbed into RunSox via
// exec.CommandContext, so a Stop() while a sox is in flight
// SIGKILLs the process and the worker exits the loop cleanly.
func (p *Pool) workerLoop(workerID int) {
	defer p.wg.Done()
	optCh := (<-chan poolJob)(p.optimizeJobs)
	upsCh := (<-chan poolJob)(p.upscaleJobs)
	for optCh != nil || upsCh != nil {
		// Phase 1: bias toward optimize if its channel is active.
		// `select` with a `default` arm is non-blocking — if no
		// optimize job is pending, fall through to Phase 2.
		if optCh != nil {
			select {
			case job, ok := <-optCh:
				if !ok {
					optCh = nil
					continue
				}
				p.processJob(workerID, job)
				continue
			default:
			}
		}
		// Phase 2: fair select across active channels. Reaches here
		// only when Phase 1's non-blocking poll found the optimize
		// channel empty (or already nil'd after close). If both
		// channels are active and both have jobs at this point,
		// Go's runtime picks pseudo-randomly — that gives upscale
		// a chance to make progress during the windows when
		// optimize happens to drain. See the docstring for the
		// honest sustained-stream caveat.
		select {
		case job, ok := <-optCh:
			if !ok {
				optCh = nil
				continue
			}
			p.processJob(workerID, job)
		case job, ok := <-upsCh:
			if !ok {
				upsCh = nil
				continue
			}
			p.processJob(workerID, job)
		}
	}
}

// processJob runs one job to completion (or per-job timeout). Lives
// in its own method so `defer cancel()` on the per-job timeout
// context releases at the end of THIS job rather than accumulating
// until workerLoop exits — running for the lifetime of the worker
// would leak a pending timer per processed job.
//
// Shutdown gating uses `p.closed.Load()`, NOT `p.stopCtx.Err()`.
// Stop() flips `p.closed` BEFORE it cancels `p.stopCtx`, so during
// the gap between those two operations a worker that pulled a
// buffered job sees `stopCtx.Err() == nil` even though Stop has
// been called — the suppression check would falsely classify the
// graceful-shutdown error as a real failure (CodeRabbit on PR #162).
// Reading the atomic flag instead closes the window. The flag is
// monotonic (false→true, never reverses), so a single read at each
// branch suffices.
//
// Error branches mirror the pre-timeout shape 1:1: when the SERVER
// is stopping (`p.closed.Load()`) we suppress both the
// failure-counter increment AND the state-change fire — graceful-
// shutdown noise. When the server is up, every error path bumps
// `failedCnt` and fires the callback after `releaseDedup`, including
// the new per-job timeout branch (logged distinctly so operators
// can tell a hung-sox kill from an internal sox failure).
func (p *Pool) processJob(workerID int, job poolJob) {
	// `startedAt` feeds durationSeconds on every failure / completion
	// path so the Coordinator's rolling-throughput average sees the
	// wall-clock cost of each job regardless of which exit path is
	// taken. Captured once before any I/O so panic-recovery has a
	// meaningful value too.
	startedAt := time.Now().UTC()

	// Panic safety: a panic in p.runner (sox subprocess plumbing) or
	// p.store.UpsertVariant (SQLite write) would otherwise (a) leak
	// the (source, variant) dedup slot for the lifetime of the
	// process — effectively blacklisting that variant from re-
	// scheduling until restart — AND (b) crash the worker goroutine,
	// reducing pool capacity until restart. The recover here contains
	// the panic to this single job; the `released` flag preserves
	// PR #136's documented release-then-fire ordering on the normal
	// paths (the deferred cleanup runs ONLY when a normal path didn't
	// get a chance to release first). The recovered panic is logged
	// + bumps `failedCnt` so it's observable in the pool stats and
	// admin UI; the worker stays alive to handle the next job.
	released := false
	defer func() {
		panicVal := recover()
		if panicVal != nil {
			logger.Error("pool: recovered panic in job",
				"path", job.spec.SourceLibraryRel,
				"variantID", job.spec.VariantID(),
				"panic", panicVal)
			if !p.closed.Load() {
				p.failedCnt.Add(1)
				metrics.UpscaleJobsCompletedTotal.WithLabelValues("failure").Inc()
			}
		}
		if !released {
			p.finishJob(workerID, job.dedup)
			// Match the synchronous error branches' shape: fire the
			// state-change AFTER releaseDedup so the published
			// snapshot reflects the final state (job out of
			// inflight). Without this, the panic-recovery path
			// would silently skip the notification — operators would
			// see failedCnt change in the next tick but SSE clients
			// wouldn't get an immediate push update like every
			// other terminal path produces. Skipped on graceful
			// shutdown to match the runner-error branch's gate.
			// (CodeRabbit + Gemini + Greptile concurring on PR #183.)
			if !p.closed.Load() {
				// Panic-recovery failure event — surfaces the
				// recovered panic to the Coordinator so the
				// containing batch's `failed_files` advances and
				// the admin Jobs page renders the same outcome
				// it does for sox / store failures. Inject the
				// recovered panic value into errMsg so the admin
				// Jobs page shows the root cause instead of a
				// generic string (already logged above; panic
				// strings are short). Gemini r4.
				errMsg := "panic recovered in worker"
				if panicVal != nil {
					errMsg = fmt.Sprintf("panic recovered in worker: %v", panicVal)
				}
				p.fireJobFailedFor(job, errMsg, startedAt)
				p.fireStateChange()
			}
		}
	}()

	// Cooperative stop check before spending CPU on a sox
	// invocation we'll just kill.
	if p.closed.Load() {
		p.finishJob(workerID, job.dedup)
		released = true
		return
	}

	// Publish this worker's active job for the live grid — AFTER the
	// stop check so an immediately-abandoned job never flashes as active.
	// Immutable after Store; every terminal path below clears it via
	// finishJob (which runs BEFORE that path's fireStateChange, so the
	// published snapshot shows the worker idle, not stale-active).
	p.activeJobs[workerID].Store(&ActiveJob{
		SourceRel:        job.spec.SourceLibraryRel,
		SourceSampleRate: job.spec.SourceSampleRate,
		SourceBits:       job.spec.SourceBits,
		TargetSampleRate: job.spec.TargetSampleRate,
		TargetBits:       job.spec.TargetBits,
		Quality:          job.spec.Quality,
		Kind:             job.spec.Kind,
		StartedAtUnixMs:  startedAt.UnixMilli(),
	})

	jobCtx, cancel := context.WithTimeout(p.stopCtx, p.jobTimeout)
	defer cancel()

	size, err := p.runner(jobCtx, job.spec)
	if err != nil {
		// Drop cancellation noise — Stop() during graceful
		// shutdown shouldn't increment the failure counter or
		// fire the state-change callback.
		if !p.closed.Load() {
			p.failedCnt.Add(1)
			metrics.UpscaleJobsCompletedTotal.WithLabelValues("failure").Inc()
			if errors.Is(jobCtx.Err(), context.DeadlineExceeded) {
				logger.Warn("pool: sox timed out",
					"path", job.spec.SourceLibraryRel,
					"timeout", p.jobTimeout,
					"err", err)
			} else {
				logger.Warn("pool: sox failed",
					"path", job.spec.SourceLibraryRel,
					"err", err)
			}
		}
		p.finishJob(workerID, job.dedup)
		released = true
		// Fire AFTER releaseDedup so the published snapshot
		// reflects the final state (job out of inflight) —
		// CodeRabbit on PR #136 caught the inconsistency vs
		// the success / store-failure branches which already
		// fire post-release.
		if !p.closed.Load() {
			errMsg := redactSoxErr(err.Error(), job.spec)
			if errors.Is(jobCtx.Err(), context.DeadlineExceeded) {
				errMsg = "sox timed out after " + p.jobTimeout.String()
			}
			p.fireJobFailedFor(job, errMsg, startedAt)
			p.fireStateChange()
		}
		return
	}

	_, settings := job.spec.SoxArgs()
	sidecarPath := job.spec.SidecarPath()

	// Durability: flush the freshly-renamed sidecar (and its parent
	// directory entry on POSIX) BEFORE committing the
	// `track_variants` row that points at it. Without this barrier,
	// a power-loss between the DB commit and the kernel's flush of
	// the rename would leave iOS clients pointed at a non-durable
	// file. Recovery from the inverse ordering (fsync-then-commit)
	// is clean: a crash post-fsync but pre-commit means the next
	// manifest scan re-transcodes — wasted work, but no torn state.
	//
	// On fsync failure we run the same release-and-fire path as a
	// store failure (best-effort sidecar cleanup so a retry hits a
	// clean slate, jobFailed event, releaseDedup, fireStateChange).
	// Same shutdown gate as the UpsertVariant branch.
	if err := p.fsyncFn(sidecarPath); err != nil {
		if !p.closed.Load() {
			p.failedCnt.Add(1)
			metrics.UpscaleJobsCompletedTotal.WithLabelValues("failure").Inc()
			logger.Error("pool: fsync sidecar", "path", job.spec.SourceLibraryRel, "err", err)
			_ = os.Remove(sidecarPath)
			p.fireJobFailedFor(job, "fsync sidecar: "+err.Error(), startedAt)
		}
		p.finishJob(workerID, job.dedup)
		released = true
		if !p.closed.Load() {
			p.fireStateChange()
		}
		return
	}

	// Capture the completion instant ONCE so the DB row's CreatedAt
	// and the SSE event's CompletedAt point to the same wall-clock
	// moment. Without this, the row uses CreatedAtNow() (a separate
	// time.Now().UnixNano() call) and the event used a third call —
	// for a fast SQLite commit those values agree to the millisecond,
	// but iOS-side log correlation expects equality (Gemini MEDIUM
	// on PR #187).
	//
	// Captured AFTER fsync (not before) so `completedAt` and the
	// resulting `durationSeconds` include the fsync wall-clock time.
	// Pre-fix the timestamp captured before fsync excluded that
	// window — misleading on slow disks where fsync is a meaningful
	// share of per-job latency. Gemini MEDIUM on PR #251.
	completedAt := time.Now().UTC()
	row := manifest.VariantRow{
		SourcePath:    job.spec.SourceLibraryRel,
		VariantID:     job.spec.VariantID(),
		SidecarPath:   sidecarPath,
		Format:        "flac",
		SampleRate:    job.spec.TargetSampleRate,
		BitsPerSample: job.spec.TargetBits,
		SizeBytes:     size,
		SourceMTimeNS: job.spec.SourceMTimeNS,
		SourceSize:    job.spec.SourceSize,
		SoxSettings:   settings,
		CreatedAt:     completedAt.UnixNano(),
	}
	// Use jobCtx, NOT p.stopCtx — the per-job timeout
	// (defaultJobTimeout = 10 min) bounds the DB write the
	// same way it bounds the sox process. p.stopCtx only
	// fires at shutdown, so a SQLite write that hangs
	// (extremely rare but possible under deep contention)
	// would otherwise pin a worker indefinitely. CodeRabbit
	// Major on PR #216.
	if err := p.store.UpsertVariant(jobCtx, row); err != nil {
		// Suppress failure-counter increments + logging + event
		// firing during graceful shutdown — `jobCtx` is derived
		// from `p.stopCtx`, so `Stop()` cancels in-flight DB
		// writes mid-flight. Without the gate, every worker that
		// was holding a write at shutdown emits a noisy
		// "store variant: context canceled" line + fires a
		// failure event to the Coordinator, which then marks
		// the owning batch as having a failed file even though
		// the bridge is just shutting down. Mirror of the
		// `p.closed.Load()` gate around RunSox failure earlier
		// in this function. Gemini Medium on PR #217.
		if !p.closed.Load() {
			p.failedCnt.Add(1)
			metrics.UpscaleJobsCompletedTotal.WithLabelValues("failure").Inc()
			logger.Error("pool: store variant", "path", job.spec.SourceLibraryRel, "err", err)
			// Best-effort: remove the orphan sidecar so a
			// retry from a clean slate succeeds.
			_ = os.Remove(row.SidecarPath)
			// Surface store-side failures to the Coordinator
			// too — admin Jobs page distinguishes them from
			// sox failures via the errMsg prefix.
			p.fireJobFailedFor(job, "store variant: "+err.Error(), startedAt)
		}
		// Release the dedup slot BEFORE publishing the state change
		// so the publisher's snapshot reflects the post-failure
		// `inflight` set, not a transient state still holding this
		// failed job. Mirrors the success branch's
		// `releaseDedup → fireStateChange` ordering (documented at
		// the per-job completion comment below) — CodeRabbit Minor
		// on PR #217 caught the inconsistency. The previous shape
		// fired the SSE first while the failed job was still in
		// `p.inflight`, briefly publishing a stale snapshot that
		// iOS clients then had to reconcile away on the next tick.
		p.finishJob(workerID, job.dedup)
		released = true
		if !p.closed.Load() {
			// Worker isn't stalled by the publisher's CountVariants
			// DB query — Gemini high-severity review on PR #136. The
			// publisher consumes asynchronously on its own goroutine.
			p.fireStateChange()
		}
		return
	}
	p.doneCnt.Add(1)
	metrics.UpscaleJobsCompletedTotal.WithLabelValues("success").Inc()
	dur := time.Since(startedAt).Seconds()
	metrics.UpscaleDurationHist.Observe(dur)
	metrics.UpscaleDurationWindow.Observe(dur)
	p.finishJob(workerID, job.dedup)
	released = true
	// Per-job completion event fires AFTER UpsertVariant commits
	// (success branch above) and AFTER releaseDedup so the
	// published snapshot reflects the final state. Path field is
	// the raw `job.spec.SourceLibraryRel` — byte-identical to the
	// manifest's `Track.path`, which is what iOS keys on for its
	// reverse index. Reformatting / normalising here would break
	// the iOS-side constant-time path lookup. Invoked OUTSIDE
	// p.mu / dedup lock — fireJobComplete blocking-sends to the
	// publisher's bounded channel, which provides backpressure
	// without losing events.
	p.fireJobComplete(jobCompleteEvent{
		path:            job.spec.SourceLibraryRel,
		variantID:       job.spec.VariantID(),
		sampleRate:      job.spec.TargetSampleRate,
		bitsPerSample:   job.spec.TargetBits,
		completedAt:     completedAt,
		durationSeconds: completedAt.Sub(startedAt).Seconds(),
		batchID:         job.spec.BatchID,
	})
	p.fireStateChange()
}

// redactSoxErr strips absolute filesystem paths and tempfile names
// from sox stderr before the error string lands in
// `upscale_batches.error` (read by the admin Jobs page) and the
// SSE event payload. The bridge privacy contract bans surfacing
// absolute library paths; the library-relative path is already
// carried on the event's `path` field, so the error message only
// needs the sox-internal reason (codec mismatch, header
// corruption, etc.).
//
// Two redaction passes:
//
//  1. If `spec.SourceAbsPath` is set AND it contains
//     `spec.SourceLibraryRel` as a suffix (the normal case in
//     production: absolute path = `<root>/<libraryRel>`), every
//     occurrence of the absolute path in sox stderr is replaced
//     by the library-relative form. This covers the most common
//     sox failure shape: `sox FAIL formats: can't open input file
//     '/Users/alice/Music/…': Not a directory`.
//
//  2. Output sidecar path scrubbing — sox stderr can carry the
//     `<sidecar>.tmp` output path too. Strip everything up to
//     and including the OutputDir prefix so only the sidecar
//     basename remains.
//
// `exit status N: ` / `sox FAIL ` / `sox WARN ` / `sox: ` leading
// prefixes are also trimmed so the admin Jobs page surfaces the
// actual reason.
//
// Cap at 4 KiB — runaway sox stderr (e.g. corrupt MP4 dumping
// hex into stderr) shouldn't bloat the upscale_batches row.
func redactSoxErr(s string, spec JobSpec) string {
	// Pass 1: scrub absolute source path → library-relative form.
	if spec.SourceAbsPath != "" {
		s = strings.ReplaceAll(s, spec.SourceAbsPath, spec.SourceLibraryRel)
	}
	// Pass 2: scrub OutputDir prefix from sidecar paths. We strip
	// the directory prefix only — the sidecar basename is opaque
	// hash + variant ID, no operator-identifying information.
	if spec.OutputDir != "" {
		s = strings.ReplaceAll(s, spec.OutputDir+"/", "")
		s = strings.ReplaceAll(s, spec.OutputDir+`\`, "")
	}
	// Pass 3: drop leading prefixes the sox runner / exec wrapper
	// adds.
	for _, prefix := range []string{"sox FAIL ", "sox WARN ", "sox: ", "exit status "} {
		if strings.HasPrefix(s, prefix) {
			s = strings.TrimPrefix(s, prefix)
			break
		}
	}
	const maxErrBytes = 4096
	if len(s) > maxErrBytes {
		// Trim at most a partial trailing rune after the cut — NOT
		// "until the whole string validates": sox stderr can carry
		// interior binary garbage, and a validate-the-world loop would
		// discard everything after the first bad byte (plus rescan
		// O(N²)). Interior invalid bytes are left as-is (JSON encodes
		// them as U+FFFD). Gemini HIGH on PR #375; keep in lockstep
		// with tailscale.trimErr's twin.
		s = fsutil.TrimPartialTrailingRune(s[:maxErrBytes])
		s += "…(truncated)"
	}
	return s
}

// finishJob clears worker `workerID`'s active-job slot AND releases the
// (source, variant) dedup slot — the two cleanups a job's terminal path
// must do together, BEFORE it fires its state-change. Clearing the slot
// before fireStateChange is load-bearing: a bare top-level `defer
// Store(nil)` would (LIFO) run AFTER the body's explicit fireStateChange
// and publish a snapshot still showing the just-finished worker as active
// until the next tick. Store(nil) on an already-nil slot (the
// cooperative-stop path runs before the slot is set) is a harmless no-op.
func (p *Pool) finishJob(workerID int, dedup string) {
	p.activeJobs[workerID].Store(nil)
	p.releaseDedup(dedup)
}

// releaseDedup drops the (source, variant) slot from the inflight
// set so a future Enqueue for the same pair can land. Must run on
// every job-completion path (success, failure, cancel).
func (p *Pool) releaseDedup(key string) {
	p.mu.Lock()
	delete(p.inflight, key)
	p.mu.Unlock()
}
