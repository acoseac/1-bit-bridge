package main

import (
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/dupes"
)

// TestDuplicatesFilterVocabularyLockstep pins the literal identity
// between the config vocabulary (what Validate + the settings PATCH
// accept) and dupes.FilterMode (what PlanSuppression switches on).
// cmd/bridge is the package that imports both, so the tripwire lives
// here; if either side renames a value, dupePolicyFromConfig would
// otherwise silently coerce it into a mode the policy layer doesn't
// recognise (which PlanSuppression treats as suppress-nothing —
// fail-open, but still a silent behaviour change).
func TestDuplicatesFilterVocabularyLockstep(t *testing.T) {
	pairs := []struct {
		cfg  string
		mode dupes.FilterMode
	}{
		{config.DuplicatesFilterOff, dupes.FilterOff},
		{config.DuplicatesFilterSameFormat, dupes.FilterSameFormat},
		{config.DuplicatesFilterHighestQuality, dupes.FilterHighestQuality},
	}
	for _, p := range pairs {
		if p.cfg != string(p.mode) {
			t.Errorf("vocabulary drift: config %q vs dupes %q", p.cfg, p.mode)
		}
	}
}

func TestDupePolicyFromConfig(t *testing.T) {
	cfg := &config.Config{}
	if got := dupePolicyFromConfig(cfg); got.Mode != dupes.FilterHighestQuality {
		t.Fatalf("empty config must resolve to the highest-quality default, got %q", got.Mode)
	}
	cfg.Duplicates.Filter = "off"
	if got := dupePolicyFromConfig(cfg); got.Mode != dupes.FilterOff {
		t.Fatalf("off must map through, got %q", got.Mode)
	}
}
