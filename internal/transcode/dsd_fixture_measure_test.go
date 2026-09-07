package transcode

import (
	"context"
	"encoding/binary"
	"math"
	"math/cmplx"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/analyze"
)

// The alias / parity measurement, against COMMITTED 5th-order fixtures.
//
// These answer the one question the design could not settle by reading:
// the decode chain decimates twice — ffmpeg's dsd2pcm from 2.8 MHz to
// 352.8 kHz, then sox to the tier's rate — and DSD's noise shelf carries
// essentially all of a 1-bit stream's power. If either stopband is not
// deep enough the shelf folds into the baseband and every rendition
// carries a noise floor the source never had.
//
// WHAT THE MEASUREMENTS FOUND (2026-09-07, ffmpeg 8.1 / sox 14.4.2):
//
//   - The shipping sox order folds NOTHING the filter-first reference
//     does not: +0.00 dB at the faithful tier, +0.38 dB peak / +0.46 dB
//     band energy at the compact tier. The design's recorded fallback
//     ("sinc BEFORE rate at 352.8k") is therefore not needed.
//   - What dsd2pcm hands sox is already band-limited: at the 352.8 kHz
//     intermediate the residual shelf peaks near -64 dBFS per bin with
//     about -38 dB of band energy in 100-130 kHz. sox's `rate -v`
//     stopband is deeper than 100 dB, so the folded product sits far
//     below the fixture's own -104 dBFS in-band content — which is
//     exactly what the differential observes.
//   - The finishing low-pass delivers -116.8 dB at 40 kHz, and sox's
//     `-t` is the FULL transition width centred on the cutoff. See
//     TestDSDLowpassSincConvention.
//   - The clip guard lands the rendition's true peak at -0.97 dBTP
//     against its -1.0 target. See TestDSDRender_LevelParity.
//
// Gated on BRIDGE_DSD_FIXTURE_TESTS=1 as well as the toolchain: they
// decode a megabyte of DSD through two subprocesses per case, which is
// not something the ordinary suite should pay for on every run.
func requireFixtureMeasurement(t *testing.T) {
	t.Helper()
	if os.Getenv("BRIDGE_DSD_FIXTURE_TESTS") != "1" {
		t.Skip("set BRIDGE_DSD_FIXTURE_TESTS=1 to run the DSD spectral measurements")
	}
	requireDSDToolchain(t)
}

