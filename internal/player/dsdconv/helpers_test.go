package dsdconv

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"math/cmplx"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/player/format"
)

// dsdRates are the DSD rates the converter takes.
var dsdRates = []int{format.DSD64RateHz, 2 * format.DSD64RateHz, 4 * format.DSD64RateHz}

func rateName(r int) string { return fmt.Sprintf("DSD%d", r/44100) }

// allPlans is every plan the package names, as the tests know them: the
// tests hold Plans to this list rather than iterate what Plans returns.
var allPlans = []Plan{
	{176400, 24}, {88200, 24}, {44100, 24},
	{176400, 16}, {88200, 16}, {44100, 16},
}

var errStubShape = errors.New("stub: dst is not two equal planes")

// stubDSD is an in-memory decode.DSDDecoder over canonical planes (bit 7
// earliest), shaped by the tests: short reads, an error at a byte, an end
// before the declared length, a hook at each read. Its reads allocate
// nothing, so a warmed converter over it shows its own allocations alone.
type stubDSD struct {
	f       format.NativeDSD
	planes  [2][]byte
	pos     int
	maxRead int // > 0: at most this many bytes a read
	failAt  int // >= 0: a read that reaches this byte stops there, with failErr
	failErr error
	endAt   int // >= 0: io.EOF at this byte, before the declared length
	hook    func(call int)
	calls   int
	closes  int
}

// newStub is a source of rate whose Frames is frames, over planes.
func newStub(rate int, frames int64, planes [2][]byte) *stubDSD {
	return &stubDSD{
		f:      format.NativeDSD{BitRateHz: rate, Channels: 2, Container: format.ContainerDSF, Frames: frames},
		planes: planes, failAt: -1, endAt: -1,
	}
}

// stubOf is a source over two channels' one-bit samples (0 or 1, one a
// byte), its Frames their length and its padding zero bits.
func stubOf(rate int, ch [2][]byte) *stubDSD {
	return newStub(rate, int64(len(ch[0])), [2][]byte{planeOf(ch[0], false), planeOf(ch[1], false)})
}

func (s *stubDSD) Format() format.NativeDSD { return s.f }

func (s *stubDSD) ReadDSD(ctx context.Context, dst [][]byte) (int, error) {
	s.calls++
	if s.hook != nil {
		s.hook(s.calls)
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if len(dst) != 2 || len(dst[0]) != len(dst[1]) {
		return 0, errStubShape
	}
	end := len(s.planes[0])
	if s.endAt >= 0 && s.endAt < end {
		end = s.endAt
	}
	limit := end
	if s.failAt >= 0 && s.failAt < limit {
		limit = s.failAt
	}
	n := len(dst[0])
	if s.maxRead > 0 && n > s.maxRead {
		n = s.maxRead
	}
	if left := limit - s.pos; n > left {
		n = left
	}
	copy(dst[0][:n], s.planes[0][s.pos:s.pos+n])
	copy(dst[1][:n], s.planes[1][s.pos:s.pos+n])
	s.pos += n
	switch {
	case s.failAt >= 0 && s.pos >= s.failAt:
		return n, s.failErr
	case s.pos >= end:
		return n, io.EOF
	}
	return n, nil
}

func (s *stubDSD) Seek(ctx context.Context, pos int64) error {
	if pos < 0 || pos > int64(len(s.planes[0])) {
		return errors.New("stub: seek out of range")
	}
	s.pos = int(pos)
	return nil
}

func (s *stubDSD) Close() error { s.closes++; return nil }

// planeOf packs one-bit samples (0 or 1, one a byte) into the canonical
// form, bit 7 earliest; the last byte's trailing bits are padding, all ones
// when padOnes.
func planeOf(bits []byte, padOnes bool) []byte {
	out := make([]byte, (len(bits)+7)/8)
	if padOnes && len(bits)%8 != 0 {
		out[len(out)-1] = 0xFF >> (len(bits) % 8)
	}
	for i, b := range bits {
		if b != 0 {
			out[i/8] |= 0x80 >> (i % 8)
		}
	}
	return out
}

// randomBits is n one-bit samples from xorshift64 seeded with seed: input
// whose every bit is the same on every architecture.
func randomBits(seed uint64, n int) []byte {
	out := make([]byte, n)
	x := seed
	for i := 0; i < n; i++ {
		if i%64 == 0 {
			x ^= x << 13
			x ^= x >> 7
			x ^= x << 17
		}
		out[i] = byte(x >> (i % 64) & 1)
	}
	return out
}

// repeatByte is n one-bit samples of the byte b over and over, bit 7 first.
func repeatByte(b byte, n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = b >> (7 - i%8) & 1
	}
	return out
}

