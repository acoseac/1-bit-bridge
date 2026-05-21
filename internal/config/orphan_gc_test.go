package config

import (
	"testing"
	"time"
)

// TestOrphanSidecarSweepInterval pins the accessor's three-way
// contract: missing-field → 0 (disabled, the default), explicit zero
// → 0, negative → clamped to 0. Mirrors the existing
// VariantSweepInterval testing convention.
func TestOrphanSidecarSweepInterval(t *testing.T) {
	t.Run("missing-field-defaults-to-zero", func(t *testing.T) {
		c := &Config{}
		if got := c.OrphanSidecarSweepInterval(); got != 0 {
			t.Errorf("missing field: got %v, want 0 (disabled)", got)
		}
	})

	t.Run("explicit-zero-is-zero", func(t *testing.T) {
		zero := 0
		c := &Config{}
		c.Integrity.OrphanSidecarSweepIntervalSec = &zero
		if got := c.OrphanSidecarSweepInterval(); got != 0 {
			t.Errorf("explicit zero: got %v, want 0", got)
		}
	})

	t.Run("negative-clamps-to-zero", func(t *testing.T) {
		neg := -300
		c := &Config{}
		c.Integrity.OrphanSidecarSweepIntervalSec = &neg
		if got := c.OrphanSidecarSweepInterval(); got != 0 {
			t.Errorf("negative: got %v, want 0 (clamped — busy-loop guard)", got)
		}
	})

	t.Run("positive-value-passes-through", func(t *testing.T) {
		secs := 1800
		c := &Config{}
		c.Integrity.OrphanSidecarSweepIntervalSec = &secs
		got := c.OrphanSidecarSweepInterval()
		want := 1800 * time.Second
		if got != want {
			t.Errorf("positive: got %v, want %v", got, want)
		}
	})
}

// TestOrphanSidecarSweepIntervalRuntimeCopyPreservesPointer pins
// the runtime-copy invariant: deep-copying a Config carries the
// explicit pointer value across (so a live SIGHUP-style reload
// preserves operator-set values rather than reverting them).
func TestOrphanSidecarSweepIntervalRuntimeCopyPreservesPointer(t *testing.T) {
	original := 600
	src := &Config{}
	src.Integrity.OrphanSidecarSweepIntervalSec = &original

	dst := Clone(src)
	if dst.Integrity.OrphanSidecarSweepIntervalSec == nil {
		t.Fatal("runtime clone dropped the explicit pointer")
	}
	if *dst.Integrity.OrphanSidecarSweepIntervalSec != original {
		t.Errorf("runtime clone changed value: got %d, want %d",
			*dst.Integrity.OrphanSidecarSweepIntervalSec, original)
	}
	// The clone MUST be a separate pointer (mutating the dst's
	// value must not mutate src). Same shape every other pointer
	// field in runtime.go preserves.
	mutated := 9999
	*dst.Integrity.OrphanSidecarSweepIntervalSec = mutated
	if *src.Integrity.OrphanSidecarSweepIntervalSec == mutated {
		t.Error("runtime clone shares pointer with source — mutation leaked")
	}
}
