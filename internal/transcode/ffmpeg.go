package transcode

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// ffmpeg-fallback decode for sources sox cannot read.
//
// sox is the only decoder the upscale pipeline has ever had, and no stock
// build carries an MP4 demuxer. ALAC is the one LOSSLESS format that lands
// there: manifest.IsLossyCodec doesn't list it, canSetBitsPerSample allows
// it, and OptimizeEligible names it outright — so it clears every gate and
// then cannot be decoded. Before PR #440 populated MP4 audio geometry the
// gate refused it early for a different reason; #572 restored an honest
// refusal via CanDecode. This file makes it WORK instead, by decoding with
// ffmpeg and piping PCM into the same sox chain.
//
// Everything sox can read still goes to sox directly. The fallback is
// deliberately NOT "anything sox refuses" — see ffmpegRoutableExt.

// ErrFFmpegDecodeIncomplete is returned when the piped decode produced a
// materially different duration than the source container reports. It means
// the sidecar was NOT written, so the candidate re-flows on a later sweep
// once the source is whole.
var ErrFFmpegDecodeIncomplete = errors.New("ffmpeg decode length disagrees with the source duration")

// ffmpegLookPath / ffprobeLookPath are the seams tests override to force the
// ffmpeg toolchain present or absent without depending on the host having it.
// Production code MUST NOT mutate them (the renameFunc / soxLookPath
// convention).
var (
	ffmpegLookPath  = func() (string, error) { return exec.LookPath("ffmpeg") }
	ffprobeLookPath = func() (string, error) { return exec.LookPath("ffprobe") }
)

// ffmpegRoutableExt is the closed set of source extensions the fallback will
// handle: the MP4 container family, which is where ALAC lives.
//
// It is a allowlist rather than "route whatever sox refused" on purpose. The
// upstream gates already exclude lossy (manifest.IsLossyCodec) and DSD, so
// anything else reaching a refusal is a shape neither decoder was chosen for,
// and handing it to ffmpeg would convert an honest "sox can't read this" into
// a mysterious mid-job failure. Widening this set is a deliberate act with a
// test behind it, not a side effect.
var ffmpegRoutableExt = map[string]bool{
	".m4a": true,
	".mp4": true,
	".m4b": true,
	".m4p": true,
}

// decodeRoute names which decoder a source takes.
type decodeRoute int

const (
	routeNone       decodeRoute = iota // neither decoder can read it
	routeSoxDirect                     // sox reads the file itself
	routeFFmpegPipe                    // ffmpeg decodes, sox resamples from stdin
)

func (r decodeRoute) String() string {
	switch r {
	case routeSoxDirect:
		return "sox"
	case routeFFmpegPipe:
		return "ffmpeg+sox"
	default:
		return "none"
	}
}

// FFmpegAvailable reports whether BOTH ffmpeg (decode) and ffprobe (geometry
// + duration) are on PATH. Both are required: without ffprobe the pipe cannot
// be told the source rate and channel count, and the truncation guard has
// nothing to compare against.
func FFmpegAvailable() bool {
	return len(MissingFFmpegBinaries()) == 0
}

// MissingFFmpegBinaries names which of the two are absent, in a stable order.
//
// It exists so a diagnostic can say what is actually wrong: a host with ffmpeg
// but no ffprobe is a real state (some minimal distro packages split them), and
// telling that operator to "install ffmpeg" sends them to look at a binary they
// already have.
func MissingFFmpegBinaries() []string {
	var missing []string
	if _, err := ffmpegLookPath(); err != nil {
		missing = append(missing, "ffmpeg")
	}
	if _, err := ffprobeLookPath(); err != nil {
		missing = append(missing, "ffprobe")
	}
	return missing
}

// resolveBin returns the absolute path the seam resolved, falling back to the
// bare name when the lookup failed or returned a PATH-relative result. Mirrors
// internal/analyze's helper: an absolute exec target is defense-in-depth, and
// it also keeps new exec sites clear of SonarCloud go:S4036.
func resolveBin(look func() (string, error), fallback string) string {
	if p, err := look(); err == nil && filepath.IsAbs(p) {
		return p
	}
	return fallback
}

// decodeRouteFor picks the decoder for one source. Pure, so the policy is
// testable without either binary installed.
//
// sox-direct wins whenever sox can read the file — the fallback exists to
// widen coverage, never to change how an already-working source is decoded.
func decodeRouteFor(info SoxInfo, ffmpegOK bool, sourcePath string) decodeRoute {
	if info.CanDecode(sourcePath) {
		return routeSoxDirect
	}
	if ffmpegOK && ffmpegRoutableExt[strings.ToLower(filepath.Ext(sourcePath))] {
		return routeFFmpegPipe
	}
	return routeNone
}

