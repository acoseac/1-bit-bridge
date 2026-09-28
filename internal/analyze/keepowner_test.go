package analyze

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

// TestAWaveformWriteAsRootKeepsTheOwner pins that the curve's staged file
// is given the owner of the curve it replaces, or of its directory
// (fsutil.KeepOwner), before it is written. Driven through
// fsutil.SimulateRootForTest, since the chown itself needs root.
func TestAWaveformWriteAsRootKeepsTheOwner(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a file takes its directory's ACL on Windows; these helpers change no owner there")
	}
	dir := t.TempDir()
	final := filepath.Join(dir, "01.flac"+WaveformExt)
	tmp := final + AnalysisTmpSuffix
	changes, restore := fsutil.SimulateRootForTest(4242, 4243)
	defer restore()
	if err := writeWaveformTmp(tmp, final, []byte("curve")); err != nil {
		t.Fatal(err)
	}
	want := fsutil.OwnerChange{Dst: final, UID: 4242, GID: 4243}
	if got := changes(); len(got) != 1 || got[0] != want {
		t.Fatalf("owner changes = %+v, want exactly %+v", got, want)
	}
	if b, err := os.ReadFile(tmp); err != nil || string(b) != "curve" {
		t.Fatalf("the curve was not written: %q %v", b, err)
	}
	if info, err := os.Stat(tmp); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("the curve's mode = %v (%v), want 0600 as os.WriteFile gave it", info.Mode(), err)
	}
}

// TestRunAnalysisAsRootKeepsTheInstallOwner: a `sudo bridge analyze` over a
// service install gives the waveform directories it makes and the curve it
// writes the install's owner. It needs sox to decode; the root test in
// cmd/bridge runs the whole command as root.
func TestRunAnalysisAsRootKeepsTheInstallOwner(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a file takes its directory's ACL on Windows; these helpers change no owner there")
	}
	requireSox(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "tone.wav")
	if out, err := exec.Command("sox", "-n", "-r", "44100", "-c", "1", src,
		"synth", "0.5", "sine", "440").CombinedOutput(); err != nil {
		t.Fatalf("sox synth: %v\n%s", err, out)
	}
	outDir := filepath.Join(dir, "waveforms")
	spec := AnalyzeSpec{SourceAbsPath: src, SourceLibraryRel: "Artist/Album/tone.wav", OutputDir: outDir}
	changes, restore := fsutil.SimulateRootForTest(4242, 4243)
	defer restore()
	res, err := RunAnalysis(context.Background(), spec)
	if err != nil {
		t.Fatalf("RunAnalysis: %v", err)
	}
	want := []fsutil.OwnerChange{
		{Dst: outDir, UID: 4242, GID: 4243},
		{Dst: filepath.Join(outDir, "Artist"), UID: 4242, GID: 4243},
		{Dst: filepath.Join(outDir, "Artist", "Album"), UID: 4242, GID: 4243},
		{Dst: res.WaveformPath, UID: 4242, GID: 4243},
	}
	got := changes()
	if len(got) != len(want) {
		t.Fatalf("owner changes = %+v, want exactly %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("owner changes = %+v, want exactly %+v", got, want)
		}
	}
}
