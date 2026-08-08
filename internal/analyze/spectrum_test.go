package analyze

import (
	"math"
	"math/rand"
	"testing"

	"gonum.org/v1/gonum/dsp/fourier"
)

// Ground truth for the bridge-side spectrum. Mirrors the iOS fixture set
// (§6.2): a genuinely hi-res source, a CD upsampled to look like one, and —
// the case the feature is worthless without — a genuinely band-limited hi-res
// master that must NOT be mistaken for the upsample.
//
// Signals are synthesised here and fed through the REAL analyzer, so what is
// measured is the shipping STFT (Hann window, 4096/512) rather than a stand-in.

// synth builds a broadband music surrogate at the analysis rate: pink-ish
// noise plus musical partials, low-passed at cutoffHz with rolloffDBPerOctave,
// and hard-walled at stopHz when set.
//
// The stop band is what makes an "upsampled" fixture honest — a steep slope is
// not a resampler. At 200 dB/octave there is still measurable energy an octave
// up; a resampler leaves the dither floor and nothing else.
func synth(seconds, cutoffHz, rolloffDBPerOctave float64, stopHz float64, seed int64) []float32 {
	n := int(float64(AnalysisSampleRate) * seconds)
	r := rand.New(rand.NewSource(seed))
	buf := make([]float64, n)
	var p0, p1, p2 float64
	for i := range buf {
		w := r.NormFloat64()
		p0 = 0.99765*p0 + w*0.0990460
		p1 = 0.96300*p1 + w*0.2965164
		p2 = 0.57000*p2 + w*1.0526913
		buf[i] = (p0 + p1 + p2 + w*0.1848) * 0.05
	}
	for h := 1; h <= 24; h++ {
		f := 110.0 * float64(h)
		if f >= float64(AnalysisSampleRate)/2 {
			break
		}
		amp := 0.25 / float64(h)
		inc := 2 * math.Pi * f / float64(AnalysisSampleRate)
		for i := range buf {
			buf[i] += amp * math.Sin(inc*float64(i))
		}
	}
	applySpectralShape(buf, cutoffHz, rolloffDBPerOctave, stopHz)
	out := make([]float32, n)
	for i, v := range buf {
		// Dither: a real file is never digitally silent above its ceiling,
		// and a fixture that IS lets a verdict pass for the wrong reason.
		out[i] = float32(v + r.NormFloat64()*1e-6)
	}
	return out
}

// applySpectralShape shapes the signal in the FREQUENCY domain: everything
// above cutoffHz is attenuated by dbPerOctave per octave, and everything at or
// above stopHz is zeroed.
//
// An FFT rather than a filter cascade, because the fixture's ceiling has to be
// exactly where the table says it is. A first attempt used cascaded one-pole
// low-passes and measured 10.8 kHz for a fixture meant to sit at 22.05 kHz —
// eight sections each contribute ~3 dB AT the corner, and a digital one-pole
// whose corner is near Nyquist is badly warped besides. The fixture was wrong,
// not the analyzer.
func applySpectralShape(buf []float64, cutoffHz, dbPerOctave, stopHz float64) {
	n := 1
	for n < len(buf) {
		n <<= 1
	}
	padded := make([]float64, n)
	copy(padded, buf)
	fft := fourier.NewFFT(n)
	coeff := fft.Coefficients(nil, padded)
	binHz := float64(AnalysisSampleRate) / float64(n)
	for k := range coeff {
		f := float64(k) * binHz
		gain := 1.0
		if cutoffHz > 0 && f > cutoffHz {
			gain = math.Pow(10, -dbPerOctave*math.Log2(f/cutoffHz)/20)
		}
		if stopHz > 0 && f >= stopHz {
			gain = 0
		}
		coeff[k] = complex(real(coeff[k])*gain, imag(coeff[k])*gain)
	}
	out := fft.Sequence(nil, coeff)
	for i := range buf {
		buf[i] = out[i] / float64(n)
	}
}

