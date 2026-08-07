package analyze

import (
	"math"
	"math/rand"
	"testing"
)

// Tempo confidence gate (minTempoSalience). Pre-fix, estimateTempo only
// range-checked its result, so any signal long enough to autocorrelate got a
// confident BPM — a sustained organ drone reported 108, tape hiss reported 80.
// Nothing rendered BPM at the time; the file-provenance work starts to.

const confTestSeconds = 12

func confTestLen() int { return confTestSeconds * AnalysisSampleRate }

func feedAnalyzer(a *keyTempoAnalyzer, samples []float32) {
	for _, s := range samples {
		a.add(s)
	}
}

// sustainedTone is a beatless harmonic drone — an organ/cello pedal note. Zero
// transients, so the onset envelope is smooth and every lag autocorrelates
// almost perfectly.
func sustainedTone() []float32 {
	out := make([]float32, confTestLen())
	for i := range out {
		t := float64(i) / AnalysisSampleRate
		var v float64
		for h := 1; h <= 12; h++ {
			v += math.Sin(2*math.Pi*110*float64(h)*t+float64(h)) / float64(h)
		}
		out[i] = float32(0.3 * v / 2)
	}
	return out
}

// ambientPad is a sustained triad under a slow amplitude swell and vibrato —
// beatless, but with real low-frequency envelope structure.
func ambientPad() []float32 {
	out := make([]float32, confTestLen())
	freqs := []float64{220, 277.18, 329.63}
	for i := range out {
		t := float64(i) / AnalysisSampleRate
		env := 0.4 + 0.25*math.Sin(2*math.Pi*0.07*t)
		var v float64
		for k, f := range freqs {
			vib := 1 + 0.003*math.Sin(2*math.Pi*(4.3+float64(k))*t)
			v += math.Sin(2 * math.Pi * f * vib * t)
		}
		out[i] = float32(env * v / 3)
	}
	return out
}

func tapeHiss() []float32 {
	r := rand.New(rand.NewSource(7))
	out := make([]float32, confTestLen())
	for i := range out {
		out[i] = float32(0.05 * r.NormFloat64())
	}
	return out
}

// beatAt returns a percussive pulse train at bpm: a decaying 90 Hz thump every
// beat, the shape spectral flux is built to catch.
func beatAt(bpm float64) []float32 {
	out := make([]float32, confTestLen())
	period := 60.0 / bpm * AnalysisSampleRate
	for beat := 0.0; beat < float64(len(out)); beat += period {
		start := int(beat)
		for j := 0; j < 2000 && start+j < len(out); j++ {
			d := math.Exp(-float64(j) / 300)
			out[start+j] += float32(0.8 * d * math.Sin(2*math.Pi*90*float64(j)/AnalysisSampleRate))
		}
	}
	return out
}

func mixSignals(a, b []float32, ga, gb float32) []float32 {
	out := make([]float32, len(a))
	for i := range out {
		out[i] = ga*a[i] + gb*b[i]
	}
	return out
}

// assertGatesBeforeSaliencePass proves a refusal came from the salience floor
// and not from one of the cheaper gates ahead of it: the envelope is long
// enough to autocorrelate, and it carries non-zero energy after mean
// subtraction. Without this the test would still pass if the fixture were
// simply too short — pinning nothing.
func assertGatesBeforeSaliencePass(t *testing.T, a *keyTempoAnalyzer, name string) {
	t.Helper()
	maxLag := int(math.Ceil(60 * stftFrameRateHz / minTempoBPM))
	if len(a.onset) < 3*maxLag {
		t.Fatalf("%s: onset envelope is %d frames, below the %d-frame length gate — "+
			"this fixture never reaches the salience floor, so it pins nothing",
			name, len(a.onset), 3*maxLag)
	}
	var mean float64
	for _, v := range a.onset {
		mean += v
	}
	mean /= float64(len(a.onset))
	var energy float64
	for _, v := range a.onset {
		energy += (v - mean) * (v - mean)
	}
	if energy <= 0 {
		t.Fatalf("%s: onset envelope has zero energy — refused by the energy gate, "+
			"not the salience floor", name)
	}
}

// TestEstimateTempo_RefusesBeatlessSignals is the regression guard for the
// gate: signals with no beat must return "no estimate" rather than the
// confident BPM they used to produce.
func TestEstimateTempo_RefusesBeatlessSignals(t *testing.T) {
	cases := []struct {
		name    string
		samples []float32
	}{
		{"sustained harmonic drone", sustainedTone()},
		{"ambient pad", ambientPad()},
		{"tape hiss", tapeHiss()},
	}
	for _, c := range cases {
		a := newKeyTempoAnalyzer()
		feedAnalyzer(a, c.samples)
		assertGatesBeforeSaliencePass(t, a, c.name)
		if bpm, ok := a.estimateTempo(); ok {
			t.Errorf("%s: estimateTempo returned a confident %d BPM — the salience gate regressed",
				c.name, bpm)
		}
	}
}

