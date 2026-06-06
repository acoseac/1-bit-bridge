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
