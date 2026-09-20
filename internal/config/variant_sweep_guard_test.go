package config

import (
	"strings"
	"testing"
)

// TestVariantSweepMaxDeletePercent pins the relocation guard's knob: the
// default when the field is absent, both ends of the accepted range
// (100 disables the guard; 0 refuses any mass deletion while sidecars
// remain), a refusal — never a clamp — outside it, and a runtime clone
// that keeps its own pointer. Same three-way convention as its two
// interval siblings in the `integrity` group.
func TestVariantSweepMaxDeletePercent(t *testing.T) {
	t.Run("missing field is the default", func(t *testing.T) {
		if got := (&Config{}).VariantSweepMaxDeletePercent(); got != DefaultVariantSweepMaxDeletePercent {
			t.Errorf("got %d, want %d", got, DefaultVariantSweepMaxDeletePercent)
		}
		if DefaultVariantSweepMaxDeletePercent != 20 {
			t.Errorf("default = %d; the field's docblock and CLAUDE.md say 20", DefaultVariantSweepMaxDeletePercent)
		}
	})
	for _, v := range []int{0, 1, 20, 99, 100} {
		t.Run("accepted", func(t *testing.T) {
			cfg := retentionBaseConfig()
			cfg.Integrity.VariantSweepMaxDeletePercent = &v
			if err := cfg.Validate(); err != nil {
				t.Fatalf("%d refused: %v", v, err)
			}
			if got := cfg.VariantSweepMaxDeletePercent(); got != v {
				t.Errorf("accessor = %d, want %d verbatim", got, v)
			}
		})
	}
	for _, v := range []int{-1, 101, 200, 1000} {
		t.Run("refused", func(t *testing.T) {
			cfg := retentionBaseConfig()
			cfg.Integrity.VariantSweepMaxDeletePercent = &v
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("%d accepted; a typo'd 200 would silently mean disabled", v)
			}
			if !strings.Contains(err.Error(), "integrity.variantSweepMaxDeletePercent") {
				t.Errorf("%d refused for the wrong reason: %v", v, err)
			}
		})
	}
	t.Run("runtime clone keeps its own pointer", func(t *testing.T) {
		original := 35
		src := &Config{}
		src.Integrity.VariantSweepMaxDeletePercent = &original
		dst := Clone(src)
		if dst.Integrity.VariantSweepMaxDeletePercent == nil || *dst.Integrity.VariantSweepMaxDeletePercent != original {
			t.Fatalf("clone dropped or changed the value: %v", dst.Integrity.VariantSweepMaxDeletePercent)
		}
		*dst.Integrity.VariantSweepMaxDeletePercent = 99
		if *src.Integrity.VariantSweepMaxDeletePercent == 99 {
			t.Error("runtime clone shares its pointer with the source")
		}
	})
}
