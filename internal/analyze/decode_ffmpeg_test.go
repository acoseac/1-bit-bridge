package analyze

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// forceFFmpeg overrides the lookup seams so the fallback path believes the
// ffmpeg toolchain is (un)available, restoring them after the test.
func forceFFmpeg(t *testing.T, available bool) {
	t.Helper()
	origM, origP := ffmpegLookPath, ffprobeLookPath
	t.Cleanup(func() { ffmpegLookPath, ffprobeLookPath = origM, origP })
	if available {
		ffmpegLookPath = func() (string, error) { return "/usr/bin/ffmpeg", nil }
		ffprobeLookPath = func() (string, error) { return "/usr/bin/ffprobe", nil }
	} else {
		missing := errors.New("executable file not found in $PATH")
		ffmpegLookPath = func() (string, error) { return "", missing }
		ffprobeLookPath = func() (string, error) { return "", missing }
	}
}

func TestFFmpegDecodeArgs_MatchesSoxWireFormat(t *testing.T) {
	args := ffmpegDecodeArgs("/lib/a.m4a", 2)
	joined := strings.Join(args, " ")
	// Same raw wire sox emits (-t raw -e float -b 32 -L), at the 48 kHz target.
	for _, want := range []string{"-f f32le", "-ar 48000", "-ac 2", "-map 0:a:0", "-i /lib/a.m4a", "-nostdin"} {
		if !strings.Contains(joined, want) {
			t.Errorf("ffmpegDecodeArgs missing %q in: %s", want, joined)
		}
	}
	if args[len(args)-1] != "-" {
		t.Errorf("ffmpeg output must be stdout (-), got %q", args[len(args)-1])
	}
}

// TestDecodedShortOfDuration pins the ffmpeg-path truncation guard. ffmpeg can
// exit 0 after concealing a mid-stream decode error, so a truncated-but-openable
// faststart m4a would otherwise commit a partial waveform; decodeFrames rejects
// it by comparing the decoded length against the ffprobe-reported duration.
// A glitchy-but-complete file decodes ~full length and is accepted (no
// treadmill); the sox path and unknown-duration probes are never checked.
func TestDecodedShortOfDuration(t *testing.T) {
	const sr = AnalysisSampleRate
	cases := []struct {
		name      string
		tool      decoderTool
		expected  float64
		frames    int64
		wantShort bool
	}{
		// Reported case: ~58% decoded of declared duration → truncated.
		{"ffmpeg truncated 58pct", decoderFFmpeg, 100, int64(58 * sr), true},
		// Complete decode (exact) → accepted.
		{"ffmpeg complete", decoderFFmpeg, 100, int64(100 * sr), false},
		// Glitchy-but-complete: 1% short (encoder delay / rounding) → accepted.
		{"ffmpeg near-complete 99pct", decoderFFmpeg, 100, int64(99 * sr), false},
		// Just inside the 90% floor → accepted; just below → rejected.
		{"ffmpeg at 90pct floor", decoderFFmpeg, 100, int64(90 * sr), false},
		{"ffmpeg below floor 89pct", decoderFFmpeg, 100, int64(89 * sr), true},
		// sox path is intentionally unchecked even on a short decode.
		{"sox short not checked", decoderSox, 100, int64(58 * sr), false},
		// Unknown duration (probe miss) → no check, commit as before.
		{"ffmpeg unknown duration", decoderFFmpeg, 0, int64(1 * sr), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := decodedShortOfDuration(c.tool, c.expected, c.frames); got != c.wantShort {
				t.Errorf("decodedShortOfDuration(%s, %.0fs, %d frames) = %v, want %v",
					c.tool, c.expected, c.frames, got, c.wantShort)
			}
		})
	}
}

// TestFFprobeDuration_SurfacesPositiveDuration guards the expectedSec contract.
// The probe-channel tests above discard probeChannels' fourth return, so a
// regression that made the duration probe always return 0 would silently
// disable the ffmpeg truncation guard (decodedShortOfDuration short-circuits on
// expectedSec <= 0) with no failing test. This pins that ffprobeDuration — the
// source of expectedSec on the ffmpeg path — surfaces a positive, correct
// duration. Requires ffprobe (+ sox to synthesize a known-length source);
// skipped where either is absent (the macOS/Linux CI gate has both).
func TestFFprobeDuration_SurfacesPositiveDuration(t *testing.T) {
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not installed")
	}
	if _, err := exec.LookPath("sox"); err != nil {
		t.Skip("sox not installed")
	}
	src := filepath.Join(t.TempDir(), "tone.wav")
	if out, err := exec.Command("sox", "-n", "-r", "48000", "-c", "1", src,
		"synth", "3", "sine", "440").CombinedOutput(); err != nil {
		t.Skipf("sox synth: %v\n%s", err, out)
	}
	got := ffprobeDuration(context.Background(), src)
	if got < 2.5 || got > 3.5 {
		t.Errorf("ffprobeDuration(3s tone) = %.3f, want ~3.0 — a positive duration MUST be surfaced or the ffmpeg truncation guard is silently disabled", got)
	}
}

func TestDecodeCommand_SelectsBinary(t *testing.T) {
	if name, _ := decodeCommand(decoderSox, "/lib/a.flac", 2); name != "sox" {
		t.Errorf("decoderSox → %q, want sox", name)
	}
	// The ffmpeg binary is resolved through the seam, so the exec'd path is the
	// one the availability check found (not a re-PATH-resolved bare name).
	forceFFmpeg(t, true) // seam resolves to /usr/bin/ffmpeg
	if name, args := decodeCommand(decoderFFmpeg, "/lib/a.m4a", 2); name != "/usr/bin/ffmpeg" || args[0] != "-nostdin" {
		t.Errorf("decoderFFmpeg → (%q, %v), want resolved /usr/bin/ffmpeg + argv", name, args)
	}
}

func TestProbeChannels_FallsBackToFFmpegWhenSoxCannotProbe(t *testing.T) {
	forceFFmpeg(t, true)
	// A path sox can't probe (nonexistent / unreadable) → sox --i fails →
	// with the ffmpeg toolchain "present", the decoder choice flips to ffmpeg.
	bogus := filepath.Join(t.TempDir(), "missing.m4a")
	_, _, tool, _ := probeChannels(context.Background(), bogus)
	if tool != decoderFFmpeg {
		t.Errorf("tool = %s, want ffmpeg (sox can't probe + ffmpeg available)", tool)
	}
}

func TestProbeChannels_StaysSoxWhenNoFFmpeg(t *testing.T) {
	forceFFmpeg(t, false)
	bogus := filepath.Join(t.TempDir(), "missing.m4a")
	n, ok, tool, _ := probeChannels(context.Background(), bogus)
	if tool != decoderSox {
		t.Errorf("tool = %s, want sox (no ffmpeg fallback)", tool)
	}
	if ok || n != 1 {
		t.Errorf("unreadable + no fallback → (1,false), got (%d,%v)", n, ok)
	}
}
