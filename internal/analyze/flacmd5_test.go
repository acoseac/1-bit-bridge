package analyze

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
)

// synthFLAC synthesizes a small FLAC via sox (the fixture convention —
// no committed binaries) and skips the test when sox or its FLAC
// support is unavailable.
func synthFLAC(t *testing.T, bits int, channels int) string {
	t.Helper()
	requireSox(t)
	path := filepath.Join(t.TempDir(), "fixture.flac")
	cmd := exec.Command("sox", "-n", "-r", "44100",
		"-c", strconv.Itoa(channels), "-b", strconv.Itoa(bits), path,
		"synth", "2", "sine", "440", "vol", "0.5")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("sox could not synthesize FLAC (no FLAC support?): %v (%s)", err, out)
	}
	return path
}

// streamInfoMD5Offset is the file offset of STREAMINFO's 16-byte audio
// MD5: 4 magic + 4 block header + 18 bytes into the 34-byte body.
const streamInfoMD5Offset = 8 + 18

func TestFLACMD5RoundTripVerifies16Bit(t *testing.T) {
	path := synthFLAC(t, 16, 2)
	got := verifyFLACAudioMD5(context.Background(), path, decoderSox)
	if got != AudioMD5Verified {
		t.Fatalf("state = %q, want %q (sox native-depth decode must hash to STREAMINFO's MD5)",
			got, AudioMD5Verified)
	}
}

func TestFLACMD5RoundTripVerifies24Bit(t *testing.T) {
	path := synthFLAC(t, 24, 2)
	got := verifyFLACAudioMD5(context.Background(), path, decoderSox)
	if got != AudioMD5Verified {
		t.Fatalf("24-bit state = %q, want %q", got, AudioMD5Verified)
	}
}

// Cross-decoder parity: ffmpeg's pinned pcm_sNNle output must hash
// identically to sox's raw signed-LE output — both decode FLAC
// losslessly to the same native-depth byte stream.
func TestFLACMD5FFmpegDecoderParity(t *testing.T) {
	path := synthFLAC(t, 16, 2)
	if !ffmpegToolsAvailable() {
		t.Skip("ffmpeg not installed; skipping decoder-parity check")
	}
	got := verifyFLACAudioMD5(context.Background(), path, decoderFFmpeg)
	if got != AudioMD5Verified {
		t.Fatalf("ffmpeg-path state = %q, want %q", got, AudioMD5Verified)
	}
}

// A corrupted STORED checksum with intact audio is the clean way to
// force a mismatch (corrupting audio bytes makes the decode FAIL, which
// must read as "cannot verify", not "mismatch" — tested below).
func TestFLACMD5CorruptedStoredChecksumReadsMismatch(t *testing.T) {
	path := synthFLAC(t, 16, 2)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw[streamInfoMD5Offset] ^= 0xFF
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	got := verifyFLACAudioMD5(context.Background(), path, decoderSox)
	if got != AudioMD5Mismatch {
		t.Fatalf("state = %q, want %q", got, AudioMD5Mismatch)
	}
}

// The zero-MD5 sentinel: an all-zero STREAMINFO checksum means "unset"
// per the FLAC spec (live-capture encoders) — it must read as "cannot
// verify", NEVER as a mismatch.
func TestFLACMD5ZeroSentinelReadsAbsent(t *testing.T) {
	path := synthFLAC(t, 16, 2)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 16; i++ {
		raw[streamInfoMD5Offset+i] = 0
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := verifyFLACAudioMD5(context.Background(), path, decoderSox); got != "" {
		t.Fatalf("state = %q, want \"\" (zeroed checksum is unset, not wrong)", got)
	}
}

// A decode failure must read as "cannot verify" — a truncated pipe
// hashes differently by construction, and reporting mismatch would flag
// good-but-unreadable files as corrupt.
func TestFLACMD5TruncatedFileReadsAbsentNotMismatch(t *testing.T) {
	path := synthFLAC(t, 16, 2)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw[:len(raw)/2], 0o600); err != nil {
		t.Fatal(err)
	}
	if got := verifyFLACAudioMD5(context.Background(), path, decoderSox); got == AudioMD5Mismatch {
		t.Fatal("a truncated decode must never report mismatch")
	}
}

func TestReadFLACStreamInfoRejectsNonFLAC(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not.flac")
	if err := os.WriteFile(path, []byte("definitely not a flac file"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readFLACStreamInfo(path); err == nil {
		t.Fatal("expected an error for a non-FLAC file")
	}
}

func TestVerifiableBitDepthTruthTable(t *testing.T) {
	for bits, want := range map[int]bool{
		8: true, 16: true, 24: true, 32: true,
		12: false, 20: false, 0: false, 17: false,
	} {
		if got := verifiableBitDepth(bits); got != want {
			t.Fatalf("verifiableBitDepth(%d) = %v, want %v", bits, got, want)
		}
	}
}
