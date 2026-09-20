package config

import (
	"fmt"
	"strings"
	"testing"
)

// TestVariantSweepMaxDeletePercentDefault pins the relocation guard's
// knob when the field is absent, and that the default is the number the
// field's docblock and CLAUDE.md quote.
func TestVariantSweepMaxDeletePercentDefault(t *testing.T) {
	if got := (&Config{}).VariantSweepMaxDeletePercent(); got != DefaultVariantSweepMaxDeletePercent {
		t.Errorf("got %d, want %d", got, DefaultVariantSweepMaxDeletePercent)
	}
	if DefaultVariantSweepMaxDeletePercent != 20 {
		t.Errorf("default = %d; the field's docblock and CLAUDE.md say 20", DefaultVariantSweepMaxDeletePercent)
	}
}

// TestVariantSweepMaxDeletePercentBounds pins both ends of the accepted
// range (100 disables the guard; 0 refuses any mass deletion while
// sidecars remain) and a refusal — never a clamp — outside it. Same
// three-way convention as its two interval siblings in the `integrity`
// group.
func TestVariantSweepMaxDeletePercentBounds(t *testing.T) {
	for _, v := range []int{0, 1, 20, 99, 100} {
		t.Run(fmt.Sprintf("accepted/%d", v), func(t *testing.T) {
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
		t.Run(fmt.Sprintf("refused/%d", v), func(t *testing.T) {
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
}

// TestVariantSweepMaxDeletePercentRuntimeClone pins that the runtime
// clone keeps its own pointer, like every other pointer field in
// runtime.go.
func TestVariantSweepMaxDeletePercentRuntimeClone(t *testing.T) {
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
}
