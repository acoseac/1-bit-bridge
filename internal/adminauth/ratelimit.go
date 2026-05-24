package adminauth

import (
	"sync"
	"time"
)

// Rate-limit policy. 5 failed attempts per (clientIP, username)
// per 15 minutes; on threshold, the handler should slow the
// response (5s wait) instead of returning 429 — don't tell the
// attacker they're being throttled, and don't fast-fail (which
// would otherwise let them parallelise attempts cheaply).
//
// The 5s delay is the load-bearing part: it stretches a 100-attempt
// dictionary attack from microseconds to 500 seconds while bcrypt's
// per-attempt cost (~250ms at cost 12) does the heavy lifting on
// the verification side. Hostile networks see a hard ceiling on
// throughput.
const (
	RateLimitMaxAttempts = 5
	RateLimitWindow      = 15 * time.Minute
	RateLimitDelay       = 5 * time.Second

	// janitorInterval is how often we sweep expired buckets out
	// of the map. Cheap (one Lock + iterate); doesn't need to be
	// frequent because buckets are small and short-lived.
	janitorInterval = 1 * time.Hour

	// maxBuckets caps the total number of (clientIP, username)
	// tuples the limiter will track at once. Bounds the map's
	// memory footprint against a DoS-by-cardinality attack
	// (Gemini medium review on PR #290): without a cap, an
	// attacker spraying random usernames + spoofed IPs could
	// grow the map unbounded between janitor sweeps. When the
	// map hits maxBuckets, RecordFailure aggressively evicts
	// the oldest entries (by lastAttemptAt) to make room.
	//
	// Trade-off: at the cap, recently-failed attackers whose
	// buckets were evicted lose their throttle. Verify() still
	// rejects bad credentials at bcrypt-cost speed (~250 ms
	// each), so the attacker gets no speed advantage from the
	// eviction — they pay bcrypt's serial cost regardless.
	//
	// Sizing: each bucket is ~120 bytes including map overhead;
	// 10 000 entries ≈ 1.2 MB worst-case. Generous headroom for
	// any realistic home / small-team deployment.
	maxBuckets = 10_000

	// evictBatch is the number of stale-oldest buckets dropped
	// per overflow event. Larger than 1 to amortise the O(N)
	// scan cost — a sustained attack landing one bucket per
	// request would otherwise scan the full map on every call.
	evictBatch = 100
)

// bucket tracks failed attempts for a single (IP, username) key.
// attempts is the count within the current window; firstAttempt
// marks when the window started (rolling-window reset semantics).
type bucket struct {
	attempts      int
	firstAttempt  time.Time
	lastAttemptAt time.Time
}

// RateLimiter is a process-local, in-memory failed-login tracker.
// No persistence — restart wipes the state. That's fine; the
// bcrypt cost per attempt prevents online brute force from being
// meaningful in any time window the limiter would care about, and
// a determined attacker willing to wait through restarts can be
// blocked at the firewall layer.
type RateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	now     func() time.Time
	stopCh  chan struct{}
	done    chan struct{}
}

// NewRateLimiter creates a limiter and starts its janitor
// goroutine. The janitor exits when Stop is called.
func NewRateLimiter() *RateLimiter {
	rl := &RateLimiter{
		buckets: make(map[string]*bucket),
		now:     time.Now,
		stopCh:  make(chan struct{}),
		done:    make(chan struct{}),
	}
	go rl.runJanitor()
	return rl
}

// Stop terminates the janitor goroutine. Safe to call multiple
// times; subsequent calls are no-ops.
func (rl *RateLimiter) Stop() {
	select {
	case <-rl.stopCh:
		return
	default:
	}
	close(rl.stopCh)
	<-rl.done
}

// Allow reports whether the (clientIP, username) bucket is below
// the failure threshold. Does NOT increment — call RecordFailure
// after a verification attempt fails. Allow / RecordFailure are
// split so a successful Verify doesn't artificially bump the
// counter on the success path.
func (rl *RateLimiter) Allow(clientIP, username string) bool {
	key := clientIP + "|" + username
	rl.mu.Lock()
	defer rl.mu.Unlock()
	b, ok := rl.buckets[key]
	if !ok {
		return true
	}
	if rl.now().Sub(b.firstAttempt) > RateLimitWindow {
		// Window expired; the limiter no longer holds this
		// bucket against the caller. RecordFailure will reset
		// the window if a fresh failure lands.
		return true
	}
	return b.attempts < RateLimitMaxAttempts
}

