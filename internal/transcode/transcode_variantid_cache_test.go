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
