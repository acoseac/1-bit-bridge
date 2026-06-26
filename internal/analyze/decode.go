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

// decoderTool selects which external program decodes the source to raw PCM.
// sox is the primary decoder on every host; ffmpeg is the fallback for
// containers sox can't read (notably AAC/m4a — Debian/Ubuntu sox ships no AAC
// handler, so a Linux bridge fails every .m4a with "no handler for file
// extension `m4a'"). Both produce byte-identical little-endian float32 48 kHz
// interleaved PCM, so the streaming reader is decoder-agnostic.
type decoderTool int

const (
	decoderSox decoderTool = iota
	decoderFFmpeg
)

func (d decoderTool) String() string {
	if d == decoderFFmpeg {
		return "ffmpeg"
	}
	return "sox"
}

// ffmpegLookPath / ffprobeLookPath are the seams tests override to force the
// ffmpeg toolchain present/absent without depending on the host having it
// installed. Production resolves the real binaries on PATH. (Same DI shape as
// renameFunc / commandContext elsewhere — production MUST NOT mutate them.)
var (
	ffmpegLookPath  = func() (string, error) { return exec.LookPath("ffmpeg") }
	ffprobeLookPath = func() (string, error) { return exec.LookPath("ffprobe") }
	soxLookPath     = func() (string, error) { return exec.LookPath("sox") }
)

// ffmpegToolsAvailable reports whether BOTH ffmpeg (decode) and ffprobe
// (channel probe) are on PATH — the fallback needs both. Cheap (two PATH
// stats) and only called when sox has already failed to probe a file, so the
// common all-sox path pays nothing.
func ffmpegToolsAvailable() bool {
	if _, err := ffmpegLookPath(); err != nil {
		return false
	}
	_, err := ffprobeLookPath()
	return err == nil
}

// resolveBin returns the absolute path the seam resolved (so the binary that
// gets EXEC'd is exactly the one the availability check found — no second PATH
// lookup, no check-vs-exec TOCTOU, and the test seam fully controls the binary
// rather than only gating availability). Falls back to the bare name when the
// lookup fails (exec.CommandContext then PATH-resolves it, as before).
func resolveBin(look func() (string, error), fallback string) string {
	if p, err := look(); err == nil && p != "" {
		return p
	}
	return fallback
}

// ffmpegDecodeArgs builds the ffmpeg argv that decodes srcAbs to the SAME
// headerless little-endian float32 48 kHz `channels`-channel PCM on stdout that
// decodeArgs (sox) produces — so streamFloat32LE reads either identically.
//
//   - `-map 0:a:0` takes the first AUDIO stream only: m4a/mp4 frequently embeds
//     a cover image as a video stream, and without the map ffmpeg would try to
//     encode that into the raw-PCM output and fail.
//   - `-f f32le` is the raw little-endian float32 format (codec pcm_f32le) —
//     the exact wire sox emits with `-t raw -e float -b 32 -L`.
//   - `-ac/-ar` pin the source channel count + the 48 kHz analysis target
//     (load-bearing for the BS.1770 K-weighting), matching sox's `-c/-r`.
//   - `-nostdin` so ffmpeg never blocks reading the controlling terminal;
//     `-loglevel error` keeps stderr to real failures for redaction.
//
// NOTE on truncated sources: ffmpeg exits 0 even when it conceals a mid-stream
// decode error, so a truncated-but-openable faststart m4a (moov atom at the
// front — e.g. a partially-uploaded rclone-to-B2 sync) would decode only its
// first N seconds yet exit cleanly. `-xerror` is deliberately NOT used (it
// would also abort on a single concealed bad frame in an otherwise-complete
// file, permanently failing analysis for it). Instead decodeFrames compares the
// decoded length against the ffprobe-reported duration and rejects a materially
// short decode — distinguishing "truncated" (reject, self-heals on re-upload)
// from "glitchy but complete" (accept). See decodedShortOfDuration.
func ffmpegDecodeArgs(srcAbs string, channels int) []string {
	return []string{
		"-nostdin", "-hide_banner", "-loglevel", "error",
		"-i", srcAbs,
		"-map", "0:a:0",
		"-ac", strconv.Itoa(channels), "-ar", "48000",
		"-f", "f32le",
		"-",
	}
}

