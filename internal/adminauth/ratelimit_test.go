package adminauth

import (
	"fmt"
	"testing"
	"time"
)

func TestRateLimiterAllowsBeforeThreshold(t *testing.T) {
	rl := NewRateLimiter()
	defer rl.Stop()
	for i := 0; i < RateLimitMaxAttempts-1; i++ {
		if !rl.Allow("1.2.3.4", "admin") {
			t.Errorf("Allow should permit before threshold (attempt %d)", i)
		}
		rl.RecordFailure("1.2.3.4", "admin")
	}
	// One more failure should still leave Allow true (we haven't
	// crossed the threshold yet — Allow checks `< MaxAttempts`).
	if !rl.Allow("1.2.3.4", "admin") {
		t.Error("Allow should permit at attempt threshold-1")
	}
	rl.RecordFailure("1.2.3.4", "admin")
	if rl.Allow("1.2.3.4", "admin") {
		t.Error("Allow should refuse at attempt threshold")
	}
}

func TestRateLimiterScopedPerKey(t *testing.T) {
	rl := NewRateLimiter()
	defer rl.Stop()
	for i := 0; i < RateLimitMaxAttempts; i++ {
		rl.RecordFailure("1.2.3.4", "admin")
	}
	if rl.Allow("1.2.3.4", "admin") {
		t.Error("exhausted bucket should refuse")
	}
	// Different IP same user: independent bucket.
	if !rl.Allow("5.6.7.8", "admin") {
		t.Error("different IP should not be locked by sibling bucket")
	}
	// Same IP different user: also independent.
	if !rl.Allow("1.2.3.4", "other") {
		t.Error("different username should not be locked by sibling bucket")
	}
}

func TestRateLimiterWindowExpiresAndResets(t *testing.T) {
	rl := NewRateLimiter()
	defer rl.Stop()
	tick := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	rl.now = func() time.Time { return tick }

	for i := 0; i < RateLimitMaxAttempts; i++ {
		rl.RecordFailure("1.2.3.4", "admin")
	}
	if rl.Allow("1.2.3.4", "admin") {
		t.Error("exhausted bucket should refuse")
	}

	// Advance past the window — Allow should permit again, and the
	// next RecordFailure should reset the window cleanly.
	tick = tick.Add(RateLimitWindow + time.Minute)
	if !rl.Allow("1.2.3.4", "admin") {
		t.Error("Allow after window expiry should permit")
	}
	rl.RecordFailure("1.2.3.4", "admin")
	if !rl.Allow("1.2.3.4", "admin") {
		t.Error("After window-reset, single failure should still permit")
	}
}

func TestRateLimiterRecordSuccessClearsBucket(t *testing.T) {
	rl := NewRateLimiter()
	defer rl.Stop()
	for i := 0; i < RateLimitMaxAttempts; i++ {
		rl.RecordFailure("1.2.3.4", "admin")
	}
	rl.RecordSuccess("1.2.3.4", "admin")
	if !rl.Allow("1.2.3.4", "admin") {
		t.Error("RecordSuccess should clear the failure history")
	}
}

func TestRateLimiterCapsMapSizeUnderHighCardinality(t *testing.T) {
	// Gemini-medium DoS-by-cardinality guard: an attacker spraying
	// failed logins with random (IP, username) pairs MUST NOT grow
	// the map without bound. At the cap, the limiter evicts older
	// entries to make room for newer ones; total size stays
	// ≤ maxBuckets regardless of input cardinality.
	rl := NewRateLimiter()
	defer rl.Stop()

	// Inject a stepping clock so eviction's ordering by
	// lastAttemptAt is deterministic (the older 1.2× of entries
	// should be the eviction candidates after the cap fills).
	tick := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	rl.now = func() time.Time { return tick }

	// Seed 1.2× the cap so eviction definitely kicks in. Don't
	// go 2× — the test would take a while at maxBuckets=10 000
	// because evictOldestLocked is O(N) per overflow event.
	inputCardinality := maxBuckets + maxBuckets/5
	for i := 0; i < inputCardinality; i++ {
		ip := fmt.Sprintf("203.0.113.%d", i%256)
		user := fmt.Sprintf("u%d", i)
		rl.RecordFailure(ip, user)
		tick = tick.Add(time.Microsecond)
	}

	rl.mu.Lock()
	size := len(rl.buckets)
	rl.mu.Unlock()
	if size > maxBuckets {
		t.Errorf("len(buckets) = %d, want ≤ %d (cap)", size, maxBuckets)
	}
	// Loose lower bound — we should still hold near-cap entries
	// (eviction is partial-batch, not wipe-everything).
	if size < maxBuckets-evictBatch {
		t.Errorf("len(buckets) = %d, want close to %d (evicting too aggressively)",
			size, maxBuckets)
	}
}

func TestRateLimiterSweepRemovesStale(t *testing.T) {
	rl := NewRateLimiter()
	defer rl.Stop()
	tick := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	rl.now = func() time.Time { return tick }
	rl.RecordFailure("1.2.3.4", "admin")

	rl.mu.Lock()
	count := len(rl.buckets)
	rl.mu.Unlock()
	if count != 1 {
		t.Fatalf("expected 1 bucket, got %d", count)
	}

	// Advance past the window, sweep, expect bucket gone.
	tick = tick.Add(RateLimitWindow + time.Minute)
	rl.sweep()
	rl.mu.Lock()
	count = len(rl.buckets)
	rl.mu.Unlock()
	if count != 0 {
		t.Errorf("expected sweep to drop stale bucket, got %d remaining", count)
	}
}

func TestRedirectIsSafeRelativePath(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"/library", true},
		{"/devices", true},
		{"/api/stats", true},
		{"/", true},
		{"", false},
		{"//attacker.com", false},
		{"//attacker.com/path", false},
		{"/\\attacker.com", false},
		{"https://attacker.com", false},
		{"http://attacker.com", false},
		{"attacker.com", false},
		{"/path:with:colon", false}, // defense-in-depth: refuse scheme-like sequences
		{"/path\\backslash", false}, // Windows path-coercion defense
		{"/normal/path/with-hyphens", true},
		{"/path with spaces", true},
	}
	for _, tc := range cases {
		got := IsSafeRelativePath(tc.in)
		if got != tc.want {
			t.Errorf("IsSafeRelativePath(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
