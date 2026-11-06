package config

import "testing"

// TestTheOffSwitchLoads: enrich.enabled and enrich.deezerEnabled are
// settings, so a config that writes either loads and reads as it says, and
// each is set from the environment by the name the struct derives for it,
// through the real Load (backlog B617). On main a config that wrote either
// did not load ("that key is not a field of type config.EnrichConfig"), and
// the environment's names set nothing: enrichment had no off switch.
func TestTheOffSwitchLoads(t *testing.T) {
	for _, c := range []struct {
		name           string
		yaml           string
		env            map[string]string
		master, deezer bool
	}{
		{name: "enrich.enabled: false", yaml: "enrich:\n  enabled: false\n", deezer: true},
		{name: "enrich.deezerEnabled: false", yaml: "enrich:\n  deezerEnabled: false\n", master: true},
		{name: "both written true", yaml: "enrich:\n  enabled: true\n  deezerEnabled: true\n", master: true, deezer: true},
		{name: "BRIDGE_ENRICH_ENABLED=false", env: map[string]string{"BRIDGE_ENRICH_ENABLED": "false"}, deezer: true},
		{name: "BRIDGE_ENRICH_DEEZER_ENABLED=false", env: map[string]string{"BRIDGE_ENRICH_DEEZER_ENABLED": "false"}, master: true},
		{name: "the environment over the file", yaml: "enrich:\n  enabled: false\n  deezerEnabled: false\n",
			env:    map[string]string{"BRIDGE_ENRICH_ENABLED": "true", "BRIDGE_ENRICH_DEEZER_ENABLED": "true"},
			master: true, deezer: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			for k, v := range c.env {
				t.Setenv(k, v)
			}
			cfg, err := Load(writeEnrichTestConfig(t, c.yaml))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if got := cfg.Enrich.EffectiveEnabled(); got != c.master {
				t.Errorf("enrichment on = %v, want %v", got, c.master)
			}
			if got := cfg.Enrich.EffectiveDeezerEnabled(); got != c.deezer {
				t.Errorf("Deezer on = %v, want %v", got, c.deezer)
			}
		})
	}
}
