package dsdchime

import (
	"context"
	"errors"
	"io"
	"math"
	"math/bits"
	"math/cmplx"
	"sync"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/player/dsdconv"
	"github.com/acoseac/1-bit-bridge/internal/player/format"
)

// chimeSource is a decode.DSDDecoder over a chime, read through its ReadAt,
// as the player's check will hand the chime to its DSD path.
type chimeSource struct {
	c   Chime
	off int64
}

func (s *chimeSource) Format() format.NativeDSD { return s.c.Format() }

func (s *chimeSource) ReadDSD(ctx context.Context, dst [][]byte) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	n, err := s.c.ReadAt(dst, s.off)
	s.off += int64(n)
	return n, err
}

func (s *chimeSource) Seek(_ context.Context, pos int64) error { s.off = pos; return nil }
func (s *chimeSource) Close() error                            { return nil }

// pcmRate is the plan the chime is measured through: 176.4 kHz at 24 bits,
// whose rounding sits far under DSD256's floor, where a 16-bit plan's
// TPDF dither would cover it. A 1/44,100 s frame is four of its samples.
const pcmRate = 176400

var (
	convertedMu sync.Mutex
	converted   = map[int][2][]float64{}
)

// pcmOf is the chime at rate converted by internal/player/dsdconv, the
// player's own DSD to PCM path, under the 176.4 kHz 24-bit plan: each
// channel's samples as fractions of full scale, which is the one-bit range,
// so the chime's level reads −36.02 dBFS. Its frame k is centred on the
// chime's sample k·D. One conversion of each rate serves every test.
func pcmOf(t *testing.T, rate int) [2][]float64 {
	t.Helper()
	convertedMu.Lock()
	defer convertedMu.Unlock()
	if x, ok := converted[rate]; ok {
		return x
	}
	c, err := Get(context.Background(), rate)
	if err != nil {
		t.Fatalf("%s: %v", rateName(rate), err)
	}
	cv, err := dsdconv.New(&chimeSource{c: c}, dsdconv.Plan{RateHz: pcmRate, ValidBits: 24}, dsdconv.Options{})
	if err != nil {
		t.Fatalf("%s: %v", rateName(rate), err)
	}
	var out [2][]float64
	buf := make([]int32, 2*4096)
	for {
		n, err := cv.ReadFrames(context.Background(), buf)
		for i := 0; i < n; i += 2 {
			out[0] = append(out[0], float64(buf[i])/(1<<23))
			out[1] = append(out[1], float64(buf[i+1])/(1<<23))
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("%s: %v", rateName(rate), err)
		}
	}
	if cv.Clipped() != 0 {
		t.Fatalf("%s: %d samples clipped", rateName(rate), cv.Clipped())
	}
	converted[rate] = out
	return out
}

// fft is an in-place radix-2 transform, len(x) a power of two (dsdconv's
// test helper, which another package's test cannot share).
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

// spectrum windows xs with the four-term Blackman-Harris window (dsdconv's
// measurement window, whose sidelobes stay under −92 dB), zero-pads it to n
// points and transforms it; it answers the bins and the window's sum and
// sum of squares.
func spectrum(xs []float64, n int) (x []complex128, sum, sum2 float64) {
	x = make([]complex128, n)
	const a0, a1, a2, a3 = 0.35875, 0.48829, 0.14128, 0.01168
	for i, v := range xs {
		p := 2 * math.Pi * float64(i) / float64(len(xs)-1)
		w := a0 - a1*math.Cos(p) + a2*math.Cos(2*p) - a3*math.Cos(3*p)
		sum += w
		sum2 += w * w
		x[i] = complex(v*w, 0)
	}
	fft(x)
	return x, sum, sum2
}

func nextPow2(n int) int {
	p := 1
	for p < n {
		p <<= 1
	}
	return p
}

// inBand is the mean square of xs (rate fs) in 20 Hz to 20 kHz, and its
// largest component there as a sine's amplitude: xs windowed (spectrum),
// each bin's power normalised by the window's energy, so that a sine of
// amplitude A sums to A²/2 and white noise to its share of the band, and
// summed over the bins from 20 Hz to 20 kHz; the largest bin read as the
// amplitude of a sine centred on it, 2|X|/Σw.
func inBand(xs []float64, fs float64, n int) (power, spur float64) {
	x, sum, sum2 := spectrum(xs, n)
	lo, hi := int(math.Ceil(20*float64(n)/fs)), int(math.Floor(20000*float64(n)/fs))
	for k := lo; k <= hi; k++ {
		a := cmplx.Abs(x[k])
		power += a * a
		spur = math.Max(spur, 2*a/sum)
	}
	return 2 * power / (float64(n) * sum2), spur
}

