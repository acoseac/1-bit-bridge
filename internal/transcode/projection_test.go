package transcode

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"
)

// TestProjectedSize_RateAndBitsScaling pins the multiplicative model
// against canonical inputs across the common upscale pairs.
func TestProjectedSize_RateAndBitsScaling(t *testing.T) {
	tests := []struct {
		name       string
		sourceSize int64
		sourceRate int
		sourceBits int
		targetRate int
		targetBits int
		factor     float64
		// Acceptable absolute deviation from the model output (rounding
		// + the math.Round inside ProjectedSize gives ±1 at worst).
		wantTolerance int64
	}{
		{
			name:          "44.1/16 → 192/24 (typical upscale)",
			sourceSize:    100_000_000, // 100 MB FLAC source
			sourceRate:    44100,
			sourceBits:    16,
			targetRate:    192000,
			targetBits:    24,
			factor:        FLACCompressionFactor24Bit,
			wantTolerance: 2,
		},
		{
			name:          "48/24 → 96/24 (octave only)",
			sourceSize:    200_000_000,
			sourceRate:    48000,
			sourceBits:    24,
			targetRate:    96000,
			targetBits:    24,
			factor:        FLACCompressionFactor24Bit,
			wantTolerance: 2,
		},
		{
			name:          "16/16 → 16/16 same target (no upscale)",
			sourceSize:    50_000_000,
			sourceRate:    44100,
			sourceBits:    16,
			targetRate:    44100,
			targetBits:    16,
			factor:        FLACCompressionFactor16Bit,
			wantTolerance: 2,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ProjectedSize(tt.sourceSize, tt.sourceRate, tt.sourceBits,
				tt.targetRate, tt.targetBits, tt.factor)
			// Recompute the expected model output independently — the test
			// is the contract; if the formula changes, the test should
			// fail on every case until intentionally re-baselined.
			want := int64(math.Round(
				float64(tt.sourceSize) *
					(float64(tt.targetRate) / float64(tt.sourceRate)) *
					(float64(tt.targetBits) / float64(tt.sourceBits)) *
					tt.factor,
			))
			if abs64(got-want) > tt.wantTolerance {
				t.Errorf("ProjectedSize = %d, want %d (±%d)", got, want, tt.wantTolerance)
			}
		})
	}
}

// TestProjectedSize_DegenerateInputs locks the zero-on-bad-input
// contract. Every parameter is independently tested to ensure no
// single zero/negative value leaks through as a non-zero estimate.
func TestProjectedSize_DegenerateInputs(t *testing.T) {
	const (
		size        = 1_000_000.0
		srcRate     = 44100.0
		srcBits     = 16.0
		tgtRate     = 192000.0
		tgtBits     = 24.0
		validFactor = FLACCompressionFactor24Bit
	)
	cases := []struct {
		name string
		args [6]float64 // {size, srcRate, srcBits, tgtRate, tgtBits, factor}
	}{
		{"zero sourceSize", [6]float64{0, srcRate, srcBits, tgtRate, tgtBits, validFactor}},
		{"negative sourceSize", [6]float64{-size, srcRate, srcBits, tgtRate, tgtBits, validFactor}},
		{"zero sourceRate", [6]float64{size, 0, srcBits, tgtRate, tgtBits, validFactor}},
		{"zero sourceBits", [6]float64{size, srcRate, 0, tgtRate, tgtBits, validFactor}},
		{"zero targetRate", [6]float64{size, srcRate, srcBits, 0, tgtBits, validFactor}},
		{"zero targetBits", [6]float64{size, srcRate, srcBits, tgtRate, 0, validFactor}},
		{"zero factor", [6]float64{size, srcRate, srcBits, tgtRate, tgtBits, 0}},
		{"negative factor", [6]float64{size, srcRate, srcBits, tgtRate, tgtBits, -0.5}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ProjectedSize(
				int64(c.args[0]),
				int(c.args[1]), int(c.args[2]),
				int(c.args[3]), int(c.args[4]),
				c.args[5],
			)
			if got != 0 {
				t.Errorf("ProjectedSize = %d, want 0 for degenerate input %v", got, c.args)
			}
		})
	}
}

