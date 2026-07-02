package api

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"golang.org/x/time/rate"
)

// TestPairingRateLimiter_BurstThenThrottles is the headline contract:
// the first `burst` requests from a single IP succeed; the (burst+1)th
// is rejected; after waiting one refill interval, one more succeeds.
//
// Drives a clock-injected limiter so the test runs in microseconds
// rather than waiting on wall-clock 5s gaps.
func TestPairingRateLimiter_BurstThenThrottles(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	rl := &pairingRateLimiter{
		limiters: make(map[string]*rateEntry),
		burst:    pairingRateBurst,
		limit:    rate.Every(pairingRateRefillInterval),
		maxAge:   pairingRateGCMaxAge,
		now:      func() time.Time { return now },
	}

	// First 5 requests fit in the burst — all allowed.
	for i := 0; i < pairingRateBurst; i++ {
		if !rl.allow("1.2.3.4") {
			t.Fatalf("request %d should be allowed (within burst)", i+1)
		}
	}
	// 6th is throttled.
	if rl.allow("1.2.3.4") {
		t.Errorf("request beyond burst should be throttled (got allowed)")
	}

	// Advance one refill interval — the bucket gains one token, so
	// the next request succeeds. Without this clock-advance step the
	// test only covers the throttle direction; CodeRabbit flagged
	// that a refill-path regression could still pass.
	now = now.Add(pairingRateRefillInterval)
	if !rl.allow("1.2.3.4") {
		t.Errorf("request after one refill interval should be allowed")
	}
	// Immediately retrying without further clock advance must
	// throttle again — the bucket is back at zero after the previous
	// allow consumed the freshly-refilled token.
	if rl.allow("1.2.3.4") {
		t.Errorf("second request right after refill (no further clock advance) should be throttled")
	}
}

// TestPairingRateLimiter_PerIPIsolation: throttling one IP doesn't
// affect another. A noisy attacker on IP A can't lock out a
// legitimate user on IP B.
func TestPairingRateLimiter_PerIPIsolation(t *testing.T) {
	rl := newPairingRateLimiter()
	// Burn through A's budget.
	for i := 0; i < pairingRateBurst+5; i++ {
		rl.allow("10.0.0.1")
	}
	// B starts fresh — first request must succeed.
	if !rl.allow("10.0.0.2") {
		t.Errorf("IP B should be allowed (separate bucket from A)")
	}
}

// TestPairingRateLimiter_EmptyIPFallsOpen: empty IP (RemoteAddr parse
// failure) must not block — falling open beats locking out every
// legitimate request behind a flaky proxy.
func TestPairingRateLimiter_EmptyIPFallsOpen(t *testing.T) {
	rl := newPairingRateLimiter()
	for i := 0; i < pairingRateBurst*3; i++ {
		if !rl.allow("") {
			t.Fatalf("empty IP request %d should be allowed (fall-open)", i+1)
		}
	}
}

// TestPairingRateLimiter_GCDropsStaleEntries: a limiter unused for
// more than maxAge gets reclaimed on the next sweep, keeping the
// limiters map bounded under high churn.
func TestPairingRateLimiter_GCDropsStaleEntries(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	rl := &pairingRateLimiter{
		limiters: make(map[string]*rateEntry),
		burst:    pairingRateBurst,
		limit:    rate.Every(pairingRateRefillInterval),
		maxAge:   1 * time.Hour,
		now:      func() time.Time { return now },
	}

	rl.allow("4.4.4.4")
	if _, ok := rl.limiters["4.4.4.4"]; !ok {
		t.Fatal("limiter should exist after first allow")
	}
	// Advance the clock past maxAge.
	now = now.Add(2 * time.Hour)
	rl.gc()
	if _, ok := rl.limiters["4.4.4.4"]; ok {
		t.Errorf("stale limiter should have been reclaimed")
	}

	// A fresh limiter for the same IP starts with full burst.
	for i := 0; i < pairingRateBurst; i++ {
		if !rl.allow("4.4.4.4") {
			t.Fatalf("request %d after GC should be allowed (fresh bucket)", i+1)
		}
	}
}

// TestPairingRateLimiter_BoundedMapEvictsUnderLoad covers the
// memory-DoS hardening (CodeRabbit major review on PR #133): a
// high-cardinality spray of distinct IPs MUST NOT grow the map
// without bound. With a small test cap of 4, inserting 100 distinct
// IPs leaves the map at ≤ cap. The per-IP token state is allowed to
// be evicted under pressure — that's the documented trade-off for
// keeping memory bounded.
func TestPairingRateLimiter_BoundedMapEvictsUnderLoad(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	rl := &pairingRateLimiter{
		limiters:   make(map[string]*rateEntry),
		burst:      pairingRateBurst,
		limit:      rate.Every(pairingRateRefillInterval),
		maxAge:     pairingRateGCMaxAge,
		maxEntries: 4,
		now:        func() time.Time { return now },
	}

	// 100 distinct IPs, each making a single request. Without the
	// cap, the map would grow to 100. With it, the map is forced
	// to ≤ 4 via gc-then-evict-oldest.
	for i := 0; i < 100; i++ {
		ip := "10.0.0." + fmt.Sprint(i)
		_ = rl.allow(ip)
		// Advance the clock per-iteration so lastSeen ordering is
		// well-defined for the eviction policy.
		now = now.Add(time.Millisecond)
	}
	if got := len(rl.limiters); got > 4 {
		t.Errorf("map size = %d, want ≤ 4 (cap)", got)
	}
}

