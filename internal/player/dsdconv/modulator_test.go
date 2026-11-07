package dsdconv

import (
	"crypto/sha256"
	"encoding/hex"
	"math"
	"testing"
)

// ciff is the 5th-order low-distortion CIFF modulator of
// internal/transcode/testdata/gen/dsd_fixtures.py (its modulate), ported
// line for line: the same coefficients, the same order of operations and
// the same quantizer test, with every product converted to float64 on its
// own, which forbids Go to fuse it with the add beside it (arm64 may fuse
// otherwise) and rounds it once, as CPython does. sample(i) is the input
// before FULL_SCALE's 0.5, and it is called for i = 0, 1, ... in order. The
// bits are one a byte, 0 or 1.
//
// That generator modulates DSD64 only; this one modulates at whatever rate
// its sample function was made for, which is how the fixtures at DSD128 and
// DSD256 are modulated at their own rates, never a DSD64 stream repeated or
// relabelled. Fixtures over a megabyte are made here, never committed.
func ciff(n int, sample func(i int) float64) []byte {
	const (
		b0, b1, b2, b3, b4 = 0.40, 0.20, 0.10, 0.05, 0.02
		a0, a1, a2, a3, a4 = 1.0, 1.0, 1.0, 1.0, 1.0
		fullScale          = 0.5
	)
	var s0, s1, s2, s3, s4 float64
	out := make([]byte, n)
	for i := 0; i < n; i++ {
		u := float64(sample(i) * fullScale)
		v := u + float64(a0*s0) + float64(a1*s1) + float64(a2*s2) + float64(a3*s3) + float64(a4*s4)
		y := -1.0
		if v >= 0 {
			y = 1.0
			out[i] = 1
		}
		e := u - y
		s4 = s4 + float64(b4*s3)
		s3 = s3 + float64(b3*s2)
		s2 = s2 + float64(b2*s1)
		s1 = s1 + float64(b1*s0)
		s0 = s0 + float64(b0*e)
	}
	return out
}

// tones is a sample function of sines at freqs, each of amplitude amp, at
// rate: the generator's tone (one sine, amp 1) and twin_tone (two, amp
// 0.5). Its sines are math.Sin's, whose last ulp may differ from one libm
// and one architecture to another, so a fixture made with it has the same
// spectrum everywhere but not the same bits; a test that needs the same
// bits everywhere uses randomBits or rotation.
func tones(rate int, amp float64, freqs ...float64) func(int) float64 {
	twoPi := 2.0 * math.Pi
	ws := make([]float64, len(freqs))
	for k, f := range freqs {
		ws[k] = float64(twoPi*f) / float64(rate)
	}
	return func(i int) float64 {
		var v float64
		for _, w := range ws {
			v += math.Sin(float64(w * float64(i)))
		}
		return float64(amp * v)
	}
}

// rotation is a sine made by rotating (cos, sin) through c and s, each
// step two products and a sum per component, every operation one rounding:
// the same samples in CPython and in Go on every architecture, with no libm
// call. It must be called for i = 0, 1, ... in order, as ciff calls it.
func rotation(c, s float64) func(int) float64 {
	y, x := 0.0, 1.0
	return func(int) float64 {
		v := y
		y, x = float64(y*c)+float64(x*s), float64(x*c)-float64(y*s)
		return v
	}
}

// TestTheModulatorIsThePythonGenerators holds the port to the generator it
// ports. The digest is the Python tool's own: modulate() of
// internal/transcode/testdata/gen/dsd_fixtures.py, loaded by path and
// driven with the same rotation (c 0x1.ffffacda93a18p-1, s
// 0x1.23ca52113ba91p-9, cos and sin of 2π·1000/2822400), for one second at
// DSD64, run by CPython 3.14.4 on linux/amd64 on 2026-11-06; the SHA-256 is
// of the one-bit samples, one byte each. It was not computed by this
// package.
//
// The generator's own tones go through libm's sin, and a 1-bit loop is
// chaotic: one ulp of a sample flips a decision some time later, and the
// streams part. Measured that day: the committed fixtures in
// internal/transcode/testdata, made on a Mac, and this host's run of the
// same script agree for the first 37,038 samples of the 1 kHz tone (23,434
// of the 50 kHz probe) and then differ in 443,142 (476,710) of 2,822,400.
// So the port is held to the generator through an input with no libm in
// it, and the tones of the other tests are fixtures of the same modulator,
// alike in spectrum on every host, not in bits.
func TestTheModulatorIsThePythonGenerators(t *testing.T) {
	const pythonDigest = "c164f2ef37c9eb01d8c07404b4e81f3297f80e3009f0e7e13e137dbd4f804154"
	bits := ciff(2822400, rotation(0x1.ffffacda93a18p-1, 0x1.23ca52113ba91p-9))
	ones := 0
	for _, b := range bits {
		ones += int(b)
	}
	sum := sha256.Sum256(bits)
	if got := hex.EncodeToString(sum[:]); got != pythonDigest {
		t.Fatalf("the port's one-bit samples hash to %s, the Python generator's to %s (%d ones of %d)", got, pythonDigest, ones, len(bits))
	}
	if ones != 1411200 {
		t.Errorf("%d ones, the Python run had 1411200", ones)
	}
}
