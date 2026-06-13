package analyze

import (
	"math"
	"math/cmplx"

	"gonum.org/v1/gonum/dsp/fourier"
)

// Signal-derived musical key (Krumhansl-Schmuckler) + tempo (onset
// autocorrelation), computed from the same streaming mono 48 kHz decode
// the peaker + loudness meter share. Both are **best-effort estimates**:
// the bridge surfaces them only when the source carries no curated tag,
// and iOS labels them "estimated". Wrong-but-confident is the failure to
// avoid, so the estimators gate on having enough signal and fall back to
// "no estimate" rather than guessing from too little.
//
// One STFT (4096-pt Hann, 512 hop = 87.5% overlap) feeds both:
//   - Key: per-window magnitude → a 12-bin chroma (midrange only, where
//     the 11.7 Hz bins resolve semitones), correlated against the 24
//     Krumhansl-Kessler key profiles.
//   - Tempo: spectral flux per hop → an onset envelope → autocorrelation,
//     weighted by a log-domain Gaussian prior (centre ~120 BPM) so the
//     dominant lag isn't an octave (half/double) of the real tempo.
//
// 512-hop (vs the 2048 the chroma alone would need) buys the onset
// envelope a 93.75 Hz frame rate, which is what gives tempo usable
// resolution — at 23 Hz the integer lags map to coarse BPM steps.

const (
	stftWindow      = 4096
	stftHop         = 512
	stftFrameRateHz = float64(AnalysisSampleRate) / float64(stftHop) // 93.75

	// Chroma is accumulated only from bins between C3 (~130.8 Hz) and
	// C6 (~1046.5 Hz). Below C3 the 11.7 Hz bin spacing can't separate
	// adjacent semitones; above C6 partials dominate and smear the
	// pitch-class estimate.
	chromaMinHz = 130.0
	chromaMaxHz = 1050.0

	// Estimate gates — too little signal returns "no estimate" rather
	// than a guess. ~2 s of windows for key, enough onset frames to cover
	// several of the slowest beat periods for tempo.
	minWindowsForKey = 180

	// maxAnalyzeWindows caps STFT processing at ~30 min of audio (at the
	// 512-sample hop). Key + tempo are stable estimates well before then,
	// so processing the tail of a multi-hour input (a DJ set, an
	// audiobook, a corrupt header claiming a vast length) only burns CPU
	// and grows the onset envelope unboundedly. Past the cap, add() is a
	// no-op — chroma + onset are frozen at a representative prefix. Mirrors
	// the waveform peaker's maxWaveformBuckets guard.
	maxAnalyzeWindows = 30 * 60 * AnalysisSampleRate / stftHop // ~168750

	// Tempo search range + the lag window it implies at the onset frame
	// rate. lag = 60 · frameRate / bpm.
	minTempoBPM = 50.0
	maxTempoBPM = 200.0

	// Perceptual prior: a log2-domain Gaussian centred here biases the
	// autocorrelation away from octave errors (the classic half/double
	// tempo confusion). Sigma is in octaves.
	tempoPriorCentreBPM = 120.0
	tempoPriorSigmaOct  = 0.9
)

// Krumhansl-Kessler key profiles (the canonical major/minor tonal
// hierarchies). Index 0 is the tonic; the estimator rotates the chroma so
// each candidate tonic aligns with index 0 before correlating.
var (
	ksMajor = [12]float64{6.35, 2.23, 3.48, 2.33, 4.38, 4.09, 2.52, 5.19, 2.39, 3.66, 2.29, 2.88}
	ksMinor = [12]float64{6.33, 2.68, 3.52, 5.38, 2.60, 3.53, 2.54, 4.75, 3.98, 2.69, 3.34, 3.17}
)

// binChromaBin maps one FFT bin in the chroma midrange to the two nearest
// pitch classes, split by how close the bin's frequency sits to each
// (linear in the semitone domain — handles bins falling between semitones
// and non-A440 tuning without a hard round-to-nearest).
type binChromaBin struct {
	bin      int
	pc1, pc2 int
	w1, w2   float64
}

type keyTempoAnalyzer struct {
	fft      *fourier.FFT
	hann     []float64 // window coefficients
	buf      []float64 // sliding sample buffer (len stftWindow)
	bufLen   int
	windowed []float64 // buf · hann, reused
	coeff    []complex128
	mag      []float64 // magnitude spectrum, reused
	prevMag  []float64 // previous window's magnitude (for spectral flux)

	binMap   []binChromaBin // precomputed midrange bin → pitch-class split
	chroma   [12]float64
	onset    []float64 // spectral flux per hop (the onset envelope)
	windows  int
	havePrev bool
}

func newKeyTempoAnalyzer() *keyTempoAnalyzer {
	n := stftWindow
	a := &keyTempoAnalyzer{
		fft:      fourier.NewFFT(n),
		hann:     make([]float64, n),
		buf:      make([]float64, n),
		windowed: make([]float64, n),
		coeff:    make([]complex128, n/2+1),
		mag:      make([]float64, n/2+1),
		prevMag:  make([]float64, n/2+1),
	}
	// Hann window.
	for i := 0; i < n; i++ {
		a.hann[i] = 0.5 - 0.5*math.Cos(2*math.Pi*float64(i)/float64(n-1))
	}
	// Precompute the midrange bin → pitch-class split.
	binHz := float64(AnalysisSampleRate) / float64(n)
	for bin := 1; bin <= n/2; bin++ {
		freq := float64(bin) * binHz
		if freq < chromaMinHz || freq > chromaMaxHz {
			continue
		}
		// MIDI-ish pitch in semitones; pitch class is the fractional
		// position split across the two nearest classes.
		pitch := 12*math.Log2(freq/440.0) + 69.0
		lower := math.Floor(pitch)
		frac := pitch - lower
		pc1 := ((int(lower) % 12) + 12) % 12
		pc2 := ((int(lower)+1)%12 + 12) % 12
		a.binMap = append(a.binMap, binChromaBin{
			bin: bin, pc1: pc1, pc2: pc2, w1: 1 - frac, w2: frac,
		})
	}
	return a
}