// RecordFailure increments the failed-attempt counter for the
// given key. Resets the window when the previous window expired.
// Caller hands the resulting bucket state back via Allow on the
// next attempt.
//
// Map-size guard: when len(buckets) ≥ maxBuckets AND the key
// isn't already present, we evict a batch of the
// oldest-by-lastAttemptAt entries before adding the new one. This
// bounds the map's memory footprint under a high-cardinality
// attack — see maxBuckets docstring for the threat model.
func (rl *RateLimiter) RecordFailure(clientIP, username string) {
	key := clientIP + "|" + username
	now := rl.now()
	rl.mu.Lock()
	defer rl.mu.Unlock()
	b, ok := rl.buckets[key]
	if !ok || now.Sub(b.firstAttempt) > RateLimitWindow {
		if !ok && len(rl.buckets) >= maxBuckets {
			rl.evictOldestLocked(evictBatch)
		}
		rl.buckets[key] = &bucket{
			attempts:      1,
			firstAttempt:  now,
			lastAttemptAt: now,
		}
		return
	}
	b.attempts++
	b.lastAttemptAt = now
}

// evictOldestLocked drops the n oldest entries (by lastAttemptAt)
// from the map. Caller MUST hold rl.mu. O(N) where N is the
// current map size; called only at the maxBuckets boundary so the
// amortised cost is O(N / evictBatch) per RecordFailure.
//
// For small N relative to the cap (the common case — the map is
// usually nowhere near full), this never runs. At the cap, the
// batch eviction amortises the scan: one O(N) walk drops
// `evictBatch` entries, so the next evictBatch RecordFailure calls
// pay nothing.
func (rl *RateLimiter) evictOldestLocked(n int) {
	if n <= 0 || len(rl.buckets) == 0 {
		return
	}
	// Find the n oldest lastAttemptAt timestamps. Single pass
	// gathering the candidates; for n much smaller than N, a
	// partial-sort approach would be cheaper, but at our scale
	// (n=100, N≤10 000) a simple sort of all entries is fine.
	type kv struct {
		key string
		at  time.Time
	}
	all := make([]kv, 0, len(rl.buckets))
	for k, b := range rl.buckets {
		all = append(all, kv{k, b.lastAttemptAt})
	}
	// Partial sort: bubble the n smallest to the front. Avoids
	// pulling in sort package for this single use. n is bounded
	// by evictBatch=100, so this is at most 100 × len(all)
	// comparisons — ≤ 1M comparisons at the cap; cheap.
	if n > len(all) {
		n = len(all)
	}
	for i := 0; i < n; i++ {
		minIdx := i
		for j := i + 1; j < len(all); j++ {
			if all[j].at.Before(all[minIdx].at) {
				minIdx = j
			}
		}
		all[i], all[minIdx] = all[minIdx], all[i]
	}
	for i := 0; i < n; i++ {
		delete(rl.buckets, all[i].key)
	}
}

// RecordSuccess clears the failure history for a key — a
// successful login should give the operator a clean slate so a
// past typo storm doesn't carry forward.
func (rl *RateLimiter) RecordSuccess(clientIP, username string) {
	key := clientIP + "|" + username
	rl.mu.Lock()
	delete(rl.buckets, key)
	rl.mu.Unlock()
}

// runJanitor sweeps expired buckets. Idle for janitorInterval
// then takes the lock briefly. The work is bounded by len(buckets)
// which is bounded by the number of recently-active (IP, user)
// pairs — small in any realistic deployment.
func (rl *RateLimiter) runJanitor() {
	defer close(rl.done)
	tick := time.NewTicker(janitorInterval)
	defer tick.Stop()
	for {
		select {
		case <-rl.stopCh:
			return
		case <-tick.C:
			rl.sweep()
		}
	}
}

func (rl *RateLimiter) sweep() {
	cutoff := rl.now().Add(-RateLimitWindow)
	rl.mu.Lock()
	defer rl.mu.Unlock()
	for k, b := range rl.buckets {
		if b.lastAttemptAt.Before(cutoff) {
			delete(rl.buckets, k)
		}
	}
}