// TestDefaultCompressionFactor pins the per-bit-depth resolver.
func TestDefaultCompressionFactor(t *testing.T) {
	tests := []struct {
		bits int
		want float64
	}{
		{16, FLACCompressionFactor16Bit},
		{24, FLACCompressionFactor24Bit},
		{32, FLACCompressionFactor24Bit}, // 32-bit falls through to 24-bit factor
		{0, FLACCompressionFactor24Bit},  // defensive default
		{-1, FLACCompressionFactor24Bit},
	}
	for _, tt := range tests {
		got := DefaultCompressionFactor(tt.bits)
		if got != tt.want {
			t.Errorf("DefaultCompressionFactor(%d) = %v, want %v", tt.bits, got, tt.want)
		}
	}
}

// TestDiskHasHeadroom_HappyPath probes a real directory (t.TempDir())
// for free space and asserts ok=true with a non-zero freeBytes count.
// We can't deterministically know the free-byte count on the CI host,
// so the assertion is on the structural contract rather than a
// numeric value.
func TestDiskHasHeadroom_HappyPath(t *testing.T) {
	dir := t.TempDir()
	ok, free, err := DiskHasHeadroom(dir, 0, DefaultDiskSafetyMargin)
	if err != nil {
		t.Fatalf("DiskHasHeadroom: %v", err)
	}
	if !ok {
		t.Errorf("ok = false on a fresh temp dir with no projected bytes")
	}
	if free <= 0 {
		t.Errorf("freeBytes = %d, want > 0", free)
	}
}

// TestDiskHasHeadroom_RefusalCarriesError verifies the typed error
// shape when the projection exceeds free space. The projected value
// is set to a deliberately absurd MaxInt64 so the path is taken on
// any plausible test host.
func TestDiskHasHeadroom_RefusalCarriesError(t *testing.T) {
	dir := t.TempDir()
	const projected = math.MaxInt64
	ok, free, err := DiskHasHeadroom(dir, projected, DefaultDiskSafetyMargin)
	if ok {
		t.Fatalf("ok = true with projected = MaxInt64")
	}
	if free <= 0 {
		t.Errorf("freeBytes = %d, want > 0 (probe still ran)", free)
	}
	if !errors.Is(err, ErrInsufficientDiskSpace) {
		t.Errorf("err = %v, want errors.Is(ErrInsufficientDiskSpace)", err)
	}
	var typed *InsufficientDiskSpaceError
	if !errors.As(err, &typed) {
		t.Fatalf("err = %v, want errors.As *InsufficientDiskSpaceError", err)
	}
	if typed.AvailableBytes != free {
		t.Errorf("typed.AvailableBytes = %d, freeBytes = %d", typed.AvailableBytes, free)
	}
	if typed.Dir != dir {
		t.Errorf("typed.Dir = %q, want %q", typed.Dir, dir)
	}
}

// TestDiskHasHeadroom_NegativeMarginClamped ensures a malformed
// (negative or NaN) safety margin doesn't underflow the required-
// bytes calculation and falsely approve a batch.
func TestDiskHasHeadroom_NegativeMarginClamped(t *testing.T) {
	dir := t.TempDir()
	ok, _, err := DiskHasHeadroom(dir, 1, -1.5)
	if err != nil {
		t.Fatalf("DiskHasHeadroom: %v", err)
	}
	if !ok {
		t.Errorf("ok = false; 1 byte projected against any real free space should approve")
	}
}

// TestAvailableDiskSpace_RealDir is a thin smoke test on the
// per-platform implementation. The exact value depends on the host
// — we just assert the call doesn't fail and returns a positive
// number on a valid directory.
func TestAvailableDiskSpace_RealDir(t *testing.T) {
	dir := t.TempDir()
	n, err := AvailableDiskSpace(dir)
	if err != nil {
		t.Fatalf("AvailableDiskSpace: %v", err)
	}
	if n <= 0 {
		t.Errorf("AvailableDiskSpace = %d, want > 0", n)
	}
}

// TestAvailableDiskSpace_MissingDir asserts the probe surfaces an
// error for a non-existent path rather than silently returning zero.
// Critical: the coordinator's pre-flight refuses a batch on probe
// failure; silently returning zero would mis-classify "can't check"
// as "no headroom" and confuse operators.
func TestAvailableDiskSpace_MissingDir(t *testing.T) {
	missing := filepath.Join(os.TempDir(), "definitely-not-a-real-dir-1bit-bridge-xyz123")
	// Make sure it really doesn't exist.
	_ = os.RemoveAll(missing)
	_, err := AvailableDiskSpace(missing)
	if err == nil {
		t.Errorf("AvailableDiskSpace on missing dir returned nil error")
	}
}

func abs64(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}