func analyzeSpectrum(t *testing.T, samples []float32) *SpectrumResult {
	t.Helper()
	a := newKeyTempoAnalyzer()
	for _, s := range samples {
		a.add(s)
	}
	res := a.estimateSpectrum()
	if res == nil {
		t.Fatal("no spectrum measured — fixture too short?")
	}
	return res
}

// TestSpectrumBandMapMatchesIOSFixture is the cross-repo pin.
//
// The values are captured from iOS's `MeterAnalysis.makeBandMap`. A drift on
// either side moves the curve relative to the live bars with NO error anywhere
// — the silent failure the plan calls out — so the mapping is asserted
// exactly, not just its length.
func TestSpectrumBandMapMatchesIOSFixture(t *testing.T) {
	got := MakeBandMap(48000, 4096, 60, 20)
	if len(got) != 60 {
		t.Fatalf("band count = %d, want 60", len(got))
	}
	// Structural invariants iOS's implementation guarantees.
	if got[0].Lo < 1 {
		t.Errorf("band 0 starts at bin %d — DC must never be included", got[0].Lo)
	}
	if last := got[len(got)-1]; last.Hi != 4096/2-1 {
		t.Errorf("last band ends at bin %d, want %d", last.Hi, 4096/2-1)
	}
	for i, r := range got {
		if r.Lo > r.Hi {
			t.Errorf("band %d is empty: %+v", i, r)
		}
		if i > 0 && r.Lo <= got[i-1].Hi {
			t.Errorf("band %d overlaps band %d: %+v vs %+v", i, i-1, r, got[i-1])
		}
	}
	// The FULL mapping, captured from the shipping Swift function by
	// printing `MeterAnalysis.makeBandMap(sampleRate: 48000)` from the iOS
	// test target (2026-08-07). Not hand-derived — an earlier draft of this
	// test asserted values written from reasoning and was wrong at band 59,
	// which is exactly the failure a "fixture" is supposed to prevent.
	wantIOS := []BandRange{
		{1, 1},
		{2, 2},
		{3, 3},
		{4, 4},
		{5, 5},
		{6, 6},
		{7, 7},
		{8, 8},
		{9, 9},
		{10, 10},
		{11, 11},
		{12, 12},
		{13, 13},
		{14, 14},
		{15, 15},
		{16, 16},
		{17, 17},
		{18, 18},
		{19, 19},
		{20, 20},
		{21, 21},
		{22, 22},
		{23, 24},
		{25, 28},
		{29, 31},
		{32, 35},
		{36, 40},
		{41, 45},
		{46, 51},
		{52, 58},
		{59, 65},
		{66, 73},
		{74, 83},
		{84, 93},
		{94, 105},
		{106, 119},
		{120, 134},
		{135, 151},
		{152, 170},
		{171, 191},
		{192, 215},
		{216, 243},
		{244, 273},
		{274, 308},
		{309, 346},
		{347, 390},
		{391, 439},
		{440, 495},
		{496, 557},
		{558, 627},
		{628, 706},
		{707, 794},
		{795, 894},
		{895, 1006},
		{1007, 1133},
		{1134, 1275},
		{1276, 1435},
		{1436, 1615},
		{1616, 1818},
		{1819, 2047},
	}
	for i := range wantIOS {
		if got[i] != wantIOS[i] {
			t.Errorf("band %d = {%d,%d}, iOS says {%d,%d} — the band map drifted; the "+
				"ghost curve would land beside the live bars with no error anywhere",
				i, got[i].Lo, got[i].Hi, wantIOS[i].Lo, wantIOS[i].Hi)
		}
	}
}