// ffprobeChannels reads the source channel count via ffprobe's first audio
// stream. Same (channels, ok) contract as probeChannels' sox path: a value
// outside 1..maxAnalysisChannels (or any probe failure) yields (1, false) so
// the caller decodes mono and SKIPS loudness rather than trusting a guess.
func ffprobeChannels(ctx context.Context, srcAbs string) (int, bool) {
	out, err := exec.CommandContext(ctx, resolveBin(ffprobeLookPath, "ffprobe"),
		"-v", "error", "-select_streams", "a:0",
		"-show_entries", "stream=channels",
		"-of", "default=nw=1:nk=1", srcAbs).Output()
	if err != nil {
		return 1, false
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil || n < 1 || n > maxAnalysisChannels {
		return 1, false
	}
	return n, true
}

// parseDuration parses a probe's seconds-as-float stdout, returning 0 for any
// invalid value (parse error, "N/A", non-positive, or non-finite) so the caller
// treats it as "duration unknown, skip the truncation check" rather than
// rejecting. Shared by ffprobeDuration + soxDuration so both probes stay in
// lockstep on the validity rule.
func parseDuration(out []byte) float64 {
	d, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if err != nil || d <= 0 || math.IsInf(d, 0) || math.IsNaN(d) {
		return 0
	}
	return d
}

// ffprobeDuration reads the container duration (seconds) via ffprobe's
// format=duration. Returns 0 on ANY failure — the caller treats 0 as "duration
// unknown, skip the truncation check" rather than rejecting, so a probe miss
// can never block a legitimately-complete file. Only called on the ffmpeg
// fallback path (m4a/mp4 containers, which carry a format duration).
func ffprobeDuration(ctx context.Context, srcAbs string) float64 {
	out, err := exec.CommandContext(ctx, resolveBin(ffprobeLookPath, "ffprobe"),
		"-v", "error", "-show_entries", "format=duration",
		"-of", "default=nw=1:nk=1", srcAbs).Output()
	if err != nil {
		return 0
	}
	return parseDuration(out)
}

// soxDuration reads the source duration (seconds) via `sox --i -D` for the sox
// decode path. Returns 0 on ANY failure (so the caller skips the truncation
// check rather than rejecting). sox's duration is reliable for the formats it
// handles: FLAC/WAV/AIFF carry it in the header, and sox reports an accurate
// duration for VBR MP3 too — both Xing-tagged AND header-less files match their
// decoded length (verified against sox 14.x: an 8.0s VBR MP3 with and without
// the Info tag both probe to within <0.1% of the decoded length), so it does
// not false-reject complete VBR MP3s.
//
// Resolves sox to its absolute path via resolveBin (the soxLookPath seam) so
// the exec target is fixed rather than PATH-relative — matching how this file
// already resolves ffmpeg/ffprobe and the filepath.IsAbs defense-in-depth used
// for the Tailscale CLI.
func soxDuration(ctx context.Context, srcAbs string) float64 {
	out, err := exec.CommandContext(ctx, resolveBin(soxLookPath, "sox"), "--i", "-D", srcAbs).Output()
	if err != nil {
		return 0
	}
	return parseDuration(out)
}

// minDecodedFraction is the lower bound on (decoded length / probed duration)
// for a decode to be accepted. Below it the source is treated as truncated.
// 0.90 leaves generous headroom for legitimate encoder-delay/padding,
// container-duration rounding, and VBR estimate drift (all sub-second / sub-1%
// in practice) while still catching a real truncation — the field-reported case
// decoded ~58% of its declared duration.
const minDecodedFraction = 0.90

// decodedShortOfDuration reports whether a CLEAN decode that produced
// totalFrames at AnalysisSampleRate fell materially short of the probed
// expectedSec — the signature of a truncated-but-openable source the decoder
// concealed and exited 0 on (ffmpeg conceals a mid-stream error; sox exits 0
// after a FLAC LOST_SYNC). Returns false (no rejection) when expectedSec <= 0
// (duration unknown — commit as before). A glitchy-but-COMPLETE file decodes to
// full length and passes, so a mid-stream FLAC glitch that resyncs to EOF is
// NOT treadmilled — that case is exactly why this is a duration check and not
// stderr-error matching, which fires identically on truncated and resynced-
// complete files.
func decodedShortOfDuration(expectedSec float64, totalFrames int64) bool {
	if expectedSec <= 0 {
		return false
	}
	decodedSec := float64(totalFrames) / float64(AnalysisSampleRate)
	return decodedSec < expectedSec*minDecodedFraction
}

// decodeCommand returns the binary + argv for the chosen decoder.
func decodeCommand(tool decoderTool, srcAbs string, channels int) (string, []string) {
	if tool == decoderFFmpeg {
		return resolveBin(ffmpegLookPath, "ffmpeg"), ffmpegDecodeArgs(srcAbs, channels)
	}
	return resolveBin(soxLookPath, "sox"), decodeArgs(srcAbs, channels)
}

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

// probeChannels reads the source channel count AND picks the decoder for the
// streaming decode that follows. sox is tried first (the primary decoder on
// every host); when sox can't report the layout — which on a Linux bridge
// includes every AAC/m4a file (no sox AAC handler) — and the ffmpeg toolchain
// is present, it re-probes via ffprobe and selects ffmpeg for the decode.
//
// Returns (channels, ok, tool, expectedSec): ok==false means the channel count
// is unknown, so the caller decodes mono and SKIPS loudness rather than trusting
// a guessed layout (a wrong guess would silently store a biased ReplayGain). The
// decision is made HERE, off the cheap probe, so the expensive streaming decode
// never has to fail-and-retry. expectedSec is the probed duration (sox `--i -D`
// on the sox path, ffprobe `format=duration` on the ffmpeg path) used by
// decodeFrames to reject a truncated decode; 0 means "unknown, skip the check".
func probeChannels(ctx context.Context, srcAbs string) (int, bool, decoderTool, float64) {
	if out, err := exec.CommandContext(ctx, resolveBin(soxLookPath, "sox"), "--i", "-c", srcAbs).Output(); err == nil {
		if n, perr := strconv.Atoi(strings.TrimSpace(string(out))); perr == nil && n >= 1 && n <= maxAnalysisChannels {
			// Probe the sox duration off the same cheap pre-decode step so
			// decodeFrames can reject a truncated source (e.g. a FLAC that sox
			// opens via its intact STREAMINFO header, decodes ~half, and exits 0
			// on LOST_SYNC). 0 on probe miss → no check (commit as before).
			return n, true, decoderSox, soxDuration(ctx, srcAbs)
		}
		// sox answered but with an unparseable / out-of-range count — fall
		// through to the ffmpeg path (it may still decode cleanly).
	}
	if ffmpegToolsAvailable() {
		// Probe the duration once here (off the same cheap pre-decode step) so
		// decodeFrames can spot a truncated-but-openable source after a clean
		// ffmpeg exit. 0 on probe miss → no truncation check (commit as before).
		dur := ffprobeDuration(ctx, srcAbs)
		if n, ok := ffprobeChannels(ctx, srcAbs); ok {
			return n, true, decoderFFmpeg, dur
		}
		// ffmpeg present but the channel probe failed: decode mono via ffmpeg
		// + skip loudness (same contract as the sox channelsOK=false path).
		return 1, false, decoderFFmpeg, dur
	}
	return 1, false, decoderSox, 0
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
//
// **ffmpeg truncation guard**: ffmpeg can exit 0 after concealing a
// mid-stream decode error, so a truncated-but-openable faststart m4a would
// otherwise return (partialFrames, nil). When expectedSec > 0 (the ffmpeg
// path's ffprobe duration), a decode that falls materially short of it is
// rejected as truncated (decodedShortOfDuration) so nothing is committed and
// the candidate re-flows until fully uploaded. sox callers pass expectedSec==0
// and are unaffected.
func decodeFrames(ctx context.Context, srcAbs string, channels int, tool decoderTool, expectedSec float64, onFrame func(frame []float64)) (totalFrames int64, err error) {
	if channels < 1 {
		channels = 1
	}
	name, args := decodeCommand(tool, srcAbs, channels)
	cmd := exec.CommandContext(ctx, name, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return 0, fmt.Errorf("stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("start %s: %w", name, err)
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
		// redactSoxErr strips the absolute path (the privacy-load-bearing part);
		// its sox-prefix trimming is a harmless no-op on ffmpeg stderr.
		return totalFrames, fmt.Errorf("%s: %w (stderr: %s)",
			tool, werr, redactSoxErr(strings.TrimSpace(stderr.String()), srcAbs))
	}
	processReleased = true
	// A clean exit does NOT guarantee a complete decode: ffmpeg conceals a
	// mid-stream error and exits 0, and sox exits 0 after a FLAC LOST_SYNC even
	// on a truncated file. Reject when the decoded length is materially short of
	// the probed duration so a partial waveform is never committed (it would be
	// keyed to the truncated file's mtime+size and never re-analyzed). A
	// glitchy-but-COMPLETE file decodes to full length and is accepted: a
	// mid-stream FLAC glitch resyncs to EOF and prints the SAME
	// `sox FAIL ... LOST_SYNC` as a real truncation, so stderr matching cannot
	// tell them apart — the decoded-length check can. Unknown duration: no-op.
	if decodedShortOfDuration(expectedSec, totalFrames) {
		return totalFrames, fmt.Errorf("%s: decoded %.1fs of %.1fs probed — source appears truncated",
			tool, float64(totalFrames)/float64(AnalysisSampleRate), expectedSec)
	}
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