// renderFixture runs the SHIPPING chain over a committed fixture and
// returns the sidecar path. Deliberately `Run`, not a hand-assembled
// pipeline: a measurement of something adjacent to the shipped code
// answers a question nobody asked.
func renderFixture(t *testing.T, fixture string, kind JobKind, rate, bits int) (string, RunResult) {
	t.Helper()
	root := t.TempDir()
	lib := filepath.Join(root, "lib", "Album")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(lib, filepath.Base(fixture))
	raw, err := os.ReadFile(filepath.Join("testdata", fixture))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	if err := os.WriteFile(src, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	outDir := filepath.Join(root, "variants")
	spec := JobSpec{
		SourceAbsPath: src, SourceLibraryRel: "Album/" + filepath.Base(fixture),
		SourceSampleRate: 2822400, SourceIsDSD: true, SourceChannels: 2, SourceDurationSec: 1.0,
		TargetSampleRate: rate, TargetBits: bits, Quality: QualityVeryHigh,
		OutputDir: outDir, TempDir: filepath.Join(root, "tmp"), Kind: kind,
	}
	if err := spec.FreshnessFromFile(); err != nil {
		t.Fatal(err)
	}
	res, err := Run(context.Background(), spec)
	if err != nil {
		t.Fatalf("render %s: %v", fixture, err)
	}
	if res.SizeBytes <= 0 {
		t.Fatalf("render %s produced %d bytes", fixture, res.SizeBytes)
	}
	return spec.SidecarPath(), res
}

// stageAEffects returns the effect argv the SHIPPING Stage A applies for a
// tier, taken from the production builder rather than retyped: everything
// after the scratch path is the effect chain.
func stageAEffects(kind JobKind, targetRate int) []string {
	spec := JobSpec{TargetSampleRate: targetRate, Quality: QualityVeryHigh, Kind: kind}
	const scratch = "/scratch.sox"
	args := spec.dsdStageAArgs(sourceGeometry{SampleRate: 352800, Channels: 2}, "/tmp", scratch)
	for i, a := range args {
		if a == scratch {
			return args[i+1:]
		}
	}
	return nil
}

// runStageAVariant decodes a fixture through ffmpeg and applies a chosen
// sox effect chain, returning mono float64 samples at the target rate.
//
// Stage A is where an alias would be created — Stage C only applies gain,
// dither and FLAC, none of which can fold a frequency. Measuring Stage A
// keeps the shipping and reference runs at IDENTICAL level (both carry the
// pre-attenuation and neither carries the clip-guarded gain), so their
// spectra subtract directly with no normalisation step to get wrong.
func runStageAVariant(t *testing.T, fixture string, effects []string, targetRate int) []float64 {
	t.Helper()
	src := filepath.Join("testdata", fixture)
	out := filepath.Join(t.TempDir(), "stagea.f64")
	args := []string{"-t", "raw", "-e", "float", "-b", "32", "-L", "-r", "352800", "-c", "2", "-",
		"-t", "raw", "-e", "float", "-b", "64", "-L", "-c", "1", out, "remix", "1"}
	args = append(args, effects...)

	ff := exec.Command("ffmpeg", ffmpegDSDDecodeArgs(src)...)
	sx := exec.Command("sox", args...)
	pipe, err := ff.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	sx.Stdin = pipe
	if err := sx.Start(); err != nil {
		t.Fatalf("sox start: %v", err)
	}
	if err := ff.Run(); err != nil {
		t.Fatalf("ffmpeg %v: %v", ffmpegDSDDecodeArgs(src), err)
	}
	if err := sx.Wait(); err != nil {
		t.Fatalf("sox %v: %v", args, err)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	// A length that is not a whole number of float64s means sox was cut
	// off mid-sample; the loop below would silently drop the remainder and
	// measure a file nobody wrote.
	if len(raw) == 0 || len(raw)%8 != 0 {
		t.Fatalf("stage A wrote %d bytes, not a non-zero multiple of 8 — sox was truncated", len(raw))
	}
	xs := make([]float64, len(raw)/8)
	for i := range xs {
		xs[i] = math.Float64frombits(binary.LittleEndian.Uint64(raw[i*8:]))
	}
	if len(xs) < targetRate/2 {
		t.Fatalf("stage A produced %d samples at %d Hz — expected about a second", len(xs), targetRate)
	}
	return xs
}

// decodeMono reads a FLAC into mono float64 samples via sox, at whatever
// rate the file carries.
func decodeMono(t *testing.T, path string) []float64 {
	t.Helper()
	out, err := exec.Command("sox", path, "-t", "raw", "-e", "float", "-b", "64", "-c", "1", "-").Output()
	if err != nil {
		t.Fatalf("sox decode %s: %v", path, err)
	}
	if len(out) == 0 || len(out)%8 != 0 {
		t.Fatalf("sox decode of %s wrote %d bytes, not a non-zero multiple of 8", path, len(out))
	}
	xs := make([]float64, len(out)/8)
	for i := range xs {
		xs[i] = math.Float64frombits(binary.LittleEndian.Uint64(out[i*8:]))
	}
	return xs
}

// fft is an in-place iterative radix-2 Cooley-Tukey. n must be a power of
// two. Small enough to keep in the test rather than take a dependency for
// three assertions.
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
		ang := -2 * math.Pi / float64(length)
		wl := cmplx.Exp(complex(0, ang))
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

// fftSizeFor picks the largest power of two that fits in the samples a
// render actually produced, capped at 65536.
//
// The cap is the resolution the measurement wants; the floor is what the
// compact tier can supply. A 1-second DSD64 fixture decimates to 44 100
// samples at 44.1 kHz, which is BELOW 65536 — so a fixed 65536-point FFT
// fatals on exactly the tier that decimates hardest and is therefore the
// most interesting one to measure. Lengthening the fixtures to 2 s would
// also work and costs 1.4 MB of committed binary per fixture; halving the
// bin count costs 1.35 Hz of resolution at 44.1 kHz, which is far finer
// than any spur this set is hunting. Callers log the size they got, so a
// reduced-resolution run says so in its own output rather than quietly
// measuring something narrower than the reader assumes.
func fftSizeFor(available int) int {
	n := 1
	for n*2 <= available && n*2 <= 65536 {
		n *= 2
	}
	return n
}

// spectrumDBFS returns a Blackman-Harris-windowed magnitude spectrum in
// dBFS, one bin per index, plus the bin width in Hz. Blackman-Harris (not
// Hann) because the question is what sits at -110 dBFS, and Hann's
// -31.5 dB sidelobes would smear a full-scale tone across the very floor
// being measured.
func spectrumDBFS(t *testing.T, xs []float64, rate int, n int) ([]float64, float64) {
	t.Helper()
	if len(xs) < n {
		t.Fatalf("only %d samples, need %d for the FFT", len(xs), n)
	}
	if n < 2 {
		// The window divides by n-1.
		t.Fatalf("FFT size %d is too small to window", n)
	}
	// Skip the first 20% — the decimation filters have a transient, and a
	// spur measured inside it belongs to the filter warming up, not the
	// steady-state stopband.
	off := len(xs) / 5
	if off+n > len(xs) {
		off = len(xs) - n
	}
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
	// Coherent gain normalisation: a full-scale sine reads 0 dBFS.
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
	return mags, float64(rate) / float64(n)
}

func peakInBand(mags []float64, binHz, loHz, hiHz float64) (peakDB float64, peakHz float64) {
	peakDB = -300
	for i, v := range mags {
		f := float64(i) * binHz
		if f < loHz || f > hiHz {
			continue
		}
		if v > peakDB {
			peakDB, peakHz = v, f
		}
	}
	return
}

// bandEnergyDB sums the power of every bin in a band. A single spur moves
// the peak; a broadband lift — which is what a folded noise shelf looks
// like — moves this and barely moves the peak, so the alias measurement
// asserts on both.
func bandEnergyDB(mags []float64, binHz, loHz, hiHz float64) float64 {
	var sum float64
	for i, v := range mags {
		f := float64(i) * binHz
		if i == 0 || f < loHz || f > hiHz {
			continue
		}
		sum += math.Pow(10, v/10)
	}
	if sum <= 0 {
		return -300
	}
	return 10 * math.Log10(sum)
}

// soxSteadyStateRMSdB synthesises a sine at the DSD intermediate rate,
// runs it through a sox effect chain, and returns the RMS of the MIDDLE
// HALF of the result.
//
// The middle half is not fastidiousness — it is the difference between a
// right answer and one that is 61 dB wrong. `sox … stats` over a short
// file reports the RMS of the whole thing INCLUDING the FIR's edge
// transient, and for a 254-tap filter killing a 40 kHz tone that
// transient dominates completely: whole-file -64.56 dBFS against a
// steady-state -125.85 dBFS, measured 2026-09-07. A sweep built on
// `stats` therefore reads a 117 dB filter as a 55 dB one and looks like
// a broken stopband.
//
// Note `-r` must precede `-n`: sox binds options to the file that FOLLOWS
// them, so `sox -n -r 352800 …` sets the OUTPUT rate and leaves `synth`
// generating at sox's 48 kHz default. An 80 kHz request then aliases to
// 16 kHz and every stopband frequency silently lands in the passband.
func soxSteadyStateRMSdB(t *testing.T, freqHz int, effects []string) float64 {
	t.Helper()
	dir := t.TempDir()
	tone := filepath.Join(dir, "tone.f32")
	out := filepath.Join(dir, "out.f64")

	gen := exec.Command("sox", "-r", "352800", "-n", "-c", "1",
		"-t", "raw", "-e", "float", "-b", "32", "-L", tone,
		"synth", "0.5", "sine", strconv.Itoa(freqHz), "gain", "-6")
	if outp, err := gen.CombinedOutput(); err != nil {
		t.Fatalf("sox synth %d Hz: %v: %s", freqHz, err, outp)
	}

	args := []string{"-t", "raw", "-e", "float", "-b", "32", "-L", "-r", "352800", "-c", "1", tone,
		"-t", "raw", "-e", "float", "-b", "64", "-L", out}
	args = append(args, effects...)
	if outp, err := exec.Command("sox", args...).CombinedOutput(); err != nil {
		t.Fatalf("sox %v: %v: %s", args, err, outp)
	}

	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) == 0 || len(raw)%8 != 0 {
		t.Fatalf("sox wrote %d bytes, not a non-zero multiple of 8 — the filtered tone is truncated", len(raw))
	}
	n := len(raw) / 8
	if n < 1000 {
		t.Fatalf("filtered tone is only %d samples", n)
	}
	var sum float64
	lo, hi := n/4, 3*n/4
	for i := lo; i < hi; i++ {
		v := math.Float64frombits(binary.LittleEndian.Uint64(raw[i*8:]))
		sum += v * v
	}
	mean := sum / float64(hi-lo)
	if mean <= 0 {
		return -300
	}
	return 10 * math.Log10(mean)
}

// TestDSDLowpassSincConvention pins the two things about the faithful
// tier's finishing low-pass that the argv does not state on its face.
//
// FIRST, sox's `-t` convention. `sinc -a 110 -t 10000 -35000` could mean
// a 10 kHz transition ENDING at 35 kHz, STARTING at it, or centred on it,
// and the three differ by a 10 kHz shift in what survives. Measured: the
// response is -6.02 dB at exactly 35 kHz and flat to 31 kHz, so `-t` is
// the FULL transition width CENTRED on the cutoff — 30 kHz passband edge,
// 40 kHz stop edge, which is what the design intends and what the
// docblock on dsdLowpassArgs claims.
//
// SECOND, the stopband sox actually delivers, which is the number the
// design leans on when it says the DSD shelf cannot reach the output:
// -116.8 dB at 40 kHz, comfortably past the 110 dB `-a` asks for.
//
// Measured 2026-09-07 (sox 14.4.2), relative to the unfiltered tone:
//
//	 1 kHz  +0.00    30 kHz  +0.00    35 kHz   -6.02
//	20 kHz  +0.00    31 kHz  -0.01    40 kHz -116.84
func TestDSDLowpassSincConvention(t *testing.T) {
	// sox only: this synthesises and filters with sox and never decodes
	// DSD, so requiring ffmpeg would skip the convention pin on a host
	// that can perfectly well run it.
	requireSox(t)
	ref := soxSteadyStateRMSdB(t, 1000, nil)
	for _, tc := range []struct {
		freq   int
		wantLo float64 // inclusive bounds on the level RELATIVE to unfiltered
		wantHi float64
		what   string
	}{
		{1000, -0.1, 0.1, "passband"},
		{20000, -0.1, 0.1, "passband"},
		{30000, -0.1, 0.1, "passband edge — the audio band and the DSD shelf's foot both sit below here"},
		{35000, -6.4, -5.7, "the cutoff: -6 dB here is what makes -t the FULL width, centred"},
		{40000, -300, -100, "stop edge"},
		{60000, -300, -100, "deep stopband"},
	} {
		got := soxSteadyStateRMSdB(t, tc.freq, dsdLowpassArgs) - ref
		t.Logf("%6d Hz: %+8.2f dB relative to unfiltered (%s)", tc.freq, got, tc.what)
		if got < tc.wantLo || got > tc.wantHi {
			t.Errorf("%d Hz: %+.2f dB relative, want [%.2f, %.2f] — %s",
				tc.freq, got, tc.wantLo, tc.wantHi, tc.what)
		}
	}
}

// TestDSDRender_AliasRejection is THE measurement this fixture set exists
// for: DSD's noise shelf above 50 kHz carries essentially all of a 1-bit
// stream's power, so if the decimation's stopband is not deep enough the
// shelf folds into the audio band and every rendition carries a floor the
// source never had.
//
// It is a DIFFERENTIAL, and that is the whole design. An absolute bar was
// tried first and could not answer the question: the 5th-order fixture's
// OWN in-band content near 20 kHz sits around -104 dBFS (odd-order
// modulator distortion and the NTF's rise, both properties of the source,
// both measured), which is above any bar a folded shelf would need to
// clear. Asserting an absolute floor therefore measures the fixture.
//
// So each tier is rendered twice through the SAME Stage A: once with the
// shipping effect order (taken from the production argv builder), and once
// with a reference order that low-passes at the 352.8 kHz intermediate
// BEFORE decimating — the "sinc before rate" fallback the design records.
// A reference that filters while massively oversampled cannot alias, so
// any in-band energy the shipping order has AND the reference does not is
// energy the shipping decimation folded down. Whatever both share is the
// fixture's own, and subtracts out.
//
// Measured 2026-09-07 (ffmpeg 8.1, sox 14.4.2), the numbers this pins:
//
//	faithful 176.4k  ship -103.81 dBFS / ref -103.80  → peak +0.01 dB, energy +0.00 dB
//	compact   44.1k  ship -105.01 dBFS / ref -105.39  → peak +0.38 dB, energy +0.45 dB
//
// i.e. the shipping order folds nothing the reference does not, and the
// recorded "sinc before rate" fallback is not needed. The 1.0 dB bar is
// about twice the worst measured difference.
func TestDSDRender_AliasRejection(t *testing.T) {
	requireFixtureMeasurement(t)
	for _, tc := range []struct {
		name string
		kind JobKind
		rate int
		// reference: low-pass at the intermediate rate, THEN decimate. The
		// compact tier's shipping chain carries no low-pass at all (`rate`'s
		// own filter is the anti-alias), so its reference adds one at the
		// audio band edge.
		reference []string
	}{
		{"faithful 176.4k", JobKindPCMRender, 176400,
			append(append([]string{}, dsdLowpassArgs...), "rate", "-v", "-L", "176400")},
		{"compact 44.1k", JobKindOptimize, 44100,
			[]string{"sinc", "-a", "110", "-t", "2000", "-20000", "rate", "-v", "-L", "44100"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ship := runStageAVariant(t, "dsd64-50khz-alias.dsf", stageAEffects(tc.kind, tc.rate), tc.rate)
			ref := runStageAVariant(t, "dsd64-50khz-alias.dsf", tc.reference, tc.rate)

			n := fftSizeFor(len(ship))
			if m := fftSizeFor(len(ref)); m < n {
				n = m
			}
			shipMags, binHz := spectrumDBFS(t, ship, tc.rate, n)
			refMags, _ := spectrumDBFS(t, ref, tc.rate, n)

			shipPk, shipHz := peakInBand(shipMags, binHz, 20, 20000)
			refPk, _ := peakInBand(refMags, binHz, 20, 20000)
			shipE := bandEnergyDB(shipMags, binHz, 20, 20000)
			refE := bandEnergyDB(refMags, binHz, 20, 20000)

			t.Logf("%s: shipping peak %.2f dBFS @ %.0f Hz, energy %.2f dB | reference peak %.2f dBFS, energy %.2f dB | delta peak %+.2f dB, energy %+.2f dB (%d-point FFT, bin %.2f Hz)",
				tc.name, shipPk, shipHz, shipE, refPk, refE, shipPk-refPk, shipE-refE, n, binHz)

			if d := shipPk - refPk; d > 1.0 {
				t.Errorf("in-band peak is %+.2f dB above the filter-first reference (%.2f vs %.2f dBFS) — the shipping decimation is folding the 50 kHz tone down",
					d, shipPk, refPk)
			}
			if d := shipE - refE; d > 1.0 {
				t.Errorf("in-band energy is %+.2f dB above the filter-first reference (%.2f vs %.2f dB) — a broadband fold, which is what an inadequate stopband looks like",
					d, shipE, refE)
			}
			// A coarse absolute pin so a filter that vanished entirely is
			// caught even if the reference somehow lost it too: with no
			// anti-alias at all the 50 kHz tone lands in band near -12 dBFS.
			if shipPk > -80 {
				t.Errorf("in-band peak %.2f dBFS at %.0f Hz — far above the fixture's own ~-104 dBFS content; the anti-alias filter looks absent",
					shipPk, shipHz)
			}
		})
	}
}

// TestDSDRender_LevelParity pins what the clip guard actually promises,
// end to end: after Stage C the rendition's TRUE peak sits at the -1 dBTP
// ceiling, because G is defined as `clamp(0, 6, -TP_unity - 1)` and Stage
// C applies `6.0206 + G` to a scratch that sits exactly 6.0206 dB below
// unity. Those two arithmetic halves have to agree or the level is wrong
// by a constant, which is the failure an earlier draft of Stage C shipped
// (it wrote `6.0206 + G - 6`, leaving every rendition 6 dB quiet).
//
// Measuring the true peak rather than the spectrum is deliberate: the
// spectrum reads the TONE, and the guard is computed from the SIGNAL,
// which for a 1-bit source carries residual shelf on top of the tone. The
// two differ by about 0.8 dB on this fixture — enough that a tone-based
// bar would have to be loose enough to hide a real error.
//
// Measured 2026-09-07: applied gain +5.00 dB, rendered true peak
// -0.97 dBTP, 1 kHz tone -1.78 dBFS. The unity tone peaks at -6.02 dBFS,
// so +5.00 puts it at -1.02 — the spectrum reads 0.76 dB under that
// because 1 kHz does not land on a bin and Blackman-Harris costs up to
// 0.83 dB of scalloping. That artefact is exactly why the ASSERTION is on
// the true peak and the tone is only logged.
func TestDSDRender_LevelParity(t *testing.T) {
	requireFixtureMeasurement(t)
	out, res := renderFixture(t, "dsd64-1khz-m6dbfs.dsf", JobKindPCMRender, 176400, 24)

	if res.AppliedGainDB == nil {
		t.Fatal("render reported no applied gain — a DSD rendition must always record one")
	}
	renderedTP, ok, err := analyze.TruePeakDBTP(context.Background(), out, 2)
	if err != nil || !ok {
		t.Fatalf("true peak of the rendition: ok=%v err=%v", ok, err)
	}

	xs := decodeMono(t, out)
	mags, binHz := spectrumDBFS(t, xs, 176400, fftSizeFor(len(xs)))
	tonePk, toneHz := peakInBand(mags, binHz, 500, 1500)
	// The worst thing in 3-20 kHz is the fixture's own 19th harmonic (the
	// modulator's odd-order distortion, ~-84 dBFS after the render's gain),
	// NOT anything the chain introduced — the alias differential above is
	// what separates those. Logged so the number is on the record; not
	// asserted, because it is a property of the fixture.
	residue, residueHz := peakInBand(mags, binHz, 3000, 20000)

	t.Logf("level parity: applied gain %+.2f dB, rendered true peak %.2f dBTP, 1 kHz tone %.2f dBFS @ %.0f Hz, worst 3-20 kHz source residue %.1f dBFS @ %.0f Hz",
		*res.AppliedGainDB, renderedTP, tonePk, toneHz, residue, residueHz)

	if *res.AppliedGainDB <= 0 || *res.AppliedGainDB > 6 {
		t.Errorf("applied gain %+.2f dB is outside the clamp — a -6 dBFS source should take most of the nominal +6",
			*res.AppliedGainDB)
	}
	// The gain PERSISTED must be the gain the guard computes from the peak
	// PERSISTED beside it. The range check above and the true-peak ceiling
	// below can both pass while the recorded number is wrong — the ceiling
	// measures what sox applied, not what we wrote down — and the recorded
	// number is the one iOS subtracts to honour a 0 dB preference, so a
	// divergence is silently audible on the device and nowhere else.
	// Asserting against ClipGuardedGainDB pins the WIRING; the arithmetic
	// itself has its own table in TestClipGuardedGainDB.
	if res.TruePeakDBTP == nil {
		t.Fatal("render reported no true peak — the applied gain has nothing to be derived from")
	}
	if want := ClipGuardedGainDB(*res.TruePeakDBTP); *res.AppliedGainDB != want {
		t.Errorf("applied gain %+.3f dB but the guard computes %+.3f dB from the recorded true peak %.3f dBTP",
			*res.AppliedGainDB, want, *res.TruePeakDBTP)
	}
	// The ceiling itself. Tolerance covers the 0.1 dB rounding of G plus
	// the meter reading a 24-bit dithered file rather than the float
	// scratch G was computed from.
	if renderedTP < -1.35 || renderedTP > -0.65 {
		t.Errorf("rendered true peak %.2f dBTP — the clip guard targets -1.0 dBTP, so Stage C's gain and G disagree",
			renderedTP)
	}
	if renderedTP > -0.1 {
		t.Errorf("rendered true peak %.2f dBTP is at or above full scale — the rendition clips", renderedTP)
	}
}

// TestDSDRender_CCIFIntermodulation: 19+20 kHz at equal amplitude puts
// the filters under maximum HF load; a nonlinearity anywhere in the chain
// shows up as a 1 kHz difference product that is not in the source.
func TestDSDRender_CCIFIntermodulation(t *testing.T) {
	requireFixtureMeasurement(t)
	out, _ := renderFixture(t, "dsd64-ccif-19-20khz.dsf", JobKindPCMRender, 176400, 24)
	xs := decodeMono(t, out)
	mags, binHz := spectrumDBFS(t, xs, 176400, fftSizeFor(len(xs)))
	t19, _ := peakInBand(mags, binHz, 18800, 19200)
	t20, _ := peakInBand(mags, binHz, 19800, 20200)
	imd, imdHz := peakInBand(mags, binHz, 900, 1100)
	t.Logf("CCIF: 19 kHz %.1f dBFS, 20 kHz %.1f dBFS, 1 kHz difference product %.1f dBFS at %.0f Hz",
		t19, t20, imd, imdHz)
	if imd > -80 {
		t.Errorf("CCIF difference product %.1f dBFS at %.0f Hz exceeds -80 dBFS", imd, imdHz)
	}
}
