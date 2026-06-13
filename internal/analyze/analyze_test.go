package analyze

import (
	"context"
	"encoding/binary"
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
