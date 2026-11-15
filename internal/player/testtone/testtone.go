// Package testtone builds the bridge's test tone: a 1 kHz sine on both
// channels, never louder than tonebounds.Loudest dBFS, packed for one rate,
// valid bits and format. bridge output pcm-test, the player's hardware
// harness run by hand (B470 PR 2a), builds it, and so does the player's
// backend for the console's test tone (B601). Build asks the level's bounds
// (tonebounds.Check) before it computes anything, so no caller builds a
// louder tone, whatever it checked first. The bounds are internal/tonebounds',
// which the console, importing no player package, refuses a request by.
package testtone

import (
	"fmt"
	"math"

	"github.com/acoseac/1-bit-bridge/internal/player/format"
	"github.com/acoseac/1-bit-bridge/internal/tonebounds"
)

// Tone is the tone packed for one rate, valid bits and format: the frames a 1
// kHz sine at that rate takes to come back to its first sample exactly, so
// whole repeats of Packed play without a seam. Amplitude is its peak and
// FullScale the format's, as sample values.
type Tone struct {
	Packed    []byte
	Frames    int
	Amplitude int64
	FullScale int64
}

// Build builds the tone for a rate, valid bits (16, 24 or 32), format and
// level, after tonebounds.Check: a sine whose peak is the level's fraction of
// full scale, rounded down, each sample rounded to the nearest, on both
// channels, packed by the format package's packer.
func Build(rate, bits int, f format.ALSAFormat, level float64) (Tone, error) {
	if err := tonebounds.Check(level); err != nil {
		return Tone{}, err
	}
	if rate < 1 {
		return Tone{}, fmt.Errorf("testtone: a rate of %d Hz", rate)
	}
	if bits != 16 && bits != 24 && bits != 32 {
		return Tone{}, fmt.Errorf("testtone: %d valid bits: the tone is 16, 24 or 32", bits)
	}
	frames := rate / gcd(rate, tonebounds.ToneHz)
	full := int64(1)<<(bits-1) - 1
	amp := int64(math.Floor(float64(full) * math.Pow(10, level/20)))
	samples := make([]int32, 2*frames)
	for n := range frames {
		v := int32(math.Round(float64(amp) * math.Sin(2*math.Pi*tonebounds.ToneHz*float64(n)/float64(rate))))
		samples[2*n], samples[2*n+1] = v, v
	}
	size := format.PackedBytes(len(samples), f)
	if size < 0 {
		return Tone{}, fmt.Errorf("testtone: no packing %q", f)
	}
	packed := make([]byte, size)
	if _, err := format.Pack(packed, samples, bits, f); err != nil {
		return Tone{}, err
	}
	return Tone{Packed: packed, Frames: frames, Amplitude: amp, FullScale: full}, nil
}

// gcd is the greatest common divisor of two positive integers.
func gcd(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}
