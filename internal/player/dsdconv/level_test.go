package dsdconv

import (
	"math"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/dsdtone"
)

// TestUnityGainLevels: bit 1 is +1.0, bit 0 is -1.0 and PCM full scale is
// the one-bit amplitude (B728 §4.4), so a -20 dBFS sine from dsdtone's
// first-order modulator comes out at -20 dBFS ± 0.05 dB, at every DSD rate
// under every plan. The tone is read by correlation over whole periods past
// the filter's start; the first-order modulator's -45 dBFS of in-band noise
// does not reach that estimate.
func TestUnityGainLevels(t *testing.T) {
	for _, rate := range dsdRates {
		bits := dsdtone.Modulate(rate, 0.3, math.Pow(10, -20.0/20), 1000)
		for _, p := range allPlans {
			out, _ := convert(t, [2][]byte{bits, bits}, rate, p, 4096)
			xs := channel(out, 0, p.ValidBits)
			from := p.RateHz / 20 // 50 ms in
			n := p.RateHz / 5     // 0.2 s: 200 whole periods of 1 kHz at 176.4, 88.2 and 44.1 kHz
			if from+n > len(xs) {
				t.Fatalf("%s %s: %d frames, want %d", rateName(rate), p, len(xs), from+n)
			}
			level := dbfs(toneAmplitude(xs[from:from+n], float64(p.RateHz), 1000))
			t.Logf("%s → %s: the -20 dBFS sine reads %.4f dBFS", rateName(rate), p, level)
			if math.Abs(level+20) > 0.05 {
				t.Errorf("%s %s: the -20 dBFS sine reads %.4f dBFS, outside -20 ± 0.05", rateName(rate), p, level)
			}
		}
	}
}

// TestOvermodulationSaturatesAndCounts: a sample past full scale saturates
// at ±(2^23 - 1), or ±(2^15 - 1) at 16 bits, never wraps, and is counted;
// there is no limiter. All ones is +1.0 held, past full scale once the
// filter settles; a square wave of all ones and all zeros overshoots on
// every edge. The count is the occurrence's: a new converter starts at 0.
func TestOvermodulationSaturatesAndCounts(t *testing.T) {
	for _, rate := range dsdRates {
		for _, p := range []Plan{{176400, 24}, {44100, 24}, {176400, 16}, {44100, 16}} {
			ones := repeatByte(0xFF, rate/50)
			square := make([]byte, rate/25)
			for i := range square {
				if i/(rate/2000)%2 == 0 {
					square[i] = 1
				}
			}
			for name, ch := range map[string][2][]byte{"all ones": {ones, ones}, "a square wave": {square, repeatByte(0x69, len(square))}} {
				out, c := convert(t, ch, rate, p, 1000)
				ref := referenceRun(t, ch, rate, p, nil)
				want := ref.count(0, len(ref.out)/2)
				max := int32(1)<<(p.ValidBits-1) - 1
				if want == 0 {
					t.Fatalf("%s %s %s: the fixture clips nothing", rateName(rate), p, name)
				}
				if got := c.Clipped(); got != want {
					t.Errorf("%s %s %s: Clipped() = %d, want %d", rateName(rate), p, name, got, want)
				}
				for i, v := range out {
					if v > max || v < -max {
						t.Fatalf("%s %s %s: sample %d is %d, past ±%d", rateName(rate), p, name, i, v, max)
					}
					if ref.clipped[i] && v != max && v != -max {
						t.Fatalf("%s %s %s: sample %d clipped to %d, not ±%d", rateName(rate), p, name, i, v, max)
					}
				}
				if !equal32(out, ref.out) {
					t.Errorf("%s %s %s: the samples differ from the chain's definition", rateName(rate), p, name)
				}
				// A new occurrence counts its own.
				silent, c2 := convert(t, [2][]byte{repeatByte(0x69, 8192), repeatByte(0x69, 8192)}, rate, p, 1000)
				if c2.Clipped() != 0 || len(silent) == 0 {
					t.Errorf("%s %s %s: a silent occurrence after it counts %d", rateName(rate), p, name, c2.Clipped())
				}
			}
		}
	}
}

