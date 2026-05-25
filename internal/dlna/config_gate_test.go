package dlna

import (
	"strings"
	"testing"
)

func TestShouldEnableDLNA(t *testing.T) {
	cases := []struct {
		name        string
		cfg         DLNAConfig
		mode        DeploymentMode
		wantEnabled bool
		wantReason  string // substring match
	}{
		{
			name:        "loopback_enabled",
			cfg:         DLNAConfig{Enabled: true},
			mode:        DeploymentLoopback,
			wantEnabled: true,
			wantReason:  "enabled",
		},
		{
			name:        "loopback_disabled",
			cfg:         DLNAConfig{Enabled: false},
			mode:        DeploymentLoopback,
			wantEnabled: false,
			wantReason:  "opt-out",
		},
		{
			name:        "public_with_enabled_refused",
			cfg:         DLNAConfig{Enabled: true},
			mode:        DeploymentPublic,
			wantEnabled: false,
			wantReason:  "public deployment mode",
		},
		{
			name:        "public_with_disabled_refused",
			cfg:         DLNAConfig{Enabled: false},
			mode:        DeploymentPublic,
			wantEnabled: false,
			wantReason:  "public deployment mode",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotEnabled, gotReason := ShouldEnableDLNA(tc.cfg, tc.mode)
			if gotEnabled != tc.wantEnabled {
				t.Errorf("ShouldEnableDLNA enabled = %v, want %v", gotEnabled, tc.wantEnabled)
			}
			if !strings.Contains(gotReason, tc.wantReason) {
				t.Errorf("ShouldEnableDLNA reason = %q, want substring %q", gotReason, tc.wantReason)
			}
		})
	}
}

// TestShouldEnableDLNA_publicModeRefusalIsNonOverridable is the LOAD-BEARING
// safety regression test. Any future PR that adds a "force enable" flag or
// allows public-mode DLNA via any other config path MUST fail this test.
// The gate's non-overridability is the architectural decision documented
// in the package doc; this test makes the decision structurally enforceable
// across future refactors.
func TestShouldEnableDLNA_publicModeRefusalIsNonOverridable(t *testing.T) {
	// Every reachable combination of DLNAConfig fields in public mode
	// must refuse. As DLNAConfig grows fields, extend the matrix here.
	configs := []DLNAConfig{
		{Enabled: false},
		{Enabled: true},
	}
	for _, cfg := range configs {
		enabled, reason := ShouldEnableDLNA(cfg, DeploymentPublic)
		if enabled {
			t.Fatalf("SAFETY GATE VIOLATION: cfg=%+v in public mode produced enabled=true (reason=%q). "+
				"DLNA must NEVER bind in public deployment mode. See package doc + plan.",
				cfg, reason)
		}
	}
}