func concat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// roundHalfEven is x/2^s rounded to the nearest integer, ties to even: the
// definition, written out.
func roundHalfEven(x int64, s uint) int64 {
	q := x >> s
	r := x - q<<s
	half := int64(1) << (s - 1)
	switch {
	case r > half:
		return q + 1
	case r < half:
		return q
	}
	return q + q&1
}

// reference converts two channels' one-bit samples (0 or 1, one a byte, the
// stream's Frames their length) under p at rate from the chain's
// definitions alone, a sum for each value with nothing kept between them but
// the stage outputs: no byte tables, delay lines, reads or phases. Bits
// before the first and from the last on are numeric zero. It shares the
// coefficients and nothing else with the converter, and its arithmetic is
// integer, so its samples are what every architecture computes.
func reference(t testing.TB, ch [2][]byte, rate int, p Plan) (out []int32, clipped int64) {
	t.Helper()
	r := referenceRun(t, ch, rate, p, nil)
	return r.out, r.count(0, len(r.out)/2)
}

// refResult is reference's answer: the samples, which of them saturated,
// and stage 3's sums before the rounding, per channel.
type refResult struct {
	out     []int32
	clipped []bool
	acc     [2][]int64
}

// count is the samples clipped in frames [from, to).
func (r refResult) count(from, to int) int64 {
	var n int64
	for _, c := range r.clipped[2*from : 2*to] {
		if c {
			n++
		}
	}
	return n
}

// frames is the samples of frames [from, to).
func (r refResult) frames(from, to int) []int32 { return r.out[2*from : 2*to] }

// referenceRun is reference with the 16-bit dither seeded again at each
// frame of reseed (the first frame of each occurrence of a continuous
// chain, which the stream ch is the bits of, end to end); frame 0 is seeded
// whatever it holds.
func referenceRun(t testing.TB, ch [2][]byte, rate int, p Plan, reseed []int64) refResult {
	t.Helper()
	g, err := geometryFor(rate, p)
	if err != nil {
		t.Fatalf("geometry %s %s: %v", rateName(rate), p, err)
	}
	nbits := int64(len(ch[0]))
	k := nbits / int64(g.d)
	if k == 0 {
		return refResult{}
	}
	c3 := int64(g.c3)
	pLo, pHi := -c3, int64(g.m3)*(k-1)+c3
	mLo, mHi := 2*pLo-15, 2*pHi+15
	acc := make([][]int64, 2)
	for c := 0; c < 2; c++ {
		bits := ch[c]
		bit := func(n int64) int64 {
			if n < 0 || n >= nbits {
				return 0
			}
			if bits[n] != 0 {
				return 1
			}
			return -1
		}
		s1 := make([]int64, mHi-mLo+1)
		for m := mLo; m <= mHi; m++ {
			start := m*int64(g.m1) - int64(4*g.t)
			var v int64
			for slot, gv := range g.g1 {
				v += gv * bit(start+int64(slot))
			}
			s1[m-mLo] = v
		}
		s2 := make([]int64, pHi-pLo+1)
		for q := pLo; q <= pHi; q++ {
			var v int64
			for j, gv := range g.g2 {
				v += gv * s1[2*q-15+int64(j)-mLo]
			}
			s2[q-pLo] = roundHalfEven(v, scaleBits)
		}
		a := make([]int64, k)
		for f := int64(0); f < k; f++ {
			var v int64
			centre := int64(g.m3) * f
			for i, gv := range g.g3 {
				v += gv * s2[centre-c3+int64(i)-pLo]
			}
			a[f] = v
		}
		acc[c] = a
	}
	res := refResult{out: make([]int32, 2*k), clipped: make([]bool, 2*k), acc: [2][]int64{acc[0], acc[1]}}
	max := int64(1)<<(p.ValidBits-1) - 1
	shift := uint(2*scaleBits - (p.ValidBits - 1))
	x := ditherSeed
	next := 0
	for f := int64(0); f < k; f++ {
		for next < len(reseed) && reseed[next] <= f {
			if reseed[next] == f {
				x = ditherSeed
			}
			next++
		}
		for c := 0; c < 2; c++ {
			v := acc[c][f]
			if p.ValidBits == 16 {
				x ^= x << 13
				x ^= x >> 7
				x ^= x << 17
				d := int64(uint32(x)) - int64(x>>32)
				v += d << (shift - 32)
			}
			v = roundHalfEven(v, shift)
			i := 2*f + int64(c)
			if v > max {
				v, res.clipped[i] = max, true
			} else if v < -max {
				v, res.clipped[i] = -max, true
			}
			res.out[i] = int32(v)
		}
	}
	return res
}

