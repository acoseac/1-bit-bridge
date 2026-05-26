package config

import "testing"

// TestDLNAConfig_DefaultsAreSafe pins the zero-value contract: a
// freshly-loaded bridge.yaml with no `dlna:` block leaves DLNA
// disabled, with sensible defaults applied to the rest of the
// fields so a later operator opt-in via the admin console flips
// just the one bit without needing to also set ListenAddress /
// FriendlyName / TelemetryEnabled to non-empty values.
func TestDLNAConfig_DefaultsAreSafe(t *testing.T) {
	t.Parallel()
	var cfg DLNAConfig
	if cfg.Enabled {
		t.Errorf("zero-value DLNAConfig.Enabled = true, want false (default disabled)")
	}
	if got := cfg.EffectiveDLNAListenAddress(); got != DefaultDLNAListenAddress {
		t.Errorf("EffectiveDLNAListenAddress = %q, want default %q", got, DefaultDLNAListenAddress)
	}
	if got := cfg.EffectiveDLNAFriendlyName(); got != DefaultDLNAFriendlyName {
		t.Errorf("EffectiveDLNAFriendlyName = %q, want default %q", got, DefaultDLNAFriendlyName)
	}
	if !cfg.EffectiveDLNATelemetryEnabled() {
		t.Errorf("EffectiveDLNATelemetryEnabled = false on zero-value, want true (telemetry default-on)")
	}
	if cfg.AllowTsnet {
		t.Errorf("AllowTsnet = true on zero-value, want false (tsnet opt-in by design)")
	}
}

// TestDLNAConfig_OperatorOverridesHonored verifies that an operator-
// supplied value wins over the default. Trim semantics: pure
// whitespace falls back to default (matches the FriendlyName /
// ListenAddress fallback in EffectiveMDNSEnabled's neighbouring
// helpers).
func TestDLNAConfig_OperatorOverridesHonored(t *testing.T) {
	t.Parallel()

	t.Run("listenAddress", func(t *testing.T) {
		t.Parallel()
		cfg := DLNAConfig{ListenAddress: ":9090"}
		if got := cfg.EffectiveDLNAListenAddress(); got != ":9090" {
			t.Errorf("operator listenAddress dropped: got %q", got)
		}
	})
	t.Run("friendlyName", func(t *testing.T) {
		t.Parallel()
		cfg := DLNAConfig{FriendlyName: "Listening Room"}
		if got := cfg.EffectiveDLNAFriendlyName(); got != "Listening Room" {
			t.Errorf("operator friendlyName dropped: got %q", got)
		}
	})
	t.Run("whitespaceListenAddressFallsBack", func(t *testing.T) {
		t.Parallel()
		cfg := DLNAConfig{ListenAddress: "   "}
		if got := cfg.EffectiveDLNAListenAddress(); got != DefaultDLNAListenAddress {
			t.Errorf("whitespace listenAddress should fall back to default, got %q", got)
		}
	})
	t.Run("whitespaceFriendlyNameFallsBack", func(t *testing.T) {
		t.Parallel()
		cfg := DLNAConfig{FriendlyName: "\t\n"}
		if got := cfg.EffectiveDLNAFriendlyName(); got != DefaultDLNAFriendlyName {
			t.Errorf("whitespace friendlyName should fall back to default, got %q", got)
		}
	})
}

// TestDLNAConfig_TelemetryExplicitOptOut pins the pointer-bool
// "nil → true, explicit false → false" contract that matches
// the MDNSConfig.Enabled pattern in the same file. An operator who
// explicitly sets `telemetryEnabled: false` in YAML opts out;
// omitting the field leaves the diagnostic data flowing.
func TestDLNAConfig_TelemetryExplicitOptOut(t *testing.T) {
	t.Parallel()
	disabled := false
	cfg := DLNAConfig{TelemetryEnabled: &disabled}
	if cfg.EffectiveDLNATelemetryEnabled() {
		t.Errorf("explicit telemetryEnabled=false should opt out, got true")
	}
	enabled := true
	cfg.TelemetryEnabled = &enabled
	if !cfg.EffectiveDLNATelemetryEnabled() {
		t.Errorf("explicit telemetryEnabled=true should opt in, got false")
	}
}
