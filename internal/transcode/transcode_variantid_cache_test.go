package transcode

import (
	"fmt"
	"testing"
)

// TestVariantID_memoizedMatchesUnmemoized pins the contract that
// the memoization cache produces byte-identical output to the
// unmemoized fmt.Sprintf path. Any future schema-version bump or
// format-string drift surfaces as a test failure rather than a
// silent on-disk drift.
func TestVariantID_memoizedMatchesUnmemoized(t *testing.T) {
	for _, rate := range []int{44100, 48000, 88200, 96000, 176400, 192000} {
		for _, bits := range []int{16, 24} {
			j := JobSpec{TargetSampleRate: rate, TargetBits: bits}
			memo := j.VariantID()
			direct := fmt.Sprintf("upscaled-%s-%d-%d", VariantSchemaVersion, rate, bits)
			if memo != direct {
				t.Errorf("rate=%d bits=%d: memoized %q != direct %q", rate, bits, memo, direct)
			}
		}
	}
}

// TestVariantID_unknownRateFallsThrough pins forward-compat with a
// future rate / bit-depth combination. The fmt.Sprintf fallback
// must produce the correct string even when the cache misses.
func TestVariantID_unknownRateFallsThrough(t *testing.T) {
	j := JobSpec{TargetSampleRate: 352800, TargetBits: 32}
	got := j.VariantID()
	want := fmt.Sprintf("upscaled-%s-352800-32", VariantSchemaVersion)
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestVariantID_negativeInputsFallThrough verifies the cache
// lookup is robust to malformed (rate, bits) — a defensive shape
// for future callers that might pass invalid pre-validation
// values. Predicate: never panics, always returns a well-formed
// string (might be useless for actual playback but won't crash).
func TestVariantID_negativeInputsFallThrough(t *testing.T) {
	j := JobSpec{TargetSampleRate: -1, TargetBits: -1}
	got := j.VariantID()
	want := fmt.Sprintf("upscaled-%s-%d-%d", VariantSchemaVersion, -1, -1)
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestVariantID_noBitDepthAliasing pins the lossless-key contract
// (CodeRabbit Major on PR #211). Pre-fix the cache key packed
// `bits & 0xff`, so `bits=272` (256+16) collided with the cached
// `bits=16` entry and returned the wrong VariantID. With the
// `[2]int` key, any out-of-set (rate, bits) pair misses cleanly
// and falls through to the live fmt.Sprintf.
func TestVariantID_noBitDepthAliasing(t *testing.T) {
	canonical := JobSpec{TargetSampleRate: 192000, TargetBits: 16}.VariantID()
	want := fmt.Sprintf("upscaled-%s-192000-16", VariantSchemaVersion)
	if canonical != want {
		t.Fatalf("warm cache: got %q, want %q", canonical, want)
	}
	// Pre-fix this aliased to the warm (192000, 16) slot via the
	// truncated 8-bit key.
	aliased := JobSpec{TargetSampleRate: 192000, TargetBits: 272}.VariantID()
	wantAliased := fmt.Sprintf("upscaled-%s-192000-272", VariantSchemaVersion)
	if aliased != wantAliased {
		t.Errorf("aliasing: got %q, want %q", aliased, wantAliased)
	}
	if aliased == canonical {
		t.Errorf("aliasing returned the cached canonical %q for bits=272 — key truncation bug regressed", aliased)
	}
}

// TestVariantID_memoizedNoAllocFastPath pins the perf invariant.
// A cached lookup must NOT allocate. fmt.Sprintf would; the
// memoized path returns a pre-built string reference.
func TestVariantID_memoizedNoAllocFastPath(t *testing.T) {
	// Warm the cache first.
	_ = JobSpec{TargetSampleRate: 192000, TargetBits: 24}.VariantID()

	allocs := testing.AllocsPerRun(1000, func() {
		_ = JobSpec{TargetSampleRate: 192000, TargetBits: 24}.VariantID()
	})
	if allocs > 0 {
		t.Errorf("VariantID cached path allocated %g times/op, want 0", allocs)
	}
}
