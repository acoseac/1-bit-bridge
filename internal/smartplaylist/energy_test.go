package smartplaylist

import (
	"math"
	"testing"
)

func fptr(v float64) *float64 { return &v }

func TestLoudnessToEnergy_LinearMap(t *testing.T) {
	cases := []struct {
		db   float64
		want float64
	}{
		{0, 1.0},     // ceiling
		{-24, 0.0},   // floor
		{-12, 0.5},   // midpoint of the window
		{-6, 0.75},   // 3/4
		{-18, 0.25},  // 1/4
		{6, 1.0},     // above ceiling → clamp
		{-30, 0.0},   // below floor → clamp
		{-1000, 0.0}, // pathological → clamp
	}
	for _, c := range cases {
		if got := LoudnessToEnergy(c.db); math.Abs(got-c.want) > 1e-9 {
			t.Errorf("LoudnessToEnergy(%.1f) = %.4f want %.4f", c.db, got, c.want)
		}
	}
}

func TestEnergyEnvelope_EmptyAndUnderHalf(t *testing.T) {
	if env := EnergyEnvelope(nil, 1); env != nil {
		t.Errorf("nil input should yield nil, got %v", env)
	}
	if env := EnergyEnvelope([]*float64{}, 1); env != nil {
		t.Errorf("empty input should yield nil, got %v", env)
	}
	// 1 of 4 analyzed (< half) → nil (seeded fallback on the client).
	under := []*float64{fptr(-6), nil, nil, nil}
	if env := EnergyEnvelope(under, 1); env != nil {
		t.Errorf("under-half analyzed should yield nil, got %v", env)
	}
	// Exactly half → built (2 of 4).
	half := []*float64{fptr(-6), fptr(-12), nil, nil}
	if env := EnergyEnvelope(half, 1); len(env) != 4 {
		t.Errorf("exactly-half analyzed should build a 4-element env, got %v", env)
	}
}

func TestEnergyEnvelope_NilUsesMidpoint(t *testing.T) {
	// 3 known + 1 unknown (>= half). Distinct loudness so no anti-flatten
	// noise fires (variance is high), letting us assert exact midpoint.
	in := []*float64{fptr(0), fptr(-24), fptr(-12), nil}
	env := EnergyEnvelope(in, 1)
	if len(env) != 4 {
		t.Fatalf("want 4 elements, got %d", len(env))
	}
	want := []float64{1.0, 0.0, 0.5, energyMidpoint}
	for i := range want {
		if math.Abs(env[i]-want[i]) > 1e-9 {
			t.Errorf("env[%d] = %.4f want %.4f", i, env[i], want[i])
		}
	}
}

func TestEnergyEnvelope_Downsample(t *testing.T) {
	// 100 ascending loudness values → downsampled to energyMaxBars,
	// preserving the rising contour (first bar < last bar).
	in := make([]*float64, 100)
	for i := range in {
		db := -24 + float64(i)*24/99 // -24 … 0
		in[i] = &db
	}
	env := EnergyEnvelope(in, 7)
	if len(env) != energyMaxBars {
		t.Fatalf("want %d bars after downsample, got %d", energyMaxBars, len(env))
	}
	if env[0] >= env[len(env)-1] {
		t.Errorf("rising contour not preserved: first=%.3f last=%.3f", env[0], env[len(env)-1])
	}
	for i, v := range env {
		if v < 0 || v > 1 {
			t.Errorf("env[%d]=%.4f out of [0,1]", i, v)
		}
	}
}