// TestEstimateTempo_SameSourcePlusBeatIsAccepted is the negative control for
// the test above. The refused fixtures and the accepted ones differ in exactly
// one thing: a beat. Same generator, same length, same sample rate — so a
// refusal cannot be blamed on some incidental property of the fixture, and the
// gate cannot pass by refusing everything.
func TestEstimateTempo_SameSourcePlusBeatIsAccepted(t *testing.T) {
	cases := []struct {
		name string
		base []float32
		bpm  float64
	}{
		{"drone + beat", sustainedTone(), 100},
		{"pad + beat", ambientPad(), 128},
		{"hiss + beat", tapeHiss(), 84},
	}
	for _, c := range cases {
		// Refused without the beat.
		bare := newKeyTempoAnalyzer()
		feedAnalyzer(bare, c.base)
		if _, ok := bare.estimateTempo(); ok {
			t.Fatalf("%s: the base signal alone was accepted — control is void", c.name)
		}
		// Accepted with it, at the right tempo.
		withBeat := newKeyTempoAnalyzer()
		feedAnalyzer(withBeat, mixSignals(c.base, beatAt(c.bpm), 0.8, 0.7))
		got, ok := withBeat.estimateTempo()
		if !ok {
			t.Errorf("%s: adding a %.0f BPM beat did not lift the signal over the "+
				"salience floor — the gate is over-suppressing", c.name, c.bpm)
			continue
		}
		if math.Abs(float64(got)-c.bpm) > 3 {
			t.Errorf("%s: tempo = %d BPM, want ~%.0f", c.name, got, c.bpm)
		}
	}
}

// TestEstimateTempo_RefusesFlatOnsetEnvelope covers a blind spot the salience
// gate closes: a perfectly constant onset envelope does NOT trip the `energy
// <= 0` guard, because mean subtraction over a value like 0.7 leaves float
// residue that sums above zero. Pre-fix that reported a confident ~122 BPM.
func TestEstimateTempo_RefusesFlatOnsetEnvelope(t *testing.T) {
	a := newKeyTempoAnalyzer()
	a.onset = make([]float64, 1500)
	for i := range a.onset {
		a.onset[i] = 0.7
	}
	var mean float64
	for _, v := range a.onset {
		mean += v
	}
	mean /= float64(len(a.onset))
	var energy float64
	for _, v := range a.onset {
		energy += (v - mean) * (v - mean)
	}
	if energy <= 0 {
		t.Skip("this platform's float arithmetic zeroes the residue; the energy gate " +
			"already covers this case here")
	}
	if bpm, ok := a.estimateTempo(); ok {
		t.Errorf("a perfectly flat onset envelope returned %d BPM", bpm)
	}
}

// TestPeakSalience covers the helper's contract directly, including the
// zero-spread branch a noiseless impulse train reaches.
func TestPeakSalience(t *testing.T) {
	// A flat baseline with the winner above it: zero MAD, maximal salience.
	// This is the shape an ideal impulse train's autocorrelation takes, so
	// treating zero spread as "no confidence" would refuse the clearest
	// possible beat.
	flat := make([]float64, 40)
	if got := peakSalience(flat, 1.0); !math.IsInf(got, 1) {
		t.Errorf("flat baseline with a peak above it: salience = %v, want +Inf", got)
	}
	// Flat baseline, winner ON it: no peak at all.
	if got := peakSalience(flat, 0); got != 0 {
		t.Errorf("flat baseline with no peak: salience = %v, want 0", got)
	}
	// Winner below a flat baseline is not a peak either.
	if got := peakSalience(flat, -1); got != 0 {
		t.Errorf("winner below a flat baseline: salience = %v, want 0", got)
	}
	// Normal spread: salience is in robust sigma units above the median.
	spread := make([]float64, 41)
	for i := range spread {
		spread[i] = float64(i-20) / 20 // -1..1, median 0, MAD 0.5
	}
	got := peakSalience(spread, 1.0)
	want := 1.0 / (0.5 * madToSigma)
	if math.Abs(got-want) > 0.05 {
		t.Errorf("spread baseline: salience = %.4f, want ~%.4f", got, want)
	}
	// Empty input must not panic or claim confidence.
	if got := peakSalience(nil, 1.0); got != 0 {
		t.Errorf("empty acs: salience = %v, want 0", got)
	}
}

// TestPeakSalience_HarmonicPeaksDoNotSuppressTheWinner guards the background
// estimate against a beat's own harmonic siblings: a strongly rhythmic track
// plants peaks at multiples of its beat lag inside the searched range, and an
// implementation that scored the winner against its nearest competitor rather
// than against the background would refuse exactly the most certain estimates.
//
// This does NOT pin the median/MAD choice — a mean/sd background scores this
// fixture at 4.4, comfortably over the floor, and no plausible fixture
// separates them without being contrived. That choice rests on the calibration
// sweep recorded on minTempoSalience, not on this test.
func TestPeakSalience_HarmonicPeaksDoNotSuppressTheWinner(t *testing.T) {
	acs := make([]float64, 86)
	for i := range acs {
		acs[i] = 0.01 * float64(i%3) // low, structureless background
	}
	winner := 0.9
	acs[10] = winner
	for _, harmonic := range []int{20, 40, 60, 80} {
		acs[harmonic] = 0.8 // the beat's own multiples
	}
	if got := peakSalience(acs, winner); got < minTempoSalience {
		t.Errorf("a winner with four harmonic siblings scored %.2f, below the %.1f "+
			"floor — the spread estimator is not robust to them", got, minTempoSalience)
	}
}
