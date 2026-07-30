package acoustid

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

// DefaultLengthSeconds is how many seconds of audio fpcalc fingerprints.
//
// 120 is fpcalc's own default AND the window AcoustID's reference fingerprints
// were built at, so this is a compatibility constant, not a tuning knob:
// shortening it trades match confidence for CPU. It does NOT reduce egress on
// a network-backed library — rclone's default --vfs-read-chunk-size (128 MiB)
// exceeds every music file, so the first read of byte 0 already fetches the
// whole object regardless of how little of it we decode.
const DefaultLengthSeconds = 120

// probeTimeout bounds the `fpcalc -version` spawn. Mirrors ProbeSox's 2s cap:
// long enough for a cold binary on slow storage, short enough that a broken
// PATH wrapper can't wedge startup.
const probeTimeout = 2 * time.Second

// ErrFpcalcMissing is returned by Probe/Precheck when fpcalc isn't on PATH.
// Test for it via errors.Is so callers can surface a targeted install hint.
var ErrFpcalcMissing = errors.New("fpcalc binary not found on PATH")

// ErrUnreadable reports that fpcalc could not decode the source at all
// (missing, truncated beyond recovery, not audio, unsupported codec). It is a
// property of the FILE, never of the toolchain — callers skip the track and
// move on rather than treating it as a fingerprinting outage.
var ErrUnreadable = errors.New("acoustid: source file could not be decoded")

// fpcalcLookPath / fpcalcCommand are the test seams — production points them
// at exec.LookPath / exec.CommandContext; tests inject a fake binary path and
// canned output so the full flow runs deterministically WITHOUT fpcalc on the
// host (mirrors transcode.soxLookPath/soxProbeCommand, tailscale.commandContext).
// Tests MUST restore both via t.Cleanup; production code MUST NOT mutate them.
var (
	fpcalcLookPath = exec.LookPath
	fpcalcCommand  = exec.CommandContext
)

// Info is what Probe learned about the installed fpcalc.
type Info struct {
	Path    string
	Version string // e.g. "1.6.1"; "" if the banner couldn't be parsed
}

// Probe locates fpcalc on PATH and reads its version banner.
//
// Deliberately simpler than transcode.ProbeSox, which also parses a format
// list because the bridge forces `-t flac` on every sox job and a FLAC-less
// build fails every one of them at runtime. fpcalc has no equivalent
// failure mode: it links FFmpeg and decodes whatever FFmpeg decodes, so
// "runnable" is the whole question. A version that can't be parsed is
// cosmetic and never gates anything.
//
// Bounded by probeTimeout wrapped around the INCOMING ctx, so a parent
// cancellation aborts early while the cap still applies to
// Precheck()'s context.Background().
func Probe(ctx context.Context) (Info, error) {
	var info Info
	path, err := fpcalcLookPath("fpcalc")
	if err != nil {
		return info, fmt.Errorf("%w: %v", ErrFpcalcMissing, err)
	}
	info.Path = path

	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	cmd := fpcalcCommand(ctx, path, "-version")
	// Locale-pinned so a translated banner can't defeat the parse.
	cmd.Env = append(os.Environ(), "LC_ALL=C", "LANG=C", "LANGUAGE=C")
	// CombinedOutput: some builds print the banner to stderr.
	out, runErr := cmd.CombinedOutput()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return info, fmt.Errorf("fpcalc -version timed out after %s; broken PATH wrapper or hung process", probeTimeout)
	}
	text := string(out)
	if strings.TrimSpace(text) == "" {
		if runErr != nil {
			return info, fmt.Errorf("fpcalc -version failed: %w", runErr)
		}
		return info, fmt.Errorf("fpcalc -version produced no output")
	}
	info.Version = parseFpcalcVersion(text)
	return info, nil
}

// Precheck returns nil if fpcalc is on PATH and runnable. Thin wrapper over
// Probe (one implementation, no duplication) for the boolean-only call sites.
func Precheck() error {
	_, err := Probe(context.Background())
	return err
}

// parseFpcalcVersion pulls the version token out of fpcalc's banner, which
// reads "fpcalc version 1.6.1 (FFmpeg Lavc62.28.102 ...)". Best-effort and
// cosmetic: returns "" when absent, or when the token carries no digit.
func parseFpcalcVersion(text string) string {
	const marker = "version "
	i := strings.Index(text, marker)
	if i < 0 {
		return ""
	}
	tok := strings.TrimSpace(text[i+len(marker):])
	if end := strings.IndexFunc(tok, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '('
	}); end >= 0 {
		tok = tok[:end]
	}
	if !strings.ContainsFunc(tok, func(r rune) bool { return r >= '0' && r <= '9' }) {
		return "" // no digit — not a usable version
	}
	return tok
}

