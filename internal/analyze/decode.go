package analyze

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

// maxAnalysisChannels bounds the channel count probeChannels will trust.
// Music is overwhelmingly mono/stereo; 8 covers 7.1 without letting a
// malformed header drive an absurd per-frame allocation.
const maxAnalysisChannels = 8

// decodeArgs builds the sox argv that decodes any supported source to
// headerless `channels`-channel 48 kHz little-endian float32 PCM on
// stdout.
//
//   - 48 kHz is the uniform analysis target and is load-bearing for the
//     BS.1770 loudness path: the K-weighting coefficients are the spec's
//     48 kHz values. sox inserts the rate effect automatically when the
//     source differs.
//   - Decoding at the SOURCE channel count (not a mono downmix) is
//     load-bearing for loudness: a mono downmix reads +3..+6 dB vs proper
//     multichannel R128. The peak envelope downmixes to mono separately
//     (downmixFrame), which is fine for a visual waveform.
//   - `-L` forces little-endian so the Go reader is deterministic across
//     architectures (ARM/Pi vs Intel).
//   - No `-G` (gain-guard): float output can't clip, and a guard pass
//     would shrink the displayed envelope.
func decodeArgs(srcAbs string, channels int) []string {
	return []string{
		srcAbs,
		"-t", "raw", "-e", "float", "-b", "32", "-L",
		"-c", strconv.Itoa(channels), "-r", "48000",
		"-",
	}
}

// probeChannels reads the source channel count via `sox --i -c`. It
// returns (channels, true) for a sane 1..maxAnalysisChannels value, or
// (1, false) when sox can't report it — the caller then decodes mono and
// SKIPS loudness rather than trusting a guessed channel layout (a wrong
// guess would silently store a biased ReplayGain). The probe is a cheap
// metadata read; the streaming decode is the expensive part.
func probeChannels(ctx context.Context, srcAbs string) (int, bool) {
	out, err := exec.CommandContext(ctx, "sox", "--i", "-c", srcAbs).Output()
	if err != nil {
		return 1, false
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil || n < 1 || n > maxAnalysisChannels {
		return 1, false
	}
	return n, true
}

// decodeFrames runs sox to decode srcAbs to `channels`-channel 48 kHz
// float PCM, grouping the interleaved stream into frames (one sample per
// channel) and calling onFrame for each, returning the total frame count
// (= per-channel sample count, the value the waveform duration math
// wants). PCM is processed in blocks (never buffered whole), so memory
// stays flat for long tracks.
//
// **The frame slice passed to onFrame is REUSED across calls** — callers
// must read it immediately and not retain it (the loudness meter copies
// into its ring; the peaker downmix reads in place). This mirrors the
// one-allocation budget StreamTracks holds on the read path.
//
// **Process reaping**: the sox process is killed + reaped on any early
// return / panic via the processReleased guard — an undrained stdout
// pipe would otherwise deadlock sox on a full write buffer and leak the
// process (a worker-slot leak in the pool). A non-zero sox exit
// (truncated / corrupt file) returns an error with redacted stderr so
// the caller commits nothing.
func decodeFrames(ctx context.Context, srcAbs string, channels int, onFrame func(frame []float64)) (totalFrames int64, err error) {
	if channels < 1 {
		channels = 1
	}
	cmd := exec.CommandContext(ctx, "sox", decodeArgs(srcAbs, channels)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return 0, fmt.Errorf("stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("start sox: %w", err)
	}
	processReleased := false
	defer func() {
		if !processReleased {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()

	frame := make([]float64, channels)
	fi := 0
	if serr := streamFloat32LE(stdout, func(s float32) {
		frame[fi] = float64(s)
		fi++
		if fi == channels {
			onFrame(frame)
			totalFrames++
			fi = 0
		}
	}); serr != nil {
		return totalFrames, fmt.Errorf("read pcm: %w", serr)
	}
	if werr := cmd.Wait(); werr != nil {
		processReleased = true
		return totalFrames, fmt.Errorf("sox: %w (stderr: %s)",
			werr, redactSoxErr(strings.TrimSpace(stderr.String()), srcAbs))
	}
	processReleased = true
	return totalFrames, nil
}

// streamFloat32LE reads little-endian float32 samples from r and calls
// fn for each, carrying a frame split across a read boundary via a
// 4-byte buffer. A trailing partial frame (1–3 bytes at EOF) is ignored.
func streamFloat32LE(r io.Reader, fn func(float32)) error {
	buf := make([]byte, 64*1024)
	var carry [4]byte
	rem := 0
	for {
		n, err := r.Read(buf)
		i := 0
		// Complete a carried partial frame from the front of this read.
		if rem > 0 && n > 0 {
			need := 4 - rem
			if n >= need {
				copy(carry[rem:], buf[:need])
				fn(f32LE(carry[:]))
				i = need
				rem = 0
			} else {
				copy(carry[rem:], buf[:n])
				rem += n
				i = n
			}
		}
		for ; i+4 <= n; i += 4 {
			fn(f32LE(buf[i : i+4]))
		}
		if i < n {
			rem = n - i
			copy(carry[:rem], buf[i:n])
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func f32LE(b []byte) float32 {
	return math.Float32frombits(binary.LittleEndian.Uint32(b))
}

// redactSoxErr strips the absolute source path from sox stderr (the
// bridge privacy contract bans surfacing absolute library paths),
// trims sox's leading prefixes, and caps the length. The source path
// is the only host-identifying token a decode-to-stdout invocation
// leaks (there's no output file path). Twin of
// internal/transcode.redactSoxErr — keep in lockstep.
func redactSoxErr(s, srcAbs string) string {
	if srcAbs != "" {
		s = strings.ReplaceAll(s, srcAbs, filepath.Base(srcAbs))
	}
	for _, prefix := range []string{"sox FAIL ", "sox WARN ", "sox: ", "exit status "} {
		if strings.HasPrefix(s, prefix) {
			s = strings.TrimPrefix(s, prefix)
			break
		}
	}
	const maxErrBytes = 4096
	if len(s) > maxErrBytes {
		s = fsutil.TrimPartialTrailingRune(s[:maxErrBytes]) + "…(truncated)"
	}
	return s
}
