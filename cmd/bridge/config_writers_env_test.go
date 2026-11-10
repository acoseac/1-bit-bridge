package main

import (
	"testing"

	"gopkg.in/yaml.v3"
)

// TestNoCLIWriterWritesTheOptOutsFromItsEnvironment: every writer of
// bridge.yaml but serve's console (configWriterCases) run in a shell that sets
// BRIDGE_UPDATE_CHECK_ENABLED and the two enrichment switches' variables,
// either way, writes none of the three keys into settings that said nothing of
// them: a variable set where a command runs is no setting of the file's, and
// a release from before a switch refuses its key whatever it says (backlog
// B761's round 3: until it, `bridge library add` and `bridge output dac bind`
// wrote the variables' values).
func TestNoCLIWriterWritesTheOptOutsFromItsEnvironment(t *testing.T) {
	for _, value := range []string{"true", "false"} {
		for _, tc := range configWriterCases() {
			t.Run(value+"/"+tc.name, func(t *testing.T) {
				for _, name := range []string{updateCheckVar, "BRIDGE_ENRICH_ENABLED", "BRIDGE_ENRICH_DEEZER_ENABLED"} {
					t.Setenv(name, value)
				}
				cfgPath, run := tc.setup(t)
				if code, out := run(); code != 0 {
					t.Fatalf("the writer failed (exit %d):\n%s", code, out)
				}
				raw := readBytes(t, cfgPath)
				var doc struct {
					Update map[string]any `yaml:"update"`
					Enrich map[string]any `yaml:"enrich"`
				}
				if err := yaml.Unmarshal(raw, &doc); err != nil {
					t.Fatalf("the settings the writer left do not parse: %v", err)
				}
				for leaf, v := range map[string]any{"update.checkEnabled": doc.Update["checkEnabled"],
					"enrich.enabled": doc.Enrich["enabled"], "enrich.deezerEnabled": doc.Enrich["deezerEnabled"]} {
					if v != nil {
						t.Errorf("the writer wrote %s = %v from its environment:\n%s", leaf, v, raw)
					}
				}
			})
		}
	}
}
