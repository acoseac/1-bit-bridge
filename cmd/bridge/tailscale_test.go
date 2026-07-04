package main

import (
	"strings"
	"testing"
	"time"
)

// TestWarnLECertExpiringSoon pins the 30-day threshold and the
// expired/expiring/fresh tri-state. Same test affordance shape as
// the other pure-helper tests in this repo — no autopilot
// construction, no I/O.
func TestWarnLECertExpiringSoon(t *testing.T) {
	now := time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name        string
		notAfter    time.Time
		wantContain string // "" means: must return empty
	}{
		{
			name:        "zero_time_returns_empty",
			notAfter:    time.Time{},
			wantContain: "",
		},
		{
			name:        "90_days_out_returns_empty",
			notAfter:    now.Add(90 * 24 * time.Hour),
			wantContain: "",
		},
		{
			name:        "31_days_out_returns_empty",
			notAfter:    now.Add(31 * 24 * time.Hour),
			wantContain: "",
		},
		{
			name:        "30_days_exact_warns",
			notAfter:    now.Add(30 * 24 * time.Hour),
			wantContain: "expires in 30 days",
		},
		{
			name:        "15_days_warns",
			notAfter:    now.Add(15 * 24 * time.Hour),
			wantContain: "expires in 15 days",
		},
		{
			name:        "1_day_warns",
			notAfter:    now.Add(1 * 24 * time.Hour),
			wantContain: "expires in 1 days",
		},
		{
			name:        "expired_3_days_ago",
			notAfter:    now.Add(-3 * 24 * time.Hour),
			wantContain: "EXPIRED (3 days past)",
		},
		{
			name:        "expired_1_year_ago",
			notAfter:    now.Add(-365 * 24 * time.Hour),
			wantContain: "EXPIRED (365 days past)",
		},
		{
			// Sub-day expiry rounds UP: pre-fix integer truncation
			// printed "0 days past" for a cert expired < 24h ago.
			name:        "expired_12_hours_ago_rounds_up",
			notAfter:    now.Add(-12 * time.Hour),
			wantContain: "EXPIRED (1 days past)",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := warnLECertExpiringSoon("bridge.tailnet-12345.ts.net", c.notAfter, now)
			if c.wantContain == "" {
				if got != "" {
					t.Errorf("want empty, got %q", got)
				}
				return
			}
			if !strings.Contains(got, c.wantContain) {
				t.Errorf("missing %q substring; got %q", c.wantContain, got)
			}
			// magicDNS argument MUST appear so the operator can tell which
			// cert the warning refers to (relevant when running multiple
			// bridges on the same host).
			if !strings.Contains(got, "bridge.tailnet-12345.ts.net") {
				t.Errorf("warning missing magicDNS name; got %q", got)
			}
			// Warnings MUST point at the diagnostic command operators
			// can run for context — without it the warning is just noise.
			if !strings.Contains(got, "bridge tailscale status") {
				t.Errorf("warning missing diagnostic-command hint; got %q", got)
			}
		})
	}
}

// TestWarnLECertExpiringSoon_BoundaryExactly30Days documents the
// inclusive-equals semantics at the 30-day boundary. A cert at
// exactly 30 days is INSIDE the warning window (matches the
// `<= 30` style in `bridge cert info`).
func TestWarnLECertExpiringSoon_BoundaryExactly30Days(t *testing.T) {
	now := time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC)
	// Use a time exactly 30 days minus 1 hour to avoid the floor
	// boundary; documents the same intent as the table-driven case.
	cert30dExact := now.Add(30*24*time.Hour - time.Hour)
	msg := warnLECertExpiringSoon("foo.ts.net", cert30dExact, now)
	if msg == "" {
		t.Errorf("expected warning at the 30-day boundary, got empty string")
	}
	cert30d1s := now.Add(30*24*time.Hour + time.Second)
	if warnLECertExpiringSoon("foo.ts.net", cert30d1s, now) != "" {
		t.Errorf("expected NO warning at 30 days + 1 second")
	}
}