// peakHz is the frequency of the strongest component of xs within 20 Hz of
// near: the largest bin of the windowed spectrum zero-padded to 2^19
// points, refined by the parabola through the dB magnitudes of it and its
// two neighbours.
func peakHz(xs []float64, fs, near float64) float64 {
	const n = 1 << 19
	x, _, _ := spectrum(xs, n)
	best := int((near - 20) * n / fs)
	for k := best; k <= int((near+20)*n/fs); k++ {
		if cmplx.Abs(x[k]) > cmplx.Abs(x[best]) {
			best = k
		}
	}
	db := func(k int) float64 { return 20 * math.Log10(cmplx.Abs(x[k])) }
	a, b, c := db(best-1), db(best), db(best+1)
	return (float64(best) + 0.5*(a-c)/(a-2*b+c)) * fs / n
}

// fitTone is the least-squares fit of a·cos(ωi) + b·sin(ωi) + c to xs, ω
// being f's at fs: the sinusoid at the known frequency, whose amplitude is
// hypot(a, b), and a constant, which takes the DC off the residual.
func fitTone(xs []float64, fs, f float64) (a, b, c float64) {
	var m [3][4]float64
	w := 2 * math.Pi * f / fs
	for i, v := range xs {
		basis := [3]float64{math.Cos(w * float64(i)), math.Sin(w * float64(i)), 1}
		for r := range 3 {
			for q := range 3 {
				m[r][q] += basis[r] * basis[q]
			}
			m[r][3] += basis[r] * v
		}
	}
	for p := range 3 {
		for r := p + 1; r < 3; r++ {
			k := m[r][p] / m[p][p]
			for q := p; q < 4; q++ {
				m[r][q] -= k * m[p][q]
			}
		}
	}
	var sol [3]float64
	for r := 2; r >= 0; r-- {
		v := m[r][3]
		for q := r + 1; q < 3; q++ {
			v -= m[r][q] * sol[q]
		}
		sol[r] = v / m[r][r]
	}
	return sol[0], sol[1], sol[2]
}

func db(x float64) float64 { return 10 * math.Log10(x) }

// toneHz is each channel's tone.
var toneHz = [2]float64{880, 1320}

// steady is channel ch's tone's steady middle in the converted chime: its
// section's 0.1 to 0.6 s, clear of the lead-in and the rise before it and
// of the fall and the tail after it, 440 and 660 whole periods.
func steady(x [2][]float64, ch int) (tone, other []float64) {
	from := (padFrames+ch*sectionFrames)*4 + pcmRate/10
	to := from + pcmRate/2
	return x[ch][from:to], x[1-ch][from:to]
}

// measured is a tone's reading through the converter: its frequency, its
// level (dBFS), the in-band residual's power with the tone fitted and
// taken away and the silent channel's in-band power with its DC taken
// away, each in dB under the tone's power.
type measured struct {
	hz, level, residual, silent float64
}

func measure(x [2][]float64, ch int) measured {
	xs, other := steady(x, ch)
	a, b, c := fitTone(xs, pcmRate, toneHz[ch])
	w := 2 * math.Pi * toneHz[ch] / pcmRate
	r := make([]float64, len(xs))
	for i, v := range xs {
		r[i] = v - a*math.Cos(w*float64(i)) - b*math.Sin(w*float64(i)) - c
	}
	amp := math.Hypot(a, b)
	n := nextPow2(len(xs))
	resid, _ := inBand(r, pcmRate, n)
	var mean float64
	for _, v := range other {
		mean += v
	}
	mean /= float64(len(other))
	quiet := make([]float64, len(other))
	for i, v := range other {
		quiet[i] = v - mean
	}
	silent, _ := inBand(quiet, pcmRate, n)
	return measured{
		hz:       peakHz(xs, pcmRate, toneHz[ch]),
		level:    20 * math.Log10(amp),
		residual: db(amp * amp / 2 / resid),
		silent:   db(amp * amp / 2 / silent),
	}
}

