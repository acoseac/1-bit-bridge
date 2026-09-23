package analyze

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/fsutil"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// defaultJobTimeout caps a single analysis invocation. Decoding a long
// classical movement to 48 kHz mono + bucketing tops out well under a
// minute on commodity hardware; 10 min is generous headroom that still
// structurally bounds the worker-slot leak from a corrupt header that
// puts sox into an indefinite spin (the file surfaces as a failed job,
// not a silent deadlock).
const defaultJobTimeout = 10 * time.Minute

// ErrQueueFull is returned by Enqueue when the bounded pending-job
// channel is at capacity. The `bridge analyze` driver and any HTTP
// trigger map it to a clean rejection rather than blocking.
var ErrQueueFull = errors.New("analyze pool queue is full")

// ErrPoolClosed is returned by Enqueue after Stop.
var ErrPoolClosed = errors.New("analyze pool is closed")

// Pool is the long-lived single-FIFO worker pool that runs offline
// audio analysis. Unlike internal/transcode's two-channel priority
// pool, analysis is purely background work, so a plain FIFO is the
// right shape — no CarPlay-latency bias to defend.
//
// The pending-job channel is bounded (queueCap); Enqueue is
// non-blocking (select + default → ErrQueueFull). Dedup keys on the
// source library-relative path (one waveform per source), so a
// duplicate enqueue while a job is queued or running is a silent no-op.
// A job stops being "queued or running" at the same instant it is
// counted done or failed, never later: see finishJob.
type Pool struct {
	store    *manifest.Store
	workers  int
	jobs     chan poolJob
	queueCap int

	// mu guards the dedup set and the three counters as ONE state. Enqueue
	// claims a path and counts it in one critical section, finishJob
	// releases it and counts the outcome in another, and Stats reads all of
	// them under the same lock, so no snapshot shows a job both in flight
	// and counted, or in neither place. Plain integers rather than atomics
	// on purpose: an increment written outside the lock is then a data race
	// the race detector reports, not an atomic that compiles and quietly
	// reopens the window finishJob closes.
	mu          sync.Mutex
	inflight    map[string]struct{} // key = source library-relative path
	enqueuedCnt uint64
	doneCnt     uint64
	failedCnt   uint64

	wg         sync.WaitGroup
	stopCtx    context.Context
	stopCancel context.CancelFunc
	closed     atomic.Bool

	// Injectable seams (set via PoolOption before workers start so
	// there's no data race with the worker goroutines). Production uses
	// the defaults wired in NewPool.
	runner     func(ctx context.Context, spec AnalyzeSpec) (Result, error)
	jobTimeout time.Duration
	fsyncFn    func(path string) error
	now        func() time.Time

	// Coalescing state-change publisher: a single long-lived goroutine
	// drains a cap-1 channel and invokes the wired callback. Nothing in
	// cmd/bridge wires one today: /v1/analysis/stats and the console's
	// analysis card read Stats() when asked, and the SSE topics are the
	// transcode pool's. Workers non-blocking-send signals; a full buffer
	// means a signal is already pending, so dropping the new one is
	// correct (the callback always reads a fresh snapshot).
	stateChangeMu   sync.RWMutex
	onStateChange   func()
	stateChangeChan chan struct{}
	publisherWG     sync.WaitGroup
}

type poolJob struct {
	spec  AnalyzeSpec
	dedup string
}

// PoolOption customises a Pool at construction (before workers start).
type PoolOption func(*Pool)

// WithRunner overrides the analysis runner (tests inject a stub to
// avoid spawning sox). Production uses RunAnalysis.
func WithRunner(fn func(ctx context.Context, spec AnalyzeSpec) (Result, error)) PoolOption {
	return func(p *Pool) { p.runner = fn }
}

// WithFsync overrides the durability-barrier fsync (tests inject a
// no-op or a failure stub).
func WithFsync(fn func(path string) error) PoolOption {
	return func(p *Pool) { p.fsyncFn = fn }
}

// WithJobTimeout overrides the per-job deadline (tests shrink it to
// drive the timeout branch).
func WithJobTimeout(d time.Duration) PoolOption {
	return func(p *Pool) {
		if d > 0 {
			p.jobTimeout = d
		}
	}
}

// WithClock overrides the wall clock used for the analysis row's
// created_at (tests inject a deterministic clock).
func WithClock(now func() time.Time) PoolOption {
	return func(p *Pool) {
		if now != nil {
			p.now = now
		}
	}
}