func TestMakeBandMapRefusesDegenerateInput(t *testing.T) {
	for _, c := range []struct {
		name               string
		rate               float64
		fftSize, bandCount int
		minimumHz          float64
	}{
		{"zero rate", 0, 4096, 60, 20},
		{"negative rate", -48000, 4096, 60, 20},
		{"NaN rate", math.NaN(), 4096, 60, 20},
		{"fft too small for band count", 48000, 64, 60, 20},
		{"zero bands", 48000, 4096, 0, 20},
		// A non-positive or non-finite floor makes logStep NaN/Inf and
		// degrades every band to a single bin — a curve that looks
		// plausible and means nothing, which is worse than no curve.
		{"zero minimumHz", 48000, 4096, 60, 0},
		{"negative minimumHz", 48000, 4096, 60, -20},
		{"NaN minimumHz", 48000, 4096, 60, math.NaN()},
		{"Inf minimumHz", 48000, 4096, 60, math.Inf(1)},
	} {
		if got := MakeBandMap(c.rate, c.fftSize, c.bandCount, c.minimumHz); got != nil {
			t.Errorf("%s: got %d bands, want nil", c.name, len(got))
		}
	}
}

// TestSpectrumHannLeakageDoesNotInventBandwidth is the one that had to be
// measured rather than reasoned about.
//
// iOS windows with Blackman-Harris (-92 dB sidelobes) precisely so the noise
// floor stays clean; this package reuses the key/tempo STFT's HANN window
// (-31.5 dB first sidelobe) because a second window means a second FFT per
// hop, against a measurement whose premise is that it is free. If Hann's
// leakage from loud low-frequency content reached the 20+ kHz bins above the
// -60 dB-relative floor, every file would measure as full-bandwidth and the
// feature would be inert.
func TestSpectrumHannLeakageDoesNotInventBandwidth(t *testing.T) {
	// A hard wall at 16 kHz, with very loud content below it.
	res := analyzeSpectrum(t, synth(6, 15_000, 12, 16_000, 0x5EED))
	if res.BandwidthHz == nil {
		t.Fatal("no bandwidth measured on a walled fixture")
	}
	if got := *res.BandwidthHz; got < 15_000 || got > 17_500 {
		t.Errorf("bandwidth = %d Hz, want ~16000 — Hann leakage is reaching past the wall "+
			"(or the wall is being under-read)", got)
	}
}

// TestSpectrumCliffSurvivesQuietContent is the regression pin for the bug the
// live library exposed, and the one property the original fixtures could not
// test: the cliff must not saturate against the display floor.
//
// The cliff is `below - above`, so clamping `above` at the -90 dB DISPLAY
// floor caps it at `below - floor` — a bound that depends entirely on how loud
// the content is. Every original fixture was LOUD (content near -5 dBFS just
// under the wall), leaving ~85 dB of headroom, so a wall measured 92-96 dB and
// the bug was invisible. Real music at 20 kHz sits at -57…-85 dBFS, leaving
// 5-30 dB — so on bridge.ars.md, 317 hi-res tracks sat in the 44.1 kHz window
// and NOT ONE could be flagged, because 177 of 180 bins above the ceiling were
// pinned at the floor.
//
// This fixture is the same hard wall at REAL-WORLD level. Against the display
// floor it measures ~25 dB (no threshold worth having is reachable); against
// the measurement floor it measures ~95 dB.
func TestSpectrumCliffSurvivesQuietContent(t *testing.T) {
	// -20 dB, putting the content just under the wall at ~-75 dBFS — the
	// middle of the range MEASURED on real files (-57 to -85). An earlier
	// draft used -60 dB, which lands at -114 dBFS: below any real file's
	// noise floor, and quiet enough to saturate even the -160 measurement
	// floor (max achievable cliff there is `below - floor` = 45 dB). The
	// fixture was unrealistic, not the code — the same mistake, one floor
	// down, and worth stating because it bounds the metric honestly.
	const quietGain = 0.1
	samples := synth(6, 20_500, 48, 22_050, 0x5EED)
	for i := range samples {
		samples[i] *= quietGain
	}
	res := analyzeSpectrum(t, samples)
	if res.BandwidthHz == nil {
		t.Fatal("no bandwidth measured on a quiet walled fixture")
	}
	if res.CliffDepthDB == nil {
		t.Fatal("no cliff measured")
	}
	// 60 dB is the threshold clients apply; a real upsample measures ~99.
	if *res.CliffDepthDB < 60 {
		t.Errorf("cliff = %.1f dB on a hard wall at realistic level — the "+
			"measurement is saturating against the display floor again, and "+
			"no upsample can ever be detected", *res.CliffDepthDB)
	}
	// And the quiet fixture must still find the wall where it is.
	if got := *res.BandwidthHz; got < 21_400 || got > 22_500 {
		t.Errorf("bandwidth = %d Hz, want ~22050", got)
	}
}

