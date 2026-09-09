package transcode

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"
)

// ffmpeg capability probe.
//
// FFmpegAvailable answers "are the binaries on PATH". That is the right
// question for the MP4 fallback — every ffmpeg build carries an MP4 demuxer
// and an ALAC decoder — and the wrong one for DSD: the four `dsd_*` decoders
// and the `dst` decoder are compile-time options that some packagings drop.
// A build without them accepts the file, prints "Decoder (codec dsd_lsbf_planar)
// not found" and exits non-zero — AFTER the pool has claimed a lane, probed
// geometry and started sox. Worse for the sweep: every DSD source would fail
// the same way on every pass until the variant-failure suppression hid it.
//
// So DSD routing is granted by ONE `ffmpeg -hide_banner -decoders` spawn,
// parsed FAIL-CLOSED: output that does not look like a decoder listing
// yields DecodersKnown=false, and DecodersKnown=false grants nothing. This
// is the opposite of ProbeSox's fail-OPEN posture, deliberately — a sox
// build that cannot read a file refuses it with its own diagnostic before
// any work is done, while an ffmpeg build without DSD decoders fails
// mid-job.
//
// The listing is stdout-only under -hide_banner (measured on ffmpeg 8.1 with
// an empty stderr), but CombinedOutput is parsed anyway: it costs nothing
// and covers a packaging that logs differently.

// ErrFFmpegMissing is returned by ProbeFFmpeg when ffmpeg or ffprobe is not
// on PATH. Wraps the names of the absent binaries so a diagnostic can say
// which one (a host with ffmpeg but no ffprobe is a real state).
var ErrFFmpegMissing = errors.New("ffmpeg toolchain not found on PATH")

// dsdDecoderNames are the four libavcodec DSD decoders — one per (bit order
// × planarity). DSF is LSB-first planar; DSDIFF is MSB-first interleaved;
// the other two cover the remaining container conventions. All four are
// required rather than the two the shipped containers use, because they are
// built as one family (`CONFIG_DSD_*_DECODER` share a source file) and a
// listing missing any of them is a listing from a build we do not
// understand.
var dsdDecoderNames = []string{"dsd_lsbf", "dsd_lsbf_planar", "dsd_msbf", "dsd_msbf_planar"}

// dstDecoderName is the arithmetic-coded DST decoder (DSDIFF with a DST
// chunk — the SACD ISO / DST-compressed DFF case). Tracked separately from
// the four DSD decoders: DST is heavier to decode and optional to support.
const dstDecoderName = "dst"

// ffmpegProbeTimeout bounds the `-decoders` spawn. The listing takes tens of
// milliseconds; anything past this is a hung wrapper, which must not hang a
// sweep or the doctor with it.
const ffmpegProbeTimeout = 2 * time.Second

// FFmpegInfo is what ProbeFFmpeg learned. The zero value is "nothing known"
// and grants nothing.
type FFmpegInfo struct {
	// Path is the resolved ffmpeg binary, empty when absent.
	Path string
	// Decoders is every decoder name the listing carried, in listing order.
	Decoders []string
	// DecodersKnown is true only when the listing parsed as a decoder table
	// with at least one row. False means the probe could not run or its
	// output was not recognised — and every capability below is false.
	DecodersKnown bool
	// HasDSD is true when ALL four dsd_* decoders are present.
	HasDSD bool
	// HasDST is true when the dst decoder is present.
	HasDST bool
	// MissingBinaries names which of ffmpeg / ffprobe are absent, in a
	// stable order; empty when both are on PATH.
	MissingBinaries []string
}

// Available reports whether both binaries are on PATH — the MP4 fallback's
// requirement, unchanged. DSD routing additionally needs HasDSD.
//
// The Path test is what makes the type's "the zero value grants nothing"
// promise true. MissingBinaries is nil on a zero value, so len()==0 alone
// answered TRUE for an FFmpegInfo nobody had probed — and every value that
// SHOULD answer true has Path set, because ProbeFFmpeg assigns it before
// probing and the missing-binaries early return does not. No production
// caller passed a zero value, so this changes no answer today; it removes a
// trap for the next one, starting with CanDecodeVia below.
func (i FFmpegInfo) Available() bool {
	return i.Path != "" && len(i.MissingBinaries) == 0
}

// ffmpegProbeCommand is the test seam for ProbeFFmpeg (the soxProbeCommand
// convention). Production code MUST NOT mutate it.
var ffmpegProbeCommand = exec.CommandContext