// NewPool constructs and starts a Pool with `workers` parallel decoders
// and a `queueCap`-bounded pending-job channel. Caller is expected to
// have verified sox is on PATH (transcode.PrecheckSox) — the worker
// doesn't repeat the probe per job.
func NewPool(store *manifest.Store, workers, queueCap int, opts ...PoolOption) *Pool {
	if workers < 1 {
		workers = 1
	}
	if queueCap < 1 {
		queueCap = 1
	}
	stopCtx, stopCancel := context.WithCancel(context.Background())
	p := &Pool{
		store:           store,
		workers:         workers,
		jobs:            make(chan poolJob, queueCap),
		queueCap:        queueCap,
		inflight:        make(map[string]struct{}),
		stopCtx:         stopCtx,
		stopCancel:      stopCancel,
		runner:          RunAnalysis,
		jobTimeout:      defaultJobTimeout,
		fsyncFn:         fsutil.FsyncFileAndParent,
		now:             time.Now,
		stateChangeChan: make(chan struct{}, 1),
	}
	for _, opt := range opts {
		opt(p)
	}
	for i := 0; i < workers; i++ {
		p.wg.Add(1)
		go p.workerLoop()
	}
	p.publisherWG.Add(1)
	go p.runPublisher()
	return p
}

// Enqueue submits a spec to the pool. Non-blocking; ErrQueueFull when
// the channel is full, nil (silent no-op) on a duplicate, ErrPoolClosed
// after Stop. Both the jobs send and the state-change signal run under
// p.mu; Stop acquires p.mu before closing the jobs channel and only
// closes stateChangeChan afterward (post wg.Wait), so neither send can
// race a close and panic.
func (p *Pool) Enqueue(spec AnalyzeSpec) error {
	if p.closed.Load() {
		return ErrPoolClosed
	}
	dedup := spec.SourceLibraryRel
	p.mu.Lock()
	if p.closed.Load() {
		p.mu.Unlock()
		return ErrPoolClosed
	}
	if _, ok := p.inflight[dedup]; ok {
		p.mu.Unlock()
		return nil // already queued or running
	}
	p.inflight[dedup] = struct{}{} // optimistic claim; rolled back on full
	select {
	case p.jobs <- poolJob{spec: spec, dedup: dedup}:
		p.enqueuedCnt++
		// fireStateChange BEFORE the unlock — mirrors internal/transcode
		// Pool. Stop() closes stateChangeChan only after acquiring p.mu
		// (to close jobs) + wg.Wait, so a send under the lock strictly
		// happens-before the close; a post-unlock send could race Stop
		// and panic on the closed channel. Enqueue is the only
		// fireStateChange caller not bounded by wg.Wait (workers are).
		// Non-blocking cap-1 send can't park under the lock.
		p.fireStateChange()
		p.mu.Unlock()
		return nil
	default:
		delete(p.inflight, dedup)
		p.mu.Unlock()
		return ErrQueueFull
	}
}

// SetOnStateChange wires (or rewires) the callback fired after every
// observable state transition. nil disables. Set-once at cmd/bridge
// wiring time; race-safe.
func (p *Pool) SetOnStateChange(fn func()) {
	p.stateChangeMu.Lock()
	p.onStateChange = fn
	p.stateChangeMu.Unlock()
}

func (p *Pool) notifyStateChangeFn() func() {
	p.stateChangeMu.RLock()
	defer p.stateChangeMu.RUnlock()
	return p.onStateChange
}

// fireStateChange enqueues a coalescing state-change signal. Safe under
// p.mu (non-blocking send can't park). Returns false when the buffer
// was full and the signal was dropped (used by tests).
func (p *Pool) fireStateChange() bool {
	select {
	case p.stateChangeChan <- struct{}{}:
		return true
	default:
		return false
	}
}

// runPublisher drains state-change signals and invokes the wired
// callback, one at a time, until the channel is closed (Stop). Kept off
// the worker goroutines so a slow broker callback can't stall a decode.
func (p *Pool) runPublisher() {
	defer p.publisherWG.Done()
	for range p.stateChangeChan {
		if fn := p.notifyStateChangeFn(); fn != nil {
			fn()
		}
	}
}