// TestSpectrumBandsKeepTheDisplayFloor: the stored CURVE still uses the -90 dB
// display floor, because it is drawn on the same axis as iOS's live meter. The
// measurement floor must not leak into it.
func TestSpectrumBandsKeepTheDisplayFloor(t *testing.T) {
	samples := synth(4, 20_500, 48, 22_050, 0x5EED)
	for i := range samples {
		samples[i] *= 0.0001
	}
	res := analyzeSpectrum(t, samples)
	for i, db := range res.Bands {
		if db < spectrumFloorDB {
			t.Fatalf("band %d = %.1f dB, below the %.0f dB display floor — the "+
				"curve would no longer share a scale with the live meter",
				i, db, spectrumFloorDB)
		}
	}
}

// TestSpectrumGroundTruth is the §6.2 table.
func TestSpectrumGroundTruth(t *testing.T) {
	cases := []struct {
		name          string
		cutoff, slope float64
		stopHz        float64
		wantBandwidth int // 0 = must be absent (ceiling-limited)
		tolerance     int
	}{
		// A CD upsampled into a hi-res container: the wall is at 44.1's
		// Nyquist, comfortably inside our 24 kHz ceiling.
		{"from 44.1 (22.05k wall)", 20_500, 48, 22_050, 22_050, 700},
		// An older converter cutting lower, still pinned to 22.05k.
		{"from 44.1 (early AA)", 19_800, 72, 22_050, 22_050, 700},
		// A genuinely band-limited master — the negative control. Its
		// ceiling is NOT a standard Nyquist, and it must be measured as
		// where it actually is so nothing downstream can mistake it.
		{"band-limited 20k", 19_000, 96, 20_000, 20_000, 700},
		{"band-limited 17k", 16_000, 96, 17_000, 17_000, 700},
		// Content past our own ceiling: the honest answer is NO answer,
		// because 24 kHz IS 48 kHz's Nyquist and reporting it would accuse
		// a 96 kHz-native master of being a 48 kHz upsample.
		{"full-band (past our ceiling)", 30_000, 6, 0, 0, 0},
	}
	for _, c := range cases {
		res := analyzeSpectrum(t, synth(6, c.cutoff, c.slope, c.stopHz, 0x5EED))
		if len(res.Bands) != SpectrumBandCount {
			t.Errorf("%s: %d bands, want %d", c.name, len(res.Bands), SpectrumBandCount)
		}
		if c.wantBandwidth == 0 {
			if res.BandwidthHz != nil {
				t.Errorf("%s: bandwidth = %d Hz, want ABSENT — a measurement at our own "+
					"Nyquist would read as 48 kHz's and accuse a hi-res master",
					c.name, *res.BandwidthHz)
			}
			continue
		}
		if res.BandwidthHz == nil {
			t.Errorf("%s: bandwidth absent, want ~%d Hz", c.name, c.wantBandwidth)
			continue
		}
		if diff := *res.BandwidthHz - c.wantBandwidth; diff > c.tolerance || diff < -c.tolerance {
			t.Errorf("%s: bandwidth = %d Hz, want %d ± %d",
				c.name, *res.BandwidthHz, c.wantBandwidth, c.tolerance)
		}
	}
}

