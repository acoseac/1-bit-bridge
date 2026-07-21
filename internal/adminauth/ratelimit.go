package adminauth

import (
	"net/netip"
	"slices"
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
//
// IPv6 clients share ONE bucket per /64 prefix (see bucketKey): SLAAC
// hands a host the whole /64, so keying on the full address would let
// an attacker rotate interface IDs for unlimited fresh 5-attempt
// buckets (2026-07-21 review M2).
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

// bucketKey derives the map key for a (clientIP, username) pair.
// IPv6 addresses are collapsed to their /64 prefix so an attacker
// with a typical advertised /64 can't rotate addresses for
// unlimited fresh buckets; IPv4 keys pass through unchanged. Only
// the bucket key is normalized — callers keep the full address for
// logging.
func bucketKey(clientIP, username string) string {
	return normalizeClientIP(clientIP) + "|" + username
}

// normalizeClientIP maps an IPv6 address string to its /64 prefix
// address ("2001:db8:abcd:1::1" → "2001:db8:abcd:1::"), the
// standard SLAAC subnet boundary. IPv4 and unparseable inputs are
// returned verbatim; 4-in-6 mapped addresses count as IPv4 (they
// name exactly one host); any interface zone is stripped so the
// same link-local client keys consistently.
func normalizeClientIP(ip string) string {
	addr, err := netip.ParseAddr(ip)
	if err != nil || addr.Is4() || addr.Is4In6() {
		return ip
	}
	return netip.PrefixFrom(addr.WithZone(""), 64).Masked().Addr().String()
}

// RateLimiter is a process-local, in-memory failed-login tracker.
// No persistence — restart wipes the state. That's fine; the
// bcrypt cost per attempt prevents online brute force from being
// meaningful in any time window the limiter would care about, and
// a determined attacker willing to wait through restarts can be
// blocked at the firewall layer.
type RateLimiter struct {
	mu       sync.Mutex
	buckets  map[string]*bucket
	now      func() time.Time
	stopCh   chan struct{}
	done     chan struct{}
	stopOnce sync.Once // gates the close(stopCh) — see Stop()
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
// times from any number of goroutines — subsequent calls block
// on the same `<-rl.done` receive until the janitor exits, then
// return.
//
// Pre-fix used a select/default + bare close pattern that races:
// two concurrent callers could both pass `default` then race on
// `close(stopCh)` → panic. The doc-comment claimed concurrency
// safety but the implementation didn't deliver. `sync.Once`
// closes that hole — the first caller closes stopCh + waits for
// done; subsequent callers wait on done directly (which is
// already closed by the janitor's `defer close(rl.done)`).
// CodeRabbit Major review post-PR-#292.
func (rl *RateLimiter) Stop() {
	rl.stopOnce.Do(func() {
		close(rl.stopCh)
	})
	<-rl.done
}

// Allow reports whether the (clientIP, username) bucket is below
// the failure threshold. Does NOT increment — call RecordFailure
// after a verification attempt fails. Allow / RecordFailure are
// split so a successful Verify doesn't artificially bump the
// counter on the success path.
//
// Concurrency note: Allow (check) and RecordFailure (increment) are
// two independent locked ops. A caller that runs them around a slow
// verify (bcrypt ~250 ms) lets N concurrent requests all observe
// `attempts < max` before any of them records, so the ceiling can be
// exceeded by the in-flight concurrency. That overrun is bounded and
// mostly harmless — bcrypt's per-attempt cost is the real throughput
// limiter, and this map is best-effort throttling on top of it — but
// a caller that wants the count to stay consistent under concurrency
// should prefer AllowAndReserve, which folds the check and the
// increment into a single locked op.
func (rl *RateLimiter) Allow(clientIP, username string) bool {
	key := bucketKey(clientIP, username)
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
	key := bucketKey(clientIP, username)
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

// AllowAndReserve is the concurrency-safe consolidation of Allow +
// RecordFailure: it checks the (clientIP, username) bucket against
// the failure threshold AND reserves an attempt slot in ONE locked
// op, returning true when the attempt may proceed. Because the check
// and the increment can't be interleaved by a concurrent caller, at
// most RateLimitMaxAttempts requests slip past the ceiling — closing
// the window that the separate Allow-then-verify-then-RecordFailure
// sequence leaves open across the slow bcrypt verify.
//
// The reserved slot is OPTIMISTIC and models a would-be failure: a
// caller whose subsequent verify SUCCEEDS MUST call RecordSuccess to
// clear the whole bucket, which preserves the original "a successful
// login doesn't leave the counter bumped" contract. A caller whose
// verify FAILS needs no follow-up — the reservation already counted
// it (do NOT also call RecordFailure, or the attempt double-counts).
//
// (The admin login handler still uses the Allow/RecordFailure split;
// swapping it to AllowAndReserve + RecordSuccess is the one-line
// change that activates this guard — see the handler's doc.)
func (rl *RateLimiter) AllowAndReserve(clientIP, username string) bool {
	key := bucketKey(clientIP, username)
	now := rl.now()
	rl.mu.Lock()
	defer rl.mu.Unlock()
	b, ok := rl.buckets[key]
	if !ok || now.Sub(b.firstAttempt) > RateLimitWindow {
		// Fresh window (new key, or the previous window expired):
		// admit and open a bucket with one reserved attempt. Mirror
		// RecordFailure's map-size guard so a high-cardinality spray
		// can't grow the map without bound.
		if !ok && len(rl.buckets) >= maxBuckets {
			rl.evictOldestLocked(evictBatch)
		}
		rl.buckets[key] = &bucket{
			attempts:      1,
			firstAttempt:  now,
			lastAttemptAt: now,
		}
		return true
	}
	// At the ceiling: refuse WITHOUT reserving or bumping lastAttemptAt
	// — a throttled request isn't an attempt and must not slide the
	// bucket's liveness forward. Predicate matches Allow's `< max`.
	if b.attempts >= RateLimitMaxAttempts {
		return false
	}
	b.attempts++
	b.lastAttemptAt = now
	return true
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
	// Gather all entries, sort oldest-first, drop the first n.
	// slices.SortFunc is O(N log N) vs. the prior manual O(n·N)
	// selection loop, with a smaller algorithmic surface. Still
	// amortised over evictBatch RecordFailure calls (see doc above).
	type kv struct {
		key string
		at  time.Time
	}
	all := make([]kv, 0, len(rl.buckets))
	for k, b := range rl.buckets {
		all = append(all, kv{k, b.lastAttemptAt})
	}
	if n > len(all) {
		n = len(all)
	}
	slices.SortFunc(all, func(a, b kv) int {
		return a.at.Compare(b.at)
	})
	for i := 0; i < n; i++ {
		delete(rl.buckets, all[i].key)
	}
}

// RecordSuccess clears the failure history for a key — a
// successful login should give the operator a clean slate so a
// past typo storm doesn't carry forward.
func (rl *RateLimiter) RecordSuccess(clientIP, username string) {
	key := bucketKey(clientIP, username)
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
