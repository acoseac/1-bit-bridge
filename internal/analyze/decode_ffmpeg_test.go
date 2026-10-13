package analyze

import (
	"context"
	"errors"
	"math"
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

// TestDecodedShortOfDuration pins the truncation guard both decoders share.
// ffmpeg can exit 0 after concealing a mid-stream decode error, and sox exits
// 0 after a FLAC LOST_SYNC, so a truncated-but-openable file would otherwise
// commit a partial waveform. A glitchy-but-complete file decodes to full
// length and is accepted. The allowance column is the codec priming slack:
// 0 for the long-file rows, aacPrimingAllowance for the Apple AAC rows.
func TestDecodedShortOfDuration(t *testing.T) {
	const sr = AnalysisSampleRate
	frames := func(sec float64) int64 {
		return int64(math.Round(sec * float64(sr)))
	}
	cases := []struct {
		name      string
		expected  float64
		frames    int64
		allow     float64
		wantShort bool
	}{
		// Reported case: ~58% decoded of declared duration → truncated.
		{"truncated 58pct", 100, int64(58 * sr), 0, true},
		// Complete decode (exact) → accepted. This is also the glitchy-but-
		// complete FLAC case (mid-stream LOST_SYNC that resyncs to EOF): full
		// length decoded, so it is committed rather than treadmilled.
		{"complete", 100, int64(100 * sr), 0, false},
		// 1% short (rounding / VBR drift) → accepted.
		{"near-complete 99pct", 100, int64(99 * sr), 0, false},
		// Just inside the 90% floor → accepted; just below → rejected.
		{"at 90pct floor", 100, int64(90 * sr), 0, false},
		{"below floor 89pct", 100, int64(89 * sr), 0, true},
		// Unknown duration (probe miss) → no check, commit as before.
		{"unknown duration", 0, int64(1 * sr), 0, false},
		// Zero frames against a known duration → truncated.
		{"zero frames", 100, 0, 0, true},
		// iTunes AAC fixture: probe 0.1161 s, decode 0.050 s. The priming
		// allowance accepts it; the ratio alone rejects it.
		{"itunes aac with priming", 0.1161, frames(0.050), aacPrimingAllowance, false},
		{"itunes aac without priming", 0.1161, frames(0.050), 0, true},
		// The same probe with an empty decode is the 60% byte cut.
		{"itunes aac empty decode", 0.1161, 0, aacPrimingAllowance, true},
		// Under about 88.9 ms the AAC allowance covers the whole probed
		// duration, so only the zero-frame rule rejects an empty decode
		// (CodeRabbit on #1205).
		{"aac 88.8ms empty decode", 0.0888, 0, aacPrimingAllowance, true},
		{"aac 40ms empty decode", 0.040, 0, aacPrimingAllowance, true},
		// afconvert 0.1 s and 0.5 s sit under the 90% line and inside 80 ms.
		{"afconvert 0.1s", 0.162540, frames(0.100), aacPrimingAllowance, false},
		{"afconvert 0.5s", 0.557279, frames(0.500), aacPrimingAllowance, false},
		// A header that claims 5 s over 3 s of audio fails at the long length.
		{"claims 5s over 3s", 5, int64(3 * sr), aacPrimingAllowance, true},
		// A +2 s overclaim fails at each measured length.
		{"claims +2s at 0.1", 2.1, frames(0.1), aacPrimingAllowance, true},
		{"claims +2s at 0.5", 2.5, frames(0.5), aacPrimingAllowance, true},
		{"claims +2s at 1", 3, frames(1), aacPrimingAllowance, true},
		{"claims +2s at 5", 7, frames(5), aacPrimingAllowance, true},
		// 60% of the content. At 0.1 s the 40 ms gap is inside the AAC
		// allowance (duration alone cannot tell it from priming); the ratio
		// alone still rejects it, and 0.5 s and longer fail with the allowance.
		{"60pct content 0.1 aac", 0.1, frames(0.06), aacPrimingAllowance, false},
		{"60pct content 0.1 bare", 0.1, frames(0.06), 0, true},
		{"60pct content 0.5 aac", 0.5, frames(0.3), aacPrimingAllowance, true},
		{"60pct content 1 aac", 1, frames(0.6), aacPrimingAllowance, true},
		{"60pct content 5 aac", 5, frames(3), aacPrimingAllowance, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := decodedShortOfDuration(c.expected, c.frames, c.allow); got != c.wantShort {
				t.Errorf("decodedShortOfDuration(%gs, %d frames, %gs) = %v, want %v",
					c.expected, c.frames, c.allow, got, c.wantShort)
			}
		})
	}
}

