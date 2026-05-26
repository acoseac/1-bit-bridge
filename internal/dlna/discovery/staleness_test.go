package discovery

import (
	"testing"
	"time"
)

func TestIsStaleRenderer_TTLBoundaries(t *testing.T) {
	now := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	ttl := 60 * time.Second

	cases := []struct {
		name      string
		lastSeen  time.Time
		want      bool
		rationale string
	}{
		{
			name:      "fresh within TTL",
			lastSeen:  now.Add(-30 * time.Second),
			want:      false,
			rationale: "30s ago, TTL=60s → not stale",
		},
		{
			name:      "exactly at TTL boundary",
			lastSeen:  now.Add(-60 * time.Second),
			want:      false,
			rationale: "interval == ttl (NOT >) → not stale (strict-greater contract)",
		},
		{
			name:      "one nanosecond past TTL",
			lastSeen:  now.Add(-60*time.Second - time.Nanosecond),
			want:      true,
			rationale: "interval > ttl by 1ns → stale",
		},
		{
			name:      "old observation",
			lastSeen:  now.Add(-10 * time.Minute),
			want:      true,
			rationale: "10min ago vs 60s TTL → stale",
		},
		{
			name:      "zero-time lastSeen",
			lastSeen:  time.Time{},
			want:      true,
			rationale: "uninitialized lastSeen sentinel → effectively very old → stale",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := IsStaleRenderer(tc.lastSeen, now, ttl)
			if got != tc.want {
				t.Errorf("IsStaleRenderer(lastSeen=%s, now=%s, ttl=%s) = %v, want %v (%s)",
					tc.lastSeen, now, ttl, got, tc.want, tc.rationale)
			}
		})
	}
}

func TestIsStaleRenderer_ClockSkewIsNotStale(t *testing.T) {
	// Future lastSeen — host suspended + woke past NTP correction.
	// Must NOT false-positive on staleness.
	now := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	future := now.Add(5 * time.Minute)
	if IsStaleRenderer(future, now, 60*time.Second) {
		t.Error("future lastSeenAt should be treated as fresh, got stale")
	}
}

func TestIsStaleRenderer_ZeroTTL_NeverStale(t *testing.T) {
	now := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	veryOld := now.Add(-365 * 24 * time.Hour) // a year old
	if IsStaleRenderer(veryOld, now, 0) {
		t.Error("zero TTL should never report stale (defensive default)")
	}
}

func TestIsStaleRenderer_NegativeTTL_NeverStale(t *testing.T) {
	now := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	veryOld := now.Add(-365 * 24 * time.Hour)
	if IsStaleRenderer(veryOld, now, -10*time.Second) {
		t.Error("negative TTL should never report stale (defensive default)")
	}
}