// sourceGeometry is what ffprobe reports about the source: the PCM shape the
// pipe must be described with, plus the container duration the completeness
// guard compares against.
type sourceGeometry struct {
	SampleRate int
	Channels   int
	Duration   float64 // seconds; 0 when unknown
}

// probeSourceGeometry reads sample rate, channel count and container duration
// in ONE ffprobe invocation.
//
// Rate and channels are REQUIRED — the raw pipe carries no header, so a
// missing value cannot be guessed and the job is refused rather than decoded
// at an invented rate. Duration is optional: 0 means "unknown", and the
// caller then skips the completeness check rather than rejecting, so a probe
// miss can never block a legitimately-complete file (the same contract
// internal/analyze's ffprobeDuration documents).
func probeSourceGeometry(ctx context.Context, srcAbs string) (sourceGeometry, error) {
	out, err := exec.CommandContext(ctx, resolveBin(ffprobeLookPath, "ffprobe"),
		"-v", "error",
		"-select_streams", "a:0",
		"-show_entries", "stream=sample_rate,channels:format=duration",
		"-of", "default=nw=1", srcAbs).Output()
	if err != nil {
		return sourceGeometry{}, fmt.Errorf("ffprobe source: %w", err)
	}
	var g sourceGeometry
	for _, line := range strings.Split(string(out), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch k {
		case "sample_rate":
			if n, err := strconv.Atoi(v); err == nil {
				g.SampleRate = n
			}
		case "channels":
			if n, err := strconv.Atoi(v); err == nil {
				g.Channels = n
			}
		case "duration":
			g.Duration = parseProbeDuration(v)
		}
	}
	if g.SampleRate <= 0 || g.Channels <= 0 {
		return sourceGeometry{}, fmt.Errorf("ffprobe source: no usable audio geometry (rate=%d channels=%d)", g.SampleRate, g.Channels)
	}
	return g, nil
}

// probeOutputDuration reads the duration of the sidecar sox just wrote.
//
// ffprobe rather than `sox --i -D` because the temp path ends in `.tmp`, and
// sox picks its handler from the extension — the same reason SoxArgs has to
// force `-t flac` on the output. ffprobe sniffs content. Returns 0 on any
// failure, which the caller treats as "skip the check".
func probeOutputDuration(ctx context.Context, path string) float64 {
	out, err := exec.CommandContext(ctx, resolveBin(ffprobeLookPath, "ffprobe"),
		"-v", "error", "-show_entries", "format=duration",
		"-of", "default=nw=1:nk=1", path).Output()
	if err != nil {
		return 0
	}
	return parseProbeDuration(strings.TrimSpace(string(out)))
}

// parseProbeDuration turns ffprobe's seconds-as-float into a usable value,
// returning 0 for anything invalid ("N/A", non-positive, non-finite) so every
// caller shares one definition of "duration unknown".
func parseProbeDuration(s string) float64 {
	d, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || d <= 0 || math.IsInf(d, 0) || math.IsNaN(d) {
		return 0
	}
	return d
}

// durationTolerance bounds how far the produced sidecar may differ from the
// source's reported duration before the job is refused.
//
// MEASURED, not chosen: across five complete ALAC decodes (44.1/48/96/192 kHz,
// mono and stereo, non-round durations) the ratio was EXACTLY 1.000000 every
// time — this path resamples, it does not re-time. A half-truncated faststart
// .m4a measured 0.488. The gap is enormous, so 2% is generous and still
// catches every realistic partial upload.
//
// The check is TWO-SIDED, and the upper bound is not symmetry for its own
// sake: describing the raw pipe with a rate LOWER than the source's makes the
// output LONGER (measured: a 44.1 kHz source declared as 22050 produced
// exactly 2.0x the duration), which a lower-bound-only guard accepts while
// writing a half-speed variant. internal/analyze's one-sided form is correct
// for its own purpose; this path needs both.
const durationTolerance = 0.02

// decodeLengthDisagrees reports whether a produced duration is materially
// different from the source's. Unknown (0) on either side means "no verdict"
// — never a rejection.
func decodeLengthDisagrees(sourceSec, producedSec float64) bool {
	if sourceSec <= 0 || producedSec <= 0 {
		return false
	}
	ratio := producedSec / sourceSec
	return ratio < 1-durationTolerance || ratio > 1+durationTolerance
}

