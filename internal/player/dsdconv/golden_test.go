package dsdconv

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/player/format"
)

// TestTheChainIsTheIndependentToolsGolden holds the package to a golden it
// did not compute. internal/transcode/testdata/gen/dsdconv_golden.py is
// written from the chain's design (B728 §4.2 and this package's
// documentation, which its docstring sets out) and shares no code with it:
// it designs the coefficients in floats with + - * / and sqrt alone, and
// converts by the chain's definition, a sum for each value, with no table,
// delay line or read. Its lines, run by CPython 3.14.4 on linux/amd64 on
// 2026-11-07, are the digests below: SHA-256 of each filter's taps as int64
// little-endian (g1 over its 8T slots, slot 0 included), and for each plan
// of the input's one-bit samples (a byte each, the left channel's, then the
// right's) and of the output's samples, int32 little-endian, interleaved
// left, right. The test runs on every architecture: the binary built for the
// Pi carries every digest, and reads no file.
//
// The input (goldenInput) is libm-free: the generator's modulator over a
// rotation from hex constants at 1 kHz on the left and 15 kHz on the right,
// then a run of the byte 0x1C and its complement, whose stage-1 sums are
// 64 mod 128, so the 24-bit plan rounds an exact tie in each of its frames'
// samples there (the tool counts 464, the left channel's, which half away
// from zero would move, and the right's, which half up would), and a run of
// ones and zeros, full scale, which saturates (367 samples at 176.4 kHz, 338
// at 44.1). The ends start and stop at numeric zero, and the 16-bit plan's
// dither is drawn for each sample. Its own digest is checked first, so a
// difference in the input is never taken for one in the chain.
func TestTheChainIsTheIndependentToolsGolden(t *testing.T) {
	r64 := format.DSD64RateHz
	p176, p44 := Plan{176400, 24}, Plan{44100, 16}
	for _, c := range []struct {
		name   string
		taps   []int64
		n      int
		digest string
	}{
		{"g1 DSD64", geom(t, r64, p176).g1, 80, "1dbe5cc4e48c3e14043f405e577df46d793ab891f959d14c713948f868b2da10"},
		{"g1 DSD128", geom(t, 2*r64, p176).g1, 160, "e5ac798daf4dacf480acdbe58f6765211419190bc06ff11875b2121e80a8e956"},
		{"g1 DSD256", geom(t, 4*r64, p176).g1, 304, "dff1664f4c2a15298f5619320acea5e886281a31a1b293756e7ae1d4b0138a70"},
		{"g2", geom(t, r64, p176).g2, 31, "8a193bb9cb833abef7100d8eb60c64900b6d3544659abc58b88e7625d3c20e14"},
		{"g3 30000-40000", geom(t, r64, p176).g3, 127, "6914606d90378ef6dbce69d4bfac5cb4ead7b5110ebb39e94a198b251f099c8d"},
		{"g3 20000-22050", geom(t, r64, p44).g3, 613, "80eb4146c61fb53f0ccf614c0ed72555bd5240827e7655c57dd4a7dbae60282b"},
	} {
		var raw []byte
		for _, v := range c.taps {
			raw = binary.LittleEndian.AppendUint64(raw, uint64(v))
		}
		if got := sha256Hex(raw); len(c.taps) != c.n || got != c.digest {
			t.Errorf("taps %s: %d of them hashing to %s; the tool's %d hash to %s", c.name, len(c.taps), got, c.n, c.digest)
		}
	}
	for _, c := range []struct {
		plan           Plan
		input, output  string
		clipped, ticks int64
	}{
		{p176, "4d5ae95d2ef71e1895070e164e23437766cf25f0bdaeef217e77eef051f7aa19",
			"df0a5c4001eba1dc2645a0e1b263e4f2a515636275cb82a36dd868f2c6227890", 367, 2000},
		{p44, "44ed344b898ef451833ca8629656dcf68142c7ca48a50e6d8518cbea9af6a531",
			"1b9635bc7d0a87235448f2cf9815236d87292963e2ef9f4f0e496c67e09835f9", 338, 2000},
	} {
		d, err := c.plan.Decimation(r64)
		if err != nil {
			t.Fatal(err)
		}
		ch := goldenInput(d)
		if got := sha256Hex(concat(ch[0], ch[1])); got != c.input {
			t.Errorf("%s: the input hashes to %s, the tool's to %s: the modulator, the rotation or the runs differ, not the chain", c.plan, got, c.input)
			continue
		}
		conv := mustNew(t, stubOf(r64, ch), c.plan, Options{})
		out, err := drain(t, conv, 500)
		if !errors.Is(err, io.EOF) {
			t.Fatalf("%s: %v", c.plan, err)
		}
		var raw []byte
		for _, v := range out {
			raw = binary.LittleEndian.AppendUint32(raw, uint32(v))
		}
		if got := sha256Hex(raw); int64(len(out)) != 2*c.ticks || got != c.output {
			t.Errorf("%s: %d frames hashing to %s; the tool's %d hash to %s", c.plan, len(out)/2, got, c.ticks, c.output)
		}
		if conv.Clipped() != c.clipped {
			t.Errorf("%s: %d samples clipped, the tool's %d", c.plan, conv.Clipped(), c.clipped)
		}
	}
}

// goldenInput is the tool's input at a total ratio of d: 2,000 frames' bits
// a channel, the modulator over a 1 kHz rotation on the left and a 15 kHz
// one on the right (cos and sin of 2π·f/2822400, as the tool gives them),
// then frames 750 to 1124's bits the byte 0x1C over and over on the left and
// its complement on the right, and frames 1500 to 1749's all ones on the
// left and all zeros on the right.
func goldenInput(d int) [2][]byte {
	n := 2000 * d
	ch := [2][]byte{
		ciff(n, rotation(0x1.ffffacda93a18p-1, 0x1.23ca52113ba91p-9)),
		ciff(n, rotation(0x1.ffb6edd6c9eabp-1, 0x1.1180b826db0f3p-5)),
	}
	for i := 750 * d; i < 1125*d; i++ {
		bit := byte(0x1C) >> (7 - i%8) & 1
		ch[0][i], ch[1][i] = bit, 1-bit
	}
	for i := 1500 * d; i < 1750*d; i++ {
		ch[0][i], ch[1][i] = 1, 0
	}
	return ch
}

func geom(t *testing.T, rate int, p Plan) *geometry {
	t.Helper()
	g, err := geometryFor(rate, p)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