// Stop drains the queue, kills any in-flight sox, and blocks until every
// worker AND the publisher have exited. Idempotent. Ordering is
// load-bearing: close(jobs) under p.mu (so an in-flight Enqueue can't
// send on a closed channel), cancel stopCtx (kill sox), wg.Wait (no
// more sends possible), then close + drain the publisher channel.
func (p *Pool) Stop() {
	if p.closed.Swap(true) {
		return
	}
	p.mu.Lock()
	close(p.jobs)
	p.mu.Unlock()
	p.stopCancel()
	p.wg.Wait()
	close(p.stateChangeChan)
	p.publisherWG.Wait()
}

func (p *Pool) workerLoop() {
	defer p.wg.Done()
	for job := range p.jobs {
		p.processJob(job)
	}
}

// jobOutcome is the counter finishJob adds a job to. outcomeUncounted is a
// job the pool was shutting down under: its path must still be released,
// but it is neither a success nor a failure of the source.
type jobOutcome int

const (
	outcomeUncounted jobOutcome = iota
	outcomeDone
	outcomeFailed
)

// finishJob is the last thing that happens to every job, however it went:
// it releases the path's dedup slot and adds the job to its outcome counter
// in ONE critical section, under the p.mu that Enqueue's dedup check and
// Stats both take.
//
// One step, because each half answers a question somebody acts on, and
// splitting them opens a window whichever order they run in. Counting first
// is what this pool used to do, with the failure's strike and WARN written
// in between: a caller that saw the count and re-enqueued the path, which is
// what a retry is, landed on a path the job still held, and Enqueue's nil
// for a duplicate dropped the retry without a trace (the #986 CI failure).
// Releasing first leaves a snapshot between the two steps that shows the job
// nowhere, neither in flight nor counted.
//
// It runs AFTER the job's own bookkeeping (the strike and its WARN, or the
// row and the cleared marker), so a count also means those have landed.
// Releasing the path any earlier would let a retry run while the previous
// attempt was still writing, and a strike landing after the retry's success
// is a verdict against a file that has just analysed cleanly.
func (p *Pool) finishJob(job poolJob, outcome jobOutcome) {
	p.mu.Lock()
	delete(p.inflight, job.dedup)
	switch outcome {
	case outcomeDone:
		p.doneCnt++
	case outcomeFailed:
		p.failedCnt++
	}
	p.mu.Unlock()
}

