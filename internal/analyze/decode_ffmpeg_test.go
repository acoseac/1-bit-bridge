package analyze

import (
	"context"
	"errors"
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

func TestDecodeCommand_SelectsBinary(t *testing.T) {
	if name, _ := decodeCommand(decoderSox, "/lib/a.flac", 2); name != "sox" {
		t.Errorf("decoderSox → %q, want sox", name)
	}
	if name, args := decodeCommand(decoderFFmpeg, "/lib/a.m4a", 2); name != "ffmpeg" || args[0] != "-nostdin" {
		t.Errorf("decoderFFmpeg → (%q, %v), want ffmpeg argv", name, args)
	}
}

func TestProbeChannels_FallsBackToFFmpegWhenSoxCannotProbe(t *testing.T) {
	forceFFmpeg(t, true)
	// A path sox can't probe (nonexistent / unreadable) → sox --i fails →
	// with the ffmpeg toolchain "present", the decoder choice flips to ffmpeg.
	bogus := filepath.Join(t.TempDir(), "missing.m4a")
	_, _, tool := probeChannels(context.Background(), bogus)
	if tool != decoderFFmpeg {
		t.Errorf("tool = %s, want ffmpeg (sox can't probe + ffmpeg available)", tool)
	}
}

func TestProbeChannels_StaysSoxWhenNoFFmpeg(t *testing.T) {
	forceFFmpeg(t, false)
	bogus := filepath.Join(t.TempDir(), "missing.m4a")
	n, ok, tool := probeChannels(context.Background(), bogus)
	if tool != decoderSox {
		t.Errorf("tool = %s, want sox (no ffmpeg fallback)", tool)
	}
	if ok || n != 1 {
		t.Errorf("unreadable + no fallback → (1,false), got (%d,%v)", n, ok)
	}
}