// repeated is p's samples each repeated r times, packed, bit 7 earliest: a
// lower rate's chime as a zero-order hold would make it at r times its
// rate.
func repeated(p []byte, r int) []byte {
	out := make([]byte, len(p)*r)
	for n := range 8 * len(out) {
		if p[n/r>>3]&(0x80>>(n/r&7)) != 0 {
			out[n>>3] |= 0x80 >> (n & 7)
		}
	}
	return out
}

// TestTheChimeIsModulatedAtItsOwnRate: no rate's chime is a lower rate's
// with each of its samples repeated (a zero-order hold, which moves 0x69's
// idle tone toward the band and shifts its offset): against each lower
// rate's so repeated, about half the samples of each channel's tone section
// differ, as two streams modulated apart do, where a repeated section would
// differ in none. Only the sections are counted: the pads and each tone's
// other channel, 70.8 % of a plane, are 0x69 at every rate
// (TestTheChimeHasItsShapeAtEveryRate), and 0x69 differs from itself
// repeated in half its bits, so they would read half whatever the sections
// held. Through the player's converter each tone is its frequency to within
// 1 Hz and its level, −36.02 dBFS, to within 0.5 dB, and the channel that is
// silent meanwhile lies 60 dB or more under it: its in-band power, its DC
// taken off, against the tone's, on the chain
// TestTheChimesInBandResidualIsUnderItsFloor measures by.
func TestTheChimeIsModulatedAtItsOwnRate(t *testing.T) {
	chimes := map[int]Chime{}
	for _, rate := range chimeRates {
		c, err := Get(context.Background(), rate)
		if err != nil {
			t.Fatalf("%s: %v", rateName(rate), err)
		}
		chimes[rate] = c
	}
	for _, hi := range chimeRates {
		bpf := hi / 44100 / 8
		for _, lo := range chimeRates {
			if lo >= hi {
				continue
			}
			for ch := range 2 {
				held := repeated(chimes[lo].planes[ch], hi/lo)
				at, n := (padFrames+ch*sectionFrames)*bpf, sectionFrames*bpf
				differ := 0
				for i := at; i < at+n; i++ {
					differ += bits.OnesCount8(chimes[hi].planes[ch][i] ^ held[i])
				}
				share := float64(differ) / float64(8*n)
				t.Logf("%s channel %d's section against %s's samples repeated: %.4f of its samples differ", rateName(hi), ch, rateName(lo), share)
				if share < 0.4 {
					t.Errorf("%s channel %d: only %.4f of its section's samples differ from %s's repeated", rateName(hi), ch, share, rateName(lo))
				}
			}
		}
	}
	want := 20 * math.Log10(level)
	for _, rate := range chimeRates {
		x := pcmOf(t, rate)
		for ch := range 2 {
			m := measure(x, ch)
			t.Logf("%s %.0f Hz: %.4f Hz, %.4f dBFS, the silent channel %.1f dB under", rateName(rate), toneHz[ch], m.hz, m.level, m.silent)
			if math.Abs(m.hz-toneHz[ch]) > 1 {
				t.Errorf("%s: the %.0f Hz tone reads %.4f Hz", rateName(rate), toneHz[ch], m.hz)
			}
			if math.Abs(m.level-want) > 0.5 {
				t.Errorf("%s: the %.0f Hz tone reads %.4f dBFS, not %.4f within 0.5 dB", rateName(rate), toneHz[ch], m.level, want)
			}
			if !(m.silent >= 60) {
				t.Errorf("%s: the silent channel lies %.1f dB under the %.0f Hz tone, not 60", rateName(rate), m.silent, toneHz[ch])
			}
		}
	}
}

// floors are the in-band residual each rate's tones must lie under, in dB
// under the tone, with a margin under what the loop gives: its noise
// transfer function's five zeros all sit at DC, so the noise it shapes
// rises toward 20 kHz, where it is 36.8 dB down at DSD64, 64.3 at DSD128
// and 93.6 at DSD256.
var floors = map[int]float64{format.DSD64RateHz: 35, 2 * format.DSD64RateHz: 65, 4 * format.DSD64RateHz: 95}

