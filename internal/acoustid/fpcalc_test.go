package acoustid

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// withFakeFpcalc points the seams at /bin/sh so the full spawn-and-parse flow
// runs deterministically with no fpcalc on the host. Mirrors
// transcode.withFakeSox. CI has no audio toolchain and must not need one.
func withFakeFpcalc(t *testing.T, stdout string, exitCode int) {
	t.Helper()
	requirePOSIXShell(t)
	origLook, origCmd := fpcalcLookPath, fpcalcCommand
	t.Cleanup(func() { fpcalcLookPath, fpcalcCommand = origLook, origCmd })

	fpcalcLookPath = func(string) (string, error) { return "/fake/fpcalc", nil }
	fpcalcCommand = func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		script := "printf '%s' '" + strings.ReplaceAll(stdout, "'", "'\\''") + "'"
		if exitCode != 0 {
			// fpcalc reports decode failures on stderr and exits non-zero.
			script += "; printf 'ERROR: could not decode\\n' >&2; exit " + strconv.Itoa(exitCode)
		}
		return exec.CommandContext(ctx, "/bin/sh", "-c", script)
	}
}

// requirePOSIXShell skips when /bin/sh is absent.
//
// The fpcalc seams are driven by a shell script standing in for the real
// binary, which is exactly right on Linux and macOS and simply does not
// exist on Windows — `exec: "/bin/sh": executable file not found in
// %PATH%`. withFakeFpcalc has always guarded for it; the
// error-redaction test grew its own inline seam later and did not, so it
// failed on Windows for want of a shell rather than for anything to do
// with path redaction. One helper so the next inline seam inherits the
// guard instead of rediscovering it.
func requirePOSIXShell(t *testing.T) {
	t.Helper()
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("test drives the fpcalc seam through /bin/sh, unavailable on this platform")
	}
}

func TestProbeReportsVersion(t *testing.T) {
	withFakeFpcalc(t, "fpcalc version 1.6.1 (FFmpeg Lavc62.28.102 Lavf62.12.102)\n", 0)
	info, err := Probe(context.Background())
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if info.Version != "1.6.1" {
		t.Errorf("Version = %q, want 1.6.1", info.Version)
	}
	if info.Path != "/fake/fpcalc" {
		t.Errorf("Path = %q", info.Path)
	}
}

func TestProbeMissingBinary(t *testing.T) {
	origLook := fpcalcLookPath
	t.Cleanup(func() { fpcalcLookPath = origLook })
	fpcalcLookPath = func(string) (string, error) { return "", errors.New("not found") }

	if _, err := Probe(context.Background()); !errors.Is(err, ErrFpcalcMissing) {
		t.Fatalf("err = %v, want ErrFpcalcMissing so callers can print an install hint", err)
	}
}

