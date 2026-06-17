package config

import (
	"testing"
	"time"
)

func TestAtlasEffectiveMetaTTL(t *testing.T) {
	if got := (AtlasConfig{}).EffectiveMetaTTL(); got != DefaultAtlasMetaTTLHours*time.Hour {
		t.Errorf("default = %v, want %v", got, DefaultAtlasMetaTTLHours*time.Hour)
	}
	if got := (AtlasConfig{MetaTTLHours: 48}).EffectiveMetaTTL(); got != 48*time.Hour {
		t.Errorf("explicit 48h = %v, want 48h", got)
	}
	if got := (AtlasConfig{MetaTTLHours: -5}).EffectiveMetaTTL(); got != DefaultAtlasMetaTTLHours*time.Hour {
		t.Errorf("negative MetaTTLHours falls back to default, got %v", got)
	}
}