// TestTheChimesInBandResidualIsUnderItsFloor measures each tone's in-band
// residual through the player's converter, step by step: the tone's
// steady middle, 0.5 s clear of its edges, since a constant amplitude
// fitted over an edge leaves the envelope in the residual; a sinusoid at
// the known frequency, and a constant, fitted by least squares and taken
// away; the residual windowed (four-term Blackman-Harris) and zero-padded
// to 2^17 points, each bin's power normalised by the window's energy, and
// the bins from 20 Hz to 20 kHz summed. Each tone's residual lies under its
// rate's floor, and its level is −36.02 dBFS within 0.5 dB. It reports, as
// a measurement, the largest in-band component of each 20 ms edge, where
// the input nears zero and an undithered loop can fall into an idle
// pattern: the edge's samples less the intended programme, windowed and
// transformed the same way over 2^16 points.
func TestTheChimesInBandResidualIsUnderItsFloor(t *testing.T) {
	want := 20 * math.Log10(level)
	for i, rate := range chimeRates {
		x := pcmOf(t, rate)
		for ch := range 2 {
			m := measure(x, ch)
			t.Logf("%s %.0f Hz: %.4f dBFS, the in-band residual %.4f dBFS, %.2f dB under the tone (floor %.0f)", rateName(rate), toneHz[ch], m.level, m.level-m.residual, m.residual, floors[rate])
			if !(m.residual >= floors[rate]) {
				t.Errorf("%s: the %.0f Hz tone's in-band residual lies %.2f dB under it, not %.0f", rateName(rate), toneHz[ch], m.residual, floors[rate])
			}
			if math.Abs(m.level-want) > 0.5 {
				t.Errorf("%s: the %.0f Hz tone reads %.4f dBFS", rateName(rate), toneHz[ch], m.level)
			}
			tn := toneOf(i, ch, rates[i].tail[ch])
			d := rate / pcmRate
			start := (padFrames+ch*sectionFrames)*4 + tn.lead/d
			ideal := func(frame int) float64 {
				j := float64((frame - start) * d)
				env := 1.0
				ramp := float64(tn.ramp)
				switch {
				case j < ramp:
					env = 0.5 - 0.5*math.Cos(math.Pi*j/ramp)
				case j >= float64(tn.n)-ramp:
					env = 0.5 + 0.5*math.Cos(math.Pi*(j-(float64(tn.n)-ramp))/ramp)
				}
				return level * env * math.Sin(2*math.Pi*toneHz[ch]*j/float64(rate))
			}
			for e, from := range []int{start, start + (tn.n-tn.ramp)/d} {
				name := [2]string{"rise", "fall"}[e]
				edge := make([]float64, tn.ramp/d)
				for k := range edge {
					edge[k] = x[ch][from+k] - ideal(from+k)
				}
				power, spur := inBand(edge, pcmRate, 1<<16)
				t.Logf("%s %.0f Hz %s: the residual %.2f dBFS in band, its largest component %.2f dBFS, %.1f dB under the tone", rateName(rate), toneHz[ch], name, db(2*power), 20*math.Log10(spur), m.level-20*math.Log10(spur))
			}
		}
	}
}

// probe is sdm.c's probe_t, the qualification's listening assets'
// measure of a boundary: the audio band, the bits low-passed at 20 kHz by
// a Kaiser-windowed sinc (β 5.65) of rate/2756 taps either side, read every
// 1/64 of that.
type probe struct {
	half, step int
	h          []float64
}

func i0(x float64) float64 {
	s, t := 1.0, 1.0
	for k := 1; k < 200; k++ {
		t *= (x / float64(2*k)) * (x / float64(2*k))
		s += t
		if t < 1e-17*s {
			break
		}
	}
	return s
}

func newProbe(rate int) probe {
	p := probe{half: rate / 2756}
	p.step = p.half / 64
	p.h = make([]float64, 2*p.half+1)
	fc, beta, sum := 20000/float64(rate), 5.65, 0.0
	for i := range p.h {
		x := float64(i - p.half)
		r := x / float64(p.half)
		v := 2 * fc
		if x != 0 {
			v = math.Sin(2*math.Pi*fc*x) / (math.Pi * x)
		}
		p.h[i] = v * i0(beta*math.Sqrt(math.Max(0, 1-r*r))) / i0(beta)
		sum += p.h[i]
	}
	for i := range p.h {
		p.h[i] /= sum
	}
	return p
}

