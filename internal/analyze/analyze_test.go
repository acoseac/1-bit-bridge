package analyze

import (
	"bytes"
	"context"
	"encoding/binary"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// requireSox skips the test when sox isn't installed — these are
// integration tests that exercise the real decode path.
func requireSox(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("sox"); err != nil {
		t.Skip("sox not installed; skipping decode integration test")
	}
}

func TestRunAnalysisEndToEnd(t *testing.T) {
	requireSox(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "tone.wav")
	// 0.5 s 440 Hz sine at 44.1 kHz — RunAnalysis resamples to 48 kHz.
	if out, err := exec.Command("sox", "-n", "-r", "44100", "-c", "1", src,
		"synth", "0.5", "sine", "440").CombinedOutput(); err != nil {
		t.Fatalf("sox synth: %v\n%s", err, out)
	}

	outDir := filepath.Join(dir, "waveforms")
	res, err := RunAnalysis(context.Background(), AnalyzeSpec{
		SourceAbsPath: src, SourceLibraryRel: "tone.wav", OutputDir: outDir,
	})
	if err != nil {
		t.Fatalf("RunAnalysis: %v", err)
	}
	if res.SchemaVersion != WaveformSchemaVersion {
		t.Fatalf("schema = %q", res.SchemaVersion)
	}

	data, err := os.ReadFile(res.WaveformPath)
	if err != nil {
		t.Fatalf("read sidecar: %v", err)
	}
	if int64(len(data)) != res.WaveformSize {
		t.Fatalf("size mismatch: file=%d result=%d", len(data), res.WaveformSize)
	}
	if string(data[0:4]) != waveformMagic {
		t.Fatalf("magic = %q", data[0:4])
	}
	if waveformTag(data) != res.WaveformTag {
		t.Fatalf("tag mismatch: %q vs %q", waveformTag(data), res.WaveformTag)
	}
	// ~0.5 s at 0.1 s buckets → ~5 buckets, duration ~500 ms.
	if count := binary.LittleEndian.Uint32(data[14:18]); count < 3 || count > 7 {
		t.Fatalf("bucket count = %d, want ~5", count)
	}
	if dur := binary.LittleEndian.Uint32(data[18:22]); dur < 400 || dur > 600 {
		t.Fatalf("duration = %d ms, want ~500", dur)
	}
	// The first 0.1 s bucket of a 440 Hz tone (≈44 cycles) must capture a
	// strongly positive peak — proves real signal was decoded, not
	// silence. (sox `synth` defaults to ≈-3 dB, so don't assert full-scale.)
	if max0 := int8(data[waveformHeaderLen+1]); max0 < 40 {
		t.Fatalf("first bucket max = %d, want a substantial positive peak", max0)
	}
}

func TestDownmixFrame(t *testing.T) {
	cases := []struct {
		name  string
		frame []float64
		want  float32
	}{
		{"mono passthrough", []float64{0.5}, 0.5},
		{"stereo average", []float64{0.4, 0.6}, 0.5},
		{"stereo cancel", []float64{0.5, -0.5}, 0.0},
		{"quad average", []float64{1, 0, -1, 0}, 0.0},
	}
	for _, c := range cases {
		if got := downmixFrame(c.frame); got != c.want {
			t.Errorf("%s: downmixFrame(%v) = %v, want %v", c.name, c.frame, got, c.want)
		}
	}
}

// TestRunAnalysisComputesLoudness: a real (non-silent) decode populates a
// plausible ReplayGain. The exact value is pinned by loudness_test.go's
// ffmpeg cross-check; here we only assert the WIRING delivers a finite,
// sane gain for a full-scale-ish tone (a -3 dB sine integrates near
// -6 LUFS → RG ≈ -12 dB; the band is wide to absorb sox's synth level).
func TestRunAnalysisComputesLoudness(t *testing.T) {
	requireSox(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "tone.wav")
	// 3 s so several 400 ms R128 blocks survive the gating AND the key
	// estimator clears its window gate comfortably.
	if out, err := exec.Command("sox", "-n", "-r", "48000", "-c", "2", src,
		"synth", "3", "sine", "440", "sine", "440").CombinedOutput(); err != nil {
		t.Fatalf("sox synth: %v\n%s", err, out)
	}
	res, err := RunAnalysis(context.Background(), AnalyzeSpec{
		SourceAbsPath: src, SourceLibraryRel: "tone.wav",
		OutputDir: filepath.Join(dir, "waveforms"),
	})
	if err != nil {
		t.Fatalf("RunAnalysis: %v", err)
	}
	if !res.HasLoudness {
		t.Fatal("HasLoudness = false, want loudness computed for a non-silent stereo tone")
	}
	if res.ReplayGainTrackDB < -25 || res.ReplayGainTrackDB > -2 {
		t.Fatalf("ReplayGainTrackDB = %.2f dB, outside the plausible [-25,-2] band", res.ReplayGainTrackDB)
	}
	// A sustained tone is tonal (a key estimate is produced end-to-end via
	// RunAnalysis) but steady (no onsets → no tempo). This pins the
	// RunAnalysis → key wiring on real audio.
	if res.KeyRoot == nil || (res.KeyMode != "major" && res.KeyMode != "minor") {
		t.Fatalf("KeyRoot/KeyMode = (%v,%q), want a key estimate from a sustained tone", res.KeyRoot, res.KeyMode)
	}
	if res.BPM != nil {
		t.Fatalf("BPM = %v, want nil (a steady tone has no rhythm)", *res.BPM)
	}
}

// TestDownmixMatchesMonoDecode is the churn-safety pin for the wf1→wf2
// bump: the peak envelope from the new source-channel decode (per-frame
// downmix) must be BYTE-IDENTICAL to the prior mono `-c 1` decode for a
// stereo source, so already-analyzed libraries keep the same waveformTag
// and iOS never re-fetches a sidecar — only the new loudness scalar syncs.
func TestDownmixMatchesMonoDecode(t *testing.T) {
	requireSox(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "stereo.wav")
	// Distinct L/R content so a downmix that didn't average both channels
	// would diverge.
	if out, err := exec.Command("sox", "-n", "-r", "48000", "-c", "2", src,
		"synth", "1.5", "sine", "330", "sine", "550").CombinedOutput(); err != nil {
		t.Fatalf("sox synth: %v\n%s", err, out)
	}
	ctx := context.Background()

	waveformVia := func(channels int) []byte {
		pk := newPeaker(waveformBucketSamples)
		total, err := decodeFrames(ctx, src, channels, decoderSox, 0, func(frame []float64) {
			pk.add(downmixFrame(frame))
		})
		if err != nil {
			t.Fatalf("decodeFrames(c=%d): %v", channels, err)
		}
		pk.finish()
		return encodeWaveform(pk, AnalysisSampleRate, waveformBucketSamples, total)
	}

	mono := waveformVia(1)   // sox's own stereo→mono mix (the prior wf1 path)
	stereo := waveformVia(2) // source-channel decode + Go (L+R)/2 downmix
	if !bytes.Equal(mono, stereo) {
		t.Fatalf("waveform bytes diverge: mono `-c 1` (%d B) vs downmixed source-channel (%d B) — wf1→wf2 would churn iOS sidecars",
			len(mono), len(stereo))
	}
}

// TestChromaExtractionFromRealChord drives the chroma extractor on real
// audio: a sustained C-major triad (C4/E4/G4) decoded through sox must
// concentrate the 12-bin chroma at exactly C(0), E(4), G(7) — verifying
// the FFT-bin → pitch-class mapping on a real spectrum. (The K-S key
// RESOLUTION is unit-tested deterministically in keytempo_test.go; a pure
// harmonics-free triad is too degenerate to pin a final key, which is why
// this asserts the extraction, not the verdict.)
func TestChromaExtractionFromRealChord(t *testing.T) {
	requireSox(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "cmaj.wav")
	// sox sums multiple `sine` voices into the single mono channel.
	if out, err := exec.Command("sox", "-n", "-r", "48000", "-c", "1", src,
		"synth", "4", "sine", "261.63", "sine", "329.63", "sine", "392.00").CombinedOutput(); err != nil {
		t.Fatalf("sox synth chord: %v\n%s", err, out)
	}
	a := newKeyTempoAnalyzer()
	if _, err := decodeFrames(context.Background(), src, 1, decoderSox, 0, func(frame []float64) {
		a.add(downmixFrame(frame))
	}); err != nil {
		t.Fatalf("decodeFrames: %v", err)
	}
	if a.windows < minWindowsForKey {
		t.Fatalf("only %d windows from a 4 s chord, want >= %d", a.windows, minWindowsForKey)
	}
	// Each chord tone must dominate clean non-chord, non-adjacent classes
	// (D and A are neither chord tones nor semitone neighbours of C/E/G).
	ref := math.Max(a.chroma[2], a.chroma[9]) // D, A
	for _, pc := range []int{0, 4, 7} {       // C, E, G
		if a.chroma[pc] < 2*ref {
			t.Fatalf("chroma[%d]=%.1f not >= 2× non-chord ref %.1f — extraction smeared",
				pc, a.chroma[pc], ref)
		}
	}
}

func TestRunAnalysisCorruptFileWritesNoSidecar(t *testing.T) {
	requireSox(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "bad.flac")
	if err := os.WriteFile(src, []byte("this is not audio"), 0o600); err != nil {
		t.Fatalf("write bad file: %v", err)
	}
	outDir := filepath.Join(dir, "waveforms")
	if _, err := RunAnalysis(context.Background(), AnalyzeSpec{
		SourceAbsPath: src, SourceLibraryRel: "bad.flac", OutputDir: outDir,
	}); err == nil {
		t.Fatal("expected error on corrupt file")
	}
	// No sidecar AND no leftover .tmp under the output dir.
	entries, err := os.ReadDir(outDir)
	if err != nil {
		t.Fatalf("read out dir: %v", err)
	}
	if len(entries) != 0 {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Fatalf("expected empty output dir, found %v", names)
	}
}
