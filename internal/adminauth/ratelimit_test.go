package adminauth

import (
	"fmt"
	"sync"
	"sync/atomic"
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

func TestAllowAndReserveIsAtomicUnderConcurrency(t *testing.T) {
	// The Allow-then-RecordFailure split lets N concurrent logins all
	// observe attempts<max before any records, blowing past the ceiling
	// by the in-flight concurrency. AllowAndReserve folds the check +
	// increment into one locked op, so exactly RateLimitMaxAttempts are
	// admitted regardless of how many requests race. Runs under -race.
	rl := NewRateLimiter()
	defer rl.Stop()

	const goroutines = 200
	var admitted int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // release all at once to maximise contention
			if rl.AllowAndReserve("1.2.3.4", "admin") {
				atomic.AddInt64(&admitted, 1)
			}
		}()
	}
	close(start)
	wg.Wait()

	if admitted != RateLimitMaxAttempts {
		t.Errorf("AllowAndReserve admitted %d of %d concurrent attempts, want exactly %d (ceiling)",
			admitted, goroutines, RateLimitMaxAttempts)
	}
	// The bucket must reflect exactly the admitted reservations — no
	// lost or double-counted increments under contention.
	rl.mu.Lock()
	got := rl.buckets["1.2.3.4|admin"].attempts
	rl.mu.Unlock()
	if got != RateLimitMaxAttempts {
		t.Errorf("bucket attempts = %d, want %d", got, RateLimitMaxAttempts)
	}
}

func TestAllowAndReserveResetsWindowAndClearsOnSuccess(t *testing.T) {
	// Sequential contract: AllowAndReserve admits RateLimitMaxAttempts
	// failures then refuses; RecordSuccess (the success-path follow-up)
	// clears the reservation; and an expired window resets cleanly.
	rl := NewRateLimiter()
	defer rl.Stop()
	tick := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	rl.now = func() time.Time { return tick }

	for i := 0; i < RateLimitMaxAttempts; i++ {
		if !rl.AllowAndReserve("1.2.3.4", "admin") {
			t.Fatalf("AllowAndReserve should admit attempt %d (< ceiling)", i)
		}
	}
	if rl.AllowAndReserve("1.2.3.4", "admin") {
		t.Error("AllowAndReserve should refuse at the ceiling")
	}
	// A successful login clears the slate (the optimistic reservation).
	rl.RecordSuccess("1.2.3.4", "admin")
	if !rl.AllowAndReserve("1.2.3.4", "admin") {
		t.Error("AllowAndReserve should admit again after RecordSuccess")
	}

	// Exhaust, then roll past the window: a fresh reservation is admitted.
	for i := 0; i < RateLimitMaxAttempts; i++ {
		rl.AllowAndReserve("5.6.7.8", "admin")
	}
	if rl.AllowAndReserve("5.6.7.8", "admin") {
		t.Error("second key should be at its ceiling")
	}
	tick = tick.Add(RateLimitWindow + time.Minute)
	if !rl.AllowAndReserve("5.6.7.8", "admin") {
		t.Error("AllowAndReserve after window expiry should admit")
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

func TestNormalizeClientIP(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		// IPv6 collapses to the /64 prefix address.
		{"2001:db8:abcd:1::1", "2001:db8:abcd:1::"},
		{"2001:db8:abcd:1:ffff:ffff:ffff:ffff", "2001:db8:abcd:1::"},
		{"2001:DB8:ABCD:1::1", "2001:db8:abcd:1::"}, // case-normalized too
		// IPv4 (and its 4-in-6 mapped form) keys on the full address.
		{"1.2.3.4", "1.2.3.4"},
		{"::ffff:1.2.3.4", "::ffff:1.2.3.4"},
		// Link-local zone stripped so one client keys consistently.
		{"fe80::1%eth0", "fe80::"},
		// Unparseable / empty input passes through verbatim.
		{"not-an-ip", "not-an-ip"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := normalizeClientIP(tc.in); got != tc.want {
			t.Errorf("normalizeClientIP(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestRateLimiterIPv6Slash64SharesBucket(t *testing.T) {
	// 2026-07-21 review M2: an attacker with an advertised /64 (the
	// SLAAC norm) rotating interface IDs MUST NOT get a fresh
	// 5-attempt bucket per address — the whole /64 shares one.
	rl := NewRateLimiter()
	defer rl.Stop()

	const a = "2001:db8:abcd:1::1"
	const b = "2001:db8:abcd:1:ffff:ffff:ffff:ffff"
	for i := 0; i < RateLimitMaxAttempts; i++ {
		rl.RecordFailure(a, "admin")
	}
	if rl.Allow(b, "admin") {
		t.Error("sibling address in the same /64 must share the exhausted bucket")
	}
	// A different /64 is an independent bucket.
	if !rl.Allow("2001:db8:abcd:2::1", "admin") {
		t.Error("different /64 must get an independent bucket")
	}
	// IPv4 behavior unchanged: different IPv4 addresses stay independent.
	for i := 0; i < RateLimitMaxAttempts; i++ {
		rl.RecordFailure("203.0.113.1", "admin")
	}
	if !rl.Allow("203.0.113.2", "admin") {
		t.Error("different IPv4 address must get an independent bucket")
	}
}

func TestRateLimiterAllowAndReserveIPv6Rotation(t *testing.T) {
	// The exact M2 attack shape against the login handler's entry
	// point: reservations spread across DIFFERENT addresses in one
	// /64 exhaust the shared bucket; RecordSuccess from any sibling
	// address clears it.
	rl := NewRateLimiter()
	defer rl.Stop()

	for i := 0; i < RateLimitMaxAttempts; i++ {
		ip := fmt.Sprintf("2001:db8:abcd:1::%d", i)
		if !rl.AllowAndReserve(ip, "admin") {
			t.Fatalf("attempt %d in a fresh /64 bucket should be admitted", i)
		}
	}
	if rl.AllowAndReserve("2001:db8:abcd:1::9999", "admin") {
		t.Error("rotation within one /64 must exhaust the shared bucket")
	}
	rl.RecordSuccess("2001:db8:abcd:1::dead:beef", "admin")
	if !rl.AllowAndReserve("2001:db8:abcd:1::1", "admin") {
		t.Error("RecordSuccess from a same-/64 sibling must clear the shared bucket")
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
