package config

import (
	"strings"
	"testing"
	"time"
)

// minimalValidAutoOptimizeConfig is the smallest Config that passes
// Validate(), mirroring the shape TestValidatePassesNonexistentLibraryRoot
// uses (Validate is a pure shape check — roots need not exist).
func minimalValidAutoOptimizeConfig() *Config {
	return &Config{
		LibraryRoots:    []string{"/nonexistent"},
		ListenAddress:   ":7788",
		AdminAddress:    "127.0.0.1:7789",
		ScanIntervalSec: 3600,
	}
}

// TestAutoOptimizeEffectiveDefaults pins the fail-safe resolution: a
// zero OR negative value takes the default rather than reading as
// "unlimited". Both fields are safety properties (queue drip, disk
// floor), so the ambiguous input must resolve to the safe side — an
// operator who typed -1 hoping to disable a cap must not get an
// unbounded sweep.
func TestAutoOptimizeEffectiveDefaults(t *testing.T) {
	for _, in := range []int{0, -1, -9999} {
		if got := (AutoOptimizeConfig{MaxPerSweep: in}).EffectiveMaxPerSweep(); got != DefaultAutoOptimizeMaxPerSweep {
			t.Errorf("EffectiveMaxPerSweep(%d) = %d, want the default %d",
				in, got, DefaultAutoOptimizeMaxPerSweep)
		}
	}
	if got := (AutoOptimizeConfig{MaxPerSweep: 42}).EffectiveMaxPerSweep(); got != 42 {
		t.Errorf("EffectiveMaxPerSweep(42) = %d, want 42", got)
	}

	for _, in := range []int64{0, -1, -(1 << 40)} {
		if got := (AutoOptimizeConfig{MinFreeBytes: in}).EffectiveMinFreeBytes(); got != DefaultAutoOptimizeMinFreeBytes {
			t.Errorf("EffectiveMinFreeBytes(%d) = %d, want the default %d",
				in, got, DefaultAutoOptimizeMinFreeBytes)
		}
	}
	if got := (AutoOptimizeConfig{MinFreeBytes: 7 << 30}).EffectiveMinFreeBytes(); got != 7<<30 {
		t.Errorf("EffectiveMinFreeBytes(7GiB) = %d, want %d", got, int64(7<<30))
	}
}

// TestAutoOptimizeIntervalInheritsScanCadence: an unset (or negative)
// intervalSec inherits scanIntervalSec. The coupling is deliberate — the
// scanner is what discovers new tracks and its post-scan hook already
// nudges a sweep, so the tick is only a safety net on the same rhythm.
//
// A negative value must CLAMP to the inherited cadence, not disable: the
// off switch is `enabled`, and a cadence typo silently becoming a second
// one would be invisible.
func TestAutoOptimizeIntervalInheritsScanCadence(t *testing.T) {
	cfg := &Config{ScanIntervalSec: 21600}
	if got := cfg.AutoOptimizeInterval(); got != 6*time.Hour {
		t.Errorf("unset intervalSec = %v, want the inherited 6h", got)
	}
	cfg.Upscale.AutoOptimize.IntervalSec = -5
	if got := cfg.AutoOptimizeInterval(); got != 6*time.Hour {
		t.Errorf("negative intervalSec = %v, want the inherited 6h (clamp, not disable)", got)
	}
	cfg.Upscale.AutoOptimize.IntervalSec = 900
	if got := cfg.AutoOptimizeInterval(); got != 15*time.Minute {
		t.Errorf("explicit intervalSec = %v, want 15m", got)
	}
}

// TestValidateRejectsAbsurdAutoOptimizeInterval pins the #529 overflow
// ceiling: `time.Duration(n) * time.Second` wraps NEGATIVE past ~9.2e9,
// and time.NewTicker PANICS on a non-positive interval — so an absurd
// YAML value would crash `bridge serve` at startup rather than merely
// misbehave.
func TestValidateRejectsAbsurdAutoOptimizeInterval(t *testing.T) {
	cfg := minimalValidAutoOptimizeConfig()
	cfg.Upscale.AutoOptimize.IntervalSec = maxIntervalSeconds + 1
	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate accepted an out-of-range autoOptimize.intervalSec")
	}
	if !strings.Contains(err.Error(), "autoOptimize.intervalSec") {
		t.Errorf("error should name the field, got: %v", err)
	}

	// The ceiling itself is legal, and so is a negative (clamped above).
	cfg.Upscale.AutoOptimize.IntervalSec = maxIntervalSeconds
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate rejected the ceiling value: %v", err)
	}
	cfg.Upscale.AutoOptimize.IntervalSec = -1
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate rejected a negative intervalSec (it clamps): %v", err)
	}
}

// TestAutoOptimizeDefaultsOff: the feature spends disk and CPU on tracks
// nobody asked for, so it must be strictly opt-in. A bridge upgrading
// into this version must not start transcoding on its own.
func TestAutoOptimizeDefaultsOff(t *testing.T) {
	cfg := minimalValidAutoOptimizeConfig()
	if cfg.Upscale.AutoOptimize.Enabled {
		t.Error("upscale.autoOptimize.enabled defaults to true; it MUST be opt-in")
	}
}
