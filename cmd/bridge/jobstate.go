package main

import (
	"context"
	"sync"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/admin"
	"github.com/acoseac/1-bit-bridge/internal/analyze"
)

// sweepStatus is the in-memory lifecycle recorder background sweepers
// (auto-analysis today; fingerprint / smart-mix / backup as they gain
// admin cards) expose to the admin console via Deps closures. Ephemeral
// by design — "since process start" semantics, matching the sweepers'
// own state; the scanner's separately persisted `last_full_scan` is the
// one durable timestamp.
//
// Generic over the per-sweep counts DTO so each sweeper carries its own
// typed breakdown without a parallel recorder implementation.
//
// Concurrency: RWMutex — reads (the admin's 10 s /api/jobs polls ×
// N tabs, plus the 5 s SSE analysis frame) outnumber writes (two per
// sweep + one per tick arm). All methods are nil-receiver-safe so call
// sites that run without admin wiring (CLI paths, tests) can pass nil.
type sweepStatus[T any] struct {
	mu        sync.RWMutex
	running   bool
	lastStart time.Time
	lastEnd   time.Time
	nextDue   time.Time
	last      *T
}

// sweepStarted marks a sweep as in flight.
func (s *sweepStatus[T]) sweepStarted() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.running = true
	s.lastStart = time.Now().UTC()
}

// sweepFinished marks the in-flight sweep done. counts == nil means the
// sweep failed or was cancelled — running clears but the previous
// successful counts are kept (an operator glancing at the card should
// still see the last real breakdown, not a wiped one).
func (s *sweepStatus[T]) sweepFinished(counts *T) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.running = false
	s.lastEnd = time.Now().UTC()
	if counts != nil {
		s.last = counts
	}
}

// scheduleNext records when the next periodic sweep is due. Zero means
// unknown / no periodic cadence (nudge-only).
func (s *sweepStatus[T]) scheduleNext(t time.Time) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextDue = t.UTC()
}

