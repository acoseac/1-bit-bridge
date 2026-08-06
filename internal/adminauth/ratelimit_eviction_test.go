package adminauth

import (
	"fmt"
	"testing"
	"time"
)

// steppingLimiter returns a limiter whose clock the caller drives.
// The returned pointer is the tick the limiter reads, so a test can
// advance time without racing the janitor goroutine (which only ever
// reads through the same rl.now).
func steppingLimiter(t *testing.T, start time.Time) (*RateLimiter, *time.Time) {
	t.Helper()
	rl := NewRateLimiter()
	t.Cleanup(rl.Stop)
	tick := start
	rl.now = func() time.Time { return tick }
	return rl, &tick
}

// A cheap username spray MUST NOT evict a live throttle.
//
// AllowAndReserve refuses at the ceiling without bumping
// lastAttemptAt, so a bucket's timestamp freezes the moment it starts
// protecting something — under a flat oldest-first eviction that makes
// it the FIRST entry dropped when the map overflows. Each filler
// request costs the attacker microseconds (Store.Verify rejects an
// unknown username before it reaches bcrypt), so maxBuckets of them
// cleared the real (ip, "admin") throttle on demand and the 5-per-15-min
// ceiling stopped meaning anything.
func TestEvictionSparesLiveThrottleUnderUsernameSpray(t *testing.T) {
	const (
		victimIP = "203.0.113.9"
		sprayIP  = "198.51.100.4"
	)
	rl, tick := steppingLimiter(t, time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC))

	// Drive the victim's bucket to the ceiling. Its lastAttemptAt is now
	// frozen, and every spray bucket below will carry a later one.
	for i := 0; i < RateLimitMaxAttempts; i++ {
		if !rl.AllowAndReserve(victimIP, "admin") {
			t.Fatalf("setup: attempt %d should be admitted (below ceiling)", i+1)
		}
		*tick = tick.Add(time.Millisecond)
	}
	if rl.AllowAndReserve(victimIP, "admin") {
		t.Fatal("setup: the victim bucket should be at the ceiling")
	}

	// The spray: distinct usernames from one IP, one attempt each, enough
	// to overflow the map several times over.
	for i := 0; i < maxBuckets+3*evictBatch; i++ {
		rl.AllowAndReserve(sprayIP, fmt.Sprintf("u%d", i))
		*tick = tick.Add(time.Microsecond)
	}

	// Allow is read-only, so this asserts without creating a bucket.
	if rl.Allow(victimIP, "admin") {
		t.Error("a cheap username spray evicted the live throttle — the login " +
			"ceiling can be cleared on demand and no longer bounds guessing")
	}

	rl.mu.Lock()
	size := len(rl.buckets)
	rl.mu.Unlock()
	if size > maxBuckets {
		t.Errorf("len(buckets) = %d, want ≤ %d — sparing throttles must not cost the memory bound",
			size, maxBuckets)
	}
}

// The eviction scan prefers an unthrottled bucket over a live throttle
// even when the unthrottled one is NEWER. Tier order, asserted directly
// rather than through a full map overflow.
func TestEvictionPrefersUnthrottledOverNewerLiveThrottle(t *testing.T) {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	rl, tick := steppingLimiter(t, base)
	*tick = base.Add(6 * time.Minute) // inside RateLimitWindow for both

	throttled := bucketKey("203.0.113.9", "admin")
	idle := bucketKey("198.51.100.4", "someone-else")

	rl.mu.Lock()
	rl.buckets[throttled] = &bucket{
		attempts:      RateLimitMaxAttempts,
		firstAttempt:  base,
		lastAttemptAt: base.Add(time.Minute), // OLDER
	}
	rl.buckets[idle] = &bucket{
		attempts:      1,
		firstAttempt:  base.Add(5 * time.Minute),
		lastAttemptAt: base.Add(5 * time.Minute), // NEWER
	}
	rl.evictOldestLocked(1)
	_, throttledSurvived := rl.buckets[throttled]
	_, idleSurvived := rl.buckets[idle]
	rl.mu.Unlock()

	if !throttledSurvived {
		t.Error("evicted the live throttle while an unthrottled bucket was available")
	}
	if idleSurvived {
		t.Error("evicted nothing — the unthrottled bucket should have been the candidate")
	}
}

// When EVERY bucket is a live throttle the memory bound still wins, but
// the throttle we spend is the NEWEST one. An attacker cannot steer that
// choice onto their target: reaching the ceiling is what freezes a
// bucket's timestamp, so every throttle they create afterwards sorts
// ahead of it.
func TestEvictionSacrificesNewestThrottleWhenAllAreLive(t *testing.T) {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	rl, tick := steppingLimiter(t, base)

	// Seed a saturated map of live throttles with a known age ordering.
	// Direct seeding rather than 50 000 AllowAndReserve calls: the point
	// under test is the ordering, and the fixture is clearer for it.
	key := func(i int) string { return bucketKey(fmt.Sprintf("203.0.113.%d", i%256), fmt.Sprintf("u%d", i)) }
	rl.mu.Lock()
	for i := 0; i < maxBuckets; i++ {
		rl.buckets[key(i)] = &bucket{
			attempts:      RateLimitMaxAttempts,
			firstAttempt:  base,
			lastAttemptAt: base.Add(time.Duration(i) * time.Millisecond),
		}
	}
	rl.mu.Unlock()
	oldest, newest := key(0), key(maxBuckets-1)

	// Still inside RateLimitWindow, so every seeded bucket is live.
	*tick = base.Add(time.Minute)
	if !rl.AllowAndReserve("192.0.2.50", "fresh-key") {
		t.Fatal("a brand-new key must still be admitted — the limiter may never wedge at the cap")
	}

	rl.mu.Lock()
	_, oldestSurvived := rl.buckets[oldest]
	_, newestSurvived := rl.buckets[newest]
	size := len(rl.buckets)
	rl.mu.Unlock()

	if !oldestSurvived {
		t.Error("spent the OLDEST live throttle — that is the one that has been " +
			"protecting an account longest, and the one an attacker wants gone")
	}
	if newestSurvived {
		t.Error("the newest live throttle should have been the one spent")
	}
	if size > maxBuckets {
		t.Errorf("len(buckets) = %d, want ≤ %d — the bound is absolute", size, maxBuckets)
	}
}
