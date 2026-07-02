package api

import (
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// pairingRateLimiter is a per-IP token bucket protecting POST
// /v1/pairing/requests from spam. Cardinality is one entry per source
// IP that has issued at least one pairing attempt in the recent past.
// Memory is bounded by `pairingRateGCInterval` + `pairingRateGCMaxAge`
// — limiters that haven't been touched in `pairingRateGCMaxAge` are
// reclaimed on the periodic sweep.
//
// Tuning rationale (burst=5, every 5s):
//
//   - Legitimate flow: a fumbling user manually re-tapping Pair while
//     the previous request is still working. Burst=5 absorbs that
//     without ever hitting 429.
//   - Spam flow: a script POSTing in a tight loop hits 429 within
//     ~5 requests, then sees ~1 success per 5 s. The 16-pending
//     queue cap (per-bridge, in pairing.Store) still protects against
//     fanout from many distinct IPs; this limiter prevents any single
//     IP from burning the operator's admin queue alone.
//
// The pairing flow is unauthenticated — the pollSecret hash IS the
// authentication for subsequent polls — so an unauthenticated rate
// limit at this entry point is the right shape.
type pairingRateLimiter struct {
	mu         sync.Mutex
	limiters   map[string]*rateEntry
	burst      int
	limit      rate.Limit
	maxAge     time.Duration
	maxEntries int              // 0 = unbounded (tests)
	now        func() time.Time // injectable for tests
}

type rateEntry struct {
	lim      *rate.Limiter
	lastSeen time.Time
}

// pairingRateBurst is the per-IP bucket capacity. See doc above.
const pairingRateBurst = 5

// pairingRateRefillInterval is the bucket refill cadence — one token
// per 5 s on the steady state. See doc above.
const pairingRateRefillInterval = 5 * time.Second

// pairingRateGCInterval is the cadence of the cleanup sweep. Hourly
// is fine — the worst case between sweeps is the size of the active
// caller set, bounded by `pairingRateGCMaxAge`.
const pairingRateGCInterval = time.Hour

// pairingRateGCMaxAge: limiters untouched for this long are dropped
// on the next sweep. 6 h covers a reasonable "operator stepped away"
// gap without holding entries forever for one-shot scanners.
const pairingRateGCMaxAge = 6 * time.Hour

// pairingRateMaxEntries hard-caps the per-IP map size. Without this,
// a high-cardinality spray (different source IPs per request) could
// inflate `p.limiters` for the full GC window before reclamation —
// turning the rate limiter itself into a memory-DoS surface (CodeRabbit
// major review on PR #133). At 10k entries × ~200 B each that's about
// 2 MB worst-case, well below process headroom even on Pi-class hosts.
// On overflow we trigger a synchronous gc() pass first, and if that
// doesn't free space we evict the oldest-lastSeen entry to make room.
const pairingRateMaxEntries = 10_000

func newPairingRateLimiter() *pairingRateLimiter {
	return &pairingRateLimiter{
		limiters:   make(map[string]*rateEntry),
		burst:      pairingRateBurst,
		limit:      rate.Every(pairingRateRefillInterval),
		maxAge:     pairingRateGCMaxAge,
		maxEntries: pairingRateMaxEntries,
		now:        time.Now,
	}
}

// allow returns true if the IP may proceed. The empty-string IP
// (failed RemoteAddr parse) is allowed — falling open beats locking
// out every legitimate request behind a flaky proxy.
//
// Routes the rate-limit decision through `lim.AllowN(now, 1)` (NOT
// `lim.Allow()`) so the injected clock is honoured. `Allow()`
// internally calls the package-level `time.Now()`, which would make
// tests using a fake clock non-deterministic for the actual
// allow/deny verdict — the lastSeen field would update on the
// injected time, but the bucket would refill on wall-clock time,
// producing surprising assertions. Gemini bot review on PR #133.
func (p *pairingRateLimiter) allow(ip string) bool {
	if ip == "" {
		return true
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	entry, ok := p.limiters[ip]
	if !ok {
		// Cap-or-evict before allocating. Without this, a
		// high-cardinality spray (different source IPs per request)
		// would let `p.limiters` grow until the next periodic GC —
		// turning the limiter itself into a memory-DoS surface.
		if p.maxEntries > 0 && len(p.limiters) >= p.maxEntries {
			p.gcLocked(now)
			if len(p.limiters) >= p.maxEntries {
				p.evictOldestLocked()
			}
		}
		entry = &rateEntry{lim: rate.NewLimiter(p.limit, p.burst)}
		p.limiters[ip] = entry
	}
	entry.lastSeen = now
	return entry.lim.AllowN(now, 1)
}

// evictOldestLocked drops the entry with the smallest lastSeen.
// Caller MUST hold p.mu. Single-pass O(N) — N is bounded by
// `pairingRateMaxEntries` (10k), so even worst-case cost is sub-µs.
// Doing this inside `allow()` only fires when (a) the map is at
// cap AND (b) gcLocked() couldn't free space — i.e. the active
// caller set genuinely fills the cap, which on a personal bridge
// is itself a noteworthy signal.
func (p *pairingRateLimiter) evictOldestLocked() {
	var oldestIP string
	var oldestSeen time.Time
	first := true
	for ip, e := range p.limiters {
		// Tie-break equal lastSeen on the IP string so eviction is
		// deterministic — Go's map iteration order is randomized, so
		// without this two entries stamped in the same instant would
		// evict arbitrarily (harmless in production, but flaky under
		// frozen-clock tests).
		if first || e.lastSeen.Before(oldestSeen) ||
			(e.lastSeen.Equal(oldestSeen) && ip < oldestIP) {
			oldestIP = ip
			oldestSeen = e.lastSeen
			first = false
		}
	}
	if oldestIP != "" {
		delete(p.limiters, oldestIP)
	}
}

// gc reclaims limiters that haven't been touched in `maxAge`. Called
// periodically by `runGC` on the bridge's lifecycle. Exposed for tests
// so they can drive a sweep without spinning a real ticker.
func (p *pairingRateLimiter) gc() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.gcLocked(p.now())
}

// gcLocked is the lock-already-held variant used by both the periodic
// `gc()` sweep and the synchronous overflow path in `allow()`. Caller
// MUST hold p.mu.
func (p *pairingRateLimiter) gcLocked(now time.Time) {
	cutoff := now.Add(-p.maxAge)
	for ip, e := range p.limiters {
		if e.lastSeen.Before(cutoff) {
			delete(p.limiters, ip)
		}
	}
}

// runGC starts a background sweeper. Returns a stop function the
// caller invokes during shutdown. Only called from `New` (production
// path) — tests construct a limiter directly and drive `gc()`
// synchronously.
func (p *pairingRateLimiter) runGC(stop <-chan struct{}) {
	t := time.NewTicker(pairingRateGCInterval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			p.gc()
		}
	}
}