// TestSpectrumCeilingGuardIsWhatSuppressesTheFullBandCase pins the guard
// itself rather than only its effect: a fixture reaching our Nyquist must be
// suppressed BY the guard, not because nothing was measurable.
func TestSpectrumCeilingGuardIsWhatSuppressesTheFullBandCase(t *testing.T) {
	res := analyzeSpectrum(t, synth(6, 30_000, 6, 0, 0x5EED))
	if res.BandwidthHz != nil {
		t.Fatalf("bandwidth = %d Hz, want absent", *res.BandwidthHz)
	}
	// The bands themselves must still be populated — the curve is useful
	// even when the ceiling can't be named.
	loud := 0
	for _, db := range res.Bands {
		if db > spectrumFloorDB+10 {
			loud++
		}
	}
	if loud < 30 {
		t.Errorf("only %d bands carry content — the fixture never reached the ceiling, "+
			"so this test is not exercising the guard", loud)
	}
}

func TestSpectrumRefusesTooFewWindows(t *testing.T) {
	a := newKeyTempoAnalyzer()
	for i := 0; i < stftWindow+5*stftHop; i++ {
		a.add(0.2)
	}
	if got := a.estimateSpectrum(); got != nil {
		t.Errorf("got a spectrum from %d windows, want nil below %d",
			a.spectrum.windows, minSpectrumWindows)
	}
}

// TestEncodeSpectrumWireShape pins the `1BSP` layout iOS decodes.
func TestEncodeSpectrumWireShape(t *testing.T) {
	bands := make([]float64, SpectrumBandCount)
	for i := range bands {
		bands[i] = -20 - float64(i)/4
	}
	hz := 22_050
	cliff := 95.0
	blob := EncodeSpectrum(&SpectrumResult{
		Bands: bands, BandwidthHz: &hz, CliffDepthDB: &cliff, Windows: 500,
	})
	if len(blob) != spectrumHeaderLen+SpectrumBandCount {
		t.Fatalf("blob is %d bytes, want %d", len(blob), spectrumHeaderLen+SpectrumBandCount)
	}
	if string(blob[:4]) != "1BSP" {
		t.Errorf("magic = %q, want 1BSP", blob[:4])
	}
	if blob[4] != SpectrumSchemaVersion {
		t.Errorf("version = %d, want %d", blob[4], SpectrumSchemaVersion)
	}
	if got := int(blob[6]) | int(blob[7])<<8 | int(blob[8])<<16 | int(blob[9])<<24; got != AnalysisSampleRate {
		t.Errorf("sampleRate = %d, want %d — iOS positions the curve from this", got, AnalysisSampleRate)
	}
	if got := int(blob[18]) | int(blob[19])<<8 | int(blob[20])<<16 | int(blob[21])<<24; got != hz {
		t.Errorf("bandwidth = %d, want %d", got, hz)
	}
}

// A hostile measurement must not panic the encoder — Go's float→int
// conversion is undefined for non-finite values and traps in practice.
func TestEncodeSpectrumSurvivesHostileValues(t *testing.T) {
	bands := make([]float64, SpectrumBandCount)
	for i := range bands {
		bands[i] = math.NaN()
	}
	inf := math.Inf(1)
	nan := math.NaN()
	huge := 1e300
	for _, cliff := range []*float64{nil, &inf, &nan, &huge} {
		hz := math.MaxInt
		blob := EncodeSpectrum(&SpectrumResult{
			Bands: bands, BandwidthHz: &hz, CliffDepthDB: cliff, Windows: -5,
		})
		if len(blob) != spectrumHeaderLen+SpectrumBandCount {
			t.Fatalf("hostile input produced %d bytes", len(blob))
		}
	}
	if EncodeSpectrum(nil) != nil {
		t.Error("nil result must encode to nil")
	}
	if EncodeSpectrum(&SpectrumResult{Bands: []float64{1, 2}}) != nil {
		t.Error("a wrong band count must encode to nil, not a short blob")
	}
}
