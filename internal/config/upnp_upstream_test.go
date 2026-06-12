package config

import (
	"strings"
	"testing"
	"time"
)

func TestUPnPUpstreamConfig_DefaultsWhenDisabled(t *testing.T) {
	u := UPnPUpstreamConfig{} // Enabled=false, no servers
	if err := u.Validate(); err != nil {
		t.Fatalf("disabled config should validate, got: %v", err)
	}
	if got := u.EffectiveMSearchInterval(); got != DefaultUPnPUpstreamMSearchInterval {
		t.Errorf("MSearchInterval default = %v; want %v", got, DefaultUPnPUpstreamMSearchInterval)
	}
	if got := u.EffectiveServerTTL(); got != DefaultUPnPUpstreamServerTTL {
		t.Errorf("ServerTTL default = %v; want %v", got, DefaultUPnPUpstreamServerTTL)
	}
}

func TestUPnPUpstreamServerConfig_DefaultRootObjectID(t *testing.T) {
	s := UPnPUpstreamServerConfig{Name: "x"}
	if got := s.EffectiveRootObjectID(); got != DefaultUPnPUpstreamRootObjectID {
		t.Errorf("default RootObjectID = %q; want %q", got, DefaultUPnPUpstreamRootObjectID)
	}
	s2 := UPnPUpstreamServerConfig{RootObjectID: "0$folders"}
	if got := s2.EffectiveRootObjectID(); got != "0$folders" {
		t.Errorf("explicit RootObjectID = %q", got)
	}
}

func TestUPnPUpstreamConfig_EffectiveDurations(t *testing.T) {
	u := UPnPUpstreamConfig{MSearchIntervalSeconds: 45, ServerTTLSeconds: 240}
	if got := u.EffectiveMSearchInterval(); got != 45*time.Second {
		t.Errorf("MSearchInterval = %v", got)
	}
	if got := u.EffectiveServerTTL(); got != 240*time.Second {
		t.Errorf("ServerTTL = %v", got)
	}
}

func TestUPnPUpstreamConfig_Validate_RequiresNameAndIdentifier(t *testing.T) {
	cases := []struct {
		name    string
		cfg     UPnPUpstreamConfig
		wantErr string
	}{
		{
			"empty name",
			UPnPUpstreamConfig{Enabled: true, Servers: []UPnPUpstreamServerConfig{{UDN: "uuid:x"}}},
			"name is required",
		},
		{
			"no UDN or manualURL",
			UPnPUpstreamConfig{Enabled: true, Servers: []UPnPUpstreamServerConfig{{Name: "2Go"}}},
			"either udn or manualDescriptionURL is required",
		},
		{
			"valid (UDN)",
			UPnPUpstreamConfig{Enabled: true, Servers: []UPnPUpstreamServerConfig{{Name: "2Go", UDN: "uuid:abc"}}},
			"",
		},
		{
			"valid (manualURL)",
			UPnPUpstreamConfig{Enabled: true, Servers: []UPnPUpstreamServerConfig{{Name: "2Go", ManualDescriptionURL: "http://h/d.xml"}}},
			"",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.cfg.Validate()
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate: %v", err)
				}
			} else {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("Validate err = %v; want substring %q", err, c.wantErr)
				}
			}
		})
	}
}

func TestUPnPUpstreamConfig_Validate_TTLMustExceedMSearchInterval(t *testing.T) {
	// Same invariant as DLNADiscoveryConfig — otherwise still-online
	// servers evict between cycles. Per the renderer-side rationale.
	u := UPnPUpstreamConfig{
		Enabled:                true,
		MSearchIntervalSeconds: 120,
		ServerTTLSeconds:       60,
		Servers:                []UPnPUpstreamServerConfig{{Name: "x", UDN: "uuid:1"}},
	}
	err := u.Validate()
	if err == nil || !strings.Contains(err.Error(), "must be > msearchIntervalSeconds") {
		t.Fatalf("err = %v; want TTL-vs-interval mismatch", err)
	}
}

func TestUPnPUpstreamConfig_Validate_DuplicatePathPrefix(t *testing.T) {
	// Two servers without explicit PathPrefix but the same Name collide.
	u := UPnPUpstreamConfig{
		Enabled: true,
		Servers: []UPnPUpstreamServerConfig{
			{Name: "Chord 2Go", UDN: "uuid:1"},
			{Name: "Chord 2Go", UDN: "uuid:2"},
		},
	}
	if err := u.Validate(); err == nil || !strings.Contains(err.Error(), "pathPrefix") {
		t.Fatalf("err = %v; want pathPrefix collision", err)
	}
	// Explicit distinct prefixes resolve the collision.
	u.Servers[1].PathPrefix = "2Go-spare"
	if err := u.Validate(); err != nil {
		t.Fatalf("distinct prefixes should validate: %v", err)
	}
}

func TestUPnPUpstreamConfig_Validate_EffectiveDurations_NotRaw(t *testing.T) {
	// Raw {MS:240, TTL:0} would falsely pass a raw-field comparison
	// (0 <= 240 yields "valid"), but the EFFECTIVE pair is {MS:240s,
	// TTL:180s default} — which IS misconfigured. Validate must catch
	// it via the effective comparison.
	u := UPnPUpstreamConfig{
		Enabled:                true,
		MSearchIntervalSeconds: 240, // > default TTL of 180s
		ServerTTLSeconds:       0,   // falls back to default 180s
		Servers:                []UPnPUpstreamServerConfig{{Name: "x", UDN: "uuid:1"}},
	}
	err := u.Validate()
	if err == nil || !strings.Contains(err.Error(), "must be >") {
		t.Fatalf("err = %v; want effective-duration mismatch", err)
	}
	// Inverse: raw {MS:0, TTL:0} falls back to default {60s, 180s} — VALID.
	ok := UPnPUpstreamConfig{
		Enabled: true,
		Servers: []UPnPUpstreamServerConfig{{Name: "x", UDN: "uuid:1"}},
	}
	if err := ok.Validate(); err != nil {
		t.Fatalf("default-pair should validate: %v", err)
	}
}

