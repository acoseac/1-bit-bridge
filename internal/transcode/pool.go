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
	// polling. Callbacks are invoked synchronously on the worker
	// goroutine — keep them lightweight.
	stateChangeMu sync.RWMutex
	onStateChange func()

	// onJobComplete fires exactly once per successful job, AFTER
	// `manifest.Store.UpsertVariant` returns nil — i.e. AFTER the
	// SQLite transaction commits, so any consumer-triggered manifest
	// re-sync will observe the new `track_variants` row and the
	// bumped `tracks.indexed_at`. Nil when not wired. cmd/bridge
	// builds an `api.UpscaleCompleteEvent` from these primitives and
	// publishes it to the SSE broker on topic `"upscale.complete"`.
	// Worker invokes the callback in a fresh goroutine OUTSIDE
	// `p.mu` (see processJob), so a slow consumer can never back-
	// pressure the transcode pool — mirrors the onStateChange
	// pattern.
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
}

// poolJob is one transcode unit on the Pool's queue. Carries the
// JobSpec the worker will execute plus the dedup key so the
// completion path can drop the slot.
type poolJob struct {
	spec  JobSpec
	dedup string
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
	}
	for i := 0; i < workers; i++ {
		p.wg.Add(1)
		go p.workerLoop()
	}
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
	// at critical severity. Now: explicit unlock per branch, fire
	// in a goroutine OUTSIDE the lock so the publisher's DB query
	// (CountVariants in UpscaleStatsSnapshot) doesn't stall the
	// caller and the broker's own mutex can't cross-mutex couple
	// with this lock.
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
		fire := p.notifyStateChangeFn()
		p.mu.Unlock()
		if fire != nil {
			go fire()
		}
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
// context). The Pool can't be reused after Stop.
//
// Idempotent: calling Stop twice is safe (the underlying CancelFunc
// is itself idempotent).
//
// **Acquires `p.mu` before closing the channel** so any in-flight
// Enqueue completes its channel send (or its dedup-rollback)
// before close() lands. Without this, a concurrent Enqueue could
// pass its `closed` re-check, attempt to send, and hit a
// "send on closed channel" panic. See the comment on Enqueue
// for the full race trace.
func (p *Pool) Stop() {
	if p.closed.Swap(true) {
		return
	}
	p.mu.Lock()
	close(p.jobs)
	p.mu.Unlock()
	p.stopCancel()
	p.wg.Wait()
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
			// Match the synchronous error branches' shape: fire
			// notifyStateChangeFn AFTER releaseDedup so the published
			// snapshot reflects the final state (job out of inflight).
			// Without this, the panic-recovery path would silently
			// skip the notification — operators would see failedCnt
			// change in the next tick but SSE clients (admin UI,
			// iOS) wouldn't get an immediate push update like every
			// other terminal path produces. Skipped on graceful
			// shutdown to match the runner-error branch's gate.
			// (CodeRabbit + Gemini + Greptile concurring on PR #183.)
			if !p.closed.Load() {
				if fire := p.notifyStateChangeFn(); fire != nil {
					go fire()
				}
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
			if fire := p.notifyStateChangeFn(); fire != nil {
				go fire()
			}
		}
		return
	}

	_, settings := job.spec.SoxArgs()
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
		CreatedAt:     CreatedAtNow(),
	}
	if err := p.store.UpsertVariant(row); err != nil {
		p.failedCnt.Add(1)
		logger.Error("pool: store variant", "source", job.spec.SourceLibraryRel, "err", err)
		// Best-effort: remove the orphan sidecar so a
		// retry from a clean slate succeeds.
		_ = os.Remove(row.SidecarPath)
		p.releaseDedup(job.dedup)
		released = true
		// Async fire so the worker isn't stalled by the
		// publisher's CountVariants DB query — Gemini high-
		// severity review on PR #136. Caller never blocks on
		// the publish.
		if fire := p.notifyStateChangeFn(); fire != nil {
			go fire()
		}
		return
	}
	p.doneCnt.Add(1)
	p.releaseDedup(job.dedup)
	released = true
	// Per-job completion event fires AFTER UpsertVariant commits
	// (success branch above) and AFTER releaseDedup so the published
	// snapshot reflects the final state. Path field is the raw
	// `job.spec.SourceLibraryRel` — byte-identical to the manifest's
	// `Track.path`, which is what iOS keys on for its reverse index.
	// Reformatting / normalising here would break the iOS-side
	// constant-time path lookup. Invoked OUTSIDE p.mu / dedup lock
	// in a fresh goroutine so a slow SSE subscriber never back-
	// pressures the worker.
	if fireJob := p.notifyJobCompleteFn(); fireJob != nil {
		go fireJob(
			job.spec.SourceLibraryRel,
			job.spec.VariantID(),
			job.spec.TargetSampleRate,
			job.spec.TargetBits,
			time.Now().UTC(),
		)
	}
	if fire := p.notifyStateChangeFn(); fire != nil {
		go fire()
	}
}

// releaseDedup drops the (source, variant) slot from the inflight
// set so a future Enqueue for the same pair can land. Must run on
// every job-completion path (success, failure, cancel).
func (p *Pool) releaseDedup(key string) {
	p.mu.Lock()
	delete(p.inflight, key)
	p.mu.Unlock()
}
