package analyze

import (
	"math"
	"testing"
)

// TestEstimateKey_ProfileRotation pins the K-S correlation + the rotation
// math: a chroma shaped exactly like a key profile rooted at R must
// estimate back to (R, that mode). This verifies the estimator inverts
// the rotation correctly (an off-by-one in the rotation would report the
// wrong tonic) and discriminates major from minor.
func TestEstimateKey_ProfileRotation(t *testing.T) {
	cases := []struct {
		name    string
		profile [12]float64
		root    int
		mode    string
	}{
		{"G major", ksMajor, 7, "major"},
		{"D minor", ksMinor, 2, "minor"},
		{"C major", ksMajor, 0, "major"},
		{"A minor", ksMinor, 9, "minor"},
		{"F# major", ksMajor, 6, "major"},
	}
	for _, c := range cases {
		a := newKeyTempoAnalyzer()
		a.windows = minWindowsForKey // satisfy the length gate
		// chroma[k] = profile value for pitch class k in a key rooted at c.root.
		for k := 0; k < 12; k++ {
			a.chroma[k] = c.profile[((k-c.root)%12+12)%12]
		}
		root, mode, ok := a.estimateKey()
		if !ok {
			t.Errorf("%s: estimateKey ok=false", c.name)
			continue
		}
		if root != c.root || mode != c.mode {
			t.Errorf("%s: got (%d, %s), want (%d, %s)", c.name, root, mode, c.root, c.mode)
		}
	}
}

// TestEstimateKey_GatesOnSignal: too few windows or empty chroma returns
// "no estimate" rather than a guess.
func TestEstimateKey_GatesOnSignal(t *testing.T) {
	a := newKeyTempoAnalyzer()
	a.chroma[0] = 5
	a.windows = minWindowsForKey - 1 // below the gate
	if _, _, ok := a.estimateKey(); ok {
		t.Error("estimateKey should refuse below minWindowsForKey")
	}
	a.windows = minWindowsForKey
	a.chroma = [12]float64{} // silence
	if _, _, ok := a.estimateKey(); ok {
		t.Error("estimateKey should refuse empty (silent) chroma")
	}
}

// TestEstimateTempo_KnownPeriodAndOctave pins the tempo math AND the
// octave-error suppression: an impulse train at the 120 BPM period
// autocorrelates equally at the 1× (120) and 2× (60) lags, and the
// perceptual prior must resolve it to 120, not 60.
func TestEstimateTempo_KnownPeriodAndOctave(t *testing.T) {
	// 120 BPM → lag = 60 · frameRate / 120 ≈ 46.875 frames.
	period := int(math.Round(60 * stftFrameRateHz / 120.0))
	onset := make([]float64, 1500)
	for i := 0; i < len(onset); i += period {
		onset[i] = 1.0
	}
	a := newKeyTempoAnalyzer()
	a.onset = onset
	bpm, ok := a.estimateTempo()
	if !ok {
		t.Fatal("estimateTempo ok=false on a clean impulse train")
	}
	if bpm < 117 || bpm > 123 {
		t.Fatalf("tempo = %d BPM, want ~120 (octave error if ~60/240)", bpm)
	}
}

// TestEstimateTempo_90BPM: a different period resolves correctly (not just
// the prior centre).
func TestEstimateTempo_90BPM(t *testing.T) {
	period := int(math.Round(60 * stftFrameRateHz / 90.0))
	onset := make([]float64, 2000)
	for i := 0; i < len(onset); i += period {
		onset[i] = 1.0
	}
	a := newKeyTempoAnalyzer()
	a.onset = onset
	bpm, ok := a.estimateTempo()
	if !ok {
		t.Fatal("estimateTempo ok=false")
	}
	if bpm < 87 || bpm > 93 {
		t.Fatalf("tempo = %d BPM, want ~90", bpm)
	}
}

// TestEstimateTempo_GatesOnShortOnset: an onset envelope too short to
// autocorrelate over the slowest beat period returns "no estimate".
func TestEstimateTempo_GatesOnShortOnset(t *testing.T) {
	a := newKeyTempoAnalyzer()
	a.onset = make([]float64, 50) // far below 3·maxLag
	if _, ok := a.estimateTempo(); ok {
		t.Error("estimateTempo should refuse a too-short onset envelope")
	}
}