// Fingerprint is one fpcalc result.
type Fingerprint struct {
	// Value is the compressed base64 Chromaprint fingerprint, the exact form
	// AcoustID's `fingerprint` query parameter expects.
	Value string

	// Duration is what fpcalc decoded, in seconds. This is the DECODE's
	// length, not necessarily the file's: comparing it against the bridge's
	// own container-derived Track.Duration is what catches a truncated source
	// whose front half still decodes cleanly.
	Duration float64

	// DistinctB64 counts distinct characters in Value, and is the gate's
	// signal for "does this audio carry enough information to identify
	// anything". See minDistinctB64Chars for the calibration.
	DistinctB64 int

	// BytesRead is how many bytes were fed to fpcalc, and is set only by
	// ComputeFromPrefix — the ordinary path lets fpcalc open the file itself,
	// so there is nothing to count. Zero therefore means "not measured",
	// never "read nothing".
	BytesRead int64
}

// DurationSeconds rounds Duration to whole seconds — the form AcoustID's
// `duration` query parameter takes.
func (f Fingerprint) DurationSeconds() int {
	if f.Duration <= 0 {
		return 0
	}
	return int(f.Duration + 0.5)
}

// fpcalcJSON is the `-json` output shape: {"duration": 45.00, "fingerprint": "AQAB..."}.
//
// NOTE the shape is conditional on flags we deliberately do NOT pass: with
// `-raw` the same `fingerprint` key becomes an ARRAY of integers rather than a
// string, so this struct would fail to decode. Compute never passes -raw (see
// DistinctB64), and any future flag change must re-check this.
type fpcalcJSON struct {
	Duration    float64 `json:"duration"`
	Fingerprint string  `json:"fingerprint"`
}

// Compute fingerprints absPath by shelling `fpcalc -json -length <n>`.
//
// ONE spawn, deliberately. The obvious way to measure fingerprint entropy is
// `-raw`, which emits the underlying sub-fingerprint integers — but `-raw` and
// the compressed base64 form are mutually exclusive output modes, and AcoustID
// needs the compressed form, so using it would mean decoding every file twice.
// Distinct-base64-character count is measured to separate just as cleanly at a
// fraction of the CPU (see minDistinctB64Chars).
//
// A non-zero exit or an unparseable payload yields ErrUnreadable: fpcalc exits
// 2 with "ERROR: ..." on stderr for anything it cannot open or decode, and
// that is a property of the file, not an outage. Errors are redacted of the
// absolute path before they escape.
func Compute(ctx context.Context, absPath string, length time.Duration) (Fingerprint, error) {
	secs := int(length.Seconds())
	if secs <= 0 {
		secs = DefaultLengthSeconds
	}
	path, err := fpcalcLookPath("fpcalc")
	if err != nil {
		return Fingerprint{}, fmt.Errorf("%w: %v", ErrFpcalcMissing, err)
	}

	cmd := fpcalcCommand(ctx, path, "-json", "-length", fmt.Sprint(secs), absPath)
	cmd.Env = append(os.Environ(), "LC_ALL=C", "LANG=C", "LANGUAGE=C")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, runErr := cmd.Output()

	// Cancellation is the caller's business, not a bad file — surface it
	// verbatim so a shutting-down sweeper doesn't record a decode failure.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return Fingerprint{}, ctxErr
	}
	if runErr != nil {
		return Fingerprint{}, fmt.Errorf("%w: %s", ErrUnreadable,
			redactFpcalcErr(stderr.String(), absPath, runErr))
	}

	var payload fpcalcJSON
	if err := json.Unmarshal(out, &payload); err != nil {
		return Fingerprint{}, fmt.Errorf("%w: decoding fpcalc output: %v", ErrUnreadable, err)
	}
	if payload.Fingerprint == "" {
		return Fingerprint{}, fmt.Errorf("%w: fpcalc returned an empty fingerprint", ErrUnreadable)
	}

	return Fingerprint{
		Value:       payload.Fingerprint,
		Duration:    payload.Duration,
		DistinctB64: distinctChars(payload.Fingerprint),
	}, nil
}

