package format_test

import (
	"math"
	"math/big"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/player/format"
)

// BytesPerChannel is Frames rounded up to a byte for every Frames a decoder
// can declare, up to math.MaxInt64, where adding 7 before dividing wraps
// negative. The expectation is computed in arbitrary precision, not with the
// formula under test.
func TestNativeDSDBytesPerChannel(t *testing.T) {
	ceil8 := func(frames int64) int64 {
		q, r := new(big.Int).QuoRem(big.NewInt(frames), big.NewInt(8), new(big.Int))
		if r.Sign() != 0 {
			q.Add(q, big.NewInt(1))
		}
		return q.Int64()
	}
	for _, frames := range []int64{
		0, 1, 7, 8, 9, 797, 106304,
		math.MaxInt64 - 8, math.MaxInt64 - 7, math.MaxInt64 - 6, math.MaxInt64 - 1, math.MaxInt64,
	} {
		got := format.NativeDSD{Frames: frames}.BytesPerChannel()
		if want := ceil8(frames); got != want {
			t.Errorf("Frames %d: BytesPerChannel %d, want %d", frames, got, want)
		}
	}
	// The result is bounded on both sides: at most 2^60 per channel, and a
	// negative Frames (which Validate refuses) counts no bytes.
	if got := (format.NativeDSD{Frames: math.MaxInt64}).BytesPerChannel(); got != 1<<60 {
		t.Errorf("Frames MaxInt64: BytesPerChannel %d, want 2^60", got)
	}
	for _, frames := range []int64{-1, -8, -9, math.MinInt64} {
		if got := (format.NativeDSD{Frames: frames}).BytesPerChannel(); got != 0 {
			t.Errorf("Frames %d: BytesPerChannel %d, want 0", frames, got)
		}
	}
}

func TestNativeDSDValidate(t *testing.T) {
	ok := format.NativeDSD{BitRateHz: format.DSD64RateHz, Channels: 2, Frames: math.MaxInt64}
	if err := ok.Validate(); err != nil {
		t.Fatalf("stereo DSD64 with the largest Frames: %v", err)
	}
	for _, d := range []format.NativeDSD{
		{BitRateHz: format.DSD64RateHz, Channels: 1},
		{BitRateHz: format.DSD64RateHz, Channels: 6},
		{BitRateHz: 3_072_000, Channels: 2},
		{BitRateHz: 16 * format.DSD64RateHz, Channels: 2},
		{BitRateHz: format.DSD64RateHz, Channels: 2, Frames: -1},
	} {
		if err := d.Validate(); err == nil {
			t.Errorf("%+v: Validate accepted it", d)
		}
	}
}
