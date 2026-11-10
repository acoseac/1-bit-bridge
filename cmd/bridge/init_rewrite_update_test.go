package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestARewriteKeepsTheUpdateCheckSwitchWrittenOff: `bridge init --force` keeps
// update.checkEnabled where the config it replaces writes it off (backlog
// B761), in either posture, and printKept says so, for the reason it keeps
// the enrichment opt-outs: the default would turn the operator's opt-out of
// the update check's requests back on without a word, and init asks for
// neither. One written on is the default and is written as no key; one that
// does not read is not kept and is named, by its line and never by its value,
// and the run says the check is on now; and the environment of whoever runs
// init is not written.
func TestARewriteKeepsTheUpdateCheckSwitchWrittenOff(t *testing.T) {
	for _, p := range rewritePostures {
		t.Run(p.name+": off is kept", func(t *testing.T) {
			cfgDir := filepath.Join(t.TempDir(), "cfg")
			rewrite := p.setUp(t, cfgDir)
			appendToConfig(t, cfgDir, "update:\n  checkEnabled: false\n")

			code, out := rewrite()
			defer logRunOnFailure(t, out)

			if code != 0 {
				t.Fatalf("the rewrite exited %d", code)
			}
			got := loadInstallConfig(t, cfgDir).Update
			if got.CheckEnabled == nil || *got.CheckEnabled {
				t.Errorf("the rewritten config has update.checkEnabled %v; want it written false", got.CheckEnabled)
			}
			if v, ok := keptValue(out, "update.checkEnabled"); !ok || v != "false" {
				t.Errorf("the run's list of what it kept gives update.checkEnabled %q (listed %v), want false", v, ok)
			}
		})
	}

	t.Run("one written on is the default", func(t *testing.T) {
		cfgDir := filepath.Join(t.TempDir(), "cfg")
		rewrite := rewritePostures[0].setUp(t, cfgDir)
		appendToConfig(t, cfgDir, "update:\n  checkEnabled: true\n")

		code, out := rewrite()
		defer logRunOnFailure(t, out)

		if code != 0 {
			t.Fatalf("the rewrite exited %d", code)
		}
		if got := loadInstallConfig(t, cfgDir).Update; got.CheckEnabled != nil {
			t.Errorf("update.checkEnabled = %v, want no key: on is the default", *got.CheckEnabled)
		}
		if _, listed := keptValue(out, "update.checkEnabled"); listed {
			t.Error("the run lists update.checkEnabled as kept; a first install writes on too")
		}
	})

	t.Run("the file's off is kept as the file reads it", func(t *testing.T) {
		cfgDir := filepath.Join(t.TempDir(), "cfg")
		rewrite := rewritePostures[0].setUp(t, cfgDir)
		// off is false to Load's decoder as to the rewrite's.
		appendToConfig(t, cfgDir, "update:\n  checkEnabled: off\n")

		code, out := rewrite()
		defer logRunOnFailure(t, out)

		if code != 0 {
			t.Fatalf("the rewrite exited %d", code)
		}
		if got := loadInstallConfig(t, cfgDir).Update; got.CheckEnabled == nil || *got.CheckEnabled {
			t.Errorf("update.checkEnabled = %v, want false kept", got.CheckEnabled)
		}
	})

	t.Run("one that does not read is named and not kept", func(t *testing.T) {
		cfgDir := filepath.Join(t.TempDir(), "cfg")
		rewrite := rewritePostures[0].setUp(t, cfgDir)
		const secret = "maybe-s3cret"
		appendToConfig(t, cfgDir, "update:\n  checkEnabled: "+secret+"\n")

		code, out := rewrite()
		defer logRunOnFailure(t, out)

		if code != 0 {
			t.Fatalf("the rewrite exited %d: a switch that does not read must not stop it", code)
		}
		if got := loadInstallConfig(t, cfgDir).Update; got.CheckEnabled != nil {
			t.Errorf("update.checkEnabled = %v, want not kept", *got.CheckEnabled)
		}
		if !strings.Contains(out, "update.checkEnabled (line ") || !strings.Contains(out, "does not read as on or off") {
			t.Errorf("the run does not name the switch it did not keep:\n%s", out)
		}
		if !strings.Contains(out, "update checks are on now") {
			t.Errorf("the run does not say what the switch not kept costs:\n%s", out)
		}
		if strings.Contains(strings.ToLower(out), secret) {
			t.Error("the run printed the value it did not keep")
		}
	})

	t.Run("a block that is no block keeps nothing and says so", func(t *testing.T) {
		cfgDir := filepath.Join(t.TempDir(), "cfg")
		rewrite := rewritePostures[0].setUp(t, cfgDir)
		appendToConfig(t, cfgDir, "update: false\n")

		code, out := rewrite()
		defer logRunOnFailure(t, out)

		if code != 0 {
			t.Fatalf("the rewrite exited %d", code)
		}
		if got := loadInstallConfig(t, cfgDir).Update; got.CheckEnabled != nil {
			t.Errorf("the rewritten config has update.checkEnabled %v; want none", *got.CheckEnabled)
		}
		if !strings.Contains(out, "update (line ") {
			t.Errorf("the run does not say the block kept nothing:\n%s", out)
		}
	})

	t.Run("the environment is not written", func(t *testing.T) {
		cfgDir := filepath.Join(t.TempDir(), "cfg")
		rewrite := rewritePostures[0].setUp(t, cfgDir)
		t.Setenv("BRIDGE_UPDATE_CHECK_ENABLED", "false")

		code, out := rewrite()
		defer logRunOnFailure(t, out)

		if code != 0 {
			t.Fatalf("the rewrite exited %d", code)
		}
		if body := readConfigFile(t, cfgDir); strings.Contains(body, "checkEnabled") {
			t.Errorf("the rewrite wrote the environment's switch into the file:\n%s", body)
		}
	})
}