// level is sdm.c's probe_level at boundary b of bit(n), ±1: the energy of
// the low-passed bits over the 2·half positions about b, in dB SA-CD over
// 10 ms, 0 dB SA-CD being a sine of half the one-bit range.
func (p probe) level(bit func(int) float64, b, rate int) float64 {
	var e float64
	for t := b - p.half; t < b+p.half; t += p.step {
		var v float64
		for j := -p.half; j <= p.half; j++ {
			v += p.h[j+p.half] * bit(t+j)
		}
		e += v * v * float64(p.step)
	}
	return 10 * math.Log10(e/(0.01*float64(rate))/0.125+1e-300)
}

// offsets are sdm.c's window() at boundary b of bit(n), ±1: end, how far
// the running sum at b stands from its mean over the w positions before
// b, and start, how far its mean over the w positions after b stands from
// it.
func offsets(bit func(int) float64, b, w int) (end, start float64) {
	var r, before, after, at float64
	for n := b - w; n <= b+w; n++ {
		switch {
		case n < b:
			before += r
		case n == b:
			at = r
		default:
			after += r
		}
		r += bit(n)
	}
	return at - before/float64(w), after/float64(w) - at
}

// bitOf is p's sample n as ±1.
func bitOf(p []byte, n int) float64 {
	if p[n>>3]&(0x80>>(n&7)) != 0 {
		return 1
	}
	return -1
}

// silent is literal 0x69's sample n as ±1, n counted from a byte's start.
func silent(n int) float64 { return float64(int(0x69>>(7-n&7)&1)*2 - 1) }

// join is what a boundary leaves: its offsets less 0x69's own over the same
// samples, and its level in the audio band.
type join struct{ end, start, click float64 }

func joinAt(bit func(int) float64, b, rate int, p probe) join {
	w := rate / 2000
	e, s := offsets(bit, b, w)
	e0, s0 := offsets(silent, b, w)
	return join{end: e - e0, start: s - s0, click: p.level(bit, b, rate)}
}

// TestTheChimesJoinsToSilenceAreClean measures every boundary of the chime,
// pad to tone and tone to pad, on both channels and at every rate, as
// sdm.c measures its own: the running sum's offsets from its local means,
// over 0.5 ms either side, against literal 0x69's over the same samples,
// within 0.02; and the audio band against 0x69, at most −100 dB SA-CD. A
// modulated stream cut at an arbitrary state clicks there, edges or no
// edges. Each tone's start is the loop's own silence, from rest, so it
// needs nothing; its end is a tail of zero input whose length the rates
// table names, and the test searches for it again: the first tail, from
// none past 20 ms, whose join stands within 0.01 and −101 dB SA-CD, a
// margin inside the bound.
func TestTheChimesJoinsToSilenceAreClean(t *testing.T) {
	for i, rate := range chimeRates {
		c, err := Get(context.Background(), rate)
		if err != nil {
			t.Fatalf("%s: %v", rateName(rate), err)
		}
		p := newProbe(rate)
		spf := rate / 44100
		for _, at := range []int{padFrames, padFrames + sectionFrames, padFrames + 2*sectionFrames} {
			b := at * spf
			for ch := range 2 {
				j := joinAt(func(n int) float64 { return bitOf(c.planes[ch], n) }, b, rate, p)
				t.Logf("%s channel %d at %.1f s: offsets %+.4f before, %+.4f after, %.1f dB SA-CD", rateName(rate), ch, float64(at)/44100, j.end, j.start, j.click)
				if math.Abs(j.end) > 0.02 || math.Abs(j.start) > 0.02 || !(j.click <= -100) {
					t.Errorf("%s channel %d at %.1f s: offsets %+.4f and %+.4f from 0x69's, %.1f dB SA-CD: not a clean join", rateName(rate), ch, float64(at)/44100, j.end, j.start, j.click)
				}
			}
		}
		for ch := range 2 {
			section := make([]byte, sectionFrames*spf/8)
			found := -1
			for k := 0; k < 400 && found < 0; k++ {
				if err := toneOf(i, ch, k).modulate(context.Background(), section); err != nil {
					t.Fatalf("%s: %v", rateName(rate), err)
				}
				end := 8 * len(section)
				bit := func(n int) float64 {
					if n < end {
						return bitOf(section, n)
					}
					return silent(n)
				}
				if j := joinAt(bit, end, rate, p); math.Abs(j.end) <= 0.01 && j.click <= -101 {
					found = k
				}
			}
			if found != rates[i].tail[ch] {
				t.Errorf("%s channel %d: the tail the search finds is %d frames past 20 ms, the table's %d", rateName(rate), ch, found, rates[i].tail[ch])
			}
		}
	}
}