// processJob runs one job: decode + waveform via the runner, fsync the
// sidecar, then commit the analysis row. Lives in its own method so the
// per-job timeout context's `defer cancel()` releases per job rather
// than accumulating for the worker's lifetime.
//
// Every way out ends in the ONE deferred finishJob, a panic included: the
// recover contains it to this job, counts it as a failure, and keeps the
// worker alive, and the release means the path is not blacklisted until
// restart. The exits used to carry their own release-and-count tails, and
// every one of them counted first. One exit point is what stops the next
// path added here from getting the order wrong again. Each path records its
// outcome where it learns it, and finishJob applies it once that path's
// bookkeeping is done.
//
// Shutdown gating reads p.closed (flipped before stopCtx is cancelled) so
// a graceful-shutdown error isn't miscounted as a real failure. It is read
// at those decision points and never after the release: `bridge analyze`
// answers an idle pool by calling Stop, so a re-read there would un-count
// the last job of the run.
func (p *Pool) processJob(job poolJob) {
	outcome := outcomeUncounted
	defer func() {
		if r := recover(); r != nil {
			logger.Error("analyze: recovered panic in job",
				"path", job.spec.SourceLibraryRel, "panic", r)
			if !p.closed.Load() {
				outcome = outcomeFailed
			}
		}
		p.finishJob(job, outcome)
		if !p.closed.Load() {
			p.fireStateChange()
		}
	}()

	if p.closed.Load() {
		return
	}

	jobCtx, cancel := context.WithTimeout(p.stopCtx, p.jobTimeout)
	defer cancel()

	res, err := p.runner(jobCtx, job.spec)
	if err != nil {
		if !p.closed.Load() {
			outcome = outcomeFailed
			// The per-job timeout is excluded from the debounce BEFORE the
			// classifier is asked: it is as likely to mean a hung mount as a
			// pathological file, so it is never a verdict about the source.
			// Same rule the transcode pool applies to its own timeout, and it
			// is platform-independent — which matters, because
			// decoderReachedAVerdict is not (see its docblock on Windows).
			//
			// Shutdown needs no arm here: Stop flips p.closed before
			// cancelling stopCtx, so a cancelled job is already outside this
			// whole block. A `jobCtx.Err() != nil` arm was written for
			// symmetry and removed again — it is unreachable, and an
			// unreachable branch that looks like a safety guard is worse than
			// none, because the next reader counts on it.
			if errors.Is(jobCtx.Err(), context.DeadlineExceeded) {
				logger.Warn("analyze: timed out",
					"path", job.spec.SourceLibraryRel, "timeout", p.jobTimeout)
			} else {
				p.noteFailure(job.spec, err)
			}
		}
		return
	}

	// Durability: flush the freshly-renamed sidecar (and its parent
	// dir on POSIX) BEFORE committing the row that points at it, so a
	// delta-sync client never races a non-durable file.
	if err := p.fsyncFn(res.WaveformPath); err != nil {
		// Deliberately do NOT remove res.WaveformPath. The sidecar path
		// is deterministic per source (reused across re-analyses), so a
		// prior committed row may already point at this exact path —
		// deleting the file on a transient failure would leave that row
		// dangling (serve → 410). A genuinely-orphan sidecar (first
		// analysis, no row) is reaped by `bridge analyze --gc`'s
		// mark-and-sweep instead. (CodeRabbit on #395, correcting the
		// round-1 unconditional-remove.)
		if !p.closed.Load() {
			outcome = outcomeFailed
			logger.Error("analyze: fsync sidecar",
				"path", job.spec.SourceLibraryRel, "err", err)
		}
		return
	}

	row := manifest.AnalysisRow{
		SourcePath:    job.spec.SourceLibraryRel,
		WaveformPath:  res.WaveformPath,
		WaveformTag:   res.WaveformTag,
		WaveformSize:  res.WaveformSize,
		SourceMTimeNS: job.spec.SourceMTimeNS,
		SourceSize:    job.spec.SourceSize,
		SchemaVersion: res.SchemaVersion,
		CreatedAt:     p.now().UnixNano(),
	}
	if res.HasLoudness {
		rg := res.ReplayGainTrackDB
		row.ReplayGainTrackDB = &rg
	}
	row.KeyRoot = res.KeyRoot
	row.KeyMode = res.KeyMode
	row.BPM = res.BPM
	row.TruePeakDB = res.TruePeakDB
	row.DRScore = res.DRScore
	row.AudioMD5State = res.AudioMD5State
	row.AudioMD5Retryable = res.AudioMD5Retryable
	if res.Spectrum != nil {
		// BandwidthHz is copied straight through INCLUDING its nil: a
		// spectrum with no bandwidth is the routine ceiling-limited case,
		// not a partial measurement (see spectrum.go).
		row.BandwidthHz = res.Spectrum.BandwidthHz
		row.Spectrum = EncodeSpectrum(res.Spectrum)
	}
	if err := p.store.UpsertAnalysis(jobCtx, row); err != nil {
		// Same as the fsync branch: don't remove the sidecar (path is
		// reused per source, so a prior row could already point at it);
		// `--gc` reconciles a true first-analysis orphan. (CodeRabbit #395.)
		if !p.closed.Load() {
			outcome = outcomeFailed
			logger.Error("analyze: store analysis",
				"path", job.spec.SourceLibraryRel, "err", err)
		}
		return
	}
	// A committed row is a success, so the consecutive-failure counter goes
	// back to zero. Without this the counter measures LIFETIME failures and a
	// file that fails twice a year — a full disk, a mount blip — eventually
	// suppresses itself with successes on either side. Best-effort: failing
	// to clear costs at most one extra retry later, and refusing to count the
	// job as done because a bookkeeping UPDATE failed would be the worse
	// trade.
	//
	// DETACHED and bounded, like noteFailure's write and for the same reason:
	// UpsertAnalysis has already committed by here, so the row is a success
	// whatever jobCtx does next. Leaving it on jobCtx meant a per-job timeout
	// or a shutdown landing in that gap left the strikes standing on a track
	// that had just analysed cleanly — and the two writes being asymmetric
	// was itself the tell. (CodeRabbit on #947.)
	clearCtx, clearCancel := context.WithTimeout(
		context.WithoutCancel(p.stopCtx), failureRecordTimeout)
	defer clearCancel()
	if err := p.store.ClearAnalysisFailure(clearCtx, job.spec.SourceLibraryRel); err != nil {
		logger.Warn("analyze: clear failure marker",
			"path", job.spec.SourceLibraryRel, "err", err)
	}
	outcome = outcomeDone
}

