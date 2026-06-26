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

// TestFFmpegDecodeArgs_AbortsOnMidStreamError pins the `-xerror` flag that
// keeps the "commit only on clean decode" invariant honest for the ffmpeg
// fallback. Without it, ffmpeg exits 0 on a truncated-but-openable m4a/mp4
// (the faststart layout) after decoding only the first N seconds, so
// decodeFrames returns (partialFrames, nil) and RunAnalysis commits a partial
// waveform that the mtime+size scan-skip gate never re-analyzes. Empirically,
// the truncated faststart m4a exits 183 with `-xerror` (→ decodeFrames errors →
// nothing committed). Structural gate: runs without ffmpeg installed; the
// empirical exit-code behaviour was verified by hand.
func TestFFmpegDecodeArgs_AbortsOnMidStreamError(t *testing.T) {
	args := ffmpegDecodeArgs("/lib/a.m4a", 2)
	found := false
	for _, a := range args {
		if a == "-xerror" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("ffmpegDecodeArgs missing -xerror (mid-stream decode errors would silently commit a partial waveform): %v", args)
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
