package transcode

import (
	"context"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
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
	store    *manifest.Store
	workers  int
	jobs     chan poolJob
	queueCap int

	mu       sync.Mutex
	inflight map[string]struct{} // key = source_path + "|" + variant_id

	wg          sync.WaitGroup
	stopCtx     context.Context
	stopCancel  context.CancelFunc
	closed      atomic.Bool
	enqueuedCnt atomic.Uint64
	doneCnt     atomic.Uint64
	failedCnt   atomic.Uint64

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
	onJobComplete func(path, variantID string, sampleRate, bitsPerSample int, completedAt time.Time)

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
	publisherWG     sync.WaitGroup
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
		store:      store,
		workers:    workers,
		jobs:       make(chan poolJob, queueCap),
		queueCap:   queueCap,
		inflight:   make(map[string]struct{}),
		stopCtx:    stopCtx,
		stopCancel: stopCancel,
		runner:     RunSox,
		jobTimeout: defaultJobTimeout,
		// stateChange capacity 1 — coalesce. jobComplete capacity
		// 2×workers — fidelity buffer per the docstring on the
		// channel fields.
		stateChangeChan: make(chan struct{}, 1),
		jobCompleteChan: make(chan jobCompleteEvent, 2*workers),
	}
	for i := 0; i < workers; i++ {
		p.wg.Add(1)
		go p.workerLoop()
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
// **Race-safe vs Stop**: pre-fix, Stop() did `close(p.jobs)`
// concurrently with an Enqueue holding `inflight[dedup]` but not
// yet at the channel send — sending on a closed channel panics
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
	select {
	case p.jobs <- poolJob{spec: spec, dedup: dedup}:
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
func (p *Pool) notifyJobCompleteFn() func(string, string, int, int, time.Time) {
	p.jobCompleteMu.RLock()
	defer p.jobCompleteMu.RUnlock()
	return p.onJobComplete
}

// SetOnJobComplete wires (or rewires) the per-job completion callback
// (see Pool.onJobComplete docstring for ordering invariants). nil
// disables notification. Set-once at cmd/bridge wiring time; race-
// safe vs concurrent calls.
func (p *Pool) SetOnJobComplete(fn func(path, variantID string, sampleRate, bitsPerSample int, completedAt time.Time)) {
	p.jobCompleteMu.Lock()
	p.onJobComplete = fn
	p.jobCompleteMu.Unlock()
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
//  1. close(p.jobs) under p.mu so any in-flight Enqueue completes
//     its channel send (or its dedup-rollback) before close()
//     lands. See Enqueue's docstring for the full race trace.
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
	close(p.jobs)
	p.mu.Unlock()
	p.stopCancel()
	p.wg.Wait()
	// Workers are guaranteed exited; safe to close publisher inputs.
	close(p.stateChangeChan)
	close(p.jobCompleteChan)
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
	for stateCh != nil || jobCh != nil {
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
				fn(evt.path, evt.variantID, evt.sampleRate, evt.bitsPerSample, evt.completedAt)
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

// Stats returns the current snapshot. Safe to call concurrently
// with Enqueue / worker activity.
func (p *Pool) Stats() PoolStats {
	p.mu.Lock()
	inflight := len(p.inflight)
	p.mu.Unlock()
	return PoolStats{
		Workers:  p.workers,
		QueueCap: p.queueCap,
		QueueLen: len(p.jobs),
		Inflight: inflight,
		Enqueued: p.enqueuedCnt.Load(),
		Done:     p.doneCnt.Load(),
		Failed:   p.failedCnt.Load(),
	}
}

// workerLoop is the body of each pool worker goroutine. Pulls
// jobs off the channel and dispatches each through processJob.
//
// The pool's stopCtx is plumbed into RunSox via
// exec.CommandContext, so a Stop() while a sox is in flight
// SIGKILLs the process and the worker exits the loop cleanly.
func (p *Pool) workerLoop() {
	defer p.wg.Done()
	for job := range p.jobs {
		p.processJob(job)
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
func (p *Pool) processJob(job poolJob) {
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
		if r := recover(); r != nil {
			logger.Error("pool: recovered panic in job",
				"source", job.spec.SourceLibraryRel,
				"variantID", job.spec.VariantID(),
				"panic", r)
			if !p.closed.Load() {
				p.failedCnt.Add(1)
			}
		}
		if !released {
			p.releaseDedup(job.dedup)
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
				p.fireStateChange()
			}
		}
	}()

	// Cooperative stop check before spending CPU on a sox
	// invocation we'll just kill.
	if p.closed.Load() {
		p.releaseDedup(job.dedup)
		released = true
		return
	}

	jobCtx, cancel := context.WithTimeout(p.stopCtx, p.jobTimeout)
	defer cancel()

	size, err := p.runner(jobCtx, job.spec)
	if err != nil {
		// Drop cancellation noise — Stop() during graceful
		// shutdown shouldn't increment the failure counter or
		// fire the state-change callback.
		if !p.closed.Load() {
			p.failedCnt.Add(1)
			if errors.Is(jobCtx.Err(), context.DeadlineExceeded) {
				logger.Warn("pool: sox timed out",
					"source", job.spec.SourceLibraryRel,
					"timeout", p.jobTimeout,
					"err", err)
			} else {
				logger.Warn("pool: sox failed",
					"source", job.spec.SourceLibraryRel,
					"err", err)
			}
		}
		p.releaseDedup(job.dedup)
		released = true
		// Fire AFTER releaseDedup so the published snapshot
		// reflects the final state (job out of inflight) —
		// CodeRabbit on PR #136 caught the inconsistency vs
		// the success / store-failure branches which already
		// fire post-release.
		if !p.closed.Load() {
			p.fireStateChange()
		}
		return
	}

	_, settings := job.spec.SoxArgs()
	// Capture the completion instant ONCE so the DB row's CreatedAt
	// and the SSE event's CompletedAt point to the same wall-clock
	// moment. Without this, the row uses CreatedAtNow() (a separate
	// time.Now().UnixNano() call) and the event used a third call —
	// for a fast SQLite commit those values agree to the millisecond,
	// but iOS-side log correlation expects equality (Gemini MEDIUM
	// on PR #187).
	completedAt := time.Now().UTC()
	row := manifest.VariantRow{
		SourcePath:    job.spec.SourceLibraryRel,
		VariantID:     job.spec.VariantID(),
		SidecarPath:   job.spec.SidecarPath(),
		Format:        "flac",
		SampleRate:    job.spec.TargetSampleRate,
		BitsPerSample: job.spec.TargetBits,
		SizeBytes:     size,
		SourceMTimeNS: job.spec.SourceMTimeNS,
		SourceSize:    job.spec.SourceSize,
		SoxSettings:   settings,
		CreatedAt:     completedAt.UnixNano(),
	}
	if err := p.store.UpsertVariant(row); err != nil {
		p.failedCnt.Add(1)
		logger.Error("pool: store variant", "source", job.spec.SourceLibraryRel, "err", err)
		// Best-effort: remove the orphan sidecar so a
		// retry from a clean slate succeeds.
		_ = os.Remove(row.SidecarPath)
		p.releaseDedup(job.dedup)
		released = true
		// Worker isn't stalled by the publisher's CountVariants
		// DB query — Gemini high-severity review on PR #136. The
		// publisher consumes asynchronously on its own goroutine.
		p.fireStateChange()
		return
	}
	p.doneCnt.Add(1)
	p.releaseDedup(job.dedup)
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
		path:          job.spec.SourceLibraryRel,
		variantID:     job.spec.VariantID(),
		sampleRate:    job.spec.TargetSampleRate,
		bitsPerSample: job.spec.TargetBits,
		completedAt:   completedAt,
	})
	p.fireStateChange()
}

// releaseDedup drops the (source, variant) slot from the inflight
// set so a future Enqueue for the same pair can land. Must run on
// every job-completion path (success, failure, cancel).
func (p *Pool) releaseDedup(key string) {
	p.mu.Lock()
	delete(p.inflight, key)
	p.mu.Unlock()
}
