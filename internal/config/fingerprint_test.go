package config

import (
	"strings"
	"testing"
	"time"
)

func TestFingerprintEffectiveDefaults(t *testing.T) {
	var zero FingerprintConfig

	if got := zero.EffectiveWorkers(); got != 1 {
		t.Errorf("EffectiveWorkers() = %d, want 1", got)
	}
	if got := zero.EffectiveMaxPerRun(); got != DefaultFingerprintMaxPerRun {
		t.Errorf("EffectiveMaxPerRun() = %d, want %d", got, DefaultFingerprintMaxPerRun)
	}
	if got := zero.EffectiveLength(); got != DefaultFingerprintLengthSeconds*time.Second {
		t.Errorf("EffectiveLength() = %v", got)
	}
	if got := zero.EffectiveSweepInterval(); got != DefaultFingerprintSweepHours*time.Hour {
		t.Errorf("EffectiveSweepInterval() = %v", got)
	}

	set := FingerprintConfig{Workers: 4, MaxPerRun: 50, LengthSeconds: 60, SweepIntervalHours: 12}
	if got := set.EffectiveWorkers(); got != 4 {
		t.Errorf("explicit Workers ignored: %d", got)
	}
	if got := set.EffectiveMaxPerRun(); got != 50 {
		t.Errorf("explicit MaxPerRun ignored: %d", got)
	}
	if got := set.EffectiveLength(); got != 60*time.Second {
		t.Errorf("explicit LengthSeconds ignored: %v", got)
	}
	if got := set.EffectiveSweepInterval(); got != 12*time.Hour {
		t.Errorf("explicit SweepIntervalHours ignored: %v", got)
	}
}

// TestFingerprintWorkersDefaultIsOne is separate from the table above because
// it is a deliberate divergence, not an arbitrary number: upscale and analysis
// both scale their pools with NumCPU, and this one does not.
//
// On a network-backed library each worker pulls whole files through the mount,
// so parallelism multiplies cache pressure by N — on a bounded rclone VFS
// cache that means evicting whatever the operator is actually listening to.
// Operators on local disk raise it themselves.
func TestFingerprintWorkersDefaultIsOne(t *testing.T) {
	if got := (FingerprintConfig{}).EffectiveWorkers(); got != 1 {
		t.Fatalf("default workers = %d, want 1 — see the Workers docblock before changing this", got)
	}
}

// TestResolvedAPIKeyPrefersEnvironment pins the precedence, which matches
// TailscaleConfig.AuthKey: the env var wins so an operator using it never
// finds their key copied into bridge.yaml by a Save().
func TestResolvedAPIKeyPrefersEnvironment(t *testing.T) {
	cfg := FingerprintConfig{APIKey: "from-yaml"}

	if got := cfg.ResolvedAPIKey(); got != "from-yaml" {
		t.Errorf("with no env set, got %q, want the YAML value", got)
	}

	t.Setenv("ACOUSTID_API_KEY", "from-env")
	if got := cfg.ResolvedAPIKey(); got != "from-env" {
		t.Errorf("env must win, got %q", got)
	}

	// Whitespace is trimmed on both paths. A trailing space is easy to
	// introduce (Windows `set VAR=value && ...` captures one) and AcoustID
	// answers a padded key with a bare "invalid API key", which sends you
	// inspecting the key rather than the whitespace around it.
	t.Setenv("ACOUSTID_API_KEY", "  padded-env  ")
	if got := cfg.ResolvedAPIKey(); got != "padded-env" {
		t.Errorf("env key not trimmed: %q", got)
	}

	t.Setenv("ACOUSTID_API_KEY", "")
	padded := FingerprintConfig{APIKey: "  padded-yaml \n"}
	if got := padded.ResolvedAPIKey(); got != "padded-yaml" {
		t.Errorf("yaml key not trimmed: %q", got)
	}

	if got := (FingerprintConfig{}).ResolvedAPIKey(); got != "" {
		t.Errorf("unset must be empty, got %q", got)
	}
}

// TestValidateFingerprintShapeOnly pins the boundary between Validate (a pure
// shape predicate) and the startup feature gate.
//
// The load-bearing case is the last one: enabled WITHOUT a key must still
// validate. A host with BRIDGE_FINGERPRINT_ENABLED set and no key has to boot
// — the feature degrades to off with a stderr line, exactly as a missing sox
// does for upscaling. Refusing to start would turn a missing optional
// credential into an outage.
func TestValidateFingerprintShapeOnly(t *testing.T) {
	// A genuinely VALID baseline, matching the shape the other config tests
	// use. Building it from a bare &Config{} would fail Validate on
	// libraryRoots, which would make every "rejected" case below pass for the
	// wrong reason — the classic way a negative test proves nothing.
	base := func() *Config {
		return &Config{
			LibraryRoots:    []string{"/nonexistent"},
			ListenAddress:   ":7788",
			AdminAddress:    "127.0.0.1:7789",
			ScanIntervalSec: 3600,
		}
	}

	t.Run("negative values are rejected", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			mut  func(*Config)
		}{
			{"workers", func(c *Config) { c.Fingerprint.Workers = -1 }},
			{"maxPerRun", func(c *Config) { c.Fingerprint.MaxPerRun = -1 }},
			{"lengthSeconds", func(c *Config) { c.Fingerprint.LengthSeconds = -1 }},
			{"sweepIntervalHours", func(c *Config) { c.Fingerprint.SweepIntervalHours = -1 }},
		} {
			t.Run(tc.name, func(t *testing.T) {
				c := base()
				// Sanity: the baseline itself must validate, or the
				// assertion below would pass regardless of the mutation.
				if err := c.Validate(); err != nil {
					t.Fatalf("baseline config is not valid: %v", err)
				}
				tc.mut(c)
				err := c.Validate()
				if err == nil {
					t.Fatalf("negative %s should be rejected", tc.name)
				}
				if !strings.Contains(err.Error(), "fingerprint."+tc.name) {
					t.Errorf("error should name the offending field, got %v", err)
				}
			})
		}
	})

	t.Run("enabled without an API key still validates", func(t *testing.T) {
		c := base()
		c.Fingerprint.Enabled = true
		c.Fingerprint.APIKey = ""
		if err := c.Validate(); err != nil {
			t.Fatalf("must not refuse to start over a missing optional credential: %v", err)
		}
	})

	t.Run("zero means default, not invalid", func(t *testing.T) {
		c := base()
		c.Fingerprint.Enabled = true
		if err := c.Validate(); err != nil {
			t.Fatalf("all-zero fingerprint config should validate: %v", err)
		}
	})
}
