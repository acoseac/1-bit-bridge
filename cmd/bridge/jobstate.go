package main

import (
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
// stays empty — the analysis pool has no per-worker grid). nil pool →
// nil closure → the admin omits the `pool` field entirely.
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
func fingerprintStateClosure(enabled, active bool, degradedReason string, status *sweepStatus[admin.FingerprintSweepCounts]) func() *admin.FingerprintJobState {
	return func() *admin.FingerprintJobState {
		running, lastStart, lastEnd, nextDue, last := status.snapshot()
		return &admin.FingerprintJobState{
			Enabled:        enabled,
			Active:         active,
			DegradedReason: degradedReason,
			Running:        running,
			LastStartedAt:  timePtrIfSet(lastStart),
			LastFinishedAt: timePtrIfSet(lastEnd),
			NextDueAt:      timePtrIfSet(nextDue),
			Last:           last,
		}
	}
}
