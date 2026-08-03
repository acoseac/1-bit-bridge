package analyze

import (
	"math"
	"testing"
)

func TestTruePeakTapsPhasesSumToUnity(t *testing.T) {
	taps := makeTruePeakTaps(truePeakOversample, truePeakTapsPerPhase)
	if len(taps) != truePeakOversample*truePeakTapsPerPhase {
		t.Fatalf("tap count = %d", len(taps))
	}
	for phase := 0; phase < truePeakOversample; phase++ {
		sum := 0.0
		for k := phase; k < len(taps); k += truePeakOversample {
			sum += taps[k]
		}
		if math.Abs(sum-1.0) > 1e-9 {
			t.Fatalf("phase %d DC gain = %v, want 1", phase, sum)
		}
	}
}

// The analytic fixture: x[n] = sin(pi/2*n + pi/4) samples a full-scale
// sine at only +/-0.7071 (sample peak -3.01 dBFS) while the underlying
// waveform peaks at 1.0. The 4x grid lands exactly on the crest, so the
// meter must recover ~0 dB — the intersample-over case the feature
// exists to catch.
func TestTruePeakFs4SineWith45DegPhaseReadsIntersampleOver(t *testing.T) {
	m := newTruePeakMeter(1)
	frame := make([]float64, 1)
	for n := 0; n < 4096; n++ {
		frame[0] = math.Sin(math.Pi/2*float64(n) + math.Pi/4)
		m.addFrame(frame)
	}
	db, ok := m.truePeakDB()
	if !ok {
		t.Fatal("expected a measurement")
	}
	if math.Abs(db) > 0.5 {
		t.Fatalf("true peak = %.2f dB, want ~0 (intersample crest recovered)", db)
	}
}

// Negative control: a slow sine whose crest sits ON the sample grid must
// not gain level from the interpolator.
func TestTruePeakOnGridSineShowsNoOvershoot(t *testing.T) {
	m := newTruePeakMeter(2)
	frame := make([]float64, 2)
	for n := 0; n < 4096; n++ {
		v := 0.5 * math.Sin(2*math.Pi*float64(n)/32)
		frame[0], frame[1] = v, v
		m.addFrame(frame)
	}
	db, ok := m.truePeakDB()
	if !ok {
		t.Fatal("expected a measurement")
	}
	want := 20 * math.Log10(0.5)
	if math.Abs(db-want) > 0.2 {
		t.Fatalf("true peak = %.2f dB, want ~%.2f", db, want)
	}
}

func TestTruePeakSilenceIsNotOK(t *testing.T) {
	m := newTruePeakMeter(2)
	frame := make([]float64, 2)
	for n := 0; n < 1000; n++ {
		m.addFrame(frame)
	}
	if _, ok := m.truePeakDB(); ok {
		t.Fatal("silence must not produce a true peak")
	}
}

func TestTruePeakNonFiniteSamplesAreNeutralized(t *testing.T) {
	m := newTruePeakMeter(1)
	m.addFrame([]float64{math.NaN()})
	m.addFrame([]float64{math.Inf(1)})
	m.addFrame([]float64{0.25})
	db, ok := m.truePeakDB()
	if !ok {
		t.Fatal("expected a measurement from the finite sample")
	}
	if db > 0.5 {
		t.Fatalf("NaN/Inf leaked into the max: %.2f dB", db)
	}
}