func TestEnergyEnvelope_AntiFlattenDeterministic(t *testing.T) {
	// All members at the same loudness → variance 0 → micro-noise injected.
	flat := make([]*float64, 12)
	for i := range flat {
		flat[i] = fptr(-12) // → 0.5
	}
	a := EnergyEnvelope(flat, 42)
	b := EnergyEnvelope(flat, 42)
	if len(a) != 12 {
		t.Fatalf("want 12 elements, got %d", len(a))
	}
	// Deterministic for the same seed.
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("non-deterministic at %d: %.6f vs %.6f", i, a[i], b[i])
		}
	}
	// Noise actually broke the flatness…
	allSame := true
	for i := 1; i < len(a); i++ {
		if a[i] != a[0] {
			allSame = false
			break
		}
	}
	if allSame {
		t.Error("anti-flatten did not perturb a flat envelope")
	}
	// …but stayed within ±amp of the base 0.5 and inside [0,1].
	for i, v := range a {
		if v < 0.5-energyNoiseAmp-1e-9 || v > 0.5+energyNoiseAmp+1e-9 {
			t.Errorf("env[%d]=%.4f outside 0.5±%.2f", i, v, energyNoiseAmp)
		}
	}
	// A different seed yields a different perturbation.
	c := EnergyEnvelope(flat, 99)
	diff := false
	for i := range a {
		if a[i] != c[i] {
			diff = true
			break
		}
	}
	if !diff {
		t.Error("different seeds produced identical noise")
	}
}

func TestModalRateHz(t *testing.T) {
	cases := []struct {
		name  string
		rates []int
		want  int
	}{
		{"empty", nil, 0},
		{"all-zero ignored", []int{0, 0}, 0},
		{"single", []int{44100}, 44100},
		{"clear modal", []int{96000, 96000, 44100}, 96000},
		{"tie breaks to highest", []int{44100, 96000}, 96000},
		{"tie of three breaks highest", []int{44100, 96000, 192000}, 192000},
		{"modal beats a higher singleton", []int{44100, 44100, 192000}, 44100},
		{"negatives ignored", []int{-1, 48000, -5}, 48000},
		{"dsd biases high on tie", []int{96000, 2822400}, 2822400},
	}
	for _, c := range cases {
		if got := ModalRateHz(c.rates); got != c.want {
			t.Errorf("%s: ModalRateHz(%v) = %d want %d", c.name, c.rates, got, c.want)
		}
	}
}

func TestSeedFromSlug_StableAndDistinct(t *testing.T) {
	seed := SeedFromSlug("heavy-rotation")
	if seed != SeedFromSlug("heavy-rotation") {
		t.Error("seed not stable for the same slug")
	}
	if seed == SeedFromSlug("auto-mix") {
		t.Error("distinct slugs collided")
	}
}

// Integration: Generate stamps Energy + ModalRateHz onto each family from its
// members' loudness/rate.
func TestGenerate_StampsEnergyAndModalRate(t *testing.T) {
	in := richInputs()
	for _, p := range []string{"a", "b", "c"} {
		f := in.Features[p]
		f.ReplayGainTrackDB = fptr(-6) // → 0.75
		f.SampleRate = func() *int { v := 96000; return &v }()
		in.Features[p] = f
	}
	out := Generate(in, testOpts(true))

	hr := byKind(out, KindHeavyRotation)
	if hr == nil {
		t.Fatal("heavy rotation family missing")
	}
	if len(hr.Energy) != len(hr.Items) {
		t.Errorf("energy len %d != item count %d", len(hr.Energy), len(hr.Items))
	}
	if hr.ModalRateHz != 96000 {
		t.Errorf("modal rate = %d want 96000", hr.ModalRateHz)
	}
	// Three identical loudnesses → anti-flatten noise around 0.75.
	for i, e := range hr.Energy {
		if e < 0.75-energyNoiseAmp-1e-9 || e > 0.75+energyNoiseAmp+1e-9 {
			t.Errorf("energy[%d]=%.4f outside 0.75±noise", i, e)
		}
	}
}

// A family whose members have no loudness gets a nil Energy (the regenerator
// then omits energy_json so iOS falls back to the seeded waveform).
func TestGenerate_NilEnergyWhenUnanalyzed(t *testing.T) {
	in := richInputs() // richInputs' features carry no ReplayGain/SampleRate
	out := Generate(in, testOpts(true))
	hr := byKind(out, KindHeavyRotation)
	if hr == nil {
		t.Fatal("heavy rotation family missing")
	}
	if hr.Energy != nil {
		t.Errorf("expected nil energy with no analyzed loudness, got %v", hr.Energy)
	}
	if hr.ModalRateHz != 0 {
		t.Errorf("expected modal rate 0 with no sample rates, got %d", hr.ModalRateHz)
	}
}