// ComputeFromPrefix fingerprints only the first limitBytes of absPath by
// piping them to fpcalc's stdin, which caps exactly how much of the file is
// asked for rather than letting fpcalc read as far as it likes.
//
// This exists to MEASURE, not as the production path, and it carries a real
// cost: fpcalc reports the length of what it decoded, so a deliberately
// truncated pipe makes Fingerprint.Duration the prefix's duration rather than
// the file's. That breaks two things at once — the decode-agreement clause
// (which exists to catch a truncated source) can no longer distinguish a
// truncated file from a truncated read, and AcoustID's `duration` parameter,
// which it matches on, would be wrong unless the caller substitutes the
// container-derived value.
//
// It is worth measuring because on a network-backed library it bounds what the
// filesystem is asked to produce. Whether that translates into less EGRESS
// depends entirely on the mount: rclone's default --vfs-read-chunk-size is
// 128 MiB, larger than any music file, so the first read of byte 0 already
// fetches the whole object and this saves nothing upstream. Under a smaller
// chunk size it does. Measure before adopting it.
func ComputeFromPrefix(ctx context.Context, absPath string, length time.Duration, limitBytes int64) (Fingerprint, error) {
	secs := int(length.Seconds())
	if secs <= 0 {
		secs = DefaultLengthSeconds
	}
	path, err := fpcalcLookPath("fpcalc")
	if err != nil {
		return Fingerprint{}, fmt.Errorf("%w: %v", ErrFpcalcMissing, err)
	}
	f, err := os.Open(absPath)
	if err != nil {
		return Fingerprint{}, fmt.Errorf("%w: %v", ErrUnreadable, filepath.Base(absPath))
	}
	defer f.Close()

	// "-" is fpcalc's own spelling for stdin (it rewrites it to "pipe:0").
	cmd := fpcalcCommand(ctx, path, "-json", "-length", fmt.Sprint(secs), "-")
	cmd.Env = append(os.Environ(), "LC_ALL=C", "LANG=C", "LANGUAGE=C")
	counter := &countingReader{r: io.LimitReader(f, limitBytes)}
	cmd.Stdin = counter
	var stderr strings.Builder
	cmd.Stderr = &stderr

	out, runErr := cmd.Output()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return Fingerprint{}, ctxErr
	}

	// A deliberately truncated stream ALWAYS makes fpcalc report a read error
	// and exit non-zero — measured, and it happens whether the truncated bytes
	// arrive by pipe or as a file. It still writes a usable fingerprint of what
	// it managed to decode, so this path parses stdout first and only consults
	// the exit status if there is nothing there.
	//
	// That is the whole reason prefix mode cannot be the production path: the
	// ordinary Compute treats that same non-zero exit as ErrUnreadable, and
	// that exit is the REAL guard against a genuinely truncated source (for
	// FLAC, fpcalc reports the STREAMINFO duration rather than the decoded
	// length, so comparing durations would not catch it). Suppressing the exit
	// status here suppresses the guard along with it.
	var payload fpcalcJSON
	if err := json.Unmarshal(out, &payload); err != nil || payload.Fingerprint == "" {
		detail := redactFpcalcErr(stderr.String(), absPath, runErr)
		if detail == "" {
			detail = "fpcalc produced no usable fingerprint from the prefix"
		}
		return Fingerprint{}, fmt.Errorf("%w: %s", ErrUnreadable, detail)
	}
	return Fingerprint{
		Value:       payload.Fingerprint,
		Duration:    payload.Duration,
		DistinctB64: distinctChars(payload.Fingerprint),
		BytesRead:   counter.n,
	}, nil
}

// countingReader records how many bytes were actually consumed, which is the
// number the prefix experiment is trying to measure.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// distinctChars counts distinct bytes in s. The Chromaprint compressed
// fingerprint is base64, so this is ASCII by construction and a byte scan is
// both correct and allocation-free.
func distinctChars(s string) int {
	var seen [256]bool
	n := 0
	for i := 0; i < len(s); i++ {
		if !seen[s[i]] {
			seen[s[i]] = true
			n++
		}
	}
	return n
}

// redactFpcalcErr strips the absolute source path from fpcalc's stderr (the
// bridge privacy contract bans surfacing absolute library paths), trims
// fpcalc's "ERROR: " prefix, and caps the length. Twin of
// internal/analyze.redactSoxErr — keep in lockstep.
func redactFpcalcErr(stderr, srcAbs string, runErr error) string {
	s := strings.TrimSpace(stderr)
	if s == "" && runErr != nil {
		s = runErr.Error()
	}
	if srcAbs != "" {
		s = strings.ReplaceAll(s, srcAbs, filepath.Base(srcAbs))
	}
	s = strings.TrimPrefix(s, "ERROR: ")
	const maxErrBytes = 4096
	if len(s) > maxErrBytes {
		s = fsutil.TrimPartialTrailingRune(s[:maxErrBytes]) + "…(truncated)"
	}
	return s
}
