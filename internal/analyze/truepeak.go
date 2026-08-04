package analyze

import "math"

// truePeakMeter measures the BS.1770-4-style true peak of the analysis
// stream: per-channel 4x polyphase windowed-sinc interpolation over the
// decoded frames, tracking the maximum absolute interpolated value across
// every channel.
//
// HONESTY NOTE — this is the true peak of the 48 kHz ANALYSIS RENDERING,
// not of the native-rate source. The package invariant is one decode per
// track at 48 kHz (see the package doc), and no native-rate path exists;
// for musical content the difference is small (the interpolator's job is
// exactly to reconstruct what a resampler would), and the iOS side's LIVE
// meter measures the native stream anyway. The wire field is named
// `truePeakDB` (not `dBTP`) and PROTOCOL.md states the derivation.
//
// The interpolation filter mirrors the iOS `MeterAnalysis` design: 4
// phases x 12 taps/phase Blackman-windowed sinc, centered on a whole
// input sample (one phase is a pure delay, so on-grid peaks pass through
// exactly), each phase normalized to unity DC gain (interpolation adds no
// level of its own). Validated against ffmpeg's ebur128 true-peak in
// truepeak_test.go rather than against a published coefficient table.
type truePeakMeter struct {
	channels int
	taps     []float64
	// Per-channel history of the previous tapsPerPhase-1 input samples
	// (flat, channel-major) so intersample peaks straddling frame-callback
	// boundaries aren't dropped.
	history []float64
	maxAbs  float64
	seen    bool
}

const (
	truePeakOversample   = 4
	truePeakTapsPerPhase = 12
)

func newTruePeakMeter(channels int) *truePeakMeter {
	if channels < 1 {
		channels = 1
	}
	return &truePeakMeter{
		channels: channels,
		taps:     makeTruePeakTaps(truePeakOversample, truePeakTapsPerPhase),
		history:  make([]float64, channels*(truePeakTapsPerPhase-1)),
	}
}

// makeTruePeakTaps builds the polyphase interpolation filter: Blackman-
// windowed sinc, total = oversample*tapsPerPhase taps, integer center at
// total/2 (a multiple of oversample when tapsPerPhase is even, which is
// what makes phase 0 a pure delay), then each polyphase branch scaled to
// sum to exactly 1.
func makeTruePeakTaps(oversample, tapsPerPhase int) []float64 {
	total := oversample * tapsPerPhase
	center := total / 2
	taps := make([]float64, total)
	for n := 0; n < total; n++ {
		t := float64(n-center) / float64(oversample)
		sinc := 1.0
		if t != 0 {
			sinc = math.Sin(math.Pi*t) / (math.Pi * t)
		}
		w := 0.42 - 0.5*math.Cos(2*math.Pi*float64(n)/float64(total-1)) +
			0.08*math.Cos(4*math.Pi*float64(n)/float64(total-1))
		taps[n] = sinc * w
	}
	for phase := 0; phase < oversample; phase++ {
		sum := 0.0
		for k := phase; k < total; k += oversample {
			sum += taps[k]
		}
		if sum == 0 {
			continue
		}
		for k := phase; k < total; k += oversample {
			taps[k] /= sum
		}
	}
	return taps
}

// addFrame consumes one interleaved frame (len == channels). Channel-count
// mismatches and non-finite samples are skipped — a NaN would poison the
// running max the same way it would poison the loudness biquads.
func (m *truePeakMeter) addFrame(frame []float64) {
	if len(frame) != m.channels {
		return
	}
	histLen := truePeakTapsPerPhase - 1
	for c := 0; c < m.channels; c++ {
		x := frame[c]
		if math.IsNaN(x) || math.IsInf(x, 0) {
			x = 0
		}
		hist := m.history[c*histLen : (c+1)*histLen]
		// Interpolated outputs at this input position: phase p uses taps
		// [k*oversample+p] against x[n-k] (newest-first over history+x).
		for p := 0; p < truePeakOversample; p++ {
			acc := 0.0
			for k := 0; k < truePeakTapsPerPhase; k++ {
				var sample float64
				if k == 0 {
					sample = x
				} else {
					sample = hist[histLen-k]
				}
				acc += m.taps[k*truePeakOversample+p] * sample
			}
			if a := math.Abs(acc); a > m.maxAbs {
				m.maxAbs = a
			}
		}
		// Slide the history: drop the oldest, append x.
		copy(hist, hist[1:])
		hist[histLen-1] = x
		m.seen = true
	}
}

// truePeakDB returns the measured true peak in dB relative to full scale,
// or ok=false when no samples were seen or the program was pure silence
// (log of zero).
func (m *truePeakMeter) truePeakDB() (float64, bool) {
	if !m.seen || m.maxAbs <= 0 {
		return 0, false
	}
	return 20 * math.Log10(m.maxAbs), true
}
