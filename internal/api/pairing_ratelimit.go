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
	mu       sync.Mutex
	limiters map[string]*rateEntry
	burst    int
	limit    rate.Limit
	maxAge   time.Duration
	now      func() time.Time // injectable for tests
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

func newPairingRateLimiter() *pairingRateLimiter {
	return &pairingRateLimiter{
		limiters: make(map[string]*rateEntry),
		burst:    pairingRateBurst,
		limit:    rate.Every(pairingRateRefillInterval),
		maxAge:   pairingRateGCMaxAge,
		now:      time.Now,
	}
}

// allow returns true if the IP may proceed. The empty-string IP
// (failed RemoteAddr parse) is allowed — falling open beats locking
// out every legitimate request behind a flaky proxy.
func (p *pairingRateLimiter) allow(ip string) bool {
	if ip == "" {
		return true
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	entry, ok := p.limiters[ip]
	if !ok {
		entry = &rateEntry{lim: rate.NewLimiter(p.limit, p.burst)}
		p.limiters[ip] = entry
	}
	entry.lastSeen = now
	return entry.lim.Allow()
}

// gc reclaims limiters that haven't been touched in `maxAge`. Called
// periodically by `runGC` on the bridge's lifecycle. Exposed for tests
// so they can drive a sweep without spinning a real ticker.
func (p *pairingRateLimiter) gc() {
	p.mu.Lock()
	defer p.mu.Unlock()
	cutoff := p.now().Add(-p.maxAge)
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