// snapshot returns a consistent copy of the recorder's state. last is a
// shallow copy of the counts struct (value types only in the DTOs used
// here), so callers can't mutate the recorder through it.
func (s *sweepStatus[T]) snapshot() (running bool, lastStart, lastEnd, nextDue time.Time, last *T) {
	if s == nil {
		return false, time.Time{}, time.Time{}, time.Time{}, nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.last != nil {
		c := *s.last
		last = &c
	}
	return s.running, s.lastStart, s.lastEnd, s.nextDue, last
}

// timePtrIfSet converts a recorder timestamp to the *time.Time shape the
// admin DTOs use — nil for the zero time so `omitempty` genuinely drops
// the field (a bare `omitempty time.Time` would emit "0001-01-01…", the
// PR #68 lesson).
func timePtrIfSet(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// analysisPoolStatsClosure adapts analyze.Pool.Stats to the admin's
// UpscalePoolStats DTO (field sets match one-for-one; ActiveWorkers
// stays empty — the analysis pool has no per-worker grid). It answers
// whatever the analysis gate says: serve builds the pool on every
// bridge (#781), and the console's handler leaves the `pool` field out
// while the gate is closed (getAnalysisStatsSnapshot). nil pool (a
// caller with none) → nil closure → the admin omits the field too.
func analysisPoolStatsClosure(pool *analyze.Pool) func() *admin.UpscalePoolStats {
	if pool == nil {
		return nil
	}
	return func() *admin.UpscalePoolStats {
		st := pool.Stats()
		return &admin.UpscalePoolStats{
			Workers:  st.Workers,
			QueueCap: st.QueueCap,
			QueueLen: st.QueueLen,
			Inflight: st.Inflight,
			Enqueued: st.Enqueued,
			Done:     st.Done,
			Failed:   st.Failed,
		}
	}
}

// analysisSweepClosure adapts the sweeper's recorder to the admin's
// AnalysisSweepState DTO. nil recorder → nil closure → `sweep` omitted.
func analysisSweepClosure(status *sweepStatus[admin.AnalysisSweepCounts]) func() *admin.AnalysisSweepState {
	if status == nil {
		return nil
	}
	return func() *admin.AnalysisSweepState {
		running, lastStart, lastEnd, nextDue, last := status.snapshot()
		return &admin.AnalysisSweepState{
			Running:        running,
			LastStartedAt:  timePtrIfSet(lastStart),
			LastFinishedAt: timePtrIfSet(lastEnd),
			NextDueAt:      timePtrIfSet(nextDue),
			Last:           last,
		}
	}
}

// nudgeTriggerClosure wraps a sweeper's nudge channel in the
// non-blocking-send shape the Deps.Trigger* closures expect. A pending
// nudge coalesces (the sweep about to run covers the request), so the
// send is always reported as accepted. nil channel → nil closure → the
// trigger endpoint 503s.
func nudgeTriggerClosure(nudge chan<- struct{}) func() bool {
	if nudge == nil {
		return nil
	}
	return func() bool {
		select {
		case nudge <- struct{}{}:
		default:
		}
		return true
	}
}

// jobRunClosure adapts any recorder to the admin's JobRunState DTO
// (lifecycle only — per-sweep counts, when a card wants them, travel on
// their own snapshot like the duplicates summary does). nil recorder →
// nil closure → field omitted. Generic so counts-bearing recorders
// (duplicates) and counts-free ones (smart-mix, backup) share it.
func jobRunClosure[T any](status *sweepStatus[T]) func() *admin.JobRunState {
	if status == nil {
		return nil
	}
	return func() *admin.JobRunState {
		running, lastStart, lastEnd, nextDue, _ := status.snapshot()
		return &admin.JobRunState{
			Running:        running,
			LastStartedAt:  timePtrIfSet(lastStart),
			LastFinishedAt: timePtrIfSet(lastEnd),
			NextDueAt:      timePtrIfSet(nextDue),
		}
	}
}

// fingerprintStateClosure builds the Deps.FingerprintState snapshot.
// Wired for EVERY serve — a feature-off bridge still gets a card
// explaining why (enabled flag + degradedReason); status is nil in
// that case and the lifecycle fields stay zero.
func fingerprintStateClosure(enabled, active func() bool, degradedReason func() string, status *sweepStatus[admin.FingerprintSweepCounts]) func() *admin.FingerprintJobState {
	return func() *admin.FingerprintJobState {
		running, lastStart, lastEnd, nextDue, last := status.snapshot()
		return &admin.FingerprintJobState{
			Enabled:        enabled != nil && enabled(),
			Active:         active != nil && active(),
			DegradedReason: valueOr(degradedReason, ""),
			Running:        running,
			LastStartedAt:  timePtrIfSet(lastStart),
			LastFinishedAt: timePtrIfSet(lastEnd),
			NextDueAt:      timePtrIfSet(nextDue),
			Last:           last,
		}
	}
}

// valueOr resolves an optional provider to its value, or a fallback when
// nil. The card reads three live gates and a nil-check per gate reads
// worse than one helper.
func valueOr[T any](f func() T, fallback T) T {
	if f == nil {
		return fallback
	}
	return f()
}

// autoOptimizeStateClosure adapts the auto-optimize sweeper's recorder
// to the admin's card DTO. Both readers are LIVE (not snapshots): the
// switches hot-apply via a settings PATCH and the toolchain verdict is a
// TTL-cached probe, so a captured boolean would leave the card claiming
// the opposite of reality until a restart.
//
// `enabled` is the operator's switches, `active` the sweeper's own gate
// (the switches AND a usable sox). They differ only when the toolchain
// is missing, and the card then says so with the degraded key rather
// than reading as switched off: a card that answered the switches alone
// said "on" through every sweep a missing sox turned into failures.
//
// nil recorder → nil closure → the card is omitted entirely. serve
// passes a recorder on every bridge, the sweeper running whatever the
// switches say (#781), so only a caller with no sweeper omits it; this
// named "a bridge with no upscale pool", which #781 retired, until
// 2026-09-29.
func autoOptimizeStateClosure(enabled, active func() bool, status *sweepStatus[admin.AutoOptimizeSweepCounts]) func() *admin.AutoOptimizeJobState {
	if status == nil || enabled == nil || active == nil {
		return nil
	}
	return func() *admin.AutoOptimizeJobState {
		running, lastStart, lastEnd, nextDue, last := status.snapshot()
		on := enabled()
		// Active means "the sweeper will do work on its next tick": it
		// reads the same predicate the sweep does, so the card cannot
		// claim work every sweep refuses.
		act := active()
		state := &admin.AutoOptimizeJobState{
			Enabled:        on,
			Active:         act,
			Running:        running,
			LastStartedAt:  timePtrIfSet(lastStart),
			LastFinishedAt: timePtrIfSet(lastEnd),
			NextDueAt:      timePtrIfSet(nextDue),
			Last:           last,
		}
		if on && !act {
			// The switches are on and the gate is not: the one thing the
			// gate adds to them is a usable sox (soxUsable: on PATH, and
			// with FLAC). The key the analysis card sends for the same
			// verdict, so the console words both alike.
			state.DegradedReason = "sox_missing"
		}
		return state
	}
}

// runSweepLoop is the shared cadence every background sweeper follows:
// settle delay → one sweep, the BOOT pass, if the cadence is on → then a
// sweep per `interval` tick or per `nudge`, until ctx is done. Extracted
// because the analysis, fingerprint and auto-optimize loops were
// byte-identical, and the subtle part deserves one home rather than three.
//
// **The single nudge drain is that subtle part.** A nudge that landed
// DURING the settle window (typically the startup scan's post-scan hook)
// is covered by the boot pass about to run, so it is drained once, and
// only when that pass runs: with no boot pass it is a request nothing has
// covered, and the select below serves it. That is the ONLY drain: a
// nudge arriving while a sweep is EXECUTING must stay buffered, so the
// select below fires a follow-up for whatever the running sweep was too
// early to see. Dropping it would lose exactly the freshly-scanned files
// the nudge exists to catch.
//
// # interval is a provider, and the loop re-reads it every iteration
//
// It used to be a captured time.Duration feeding one time.NewTicker built
// before the loop, which is why a scan-interval change needed a restart:
// the ticker never re-evaluated it. Reading a closure per iteration is
// what makes the cadence fields hot.
//
// Two consequences worth stating, because both look like bugs otherwise:
//
//   - A timer per iteration, not one ticker. A ticker cannot change
//     period, and Reset on a live ticker has enough footguns around
//     already-queued sends that a fresh timer per pass is the cheaper
//     thing to reason about. Cost is one allocation per sweep.
//   - `interval() <= 0` parks the loop instead of ending it. Dormant is
//     now a RESUMABLE state — an operator can set backup.intervalHours to
//     0 and back — so the old `interval <= 0 && nudge == nil` early return
//     had to go. A parked loop with no nudge and no rearm blocks on
//     ctx.Done alone, which costs one goroutine and is what lets the
//     0 → N transition ever be observed.
//   - The loop parks BEFORE its boot pass too: the boot pass is the first
//     pass of the schedule, so a cadence that is off when the settle
//     window ends takes none. It took one until 2026-10-02 (backlog B209):
//     the loop swept before it read the interval, and since #769 started
//     the backup ticker on every bridge, `backup.intervalHours: 0` wrote a
//     snapshot, and pruned the backups to backup.keep, at every boot. A
//     0 → N rearm then arms the timer and runs nothing (the rearm rule
//     below), so the first pass is N later and is not the boot pass.
//
// # rearm
//
// Re-reads the interval and re-arms the timer WITHOUT sweeping. The
// settings PATCH fires it when a cadence field changes, so "live" means
// live rather than "from the next tick of the old cadence" — which on a
// 6 h interval would have been a lie with a straight face.
//
// It deliberately restarts the wait rather than preserving elapsed time:
// the PATCH only fires it when the value actually changed, and an
// operator who just changed a cadence is asking for a new schedule. The
// pathological case (nudging at hour 5 of a 6 h wait, pushing the next
// sweep to hour 11) needs a same-value-different-config edit to reach.
//
// `sweep` owns its own status bookkeeping (sweepStarted / sweepFinished);
// the loop only arms `scheduleNext`, so a caller keeps whatever
// counts-on-failure semantics it needs. It is told whether it is the boot
// pass, which only the loop knows: the backup ticker's startup snapshot
// skips when a recent one exists, and a flag of the ticker's own ("the
// first call") took the first pass after a dormant boot for it.
func runSweepLoop[T any](ctx context.Context, status *sweepStatus[T], settleDelay time.Duration, interval func() time.Duration, nudge, rearm <-chan struct{}, sweep func(boot bool)) {
	// time.NewTimer + defer Stop, NOT time.After: `time.After` keeps its
	// timer alive until it fires even when ctx.Done() wins the select, and
	// runServe is re-entered every time the launcher menu restarts the
	// bridge — so each restart would strand one settle-delay timer per
	// sweeper. The PR #290 convention, same as main.go's shutdown grace.
	settle := time.NewTimer(settleDelay)
	defer settle.Stop()
	select {
	case <-ctx.Done():
		return
	case <-settle.C:
	}
	if d := intervalOf(interval); d > 0 {
		select {
		case <-nudge:
		default:
		}
		status.scheduleNext(time.Now().Add(d))
		sweep(true)
	}
	for {
		d := intervalOf(interval)
		var tickC <-chan time.Time
		var t *time.Timer
		if d > 0 {
			t = time.NewTimer(d)
			tickC = t.C
			status.scheduleNext(time.Now().Add(d))
		} else {
			// Dormant: no scheduled next. Clearing it matters — a stale
			// "next run at 14:00" on the Jobs card after the operator
			// disabled the cadence is a promise the loop will not keep.
			status.scheduleNext(time.Time{})
		}
		select {
		case <-ctx.Done():
			stopTimer(t)
			return
		case <-tickC:
			sweep(false)
		case <-nudge:
			stopTimer(t)
			sweep(false)
		case <-rearm:
			// Cadence changed. Re-read it on the next iteration; do NOT
			// sweep — a settings save is not a request to do the work.
			stopTimer(t)
		}
	}
}

// intervalOf resolves a nil provider to 0 (dormant), so a caller with no
// periodic cadence can pass nil rather than a closure returning zero.
func intervalOf(f func() time.Duration) time.Duration {
	if f == nil {
		return 0
	}
	return f()
}

// stopTimer is nil-safe: the dormant branch above leaves t nil.
func stopTimer(t *time.Timer) {
	if t != nil {
		t.Stop()
	}
}

// staticInterval adapts a fixed duration to the provider shape, for tests
// and for callers whose cadence genuinely cannot change.
func staticInterval(d time.Duration) func() time.Duration {
	return func() time.Duration { return d }
}