// analyzeFailedMsg is the one message every analysis failure logs, whatever
// its classification. Deliberately identical across the transient, the
// marker-write-failed and the first-verdict branches: an operator greps one
// string to find every refused track, and three spellings would make that
// search silently incomplete. A const so a change to one is a change to all.
const analyzeFailedMsg = "analyze: failed"

// failureRecordTimeout bounds the debounce UPDATE. It runs on a context
// DETACHED from the job's, because the job context is the thing that just
// expired or was cancelled in a neighbouring branch and the record must
// still land; a bound is what keeps that from being an unbounded write on a
// wedged database.
const failureRecordTimeout = 5 * time.Second

// noteFailure logs one analysis failure and, when the decoder reached a
// verdict about the FILE, records a strike against it.
//
// Two jobs, and the second is what makes the first quiet. A source that can
// never decode is re-offered on every sweep, so the WARN it produces is
// per-sweep-forever: the field report was 1,385 lines in 7 days for the same
// 30 truncated FLACs, which is not a disk-space problem but the reason every
// other line in the journal becomes unfindable (the same lesson the M-SEARCH
// send-failure streak suppression records). The strike count IS the dedup
// key: count == 1 means this is the first verdict against this version of
// this file, and only that one gets a WARN.
//
// The repetition rule differs from the playlist mass-delete WARN on purpose.
// There each line carries a different count and the last one is where the run
// stopped; here every line is identical by construction, which is the
// M-SEARCH shape, so it is suppressed rather than repeated.
//
// A TRANSIENT failure still logs every time. That is deliberate: it has no
// marker to dedup against, and a transient failure that repeats forever is an
// alarm the operator needs — silencing it would hide a missing sox or a
// failing disk behind the fix for a different problem.
func (p *Pool) noteFailure(spec AnalyzeSpec, err error) {
	if !SourceUnreadable(err) {
		logger.Warn(analyzeFailedMsg,
			"path", spec.SourceLibraryRel, "err", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(p.stopCtx), failureRecordTimeout)
	defer cancel()
	// The spec's own (size, mtime) is deliberately NOT passed: the store
	// stamps the strike from the manifest row, so the version a verdict
	// records is the version its predicates compare against. See
	// RecordAnalysisFailure.
	strikes, rerr := p.store.RecordAnalysisFailure(ctx, spec.SourceLibraryRel, err.Error())
	if rerr != nil {
		// The marker did not land, so the dedup key is unknown — log, as
		// this is the un-deduplicated state the feature exists to leave.
		logger.Warn(analyzeFailedMsg,
			"path", spec.SourceLibraryRel, "err", err, "markerErr", rerr)
		return
	}
	if strikes != 1 {
		// Either a repeat of a verdict already reported (>1), or a track
		// that no longer has a manifest row (0). Neither is news.
		logger.Debug("analyze: failed again",
			"path", spec.SourceLibraryRel, "strikes", strikes, "err", err)
		return
	}
	// The first verdict against this file version — the line the operator
	// acts on. It carries the decoder's own message, which for the
	// truncation case names both durations.
	logger.Warn(analyzeFailedMsg,
		"path", spec.SourceLibraryRel, "err", err,
		"retriesLeft", manifest.AnalysisFailureThreshold()-1)
}

// PoolStats is a snapshot of pool counters for the stats surface.
type PoolStats struct {
	Workers  int
	QueueCap int
	QueueLen int
	Inflight int
	Enqueued uint64
	Done     uint64
	Failed   uint64
}

// Stats returns the current snapshot. Safe to call concurrently.
//
// The in-flight count and the three counters are read in one critical
// section, the lock finishJob moves a job from one to the other under. So
// an observer can act on what it reads: Done or Failed having moved means
// that job has also left Inflight, and a re-enqueue of its path is
// accepted.
func (p *Pool) Stats() PoolStats {
	p.mu.Lock()
	defer p.mu.Unlock()
	return PoolStats{
		Workers:  p.workers,
		QueueCap: p.queueCap,
		QueueLen: len(p.jobs),
		Inflight: len(p.inflight),
		Enqueued: p.enqueuedCnt,
		Done:     p.doneCnt,
		Failed:   p.failedCnt,
	}
}
