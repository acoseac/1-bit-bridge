package analyze

import (
	"context"
	"encoding/binary"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// withRecordingSox installs a fake sox that records its argv to a file and
// emits `pcm` on stdout — so the argv TruePeakDBTP builds is observable
// without a real sox, and the meter's arithmetic can be pinned on known
// samples.
func withRecordingSox(t *testing.T, pcm []byte) (argsFile string) {
	t.Helper()
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("test uses /bin/sh which isn't available on this platform")
	}
	dir := t.TempDir()
	argsFile = filepath.Join(dir, "args.txt")
	pcmFile := filepath.Join(dir, "pcm.bin")
	if err := os.WriteFile(pcmFile, pcm, 0o644); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > '" + argsFile + "'\ncat '" + pcmFile + "'\n"
	bin := filepath.Join(dir, "sox")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	orig := soxLookPath
	t.Cleanup(func() { soxLookPath = orig })
	soxLookPath = func() (string, error) { return bin, nil }
	return argsFile
}

func f32leFrames(channels int, values ...float32) []byte {
	out := make([]byte, 0, 4*len(values)*channels)
	for _, v := range values {
		for c := 0; c < channels; c++ {
			out = binary.LittleEndian.AppendUint32(out, math.Float32bits(v))
		}
	}
	return out
}

func TestTruePeakDBTP_DecodesAtNativeRateViaSox(t *testing.T) {
	// 256 frames of an on-grid fs/32 sine at 0.5: no step at the start (the
	// interpolator rings on a step — a DC block from silence reads ~1 dB
	// hot), and an on-grid low-frequency sine has no intersample overshoot,
	// so the true peak is −6.02 dBTP within the meter's tolerance.
	vals := make([]float32, 256)
	for i := range vals {
		vals[i] = float32(0.5 * math.Sin(2*math.Pi*float64(i)/32))
	}
	argsFile := withRecordingSox(t, f32leFrames(2, vals...))

	db, ok, err := TruePeakDBTP(context.Background(), "/scratch/a1b2.stageA.sox", 2)
	if err != nil {
		t.Fatalf("TruePeakDBTP: %v", err)
	}
	if !ok {
		t.Fatal("a non-silent stream must report ok")
	}
	if want := 20 * math.Log10(0.5); math.Abs(db-want) > 0.05 {
		t.Errorf("true peak = %.3f dBTP, want %.3f", db, want)
	}
	argv, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(strings.Split(strings.TrimSpace(string(argv)), "\n"), " ")
	if want := "/scratch/a1b2.stageA.sox -t raw -e float -b 32 -L -c 2 -"; got != want {
		t.Errorf("sox argv = %q, want %q", got, want)
	}
	if strings.Contains(got, "-r ") {
		t.Errorf("the true-peak read must NOT resample: %q", got)
	}
}

func TestTruePeakDBTP_SilenceIsNotOK(t *testing.T) {
	withRecordingSox(t, f32leFrames(1, make([]float32, 32)...))
	_, ok, err := TruePeakDBTP(context.Background(), "/scratch/silent.sox", 1)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("digital silence has no peak to report")
	}
}

func TestTruePeakDBTP_DecoderFailureIsAnError(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "sox")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho 'sox FAIL formats: no handler' >&2\nexit 2\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	orig := soxLookPath
	t.Cleanup(func() { soxLookPath = orig })
	soxLookPath = func() (string, error) { return bin, nil }
	if _, _, err := TruePeakDBTP(context.Background(), "/scratch/x.sox", 2); err == nil {
		t.Error("a failing decoder must surface as an error, never as a peak")
	}
}

func TestTruePeakDBTP_RealSoxIfPresent(t *testing.T) {
	if _, err := exec.LookPath("sox"); err != nil {
		t.Skip("real sox not on PATH")
	}
	path := filepath.Join(t.TempDir(), "tone.wav")
	// A 1 kHz sine at −6 dB: its true peak sits within ~0.05 dB of −6.02.
	out, err := exec.Command("sox", "-n", "-r", "44100", "-c", "2", "-b", "24", path, "synth", "0.5", "sine", "1000", "gain", "-6").CombinedOutput()
	if err != nil {
		t.Fatalf("sox synth: %v (%s)", err, out)
	}
	db, ok, err := TruePeakDBTP(context.Background(), path, 2)
	if err != nil || !ok {
		t.Fatalf("TruePeakDBTP: ok=%v err=%v", ok, err)
	}
	if db < -6.15 || db > -5.85 {
		t.Errorf("true peak of a −6 dB sine = %.3f dBTP, want ≈ −6", db)
	}
}
