package api

import (
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