func TestUPnPUpstreamConfig_Validate_PathPrefixCollidesAcrossSlashTrim(t *testing.T) {
	// "Chord 2Go" and "Chord 2Go/" must be treated as the same prefix
	// (the walker strips slashes; the validator must do the same so
	// collisions don't bypass it).
	u := UPnPUpstreamConfig{
		Enabled: true,
		Servers: []UPnPUpstreamServerConfig{
			{Name: "A", UDN: "uuid:1", PathPrefix: "Chord 2Go"},
			{Name: "B", UDN: "uuid:2", PathPrefix: "Chord 2Go/"},
		},
	}
	if err := u.Validate(); err == nil || !strings.Contains(err.Error(), "pathPrefix") {
		t.Fatalf("err = %v; want pathPrefix collision after slash-trim", err)
	}
}

func TestUPnPUpstreamConfig_Validate_DuplicateUDN(t *testing.T) {
	u := UPnPUpstreamConfig{
		Enabled: true,
		Servers: []UPnPUpstreamServerConfig{
			{Name: "A", UDN: "uuid:abc"},
			{Name: "B", UDN: "uuid:ABC"}, // case-insensitive collision
		},
	}
	if err := u.Validate(); err == nil || !strings.Contains(err.Error(), "udn duplicates") {
		t.Fatalf("err = %v; want UDN collision", err)
	}
}

// TestValidate_PublicModeRefusesUPnPUpstream pins the public-mode
// refusal at the Config.Validate top-level (not on the UPnPUpstreamConfig
// itself — the refusal needs cfg.IsPublic() context). A public-mode
// VPS bridge can't reach the upstream's LAN-only SSDP multicast NOR its
// RFC1918 byte URLs, so the feature is structurally meaningless and
// must refuse early instead of burning CPU on a multicast loop that
// never hears anything.
func TestValidate_PublicModeRefusesUPnPUpstream(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{
		LibraryRoots:    []string{dir},
		ListenAddress:   ":7788",
		AdminAddress:    "0.0.0.0:7789",
		ScanIntervalSec: 3600,
		Deployment:      DeploymentConfig{Mode: "public", AdminTLSTerminatedByProxy: true},
		Autocert:        AutocertConfig{Domain: "bridge.example.com"},
		UPnPUpstream: UPnPUpstreamConfig{
			Enabled: true,
			Servers: []UPnPUpstreamServerConfig{
				{Name: "2Go", UDN: "uuid:abc"},
			},
		},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "upnpUpstream") || !strings.Contains(err.Error(), "public mode") {
		t.Errorf("error %q should mention upnpUpstream + public mode", err.Error())
	}
}

// TestValidate_PublicModeAllowsDisabledUPnPUpstream confirms the gate
// is enabled-only: the default (Enabled=false) MUST validate cleanly in
// public mode so VPS deployments aren't burdened with config noise.
func TestValidate_PublicModeAllowsDisabledUPnPUpstream(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{
		LibraryRoots:    []string{dir},
		ListenAddress:   ":7788",
		AdminAddress:    "0.0.0.0:7789",
		ScanIntervalSec: 3600,
		Deployment:      DeploymentConfig{Mode: "public", AdminTLSTerminatedByProxy: true},
		Autocert:        AutocertConfig{Domain: "bridge.example.com"},
		// UPnPUpstream zero-value: Enabled=false, no servers.
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("disabled upnpUpstream should validate in public mode: %v", err)
	}
}

// TestUPnPUpstreamValidate_RejectsBadFields pins the r1-review guards on
// the upnpUpstream block:
//   - an all-slashes/whitespace Name/pathPrefix resolves to an empty
//     prefix (would route the upstream's tracks at the library root);
//   - a negative msearch cadence is a typo that must fail loudly rather
//     than silently fall back to the default.
func TestUPnPUpstreamValidate_RejectsBadFields(t *testing.T) {
	cases := []struct {
		name    string
		cfg     UPnPUpstreamConfig
		wantSub string // substring the error must mention
	}{
		{
			name:    "empty resolved prefix",
			cfg:     UPnPUpstreamConfig{Enabled: true, Servers: []UPnPUpstreamServerConfig{{Name: "///", UDN: "uuid:abc"}}},
			wantSub: "prefix",
		},
		{
			name:    "negative msearch interval",
			cfg:     UPnPUpstreamConfig{Enabled: true, MSearchIntervalSeconds: -60, Servers: []UPnPUpstreamServerConfig{{Name: "2Go", UDN: "uuid:abc"}}},
			wantSub: "msearchIntervalSeconds",
		},
		{
			name:    "negative server TTL",
			cfg:     UPnPUpstreamConfig{Enabled: true, ServerTTLSeconds: -5, Servers: []UPnPUpstreamServerConfig{{Name: "2Go", UDN: "uuid:abc"}}},
			wantSub: "serverTTLSeconds",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.cfg.Validate()
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !strings.Contains(err.Error(), c.wantSub) {
				t.Errorf("error %q should mention %q", err.Error(), c.wantSub)
			}
		})
	}
}