// TestFFprobeAudioReadsCodecChannelsAndDurationInOneRun pins the single
// probe on the iTunes fixture: codec aac (which grants the priming
// allowance), 2 channels (loudness is measured, not skipped), and the
// container's 0.1161 s. One run replaced three (Gemini on #1205).
func TestFFprobeAudioReadsCodecChannelsAndDurationInOneRun(t *testing.T) {
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not installed")
	}
	src := filepath.Join("..", "manifest", "testdata", "m4a", "itunes_aac_pop.m4a")
	got := ffprobeAudio(context.Background(), src)
	if got.codec != "aac" || got.channels != 2 || !got.channelsOK || math.Abs(got.duration-0.1161) > 0.0005 {
		t.Fatalf("ffprobeAudio(itunes fixture) = %+v, want codec aac, 2 channels (ok), duration 0.1161 s", got)
	}
	missing := ffprobeAudio(context.Background(), filepath.Join(t.TempDir(), "absent.m4a"))
	if missing.codec != "" || missing.channels != 1 || missing.channelsOK || missing.duration != 0 {
		t.Fatalf("ffprobeAudio(missing file) = %+v, want the zero contract: no codec, (1, false), duration 0", missing)
	}
}

// TestFFprobeDuration_SurfacesPositiveDuration guards the expectedSec contract.
// The probe-channel tests above discard probeChannels' fourth return, so a
// regression that made the duration probe always return 0 would silently
// disable the ffmpeg truncation guard (decodedShortOfDuration short-circuits on
// expectedSec <= 0) with no failing test. This pins that ffprobeAudio — the
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
	got := ffprobeAudio(context.Background(), src).duration
	if got < 2.5 || got > 3.5 {
		t.Errorf("ffprobeAudio(3s tone).duration = %.3f, want ~3.0 — a positive duration MUST be surfaced or the ffmpeg truncation guard is silently disabled", got)
	}
}

func TestDecodeCommand_SelectsBinary(t *testing.T) {
	// Both binaries are resolved through their seam, so the exec'd path is the
	// one the availability check found (not a re-PATH-resolved bare name).
	origSox := soxLookPath
	t.Cleanup(func() { soxLookPath = origSox })
	soxLookPath = func() (string, error) { return "/usr/bin/sox", nil }
	if name, _ := decodeCommand(decoderSox, "/lib/a.flac", 2); name != "/usr/bin/sox" {
		t.Errorf("decoderSox → %q, want resolved /usr/bin/sox", name)
	}
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
	_, _, tool, _, _ := probeChannels(context.Background(), bogus)
	if tool != decoderFFmpeg {
		t.Errorf("tool = %s, want ffmpeg (sox can't probe + ffmpeg available)", tool)
	}
}

func TestProbeChannels_StaysSoxWhenNoFFmpeg(t *testing.T) {
	forceFFmpeg(t, false)
	bogus := filepath.Join(t.TempDir(), "missing.m4a")
	n, ok, tool, _, _ := probeChannels(context.Background(), bogus)
	if tool != decoderSox {
		t.Errorf("tool = %s, want sox (no ffmpeg fallback)", tool)
	}
	if ok || n != 1 {
		t.Errorf("unreadable + no fallback → (1,false), got (%d,%v)", n, ok)
	}
}