// add folds one mono sample into the sliding STFT. A window is processed
// each time stftHop new samples have arrived (after the first full
// window). Once maxAnalyzeWindows have been processed it becomes a no-op,
// freezing the estimate at a representative prefix (bounds memory + CPU).
func (a *keyTempoAnalyzer) add(s float32) {
	if a.windows >= maxAnalyzeWindows {
		return
	}
	a.buf[a.bufLen] = float64(s)
	a.bufLen++
	if a.bufLen == stftWindow {
		a.processWindow()
		copy(a.buf, a.buf[stftHop:])
		a.bufLen = stftWindow - stftHop
	}
}

func (a *keyTempoAnalyzer) processWindow() {
	for i := 0; i < stftWindow; i++ {
		a.windowed[i] = a.buf[i] * a.hann[i]
	}
	a.fft.Coefficients(a.coeff, a.windowed)
	for i := range a.coeff {
		a.mag[i] = cmplx.Abs(a.coeff[i])
	}
	for _, bc := range a.binMap {
		m := a.mag[bc.bin]
		a.chroma[bc.pc1] += m * bc.w1
		a.chroma[bc.pc2] += m * bc.w2
	}
	// Spectral flux: sum of positive bin-to-bin magnitude increases. The
	// first window has no predecessor, so it seeds prevMag only.
	if a.havePrev {
		var flux float64
		for i := range a.mag {
			if d := a.mag[i] - a.prevMag[i]; d > 0 {
				flux += d
			}
		}
		a.onset = append(a.onset, flux)
	}
	copy(a.prevMag, a.mag)
	a.havePrev = true
	a.windows++
}

// estimateKey returns the best-correlating key as (root 0..11 with C=0,
// mode "major"/"minor"). ok is false when too few windows were seen or the
// chroma is empty (silence).
func (a *keyTempoAnalyzer) estimateKey() (root int, mode string, ok bool) {
	if a.windows < minWindowsForKey {
		return 0, "", false
	}
	chroma := a.chroma
	var sum float64
	for _, v := range chroma {
		sum += v
	}
	if sum <= 0 {
		return 0, "", false
	}

	bestCorr := math.Inf(-1)
	bestRoot, bestMajor := 0, true
	var rotated [12]float64
	for r := 0; r < 12; r++ {
		for i := 0; i < 12; i++ {
			rotated[i] = chroma[(r+i)%12]
		}
		if c := pearson12(rotated, ksMajor); c > bestCorr {
			bestCorr, bestRoot, bestMajor = c, r, true
		}
		if c := pearson12(rotated, ksMinor); c > bestCorr {
			bestCorr, bestRoot, bestMajor = c, r, false
		}
	}
	if bestMajor {
		return bestRoot, "major", true
	}
	return bestRoot, "minor", true
}

// estimateTempo returns the dominant tempo in integer BPM, or ok=false
// when the onset envelope is too short to autocorrelate over the slowest
// beat period in range.
func (a *keyTempoAnalyzer) estimateTempo() (bpm int, ok bool) {
	minLag := int(math.Floor(60 * stftFrameRateHz / maxTempoBPM)) // fastest BPM → smallest lag
	maxLag := int(math.Ceil(60 * stftFrameRateHz / minTempoBPM))  // slowest BPM → largest lag
	if minLag < 1 {
		minLag = 1
	}
	if len(a.onset) < 3*maxLag {
		return 0, false
	}

	// Mean-subtract so the autocorrelation reflects periodicity, not DC.
	mean := 0.0
	for _, v := range a.onset {
		mean += v
	}
	mean /= float64(len(a.onset))
	env := make([]float64, len(a.onset))
	var energy float64
	for i, v := range a.onset {
		env[i] = v - mean
		energy += env[i] * env[i]
	}
	if energy <= 0 {
		return 0, false
	}

	bestScore := math.Inf(-1)
	bestLag := 0
	for lag := minLag; lag <= maxLag; lag++ {
		var ac float64
		for i := lag; i < len(env); i++ {
			ac += env[i] * env[i-lag]
		}
		ac /= energy // normalise so longer envelopes don't dominate
		lagBPM := 60 * stftFrameRateHz / float64(lag)
		// Log2-domain Gaussian prior → octave-error suppression.
		z := math.Log2(lagBPM/tempoPriorCentreBPM) / tempoPriorSigmaOct
		score := ac * math.Exp(-0.5*z*z)
		if score > bestScore {
			bestScore, bestLag = score, lag
		}
	}
	if bestLag == 0 {
		return 0, false
	}
	bpm = int(math.Round(60 * stftFrameRateHz / float64(bestLag)))
	if bpm < int(minTempoBPM) || bpm > int(maxTempoBPM) {
		return 0, false
	}
	return bpm, true
}

// pearson12 is the Pearson correlation between two 12-element vectors.
func pearson12(a [12]float64, b [12]float64) float64 {
	var ma, mb float64
	for i := 0; i < 12; i++ {
		ma += a[i]
		mb += b[i]
	}
	ma /= 12
	mb /= 12
	var num, da, db float64
	for i := 0; i < 12; i++ {
		x := a[i] - ma
		y := b[i] - mb
		num += x * y
		da += x * x
		db += y * y
	}
	if da <= 0 || db <= 0 {
		return 0
	}
	return num / math.Sqrt(da*db)
}
