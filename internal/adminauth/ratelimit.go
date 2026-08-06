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
	// map hits maxBuckets, the insert paths evict a batch of
	// entries to make room — see evictOldestLocked for WHICH
	// entries, which is the security-relevant half.
	//
	// **The eviction order is load-bearing, and the original
	// rationale for ignoring it was false.** That rationale read
	// "an evicted attacker gets no speed advantage — they pay
	// bcrypt's serial cost regardless". Store.Verify returns
	// ErrInvalidCredentials on a username mismatch BEFORE it
	// reaches bcrypt.CompareHashAndPassword, so filler requests
	// carrying random usernames cost microseconds, not ~250 ms.
	// A plain oldest-first eviction therefore handed an attacker
	// a cheap unlock: a throttled bucket does not bump
	// lastAttemptAt (AllowAndReserve refuses without recording),
	// so its timestamp freezes and it becomes the OLDEST entry —
	// first out on the next overflow. maxBuckets cheap requests
	// with distinct random usernames then cleared the throttle
	// on the real (ip, "admin") bucket, and the ceiling stopped
	// meaning anything.
	//
	// Sizing: each bucket is ~120 bytes including map overhead
	// (the username component of the key is length-capped at the
	// handler — see admin.maxLoginUsernameLen); 10 000 entries
	// ≈ 1.2 MB worst-case. Generous headroom for any realistic
	// home / small-team deployment.
	maxBuckets = 10_000

	// evictBatch is the number of buckets dropped per overflow
	// event (which ones is evictOldestLocked's call). Larger than
	// 1 to amortise the scan cost — a sustained attack landing one
	// bucket per request would otherwise scan the full map on
	// every call.
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

// liveThrottle reports whether this bucket is CURRENTLY refusing
// attempts: at or past the ceiling, with its window still open.
//
// Single predicate shared by Allow (which returns its negation) and
// evictOldestLocked (which protects the buckets it selects). Those two
// must not drift: an eviction scan whose idea of "throttled" is looser
// than Allow's would evict a bucket that is still locking an attacker
// out, which is exactly the bug the two-tier eviction exists to stop.
func (b *bucket) liveThrottle(now time.Time) bool {
	return b.attempts >= RateLimitMaxAttempts && now.Sub(b.firstAttempt) <= RateLimitWindow
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
	// Expressed as the negation of liveThrottle so the eviction scan
	// cannot end up with a different notion of "currently throttled".
	// Identical to the previous form: an expired window (which
	// RecordFailure / AllowAndReserve reset on the next attempt)
	// admits regardless of the accumulated count.
	return !b.liveThrottle(rl.now())
}

// RecordFailure increments the failed-attempt counter for the
// given key. Resets the window when the previous window expired.
// Caller hands the resulting bucket state back via Allow on the
// next attempt.
//
// Map-size guard: when len(buckets) ≥ maxBuckets AND the key
// isn't already present, we evict a batch before adding the new
// one. This bounds the map's memory footprint under a
// high-cardinality attack — see the maxBuckets docstring for the
// threat model and evictOldestLocked for the two-tier order that
// keeps a spray from evicting live throttles.
//
// **Note for any future caller** — unlike AllowAndReserve, this
// method bumps lastAttemptAt even for a bucket already past the
// ceiling. evictOldestLocked's tier 2 spends the NEWEST live
// throttle, so a caller that hammers one key through here keeps
// re-marking it as the next throttle to sacrifice. Harmless today:
// the admin login handler uses AllowAndReserve + RecordSuccess and
// nothing in production calls this. Give it the same
// don't-bump-at-the-ceiling treatment before wiring it to a
// request path again.
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
		// can't grow the map without bound — and note this is the
		// path a spray actually takes, so evictOldestLocked's
		// throttle-sparing tiers are what stop the spray from
		// clearing the ceiling it just ran into.
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
	//
	// The frozen timestamp is why evictOldestLocked cannot rank by
	// lastAttemptAt alone: it makes a bucket look stalest exactly while
	// it is doing its job. Don't "fix" that by bumping the timestamp
	// here — an attacker could then keep a bucket alive past the
	// janitor forever with their own traffic, and the ordering fix
	// belongs in the eviction scan, which is where it now lives.
	if b.attempts >= RateLimitMaxAttempts {
		return false
	}
	b.attempts++
	b.lastAttemptAt = now
	return true
}

// evictOldestLocked frees up to n slots in the map. Caller MUST hold
// rl.mu. O(N log N) where N is the current map size; called only at
// the maxBuckets boundary so the amortised cost is O(N log N /
// evictBatch) per insert. For small N relative to the cap (the common
// case — the map is usually nowhere near full) it never runs.
//
// **Two tiers, and the order between them is the security property.**
//
//	Tier 1 — buckets that are NOT currently throttling anyone
//	         (below the ceiling, or past their window), oldest
//	         lastAttemptAt first. These carry no protection, so
//	         dropping them costs nothing.
//	Tier 2 — live throttles, NEWEST lastAttemptAt first, and only
//	         once tier 1 is exhausted.
//
// A flat oldest-first scan over both tiers is what made the ceiling
// bypassable. AllowAndReserve deliberately refuses at the ceiling
// WITHOUT bumping lastAttemptAt, so a throttled bucket's timestamp
// freezes the moment it starts protecting something — which makes it
// the oldest entry in the map and the FIRST one a flat scan drops.
// Spraying maxBuckets distinct random usernames (microseconds each:
// Store.Verify rejects an unknown username before bcrypt) evicted the
// real (ip, "admin") throttle and reopened the ceiling on demand.
//
// Tier 2's reversed order is the same reasoning applied to the case
// where every bucket is a live throttle: the newest throttle is the
// one an attacker most likely just created, and the oldest is the one
// that has been holding someone off the longest — so the newest is
// what we spend. An attacker cannot make their target bucket the
// newest, because reaching the ceiling is what freezes its timestamp
// and every later throttle they create sorts ahead of it.
//
// The memory bound stays ABSOLUTE — tier 2 guarantees an eviction
// whenever the map is non-empty, so the insert paths always have room
// and no caller can wedge. The cost of reaching tier 2 at all is
// maxBuckets × RateLimitMaxAttempts requests inside one
// RateLimitWindow (50 000 per 15 min against a single bridge), versus
// the maxBuckets requests the flat scan needed, and it still does not
// let an attacker choose which throttle dies.
func (rl *RateLimiter) evictOldestLocked(n int) {
	if n <= 0 || len(rl.buckets) == 0 {
		return
	}
	now := rl.now()
	type kv struct {
		key string
		at  time.Time
	}
	// Sized for the common shape (a spray fills the map with
	// unthrottled buckets); throttled starts empty and grows only
	// under a sustained multi-key attack.
	evictable := make([]kv, 0, len(rl.buckets))
	var throttled []kv
	for k, b := range rl.buckets {
		if b.liveThrottle(now) {
			throttled = append(throttled, kv{k, b.lastAttemptAt})
			continue
		}
		evictable = append(evictable, kv{k, b.lastAttemptAt})
	}
	slices.SortFunc(evictable, func(a, b kv) int { return a.at.Compare(b.at) })
	// Reversed: newest live throttle first. See the docblock.
	slices.SortFunc(throttled, func(a, b kv) int { return b.at.Compare(a.at) })

	for _, tier := range [][]kv{evictable, throttled} {
		for _, e := range tier {
			if n == 0 {
				return
			}
			delete(rl.buckets, e.key)
			n--
		}
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
