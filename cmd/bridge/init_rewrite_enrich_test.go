package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestARewriteKeepsTheEnrichmentOptOuts: `bridge init --force` keeps each
// enrichment switch the config it replaces writes off (backlog B617), in
// either posture, and printKept says so, for the reason it keeps output.mpd:
// off: the default would turn the operator's opt-out back on, and init asks
// for neither. A switch written on is the default and is written as no key;
// one that does not read is not kept and is named, by its line and never by
// its value; and the environment of whoever runs init is not written.
func TestARewriteKeepsTheEnrichmentOptOuts(t *testing.T) {
	for _, p := range rewritePostures {
		t.Run(p.name+": both off are kept", func(t *testing.T) {
			cfgDir := filepath.Join(t.TempDir(), "cfg")
			rewrite := p.setUp(t, cfgDir)
			appendToConfig(t, cfgDir, "enrich:\n  enabled: false\n  deezerEnabled: false\n")

			code, out := rewrite()
			defer logRunOnFailure(t, out)

			if code != 0 {
				t.Fatalf("the rewrite exited %d", code)
			}
			got := loadInstallConfig(t, cfgDir).Enrich
			if got.Enabled == nil || *got.Enabled || got.DeezerEnabled == nil || *got.DeezerEnabled {
				t.Errorf("the rewritten config has enrich.enabled %v, enrich.deezerEnabled %v; want both written false",
					got.Enabled, got.DeezerEnabled)
			}
			for _, key := range []string{"enrich.enabled", "enrich.deezerEnabled"} {
				if v, ok := keptValue(out, key); !ok || v != "false" {
					t.Errorf("the run's list of what it kept gives %s %q (listed %v), want false", key, v, ok)
				}
			}
		})
	}

	t.Run("one written on is the default", func(t *testing.T) {
		cfgDir := filepath.Join(t.TempDir(), "cfg")
		rewrite := rewritePostures[0].setUp(t, cfgDir)
		// off is false to Load's decoder as to the rewrite's.
		appendToConfig(t, cfgDir, "enrich:\n  enabled: true\n  deezerEnabled: off\n")

		code, out := rewrite()
		defer logRunOnFailure(t, out)

		if code != 0 {
			t.Fatalf("the rewrite exited %d", code)
		}
		got := loadInstallConfig(t, cfgDir).Enrich
		if got.Enabled != nil {
			t.Errorf("enrich.enabled = %v, want no key: on is the default", *got.Enabled)
		}
		if got.DeezerEnabled == nil || *got.DeezerEnabled {
			t.Errorf("enrich.deezerEnabled = %v, want false kept", got.DeezerEnabled)
		}
		if _, listed := keptValue(out, "enrich.enabled"); listed {
			t.Error("the run lists enrich.enabled as kept; a first install writes on too")
		}
	})

	t.Run("one that does not read is named and not kept", func(t *testing.T) {
		cfgDir := filepath.Join(t.TempDir(), "cfg")
		rewrite := rewritePostures[0].setUp(t, cfgDir)
		const secret = "maybe-s3cret"
		appendToConfig(t, cfgDir, "enrich:\n  enabled: "+secret+"\n  deezerEnabled: false\n")

		code, out := rewrite()
		defer logRunOnFailure(t, out)

		if code != 0 {
			t.Fatalf("the rewrite exited %d: a switch that does not read must not stop it", code)
		}
		got := loadInstallConfig(t, cfgDir).Enrich
		if got.Enabled != nil {
			t.Errorf("enrich.enabled = %v, want not kept", *got.Enabled)
		}
		if got.DeezerEnabled == nil || *got.DeezerEnabled {
			t.Errorf("enrich.deezerEnabled = %v, want false kept beside the one that does not read", got.DeezerEnabled)
		}
		if !strings.Contains(out, "enrich.enabled (line ") || !strings.Contains(out, "does not read as on or off") {
			t.Errorf("the run does not name the switch it did not keep:\n%s", out)
		}
		if strings.Contains(strings.ToLower(out), secret) {
			t.Error("the run printed the value it did not keep")
		}
	})

	t.Run("a block that is no block keeps nothing and says so", func(t *testing.T) {
		cfgDir := filepath.Join(t.TempDir(), "cfg")
		rewrite := rewritePostures[0].setUp(t, cfgDir)
		appendToConfig(t, cfgDir, "enrich: false\n")

		code, out := rewrite()
		defer logRunOnFailure(t, out)

		if code != 0 {
			t.Fatalf("the rewrite exited %d", code)
		}
		if got := loadInstallConfig(t, cfgDir).Enrich; got.Enabled != nil || got.DeezerEnabled != nil {
			t.Errorf("the rewritten config has enrich switches %v, %v; want none", got.Enabled, got.DeezerEnabled)
		}
		if !strings.Contains(out, "enrich (line ") {
			t.Errorf("the run does not say the block kept nothing:\n%s", out)
		}
	})

	t.Run("the environment is not written", func(t *testing.T) {
		cfgDir := filepath.Join(t.TempDir(), "cfg")
		rewrite := rewritePostures[0].setUp(t, cfgDir)
		t.Setenv("BRIDGE_ENRICH_ENABLED", "false")
		t.Setenv("BRIDGE_ENRICH_DEEZER_ENABLED", "false")

		code, out := rewrite()
		defer logRunOnFailure(t, out)

		if code != 0 {
			t.Fatalf("the rewrite exited %d", code)
		}
		if body := readConfigFile(t, cfgDir); strings.Contains(body, "enrich") {
			t.Errorf("the rewrite wrote the environment's switches into the file:\n%s", body)
		}
	})
}