// TestPairingRateLimiter_BoundedMapEvictsByOldestLastSeen verifies
// the eviction policy: when at cap, the oldest-lastSeen entry is
// evicted to make room for a new IP, NOT a random or arbitrary one.
func TestPairingRateLimiter_BoundedMapEvictsByOldestLastSeen(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	rl := &pairingRateLimiter{
		limiters:   make(map[string]*rateEntry),
		burst:      pairingRateBurst,
		limit:      rate.Every(pairingRateRefillInterval),
		maxAge:     pairingRateGCMaxAge,
		maxEntries: 3,
		now:        func() time.Time { return now },
	}

	// Fill to cap with three IPs at staggered times so we know
	// which one is oldest.
	rl.allow("ip1") // lastSeen = t
	now = now.Add(time.Second)
	rl.allow("ip2") // lastSeen = t + 1s
	now = now.Add(time.Second)
	rl.allow("ip3") // lastSeen = t + 2s

	// Adding a fourth must evict ip1 (oldest lastSeen).
	now = now.Add(time.Second)
	rl.allow("ip4")

	if _, ok := rl.limiters["ip1"]; ok {
		t.Errorf("ip1 should have been evicted (oldest lastSeen)")
	}
	for _, ip := range []string{"ip2", "ip3", "ip4"} {
		if _, ok := rl.limiters[ip]; !ok {
			t.Errorf("%s should still be in the map", ip)
		}
	}
}

// TestPairingRateLimiter_EvictOldestTieBreaksByIP pins the deterministic
// tie-break: when several entries share the exact same lastSeen (same
// instant under a frozen clock), evictOldestLocked drops the
// lexicographically-smallest IP instead of an arbitrary one picked by
// Go's randomized map iteration.
func TestPairingRateLimiter_EvictOldestTieBreaksByIP(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	rl := &pairingRateLimiter{
		limiters:   make(map[string]*rateEntry),
		burst:      pairingRateBurst,
		limit:      rate.Every(pairingRateRefillInterval),
		maxAge:     pairingRateGCMaxAge,
		maxEntries: pairingRateMaxEntries,
		now:        func() time.Time { return now }, // frozen — every entry shares lastSeen
	}

	// Insert out of lexical order; all get the same (frozen) lastSeen.
	for _, ip := range []string{"10.0.0.9", "10.0.0.1", "10.0.0.5"} {
		rl.allow(ip)
	}
	for ip, e := range rl.limiters {
		if !e.lastSeen.Equal(now) {
			t.Fatalf("%s lastSeen = %v, want frozen %v (setup precondition)", ip, e.lastSeen, now)
		}
	}

	rl.evictOldestLocked()

	if _, ok := rl.limiters["10.0.0.1"]; ok {
		t.Error("10.0.0.1 (lex-smallest among tied lastSeen) should have been evicted")
	}
	if got := len(rl.limiters); got != 2 {
		t.Fatalf("want 2 entries after eviction, got %d", got)
	}
	for _, ip := range []string{"10.0.0.5", "10.0.0.9"} {
		if _, ok := rl.limiters[ip]; !ok {
			t.Errorf("%s should survive (larger IP, tied lastSeen)", ip)
		}
	}
}

// TestPairingRateLimiter_ConcurrentAllowsAreSafe: the limiter is
// shared across goroutines (each HTTP request runs in its own
// goroutine). Hammering it from many goroutines must not race or
// panic. We don't assert exact allow counts here — `rate.Limiter`
// has internal token-arithmetic that's fine to trust; the assertion
// is just "no race detected, no panic".
func TestPairingRateLimiter_ConcurrentAllowsAreSafe(t *testing.T) {
	rl := newPairingRateLimiter()
	const goroutines = 20
	const perGoroutine = 50
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func(g int) {
			defer wg.Done()
			ip := "10.0.0." + string(rune('0'+g%10))
			for i := 0; i < perGoroutine; i++ {
				_ = rl.allow(ip)
			}
		}(g)
	}
	wg.Wait()
	// Sanity: at most 10 distinct IPs were used, so at most 10
	// limiter entries — and never more than goroutines*perGoroutine.
	if len(rl.limiters) > 10 {
		t.Errorf("expected at most 10 distinct IPs in map, got %d", len(rl.limiters))
	}
}