// ProbeFFmpeg runs `ffmpeg -hide_banner -decoders` once and reports which
// decoders the build carries. Returns ErrFFmpegMissing (wrapped) when a
// binary is absent; on any other failure the returned info carries
// DecodersKnown=false alongside the error, so a caller that ignores the
// error still fails closed.
func ProbeFFmpeg(ctx context.Context) (FFmpegInfo, error) {
	info := FFmpegInfo{MissingBinaries: MissingFFmpegBinaries()}
	// The missing list directly, not Available(): Path is not set yet, and
	// Available() now requires it. This is the constructor — it knows the
	// fact first-hand and does not need the predicate.
	if len(info.MissingBinaries) > 0 {
		return info, fmt.Errorf("%w: %s", ErrFFmpegMissing, strings.Join(info.MissingBinaries, ", "))
	}
	info.Path = resolveBin(ffmpegLookPath, "ffmpeg")

	ctx, cancel := context.WithTimeout(ctx, ffmpegProbeTimeout)
	defer cancel()
	cmd := ffmpegProbeCommand(ctx, info.Path, "-hide_banner", "-decoders")
	cmd.Env = append(os.Environ(), "LC_ALL=C", "LANG=C", "LANGUAGE=C")
	out, runErr := cmd.CombinedOutput()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return info, fmt.Errorf("ffmpeg -decoders timed out after %s; broken PATH wrapper or hung process", ffmpegProbeTimeout)
	}
	text := string(out)
	if strings.TrimSpace(text) == "" {
		if runErr != nil {
			return info, fmt.Errorf("ffmpeg -decoders failed: %w", runErr)
		}
		return info, fmt.Errorf("ffmpeg -decoders produced no output")
	}
	info.Decoders, info.DecodersKnown = parseFFmpegDecoders(text)
	if !info.DecodersKnown {
		if runErr != nil {
			return info, fmt.Errorf("ffmpeg -decoders failed: %w (output: %s)", runErr, firstLine(text))
		}
		return info, fmt.Errorf("ffmpeg -decoders output not recognised as a decoder listing (starts %q)", firstLine(text))
	}
	info.HasDSD, info.HasDST = ffmpegCapabilities(info.Decoders)
	return info, nil
}

// ffmpegCapabilities derives the two DSD-relevant capabilities from a
// decoder list. Pure, so the "all four, not any" rule is pinned on its own.
func ffmpegCapabilities(decoders []string) (hasDSD, hasDST bool) {
	hasDSD = true
	for _, n := range dsdDecoderNames {
		if !slices.Contains(decoders, n) {
			hasDSD = false
			break
		}
	}
	return hasDSD, slices.Contains(decoders, dstDecoderName)
}

// parseFFmpegDecoders extracts decoder names from a `-decoders` listing.
//
// The listing is a legend, a ` ------` separator, then one row per decoder:
// six flag characters (V/A/S for the media type, then F/S/X/B/D positions,
// `.` for unset), the decoder name, and a free-text description. Rows are
// taken only AFTER a separator has been seen, and only when the first field
// is a six-character flag word starting with V, A or S — the legend's own
// lines (` A..... = Audio`) never match because their second field is `=`.
//
// known is true iff a separator was seen AND at least one row parsed. Any
// other shape — a version banner, an error, an empty table — is "not
// recognised", and the caller treats that as no capability at all.
func parseFFmpegDecoders(text string) (decoders []string, known bool) {
	seenSeparator := false
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimRight(raw, "\r")
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "------") {
			seenSeparator = true
			continue
		}
		if !seenSeparator {
			continue
		}
		fields := strings.Fields(trimmed)
		if len(fields) < 2 {
			continue
		}
		flags := fields[0]
		if len(flags) != 6 || !strings.ContainsRune("VAS", rune(flags[0])) {
			continue
		}
		if fields[1] == "=" {
			continue
		}
		decoders = append(decoders, fields[1])
	}
	return decoders, seenSeparator && len(decoders) > 0
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	// Truncate by RUNE, not byte: an 80-byte cut can land inside a
	// multi-byte UTF-8 sequence (a localised ffmpeg message, a path with
	// an accented character) and leave an invalid string in an error the
	// operator reads (Gemini on PR #863).
	if r := []rune(s); len(r) > 80 {
		s = string(r[:80]) + "…"
	}
	return s
}

// ffmpegSnapshotTTL matches the sox probe cache: long enough that a batch
// walk pays one spawn, short enough that installing the toolchain is picked
// up without a restart.
const ffmpegSnapshotTTL = 30 * time.Second

var ffmpegSnap struct {
	mu   sync.Mutex
	path string
	at   time.Time
	info FFmpegInfo
}

// FFmpegSnapshot returns the cached ProbeFFmpeg result, re-probing when the
// cache is older than ffmpegSnapshotTTL or the resolved ffmpeg path changed
// (so a toolchain installed after the last probe, or a test seam flip, is
// seen). Binary presence is re-checked LIVE on every call — it costs two
// LookPaths — so "ffmpeg vanished from PATH" is never masked by a cached
// capability. A probe that fails is cached too (as DecodersKnown=false), the
// same 30 s of quiet the sox cache gives a broken install.
//
// Serialised under one mutex on purpose: concurrent first callers (a batch
// walk fanning out) share one spawn instead of each paying it.
func FFmpegSnapshot() FFmpegInfo {
	if missing := MissingFFmpegBinaries(); len(missing) > 0 {
		return FFmpegInfo{MissingBinaries: missing}
	}
	path := resolveBin(ffmpegLookPath, "ffmpeg")
	ffmpegSnap.mu.Lock()
	defer ffmpegSnap.mu.Unlock()
	if ffmpegSnap.path == path && !ffmpegSnap.at.IsZero() && time.Since(ffmpegSnap.at) < ffmpegSnapshotTTL {
		return ffmpegSnap.info
	}
	info, _ := ProbeFFmpeg(context.Background())
	ffmpegSnap.path, ffmpegSnap.at, ffmpegSnap.info = path, time.Now(), info
	return info
}

// resetFFmpegSnapshotForTest drops the cached probe so a test that swaps the
// seams starts from a cold cache. Tests only.
func resetFFmpegSnapshotForTest() {
	ffmpegSnap.mu.Lock()
	defer ffmpegSnap.mu.Unlock()
	ffmpegSnap.path, ffmpegSnap.at, ffmpegSnap.info = "", time.Time{}, FFmpegInfo{}
}