func TestParseFpcalcVersion(t *testing.T) {
	cases := []struct{ in, want string }{
		{"fpcalc version 1.6.1 (FFmpeg Lavc62.28.102)", "1.6.1"},
		{"fpcalc version 1.5.0\n", "1.5.0"},
		{"fpcalc version 1.6.1(FFmpeg)", "1.6.1"},
		// No digit — a source build can print a bare token. Cosmetic only,
		// and it must never gate anything, so "" is the right answer.
		{"fpcalc version dev", ""},
		{"totally unexpected banner", ""},
		{"", ""},
	}
	for _, tc := range cases {
		if got := parseFpcalcVersion(tc.in); got != tc.want {
			t.Errorf("parseFpcalcVersion(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestComputeParsesJSON(t *testing.T) {
	withFakeFpcalc(t, `{"duration": 243.55, "fingerprint": "AQABz0mUaEkSRZEGAAAA"}`, 0)
	fp, err := Compute(context.Background(), "/music/a.flac", 120*time.Second)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	if fp.Value != "AQABz0mUaEkSRZEGAAAA" {
		t.Errorf("Value = %q", fp.Value)
	}
	if math.Abs(fp.Duration-243.55) > 1e-9 {
		t.Errorf("Duration = %v, want 243.55", fp.Duration)
	}
	// Rounds, not truncates: AcoustID matches on this and a systematic
	// half-second bias would skew every lookup the same way.
	if got := fp.DurationSeconds(); got != 244 {
		t.Errorf("DurationSeconds() = %d, want 244", got)
	}
	if fp.DistinctB64 != distinctChars(fp.Value) {
		t.Errorf("DistinctB64 = %d, want %d", fp.DistinctB64, distinctChars(fp.Value))
	}
}

func TestComputeUnreadableSource(t *testing.T) {
	withFakeFpcalc(t, "", 2)
	_, err := Compute(context.Background(), "/music/broken.flac", 120*time.Second)
	if !errors.Is(err, ErrUnreadable) {
		t.Fatalf("err = %v, want ErrUnreadable — a bad file is not a toolchain outage", err)
	}
}

func TestComputeEmptyFingerprintIsUnreadable(t *testing.T) {
	withFakeFpcalc(t, `{"duration": 200.0, "fingerprint": ""}`, 0)
	if _, err := Compute(context.Background(), "/music/a.flac", 0); !errors.Is(err, ErrUnreadable) {
		t.Fatalf("err = %v, want ErrUnreadable", err)
	}
}

func TestComputeGarbageOutputIsUnreadable(t *testing.T) {
	withFakeFpcalc(t, "not json at all", 0)
	if _, err := Compute(context.Background(), "/music/a.flac", 0); !errors.Is(err, ErrUnreadable) {
		t.Fatalf("err = %v, want ErrUnreadable", err)
	}
}

// TestComputeCancellationIsNotADecodeFailure — a shutting-down sweeper must
// not record a cancelled decode as a bad file, or a restart would blame the
// library for the shutdown.
func TestComputeCancellationIsNotADecodeFailure(t *testing.T) {
	withFakeFpcalc(t, "", 2)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Compute(ctx, "/music/a.flac", 0)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if errors.Is(err, ErrUnreadable) {
		t.Fatal("a cancelled decode must not be reported as an unreadable file")
	}
}

// TestComputeErrorsDoNotLeakAbsolutePaths pins the bridge's privacy contract:
// the absolute library path is the one host-identifying token a decode
// invocation carries, and it must not reach a persisted error or a log.
func TestComputeErrorsDoNotLeakAbsolutePaths(t *testing.T) {
	const abs = "/Users/someone/Music/Private Library/track.flac"
	origLook, origCmd := fpcalcLookPath, fpcalcCommand
	t.Cleanup(func() { fpcalcLookPath, fpcalcCommand = origLook, origCmd })
	fpcalcLookPath = func(string) (string, error) { return "/fake/fpcalc", nil }
	requirePOSIXShell(t)
	fpcalcCommand = func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "/bin/sh", "-c",
			"printf 'ERROR: cannot open %s\\n' '"+abs+"' >&2; exit 2")
	}

	_, err := Compute(context.Background(), abs, 0)
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), abs) {
		t.Fatalf("error leaked the absolute path: %v", err)
	}
	if !strings.Contains(err.Error(), "track.flac") {
		t.Fatalf("error should keep the basename for diagnosis: %v", err)
	}
}