// convert converts ch under p at rate through a stub source, read to its
// end in reads of chunk frames.
func convert(t testing.TB, ch [2][]byte, rate int, p Plan, chunk int) ([]int32, *Converter) {
	t.Helper()
	c, err := New(stubOf(rate, ch), p, Options{})
	if err != nil {
		t.Fatalf("New %s %s: %v", rateName(rate), p, err)
	}
	out, err := drain(t, c, chunk)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("%s %s: the read ended with %v, want io.EOF", rateName(rate), p, err)
	}
	return out, c
}

// drain reads c in reads of chunk frames until an error, and returns every
// sample read and that error.
func drain(t testing.TB, c *Converter, chunk int) ([]int32, error) {
	t.Helper()
	buf := make([]int32, 2*chunk)
	var out []int32
	for i := 0; ; i++ {
		n, err := c.ReadFrames(context.Background(), buf)
		out = append(out, buf[:n]...)
		if err != nil {
			return out, err
		}
		if n == 0 && i > 1<<24 {
			t.Fatal("the converter makes no progress")
		}
	}
}

// channel is one channel of interleaved stereo samples, as float64 of full
// scale.
func channel(xs []int32, c, bits int) []float64 {
	out := make([]float64, len(xs)/2)
	scale := float64(int64(1) << (bits - 1))
	for i := range out {
		out[i] = float64(xs[2*i+c]) / scale
	}
	return out
}

func dbfs(a float64) float64 { return 20 * math.Log10(a) }

// toneAmplitude is the amplitude of the sinusoid at f in xs (rate fs), by
// correlation over the samples given, which should span whole periods.
func toneAmplitude(xs []float64, fs, f float64) float64 {
	var re, im float64
	for n, x := range xs {
		ph := 2 * math.Pi * f * float64(n) / fs
		re += x * math.Cos(ph)
		im -= x * math.Sin(ph)
	}
	return 2 * math.Hypot(re, im) / float64(len(xs))
}

// fft is an in-place radix-2 transform, n a power of two: transcode's test
// helper (dsd_fixture_measure_test.go), which another package's test cannot
// share.
func fft(x []complex128) {
	n := len(x)
	for i, j := 1, 0; i < n; i++ {
		bit := n >> 1
		for ; j&bit != 0; bit >>= 1 {
			j ^= bit
		}
		j ^= bit
		if i < j {
			x[i], x[j] = x[j], x[i]
		}
	}
	for length := 2; length <= n; length <<= 1 {
		wl := cmplx.Exp(complex(0, -2*math.Pi/float64(length)))
		for i := 0; i < n; i += length {
			w := complex(1, 0)
			for j := 0; j < length/2; j++ {
				u, v := x[i+j], x[i+j+length/2]*w
				x[i+j], x[i+j+length/2] = u+v, u-v
				w *= wl
			}
		}
	}
}

// spectrumDBFS is a Blackman-Harris-windowed magnitude spectrum of
// xs[off:off+n] in dBFS (a full-scale sine reads 0), and its bin width:
// transcode's measurement, whose window keeps a tone's sidelobes off the
// floor being read.
func spectrumDBFS(xs []float64, off, n int, rate float64) ([]float64, float64) {
	buf := make([]complex128, n)
	var winSum float64
	const a0, a1, a2, a3 = 0.35875, 0.48829, 0.14128, 0.01168
	for i := 0; i < n; i++ {
		p := 2 * math.Pi * float64(i) / float64(n-1)
		w := a0 - a1*math.Cos(p) + a2*math.Cos(2*p) - a3*math.Cos(3*p)
		winSum += w
		buf[i] = complex(xs[off+i]*w, 0)
	}
	fft(buf)
	scale := 2.0 / winSum
	mags := make([]float64, n/2)
	for i := range mags {
		m := cmplx.Abs(buf[i]) * scale
		if m <= 0 {
			mags[i] = -300
			continue
		}
		mags[i] = 20 * math.Log10(m)
	}
	return mags, rate / float64(n)
}

func peakInBand(mags []float64, binHz, lo, hi float64) (db, hz float64) {
	db = -300
	for i, v := range mags {
		if f := float64(i) * binHz; f >= lo && f <= hi && v > db {
			db, hz = v, f
		}
	}
	return db, hz
}

// bandEnergyDB is the power of every bin in a band: a folded noise shelf
// lifts it broadly and barely moves the peak, so both are read.
func bandEnergyDB(mags []float64, binHz, lo, hi float64) float64 {
	var sum float64
	for i, v := range mags {
		if f := float64(i) * binHz; i > 0 && f >= lo && f <= hi {
			sum += math.Pow(10, v/10)
		}
	}
	if sum <= 0 {
		return -300
	}
	return 10 * math.Log10(sum)
}
