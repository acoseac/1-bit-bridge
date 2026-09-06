package config

import (
	"math"
	"strings"
	"testing"
	"time"
)

func retentionBaseConfig() *Config {
	return &Config{
		LibraryRoots: []string{"/nonexistent"}, ListenAddress: ":7788",
		AdminAddress: "127.0.0.1:7789", ScanIntervalSec: 3600,
	}
}

// TestRetentionWindowsRefuseValuesThatOverflowTheCutoff is the guard on
// the whole feature's worst outcome.
//
// The sweeper turns a window into `now.AddDate(0, 0, -days).UnixNano()`,
// and UnixNano is undefined outside 1678-2262. Past ~127,455 days the
// value wraps; roughly a third of the wraps land POSITIVE and GREATER
// THAN NOW, at which point `DELETE ... WHERE started_at < ?` matches
// every row. The day counts below are the measured ones — they are not
// hypothetical, and 999999 is the canonical "effectively infinite"
// placeholder an operator reaches for.
func TestRetentionWindowsRefuseValuesThatOverflowTheCutoff(t *testing.T) {
	// Every entry here was measured to produce a cutoff at or after now.
	wipes := []int{127455, 200000, 365000, 999999, 1000000, 9999999, 36500000, math.MaxInt32}
	for _, days := range wipes {
		t.Run("playbackHistory", func(t *testing.T) {
			cfg := retentionBaseConfig()
			cfg.Retention.PlaybackHistoryDays = days
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("playbackHistoryDays=%d accepted; its cutoff is %v, which deletes the whole table",
					days, time.Now().AddDate(0, 0, -days))
			}
			if !strings.Contains(err.Error(), "retention.playbackHistoryDays") {
				t.Fatalf("playbackHistoryDays=%d refused for the wrong reason: %v", days, err)
			}
		})
		t.Run("deviceRegistration", func(t *testing.T) {
			cfg := retentionBaseConfig()
			cfg.Retention.DeviceRegistrationDays = days
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("deviceRegistrationDays=%d accepted; its cutoff deletes every registration, "+
					"including ones bound to live tokens", days)
			}
			if !strings.Contains(err.Error(), "retention.deviceRegistrationDays") {
				t.Fatalf("deviceRegistrationDays=%d refused for the wrong reason: %v", days, err)
			}
		})
	}
}

// The ceiling must not swallow any value an operator could legitimately
// want. 100 years is the ceiling itself; everything at or under it stays
// accepted, and the 90-day floor keeps its own separate meaning.
func TestRetentionWindowsAcceptEveryUsableValue(t *testing.T) {
	for _, days := range []int{0, 90, 365, 3650, 36500} {
		cfg := retentionBaseConfig()
		cfg.Retention.PlaybackHistoryDays = days
		cfg.Retention.DeviceRegistrationDays = days
		if err := cfg.Validate(); err != nil {
			t.Errorf("days=%d must be accepted: %v", days, err)
		}
	}
	// One past the ceiling is refused; the ceiling itself is not. Both
	// fields, not just the first: the registration window is the half
	// with no ErrNoLiveTokens backstop, so an untested boundary there is
	// the worse of the two to leave open. (CodeRabbit, PR #859.)
	for _, tc := range []struct {
		name string
		set  func(*Config, int)
	}{
		{"playbackHistoryDays", func(c *Config, d int) { c.Retention.PlaybackHistoryDays = d }},
		{"deviceRegistrationDays", func(c *Config, d int) { c.Retention.DeviceRegistrationDays = d }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := retentionBaseConfig()
			tc.set(cfg, MaxRetentionDays)
			if err := cfg.Validate(); err != nil {
				t.Errorf("the ceiling itself must be accepted: %v", err)
			}
			cfg = retentionBaseConfig()
			tc.set(cfg, MaxRetentionDays+1)
			if err := cfg.Validate(); err == nil {
				t.Errorf("MaxRetentionDays+1 must be refused")
			} else if !strings.Contains(err.Error(), "retention."+tc.name) {
				t.Errorf("refused for the wrong reason: %v", err)
			}
		})
	}
}

// The floor is a SEPARATE rule and both must keep working. A 30-day
// retention silently guts the bounded smart-mix families, and the
// registration window has no floor at all because nothing reads it.
func TestRetentionFloorAndSignStillApply(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(*Config)
		want string
	}{
		{"history below the floor", func(c *Config) { c.Retention.PlaybackHistoryDays = 30 }, "floor"},
		{"history negative", func(c *Config) { c.Retention.PlaybackHistoryDays = -1 }, "negative"},
		{"registration negative", func(c *Config) { c.Retention.DeviceRegistrationDays = -1 }, "negative"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := retentionBaseConfig()
			tc.set(cfg)
			err := cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want an error mentioning %q, got %v", tc.want, err)
			}
		})
	}
	// A 30-day REGISTRATION window is fine — the floor is history-only.
	cfg := retentionBaseConfig()
	cfg.Retention.DeviceRegistrationDays = 30
	if err := cfg.Validate(); err != nil {
		t.Fatalf("deviceRegistrationDays has no floor; 30 must pass: %v", err)
	}
}

// TestNoAcceptedWindowProducesAFutureCutoff is the property the ceiling
// exists for, asserted over EVERY value Validate accepts rather than over
// a sample.
//
// The safety property is not "the cutoff is positive" — it is "the cutoff
// is never at or after now". Both outcomes below that line are correct:
//
//   - up to ~56 years back the cutoff is a positive UnixNano in the past
//     and the reap deletes exactly what it says;
//   - beyond that it crosses the 1970 epoch and goes negative, which the
//     reaps' `beforeNS <= 0` no-op turns into "delete nothing" — the
//     honest answer for a window longer than the bridge has existed.
//
// Only a cutoff at or after NOW is a wipe, and this sweeps the whole
// accepted range to prove none of them is one.
func TestNoAcceptedWindowProducesAFutureCutoff(t *testing.T) {
	now := time.Now()
	nowNS := now.UnixNano()
	// From 1, not 0: zero is the "disabled" spelling and the sweeper's
	// `days > 0` gate means the reaps never see it. (Its cutoff would be
	// exactly now, which the store's own belt refuses — the right
	// direction, and not a case this loop is about.)
	for days := 1; days <= MaxRetentionDays; days++ {
		if ns := now.AddDate(0, 0, -days).UnixNano(); ns >= nowNS {
			t.Fatalf("days=%d is inside the accepted range but yields cutoff ns=%d >= now %d "+
				"(%s) — that reap deletes every row",
				days, ns, nowNS, now.AddDate(0, 0, -days).Format("2006-01-02"))
		}
	}
	// And the constant stays inside the range where AddDate's own
	// arithmetic is still meaningful.
	if y := now.AddDate(0, 0, -MaxRetentionDays).Year(); y < 1678 {
		t.Fatalf("MaxRetentionDays=%d reaches year %d, past where UnixNano is defined at all",
			MaxRetentionDays, y)
	}
}
