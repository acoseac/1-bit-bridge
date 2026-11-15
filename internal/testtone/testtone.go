// Package testtone is the bridge's test tone: a 1 kHz sine on both channels,
// never louder than Loudest dBFS. bridge output pcm-test, the player's
// hardware harness run by hand (B470 PR 2a), builds it, and so does the
// player's backend for the console's test tone (B601). Its bounds are asked
// before anything is built (Build asks Check itself), so no caller builds a
// louder tone, whatever it checked first.
//
// It lives outside internal/player so that the console (internal/admin), which
// imports no player package, refuses a request by the very bounds the backend
// refuses it by (Check, CheckSeconds).
package testtone

import (
	"errors"
	"fmt"
	"math"

	"github.com/acoseac/1-bit-bridge/internal/player/format"
)

// ToneHz is the tone's frequency.
const ToneHz = 1000

// Loudest and Quietest bound the tone's peak, in dBFS: never louder than -30,
// since it is played on headphones by hand and through a DAC and an amplifier
// whose volume the bridge cannot see.
const (
	Loudest  = -30.0
	Quietest = -90.0
)

// MinSeconds and MaxSeconds bound the console's tone, the one the player's
// backend plays (B601). pcm-test, run by hand, takes its own --seconds, 1 to
// 600.
const (
	MinSeconds = 1
	MaxSeconds = 30
)

// ErrLevel is a level that is not a number from Quietest to Loudest, and
// ErrLength a length outside MinSeconds..MaxSeconds.
var (
	ErrLevel  = errors.New("the tone's level is a number from -90 to -30 dBFS")
	ErrLength = errors.New("the tone lasts from 1 to 30 seconds")
)

// Check refuses a level that is not a number from Quietest to Loudest dBFS:
// NaN and the infinities included.
func Check(level float64) error {
	if math.IsNaN(level) || math.IsInf(level, 0) || level > Loudest || level < Quietest {
		return ErrLevel
	}
	return nil
}

// CheckSeconds refuses a length outside MinSeconds..MaxSeconds.
func CheckSeconds(seconds int) error {
	if seconds < MinSeconds || seconds > MaxSeconds {
		return ErrLength
	}
	return nil
}

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
// level, after Check: a sine whose peak is the level's fraction of full scale,
// rounded down, each sample rounded to the nearest, on both channels, packed by
// the format package's packer.
func Build(rate, bits int, f format.ALSAFormat, level float64) (Tone, error) {
	if err := Check(level); err != nil {
		return Tone{}, err
	}
	if rate < 1 {
		return Tone{}, fmt.Errorf("testtone: a rate of %d Hz", rate)
	}
	if bits != 16 && bits != 24 && bits != 32 {
		return Tone{}, fmt.Errorf("testtone: %d valid bits: the tone is 16, 24 or 32", bits)
	}
	frames := rate / gcd(rate, ToneHz)
	full := int64(1)<<(bits-1) - 1
	amp := int64(math.Floor(float64(full) * math.Pow(10, level/20)))
	samples := make([]int32, 2*frames)
	for n := range frames {
		v := int32(math.Round(float64(amp) * math.Sin(2*math.Pi*ToneHz*float64(n)/float64(rate))))
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