// TestPrefixModeToleratesTruncationButComputeDoesNot pins the one behavioural
// difference between the two entry points, and it is the reason prefix mode
// can only ever be a measurement tool.
//
// Truncating a stream ALWAYS makes fpcalc report a read error and exit
// non-zero — measured against fpcalc 1.6.1, and it happens whether the bytes
// arrive by pipe or as a file. It still writes a usable fingerprint. Prefix
// mode therefore has to accept that exit status, because it truncated the
// input deliberately; the ordinary path must NOT, because that same exit is
// the real guard against a genuinely truncated source.
//
// For FLAC the guard cannot come from anywhere else: fpcalc reports the
// STREAMINFO duration, so a truncated FLAC still claims its full length and a
// duration comparison would see nothing wrong.
func TestPrefixModeToleratesTruncationButComputeDoesNot(t *testing.T) {
	const payload = `{"duration": 45.00, "fingerprint": "AQABz0mUaEkSRZEG"}`

	t.Run("Compute refuses a non-zero exit", func(t *testing.T) {
		withFakeFpcalc(t, payload, 2)
		if _, err := Compute(context.Background(), "/music/a.flac", 0); !errors.Is(err, ErrUnreadable) {
			t.Fatalf("err = %v, want ErrUnreadable — the exit status IS the truncation guard", err)
		}
	})

	t.Run("ComputeFromPrefix accepts one", func(t *testing.T) {
		withFakeFpcalc(t, payload, 2)
		dir := t.TempDir()
		src := filepath.Join(dir, "a.wav")
		if err := os.WriteFile(src, synthWAV(1, true), 0o600); err != nil {
			t.Fatal(err)
		}
		fp, err := ComputeFromPrefix(context.Background(), src, 0, 1024)
		if err != nil {
			t.Fatalf("ComputeFromPrefix: %v — a deliberate truncation must not fail", err)
		}
		if fp.Value == "" {
			t.Fatal("expected the fingerprint fpcalc still produced")
		}
		if fp.BytesRead == 0 {
			t.Error("BytesRead must record what was actually fed")
		}
	})

	t.Run("ComputeFromPrefix still fails with no usable output", func(t *testing.T) {
		withFakeFpcalc(t, "", 2)
		dir := t.TempDir()
		src := filepath.Join(dir, "a.wav")
		if err := os.WriteFile(src, synthWAV(1, true), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := ComputeFromPrefix(context.Background(), src, 0, 1024); !errors.Is(err, ErrUnreadable) {
			t.Fatalf("err = %v, want ErrUnreadable", err)
		}
	})
}

func TestDistinctChars(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"aaaa", 1},
		{"abc", 3},
		{"AQAAAAAA", 2},
	}
	for _, tc := range cases {
		if got := distinctChars(tc.in); got != tc.want {
			t.Errorf("distinctChars(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestDurationSecondsRounds(t *testing.T) {
	cases := []struct {
		in   float64
		want int
	}{
		{0, 0}, {-1, 0}, {243.4, 243}, {243.5, 244}, {243.99, 244}, {45.0, 45},
	}
	for _, tc := range cases {
		fp := Fingerprint{Duration: tc.in}
		if got := fp.DurationSeconds(); got != tc.want {
			t.Errorf("DurationSeconds(%v) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// --- real-binary check, skipped wherever fpcalc is absent (including CI) ---

// TestComputeAgainstRealFpcalc runs the genuine binary against audio
// synthesised in-process, so it needs no committed fixture and no sox. It is
// the end-to-end check that the flags, the JSON shape and the entropy signal
// still hold against whatever fpcalc the host actually has.
//
// The two signals are deliberately opposite: silence must land under the
// entropy floor and noise well over it. If a future fpcalc changes its output
// format or its compression, this is what notices.
func TestComputeAgainstRealFpcalc(t *testing.T) {
	if _, err := exec.LookPath("fpcalc"); err != nil {
		t.Skip("fpcalc not installed — real-binary check skipped (CI has no audio toolchain)")
	}
	dir := t.TempDir()

	silent := filepath.Join(dir, "silence.wav")
	if err := os.WriteFile(silent, synthWAV(45, false), 0o600); err != nil {
		t.Fatal(err)
	}
	noisy := filepath.Join(dir, "noise.wav")
	if err := os.WriteFile(noisy, synthWAV(45, true), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sFP, err := Compute(ctx, silent, 120*time.Second)
	if err != nil {
		t.Fatalf("Compute(silence): %v", err)
	}
	nFP, err := Compute(ctx, noisy, 120*time.Second)
	if err != nil {
		t.Fatalf("Compute(noise): %v", err)
	}

	t.Logf("silence: distinctB64=%d duration=%.2f", sFP.DistinctB64, sFP.Duration)
	t.Logf("noise:   distinctB64=%d duration=%.2f", nFP.DistinctB64, nFP.Duration)

	if sFP.DistinctB64 >= minDistinctB64Chars {
		t.Errorf("silence scored %d distinct base64 chars, at or above the floor of %d — "+
			"the entropy gate would let a silent track reach AcoustID",
			sFP.DistinctB64, minDistinctB64Chars)
	}
	if nFP.DistinctB64 < minDistinctB64Chars {
		t.Errorf("noise scored %d distinct base64 chars, below the floor of %d — "+
			"the entropy gate would refuse real audio", nFP.DistinctB64, minDistinctB64Chars)
	}
	if math.Abs(sFP.Duration-45) > 1 {
		t.Errorf("decoded duration = %.2f, want ~45", sFP.Duration)
	}
}

// synthWAV builds a 44.1kHz 16-bit mono WAV in memory: either digital silence
// or a deterministic pseudo-random signal. Deterministic so a failure is
// reproducible, and in-process so the repo carries no binary fixtures.
func synthWAV(seconds int, noise bool) []byte {
	const rate = 44100
	n := rate * seconds
	var buf bytes.Buffer
	data := make([]byte, 0, n*2)
	// xorshift, seeded to a fixed value — reproducible across runs and hosts.
	state := uint32(0x2545F491)
	for range n {
		var s int16
		if noise {
			state ^= state << 13
			state ^= state >> 17
			state ^= state << 5
			s = int16(state >> 17) // ~±16k, full-scale-ish noise
		}
		data = append(data, byte(uint16(s)), byte(uint16(s)>>8))
	}
	writeChunk := func(id string, payloadLen int) {
		buf.WriteString(id)
		_ = binary.Write(&buf, binary.LittleEndian, uint32(payloadLen))
	}
	buf.WriteString("RIFF")
	_ = binary.Write(&buf, binary.LittleEndian, uint32(36+len(data)))
	buf.WriteString("WAVE")
	writeChunk("fmt ", 16)
	_ = binary.Write(&buf, binary.LittleEndian, uint16(1))      // PCM
	_ = binary.Write(&buf, binary.LittleEndian, uint16(1))      // mono
	_ = binary.Write(&buf, binary.LittleEndian, uint32(rate))   // sample rate
	_ = binary.Write(&buf, binary.LittleEndian, uint32(rate*2)) // byte rate
	_ = binary.Write(&buf, binary.LittleEndian, uint16(2))      // block align
	_ = binary.Write(&buf, binary.LittleEndian, uint16(16))     // bits
	writeChunk("data", len(data))
	buf.Write(data)
	return buf.Bytes()
}
