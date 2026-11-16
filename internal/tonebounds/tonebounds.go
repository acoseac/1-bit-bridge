// Package tonebounds is the bounds of the bridge's test tone, a 1 kHz sine
// never louder than Loudest dBFS: its frequency, its level, and the length of
// the console's tone (B601). The player builds the tone
// (internal/player/testtone, which asks Check before it builds anything), and
// the console (internal/admin), which imports no player package, refuses a
// request by these very bounds; so they live outside internal/player and
// import nothing but the standard library.
package tonebounds

import (
	"errors"
	"math"
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
