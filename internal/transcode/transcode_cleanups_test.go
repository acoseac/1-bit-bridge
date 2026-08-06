package transcode

import (
	"math"
	"strings"
	"testing"
)

// TestProjectedSizeNoNegativeWrapAtBoundary pins finding J (bridge02-03
// review): float64(math.MaxInt64) rounds UP to 2^63, so with sourceSize =
// MaxInt64 and unit ratios `projected` equals 2^63 exactly. The pre-fix
// `projected > math.MaxInt64` guard let that through, and
// int64(math.Round(2^63)) wrapped to math.MinInt64 — a negative projected
// size. The `>=` guard clamps it.
//
// Platform note: this asserts the correct clamp on all platforms, but only
// *bites* on amd64. amd64's CVTTSD2SI yields MinInt64 for the out-of-range
// conversion (bug visible), whereas arm64's FCVTZS SATURATES to MaxInt64
// (bug masked). All production bridges + CI run amd64, so the guard is
// effective where it matters; it harmlessly passes on an arm64 dev Mac.
func TestProjectedSizeNoNegativeWrapAtBoundary(t *testing.T) {
	got := ProjectedSize(math.MaxInt64, 44100, 16, 44100, 16, 1.0)
	if got < 0 {
		t.Fatalf("ProjectedSize wrapped negative at the 2^63 boundary: %d", got)
	}
	if got != math.MaxInt64 {
		t.Fatalf("ProjectedSize should clamp to MaxInt64, got %d", got)
	}
}

// TestSafeVariantFilenameFallbackBoundedAndUnique pins finding H: the
// pathological `budget < 8` fallback (variantID > ~228 chars) must produce
// a name within the filesystem cap AND keep distinct variants of the same
// source distinct. The pre-fix `v.<sha8>` + oversized suffix re-appended
// the too-long variantID-bearing suffix (blowing the cap); the naive
// "drop the suffix" alternative would have collided variants. The fix
// hashes the variantID into the short name.
func TestSafeVariantFilenameFallbackBoundedAndUnique(t *testing.T) {
	const fsBasenameCap = 255 - sidecarTmpReserve
	longVariant := strings.Repeat("x", 260) // > 228 → forces the budget<8 fallback
	a := safeVariantFilename("Some Track.flac", longVariant)
	if len(a) > fsBasenameCap {
		t.Fatalf("fallback name exceeds fsBasenameCap: len=%d cap=%d name=%q", len(a), fsBasenameCap, a)
	}
	if !strings.HasSuffix(a, ".flac") {
		t.Fatalf("fallback name lost the .flac extension: %q", a)
	}
	// Two different variantIDs of the same source must NOT collide.
	b := safeVariantFilename("Some Track.flac", longVariant+"y")
	if a == b {
		t.Fatalf("fallback collided distinct variants: both produced %q", a)
	}
}