func equal32(a, b []int32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestDelayLinesStartAtNumericZero: the delay lines start at numeric zero,
// silence in the filter's arithmetic, never at zero bits, which are -1.0, a
// full-scale offset (B728 §4.2). DSD silence, 0x69, primed so, settles to
// the stopband's level, and its first frames are no click: at 24 bits the
// first frames and the rest stay under -100 dBFS; at 16 bits, under 2 LSB,
// the dither's own floor. Zero bits would put the first frames near
// -6 dBFS.
func TestDelayLinesStartAtNumericZero(t *testing.T) {
	for _, rate := range dsdRates {
		silence := repeatByte(0x69, rate/20)
		for _, p := range allPlans {
			out, _ := convert(t, [2][]byte{silence, silence}, rate, p, 4096)
			h, _ := p.Window(rate)
			bar := math.Pow(10, -100.0/20) * float64(int64(1)<<(p.ValidBits-1))
			if p.ValidBits == 16 {
				bar = 2
			}
			first, rest := 0.0, 0.0
			for i, v := range out {
				a := math.Abs(float64(v))
				if i/2 < 2*h {
					first = math.Max(first, a)
				} else {
					rest = math.Max(rest, a)
				}
			}
			full := float64(int64(1) << (p.ValidBits - 1))
			t.Logf("%s → %s: the first %d frames peak at %.1f LSB (%.1f dBFS), the rest at %.1f LSB", rateName(rate), p, 2*h, first, dbfs(first/full), rest)
			if first > bar || rest > bar {
				t.Errorf("%s %s: DSD silence reads %.1f LSB in its first %d frames and %.1f after, over %.1f", rateName(rate), p, first, 2*h, rest, bar)
			}
		}
	}
}

// TestTheRoundingErrorIsFlat is §4.5's check: 24-bit output rounds half to
// even with no dither, and DSD's own in-band noise decorrelates the
// rounding, so the error, the sample less stage 3's exact sum, is white:
// flat across the band within 1 dB, of mean 0 and of power one step
// squared over 12. The fixture is the 5th-order modulator's 1 kHz tone at
// -6 dBFS, modulated at each rate.
func TestTheRoundingErrorIsFlat(t *testing.T) {
	for _, rate := range dsdRates {
		bits := ciff(rate*3/10, tones(rate, 1, 1000))
		for _, p := range []Plan{{176400, 24}, {88200, 24}, {44100, 24}} {
			ref := referenceRun(t, [2][]byte{bits, bits}, rate, p, nil)
			out, _ := convert(t, [2][]byte{bits, bits}, rate, p, 4096)
			if !equal32(out, ref.out) {
				t.Fatalf("%s %s: the samples differ from the chain's definition", rateName(rate), p)
			}
			from := p.RateHz / 20
			e := make([]float64, 0, len(out)/2-from)
			for k := from; k < len(out)/2; k++ {
				exact := float64(ref.acc[0][k]) / float64(int64(1)<<37)
				e = append(e, float64(out[2*k])-exact)
			}
			var mean, power float64
			for _, v := range e {
				mean += v
				power += v * v
			}
			mean /= float64(len(e))
			power /= float64(len(e))
			bands := welchBands(e, 1024, 16)
			lo, hi := math.Inf(1), math.Inf(-1)
			var avg float64
			for _, b := range bands {
				avg += b
			}
			avg /= float64(len(bands))
			for _, b := range bands {
				lo, hi = math.Min(lo, 10*math.Log10(b/avg)), math.Max(hi, 10*math.Log10(b/avg))
			}
			t.Logf("%s → %s: the rounding error's mean %.4f LSB, power %.4f LSB² (1/12 = 0.0833), bands from %+.2f to %+.2f dB of their mean", rateName(rate), p, mean, power, lo, hi)
			if math.Abs(mean) > 0.01 {
				t.Errorf("%s %s: the rounding error's mean is %.4f LSB", rateName(rate), p, mean)
			}
			if math.Abs(power/(1.0/12)-1) > 0.05 {
				t.Errorf("%s %s: the rounding error's power is %.4f LSB², not 1/12", rateName(rate), p, power)
			}
			if lo < -1 || hi > 1 {
				t.Errorf("%s %s: the rounding error's spectrum spans %+.2f to %+.2f dB of its mean, not flat within 1 dB", rateName(rate), p, lo, hi)
			}
		}
	}
}

// welchBands is the power of xs in nb equal bands from 0 to Nyquist,
// averaged over segments of n with a Hann window and half overlap.
func welchBands(xs []float64, n, nb int) []float64 {
	bands := make([]float64, nb)
	bins := make([]float64, nb)
	buf := make([]complex128, n)
	for off := 0; off+n <= len(xs); off += n / 2 {
		for i := 0; i < n; i++ {
			w := 0.5 - 0.5*math.Cos(2*math.Pi*float64(i)/float64(n))
			buf[i] = complex(xs[off+i]*w, 0)
		}
		fft(buf)
		for i := 1; i < n/2; i++ {
			re, im := real(buf[i]), imag(buf[i])
			bands[i*nb/(n/2)] += re*re + im*im
			bins[i*nb/(n/2)]++
		}
	}
	for b := range bands {
		bands[b] /= bins[b]
	}
	return bands
}

// TestAliasingIsADifferential: DSD's noise shelf above 50 kHz carries
// almost all of a one-bit stream's power, so a stopband too shallow at any
// of the chain's three decimations folds it, or a tone, into the band. An
// absolute bar would measure the modulator (transcode's
// TestDSDRender_AliasRejection says why: the fixture's own in-band content
// near 20 kHz is around -104 dBFS at DSD64), so this is a differential
// against a reference that cannot alias: one filter at the one-bit rate,
// 160 dB down from the plan's stopband, before any decimation. Whatever the
// fixture brings, both bring, and in-band energy or a peak the converter
// has and the reference has not was folded down. Both are rounded to 24
// bits alike.
//
// Each DSD rate has stage-1 tables of its own, so each has fixtures
// modulated at its own rate by the 5th-order modulator, and each is checked
// at every target rate: 50 kHz, the generator's alias probe, and a tone
// that folds into the band at the plan's last decimation if its stopband
// fails (160 kHz to 16.4 kHz at 176.4 kHz, 70 kHz to 18.2 kHz at 88.2 kHz,
// 26 kHz to 18.1 kHz at 44.1 kHz). The bars are transcode's: 1.0 dB over
// the reference in peak and in energy, and an in-band peak under -80 dBFS.
func TestAliasingIsADifferential(t *testing.T) {
	fold := map[int]float64{176400: 160000, 88200: 70000, 44100: 26000}
	for _, rate := range dsdRates {
		for _, p := range []Plan{{176400, 24}, {88200, 24}, {44100, 24}} {
			bits := ciff(rate*3/10, tones(rate, 0.5, 50000, fold[p.RateHz]))
			out, _ := convert(t, [2][]byte{bits, bits}, rate, p, 4096)
			ship := channel(out, 0, 24)
			nfft := 8192 * (p.RateHz / 44100)
			from := p.RateHz / 20
			ref := deepReference(bits, rate, p, from, nfft)
			shipMags, binHz := spectrumDBFS(ship, from, nfft, float64(p.RateHz))
			refMags, _ := spectrumDBFS(ref, 0, nfft, float64(p.RateHz))
			shipPk, shipHz := peakInBand(shipMags, binHz, 20, 20000)
			refPk, _ := peakInBand(refMags, binHz, 20, 20000)
			shipE := bandEnergyDB(shipMags, binHz, 20, 20000)
			refE := bandEnergyDB(refMags, binHz, 20, 20000)
			t.Logf("%s → %s: converter peak %.2f dBFS at %.0f Hz, energy %.2f dB | reference peak %.2f dBFS, energy %.2f dB | delta peak %+.2f dB, energy %+.2f dB",
				rateName(rate), p, shipPk, shipHz, shipE, refPk, refE, shipPk-refPk, shipE-refE)
			if d := shipPk - refPk; d > 1.0 {
				t.Errorf("%s %s: the in-band peak is %+.2f dB over the reference: a tone folded down", rateName(rate), p, d)
			}
			if d := shipE - refE; d > 1.0 {
				t.Errorf("%s %s: the in-band energy is %+.2f dB over the reference: a broadband fold", rateName(rate), p, d)
			}
			if shipPk > -80 {
				t.Errorf("%s %s: the in-band peak is %.2f dBFS at %.0f Hz: the anti-alias filter looks absent", rateName(rate), p, shipPk, shipHz)
			}
		}
	}
}

// deepReference filters one-bit samples at their own rate with one Kaiser
// low-pass from p's passband to its stopband, 160 dB down, and returns n
// output samples from frame from, each at one-bit index k·D (to within a
// byte: a constant delay changes no magnitude), rounded half to even to 24
// bits. Test code, so math.Sin and friends are fine here.
func deepReference(bits []byte, rate int, p Plan, from, n int) []float64 {
	d := rate / p.RateHz
	const atten = 160.0
	beta := 0.1102 * (atten - 8.7)
	dw := 2 * math.Pi * float64(p.StopbandHz()-p.PassbandHz()) / float64(rate)
	taps := int(math.Ceil((atten-7.95)/(2.285*dw))) + 1
	tables := (taps + 7) / 8
	taps = 8*tables - 1
	c := (taps - 1) / 2
	fc := (float64(p.PassbandHz()) + float64(p.StopbandHz())) / 2 / float64(rate)
	h := make([]float64, 8*tables)
	i0 := func(x float64) float64 {
		s, term := 1.0, 1.0
		for k := 1; k < 500; k++ {
			term *= (x / 2) * (x / 2) / float64(k*k)
			s += term
			if term < s*1e-18 {
				break
			}
		}
		return s
	}
	var sum float64
	for i := 0; i < taps; i++ {
		m := float64(i - c)
		v := 2 * fc
		if m != 0 {
			v = math.Sin(2*math.Pi*fc*m) / (math.Pi * m)
		}
		r := m / float64(c)
		h[i] = v * i0(beta*math.Sqrt(1-r*r)) / i0(beta)
		sum += h[i]
	}
	tab := make([]float64, tables*256)
	for j := 0; j < tables; j++ {
		for b := 0; b < 256; b++ {
			var v float64
			for i := 0; i < 8; i++ {
				if b&(0x80>>i) != 0 {
					v += h[8*j+i] / sum
				} else {
					v -= h[8*j+i] / sum
				}
			}
			tab[j*256+b] = v
		}
	}
	packed := planeOf(bits, false)
	out := make([]float64, n)
	for k := 0; k < n; k++ {
		start := ((from+k)*d - c) / 8 // the byte the window starts in
		var v float64
		for j := 0; j < tables; j++ {
			if idx := start + j; idx >= 0 && idx < len(packed) {
				v += tab[j*256+int(packed[idx])]
			}
		}
		const full = 1 << 23
		out[k] = math.Max(-(full-1), math.Min(full-1, math.RoundToEven(v*full))) / full
	}
	return out
}
