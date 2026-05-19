package metrics

import (
	"sort"
	"sync"
	"time"
)

// SlidingHistogram is a time-bounded ring of timing observations
// supporting online quantile extraction. Backs /v1/diagnostics's
// p50/p99 surfaces without requiring callers to introspect
// Prometheus client_golang's histogram internals at runtime (the
// upstream library is optimized for write-concurrency and exposes
// no clean quantile-query API to sibling code).
//
// **Capacity & freshness**:
//   - 1024-slot ring, dropping the oldest observation on wrap.
//   - 60-second freshness window in Snapshot. Observations whose
//     timestamp falls behind `now - 60s` are excluded from quantile
//     computation. Without this cutoff, a quiet overnight bridge
//     would surface stale p50/p99 reflecting yesterday's scan burst
//     on the morning's first /v1/diagnostics poll. The cutoff
//     guarantees that the iOS Bridge Health card reads either zero
//     OR the actual live rate, never a misleading historical
//     residue.
//
// **Concurrency**:
//   - Mutex-protected. `Observe` is called inside SQLite transaction
//     boundaries and the upscale-job critical path; the µs-scale mutex
//     acquire is dwarfed by the actual work the call is timing.
//   - Atomic-only with `cursor.Add(1) % 1024` was considered and
//     rejected: assigning a multi-word `Sample` struct (a `uint64`
//     plus `time.Time` — which itself is composite: `wall`, `ext`,
//     and a `*Location`) to an array slot is NOT atomic in Go. Even
//     with the cursor ordered atomically, concurrent `Observe` calls
//     would tear the struct write under `go test -race`. The mutex
//     is the structurally correct primitive here.
type SlidingHistogram struct {
	mu      sync.Mutex
	samples [1024]Sample
	cursor  uint64 // monotonically incrementing; idx = cursor % 1024
}

// Sample pairs a timing observation (µs) with the wall-clock instant
// it was recorded at. The timestamp is what makes Snapshot honest:
// stale observations past the 60-s freshness window are excluded.
type Sample struct {
	Value     uint64    // microsecond timing
	Timestamp time.Time // wall-clock at observation
}

// NewSlidingHistogram returns an empty SlidingHistogram. The zero
// value is usable; this constructor exists for symmetry with
// promauto-style metric construction.
func NewSlidingHistogram() *SlidingHistogram {
	return &SlidingHistogram{}
}

// Observe records a timing (in seconds) at the current wall-clock
// instant. Seconds is the natural unit for Prometheus observations
// (DefBuckets is in seconds); the µs conversion happens here so
// downstream Snapshot quantile math operates on integers.
func (h *SlidingHistogram) Observe(seconds float64) {
	if seconds < 0 {
		// Defensive: clamp negative observations (clock jumps,
		// stop-watch reuse bugs) to zero so they don't pollute
		// quantiles with massive negative-cast uint64 values.
		seconds = 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	idx := h.cursor % uint64(len(h.samples))
	h.cursor++
	h.samples[idx] = Sample{
		Value:     uint64(seconds * 1_000_000),
		Timestamp: time.Now(),
	}
}

// Snapshot returns the p50 and p99 of observations within the last
// 60 seconds (both expressed in seconds). Returns (0, 0) when the
// window is empty — distinguishable from "all observations were
// zero" by combining with /v1/diagnostics's `upscaleJobsCompletedTotal`
// counter, which reveals whether any work has happened.
//
// **freshnessWindow** is overridable for testing — production
// callers use the default 60 s by passing zero.
func (h *SlidingHistogram) Snapshot() (p50Seconds, p99Seconds float64) {
	return h.SnapshotWithin(60 * time.Second)
}

// SnapshotWithin lets tests pick a non-default freshness window
// without time-travel hacks. Production code prefers `Snapshot`.
func (h *SlidingHistogram) SnapshotWithin(window time.Duration) (p50Seconds, p99Seconds float64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	cutoff := time.Now().Add(-window)
	valid := make([]uint64, 0, len(h.samples))
	for i := range h.samples {
		s := h.samples[i]
		// Zero-valued slots (never written) have a zero Timestamp,
		// which `After(cutoff)` rejects naturally. Real zero
		// observations (a 0-µs operation, vanishingly rare) carry
		// a non-zero Timestamp; we include them.
		if !s.Timestamp.IsZero() && s.Timestamp.After(cutoff) {
			valid = append(valid, s.Value)
		}
	}
	if len(valid) == 0 {
		return 0, 0
	}
	sort.Slice(valid, func(i, j int) bool { return valid[i] < valid[j] })
	p50us := valid[(len(valid)*50)/100]
	p99idx := (len(valid) * 99) / 100
	if p99idx >= len(valid) {
		p99idx = len(valid) - 1
	}
	p99us := valid[p99idx]
	return float64(p50us) / 1_000_000, float64(p99us) / 1_000_000
}