// ffmpegDecodeArgs builds the ffmpeg argv that decodes srcAbs to headerless
// little-endian float32 PCM on stdout, at the source's own rate and channel
// count.
//
//   - `-map 0:a:0` takes the first AUDIO stream only. An .m4a routinely embeds
//     cover art as a video stream, and without the map ffmpeg tries to encode
//     that into the raw output and fails.
//   - `-f f32le` keeps the pipe lossless end to end: sox's resampler works in
//     float internally, so handing it float costs nothing and there is no
//     intermediate quantisation.
//   - `-nostdin` so ffmpeg never blocks on the controlling terminal;
//     `-loglevel error` keeps stderr to real failures.
//
// Raw rather than `-f wav` deliberately. ffmpeg cannot seek back to patch a
// WAV header on a pipe, so it writes RIFF size 0xFFFFFFFF, and sox then prints
// `WARN wav: Premature EOF on .wav input file` on EVERY successful job —
// noise, and worse, a warning indistinguishable from the real truncation this
// path guards against. Raw also has no 4 GiB ceiling.
func ffmpegDecodeArgs(srcAbs string) []string {
	return []string{
		"-nostdin", "-hide_banner", "-loglevel", "error",
		"-i", srcAbs,
		"-map", "0:a:0",
		"-f", "f32le",
		"-",
	}
}

// soxStdinInputArgs describes the headerless stream ffmpeg is about to write,
// replacing the source path in the sox argv. Everything downstream of the
// input — gain guard, target bits, FLAC output, rate, dither — is unchanged,
// so a sidecar produced through the pipe is the same chain applied to the same
// samples.
func soxStdinInputArgs(g sourceGeometry) []string {
	return []string{
		"-t", "raw", "-e", "float", "-b", "32", "-L",
		"-r", strconv.Itoa(g.SampleRate),
		"-c", strconv.Itoa(g.Channels),
		"-",
	}
}

// runFFmpegPipe decodes j's source with ffmpeg and resamples it with sox
// reading that PCM from stdin, writing the same temp file RunSox would.
//
// Returns the produced duration so the caller can apply the completeness
// guard against the source's, and the combined stderr of both processes for
// the error path.
func runFFmpegPipe(ctx context.Context, soxArgs []string, g sourceGeometry, srcAbs string) error {
	ff := exec.CommandContext(ctx, resolveBin(ffmpegLookPath, "ffmpeg"), ffmpegDecodeArgs(srcAbs)...)
	sx := exec.CommandContext(ctx, resolveBin(func() (string, error) { return soxLookPath("sox") }, "sox"), soxArgs...)

	pipe, err := ff.StdoutPipe()
	if err != nil {
		return fmt.Errorf("ffmpeg stdout pipe: %w", err)
	}
	sx.Stdin = pipe

	var ffErr, sxErr bytes.Buffer
	ff.Stderr = &ffErr
	sx.Stderr = &sxErr

	if err := ff.Start(); err != nil {
		return fmt.Errorf("start ffmpeg: %w", err)
	}
	if err := sx.Start(); err != nil {
		// ffmpeg is already running with nothing to read its output;
		// kill it rather than leaving it to fill a pipe buffer forever.
		_ = ff.Process.Kill()
		_ = ff.Wait()
		return fmt.Errorf("start sox: %w", err)
	}

	// Wait on the READER first: os/exec documents that calling Wait on the
	// writer before the pipe is drained is a race. If sox died early ffmpeg
	// takes EPIPE and exits on its own; the Kill below is the belt for a
	// build that ignores it.
	soxWaitErr := sx.Wait()
	if soxWaitErr != nil && ff.Process != nil {
		_ = ff.Process.Kill()
	}
	ffWaitErr := ff.Wait()

	if soxWaitErr != nil {
		return fmt.Errorf("sox: %w (sox stderr: %s) (ffmpeg stderr: %s)",
			soxWaitErr, strings.TrimSpace(sxErr.String()), strings.TrimSpace(ffErr.String()))
	}
	// ffmpeg's exit code alone is NOT a completeness signal — it exits 0 on a
	// truncated-but-openable source, concealing the mid-stream error (measured
	// on a half-truncated faststart .m4a). That is what the duration guard is
	// for. A NON-zero exit is still a real failure and is reported here.
	if ffWaitErr != nil {
		return fmt.Errorf("ffmpeg: %w (stderr: %s)", ffWaitErr, strings.TrimSpace(ffErr.String()))
	}
	return nil
}
