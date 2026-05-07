// Package transcode owns offline PCM-upscaling: building sox(1)
// invocations, choosing variant identifiers + sidecar paths, and
// the worker pool the CLI subcommand and (in Phase 2.5) the HTTP
// `POST /v1/upscale` handler share.
//
// **Why shell out to sox** instead of cgo + libSoXr: keeping the
// bridge build pure-Go preserves `make build-all` cross-compilation
// to darwin/linux/windows × amd64/arm64 from a single host. SoXr is
// the same engine via a different surface; perceived audio quality
// is identical at the `-v` ("very high") preset we use here. The
// cost is a runtime dependency the operator installs once
// (`apt install sox` / `brew install sox` / `choco install sox`).
//
// Bridge `serve` and the `bridge upscale` CLI both probe for `sox`
// on PATH at startup; if missing, the CLI exits with an install
// hint and `serve` logs an error then disables the feature in-
// memory (graceful degradation — the rest of the server keeps
// running).
//
// **Bit-exact mission preserved**: transcoding is offline; the
// bridge never modulates bytes in flight. The sidecar `.flac` is
// a real on-disk file the bridge then serves bit-exact via the
// same `serveFile` path that the original source uses.
package transcode

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/logging"
)

var logger = logging.Component("transcode")

// VariantSchemaVersion is bumped when the on-disk sidecar layout
// or the SoX command shape changes in a way that makes prior
// sidecars semantically different from what a fresh run would
// produce. Today's only producer is `bridge upscale --quality
// very-high`. A future change of resampler preset (e.g. switching
// to a min-phase filter) bumps this so the iOS picker can
// distinguish "v1 upscale" from "v2 upscale" if they ever coexist
// during a transition.
const VariantSchemaVersion = "v1"

// Quality presets map to SoX `rate` flag combinations. We keep the
// mapping internal so a future `-q` knob on the CLI doesn't bake
// flag strings into the user-facing surface — operators see "very-
// high" / "high" / "medium" labels instead.
type Quality string

const (
	QualityVeryHigh Quality = "very-high" // rate -v dither -s — production default
	QualityHigh     Quality = "high"      // rate -h dither -s — for slower hosts
	QualityMedium   Quality = "medium"    // rate -m dither -s — sanity / ad-hoc
)

// JobSpec describes one source-to-sidecar conversion. Constructed
// by the CLI / handler from a (Track, target params) pair; passed
// to RunSox.
//
// `SourceAbsPath` is the absolute filesystem path to read from;
// `SourceLibraryRel` is the library-relative wire form that lands
// in the `track_variants.source_path` column (matches the manifest
// `Track.Path` field that iOS uses to construct download URLs).
// Keeping both fields here avoids a re-resolve at write time.
type JobSpec struct {
	SourceAbsPath    string
	SourceLibraryRel string
	SourceMTimeNS    int64
	SourceSize       int64
	SourceSampleRate int // Hz; 0 if unknown (won't be used in target-rate selection)
	TargetSampleRate int // Hz; e.g. 176400 / 192000
	TargetBits       int // 16/24/32
	Quality          Quality
	OutputDir        string // <dataDir>/transcoded
}

// VariantID returns the opaque identifier that uniquely names this
// JobSpec's output variant. Convention:
//
//	upscaled-<schemaVersion>-<targetRate>-<targetBits>
//
// e.g. `upscaled-v1-176400-24`. iOS keys on the `upscaled-` prefix
// to slot the variant into the share-level "prefer upscaled"
// resolution. Future variant kinds (e.g. PCM→DSD synthesis) get
// their own prefix.
func (j JobSpec) VariantID() string {
	return fmt.Sprintf("upscaled-%s-%d-%d", VariantSchemaVersion, j.TargetSampleRate, j.TargetBits)
}

// SidecarPath returns the absolute filesystem path the converted
// FLAC will land at. Filename pattern:
//
//	<sha256(SourceLibraryRel)[:16]>-<VariantID>.flac
//
// The variantID suffix is load-bearing: hashing only the source
// path would let two runs at different target rates overwrite each
// other (the user's first call with --target-rate 176400 followed
// by a second with --target-rate 192000 would clobber the first).
// Including the variantID guarantees multiple variants of the same
// source coexist safely on disk.
//
// 16 hex chars = 64 bits of entropy. Collisions are scoped to a
// single user's SourceLibraryRel namespace (typically <100k tracks)
// so 64 bits is far more than enough; truncating buys ~48 chars of
// Windows MAX_PATH headroom for deeply-nested OutputDirs.
//
// Migration: existing 64-char-hash sidecars on disk stay served by
// their existing track_variants.sidecar_path rows (the absolute
// path stored in the DB still points at them). Only newly-transcoded
// variants get the 16-char form. Zero-friction upgrade.
func (j JobSpec) SidecarPath() string {
	sum := sha256.Sum256([]byte(j.SourceLibraryRel))
	hash := hex.EncodeToString(sum[:])[:16]
	return filepath.Join(j.OutputDir, fmt.Sprintf("%s-%s.flac", hash, j.VariantID()))
}

// SoxArgs builds the argv for the sox invocation. Returns the
// exact slice exec.Command will receive (including the leading
// input/output sentinels) and a JSON-friendly settings string for
// `track_variants.sox_settings` (forensic record of what produced
// this sidecar). Pure function — no I/O — so unit tests can pin
// the exact command shape without invoking sox.
//
// Quality preset → `rate` flag mapping:
//
//	QualityVeryHigh → "-v"  (very high; ~95 dB stopband, 32-bit internal)
//	QualityHigh     → "-h"
//	QualityMedium   → "-m"
//
// `dither -s` is shaped TPDF dither when truncating the 32-bit
// internal float to the 24/32-bit integer FLAC output. Required
// for audible transparency at 24-bit and benign at 32-bit (the
// dither noise is below the LSB).
func (j JobSpec) SoxArgs() ([]string, string) {
	rateFlag := "-v"
	switch j.Quality {
	case QualityHigh:
		rateFlag = "-h"
	case QualityMedium:
		rateFlag = "-m"
	}
	// Output goes to `<sidecar>.flac.tmp` so the rename(2) at the
	// end of `RunSox` is atomic. Sox normally picks the encoder
	// from the output filename's last extension — which here is
	// `.tmp`, not `.flac`, and sox bombs with
	//   `sox FAIL formats: no handler for file extension 'tmp'`.
	// Force the format explicitly via `-t flac` on the output
	// argument so sox ignores the filename and writes FLAC. (Bug
	// found post-merge of PR #126: enqueue worked but every sox
	// invocation failed during the worker pool's actual run.)
	args := []string{
		j.SourceAbsPath,
		"-b", strconv.Itoa(j.TargetBits),
		"-t", "flac",
		j.SidecarPath() + ".tmp",
		"rate", rateFlag, strconv.Itoa(j.TargetSampleRate),
		"dither", "-s",
	}
	settings := fmt.Sprintf(
		`{"resampler":"sox","quality":%q,"rateFlag":%q,"targetRate":%d,"targetBits":%d,"schemaVersion":%q}`,
		j.Quality, rateFlag, j.TargetSampleRate, j.TargetBits, VariantSchemaVersion)
	return args, settings
}

// PickTargetRate decides the output sample rate from the source
// rate when the operator passed `--target-rate auto`. Picks the
// highest integer-ratio target within the 44.1/48 family that
// stays at or below 192 kHz — the sweet spot for modern DACs'
// integer-ratio oversampling filters:
//
//	44100  → 176400 (4× — two octaves above source)
//	88200  → 176400 (2× — one octave above source)
//	176400 → 0 (skip; source is already at the auto target)
//	48000  → 192000 (4×)
//	96000  → 192000 (2×)
//	192000 → 0 (skip)
//	other  → 0 (don't auto-pick a target for unfamiliar rates)
//
// Returns 0 when no auto target makes sense; callers treat 0 as
// "skip this track". Operators with DACs that prefer a higher
// rate (Chord M-Scaler at 705.6) override via explicit `--target-
// rate 705600`.
func PickTargetRate(sourceRate int) int {
	switch sourceRate {
	case 44100, 88200:
		return 176400
	case 48000, 96000:
		return 192000
	default:
		return 0
	}
}

// ResolveTargetRate converts the user-facing `--target-rate` flag
// (string, possibly "auto") into the integer Hz value to feed
// into SoxArgs. Returns (0, nil) when the resolution is "skip
// this source" (auto + already at/above target, or a deliberate
// integer at/below the source). Returns (>0, nil) when there's a
// real target. Returns (0, err) on a malformed flag.
func ResolveTargetRate(flagValue string, sourceRate int) (int, error) {
	flagValue = strings.TrimSpace(flagValue)
	if flagValue == "" || flagValue == "auto" {
		auto := PickTargetRate(sourceRate)
		if auto <= sourceRate {
			return 0, nil
		}
		return auto, nil
	}
	target, err := strconv.Atoi(flagValue)
	if err != nil {
		return 0, fmt.Errorf("invalid target rate %q: %w", flagValue, err)
	}
	if target <= 0 {
		return 0, fmt.Errorf("target rate must be positive, got %d", target)
	}
	if target <= sourceRate {
		// Never downsample. Source already at or above target
		// is a "nothing to do" case.
		return 0, nil
	}
	return target, nil
}

// RunSox executes one JobSpec, writes the sidecar to disk, and
// returns the on-disk size on success. Atomic-rename pattern: the
// sox output goes to `<sidecar>.tmp`, then we `os.Rename` to the
// final path on success — a crash mid-conversion leaves at most a
// `.tmp` file behind (cleaned up by `bridge upscale --gc`).
//
// The `sox` binary is located via PATH. Caller is expected to have
// already probed for it via PrecheckSox; we don't repeat the probe
// here so a worker-pool body doesn't pay the LookPath cost per
// iteration.
//
// **Cancellation**: ctx is plumbed via `exec.CommandContext`. A
// SIGINT to the CLI / SIGTERM to `bridge serve` cancels the
// outer context, exec.CommandContext SIGKILLs the in-flight sox
// process, and we clean up the partial `.tmp`. Without this a
// half-converted album could hang the operator's terminal until
// the largest file finishes (Gemini bot review on PR #108).
//
// Output directory created if missing — `bridge upscale` only
// guarantees DataDir exists, not the `transcoded` subdir.
func RunSox(ctx context.Context, j JobSpec) (int64, error) {
	if err := os.MkdirAll(j.OutputDir, 0o755); err != nil {
		return 0, fmt.Errorf("mkdir output dir: %w", err)
	}
	args, _ := j.SoxArgs()
	finalPath := j.SidecarPath()
	tmpPath := finalPath + ".tmp"
	// Defensive: clear any stale .tmp from a previous interrupted
	// run so SoX's open(O_CREAT) doesn't trip on prior crash debris.
	_ = os.Remove(tmpPath)

	// `cleanup := true; defer …` mirrors writeArtworkAtomicStream's
	// pattern in internal/enrich. Cleared on the success path after
	// the atomic rename; otherwise the deferred remove reaps the
	// .tmp on every error / panic exit, so a future maintainer can
	// add an early return without remembering the manual remove.
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()

	cmd := exec.CommandContext(ctx, "sox", args...)
	// Capture combined stdout/stderr for the error path — sox
	// writes its diagnostics to stderr and they're invaluable when
	// debugging "this one file fails".
	out, err := cmd.CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("sox: %w (stderr: %s)", err, strings.TrimSpace(string(out)))
	}
	// Atomic rename on success. Same FS as DataDir so this is a
	// rename(2), not a copy.
	if err := os.Rename(tmpPath, finalPath); err != nil {
		return 0, fmt.Errorf("rename sidecar: %w", err)
	}
	cleanup = false // success — keep the now-renamed final file
	info, err := os.Stat(finalPath)
	if err != nil {
		return 0, fmt.Errorf("stat sidecar: %w", err)
	}
	logger.Debug("sox ok",
		"source", j.SourceLibraryRel,
		"variant", j.VariantID(),
		"sidecar_bytes", info.Size())
	return info.Size(), nil
}

// PrecheckSox returns nil if the `sox` binary is on PATH and
// reports a non-error stderr to `--version`. Returns ErrSoxMissing
// if the binary can't be located, or a generic error if invocation
// fails for any other reason. Called by both `bridge upscale` (CLI
// entry point) and the `bridge serve` startup gate (Phase 2.5).
//
// **Bounded by a 2 s timeout** (CodeRabbit second-pass on PR
// #108) so a wedge from a broken PATH wrapper or a hung sox
// process can't deadlock startup. `bridge serve` runs this
// before opening the listen socket; without the timeout, every
// service-manager restart with a misbehaving sox installation
// would block forever instead of degrading cleanly.
func PrecheckSox() error {
	path, err := exec.LookPath("sox")
	if err != nil {
		return fmt.Errorf("%w: %v", ErrSoxMissing, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "--version")
	out, err := cmd.CombinedOutput()
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("sox --version timed out after 2s; broken PATH wrapper or hung process")
		}
		return fmt.Errorf("sox --version failed: %w (output: %s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// ErrSoxMissing is returned by PrecheckSox when `sox` isn't on
// PATH. Test for it via errors.Is so callers can surface a
// targeted install-hint message vs the generic "something went
// wrong" path.
var ErrSoxMissing = errors.New("sox binary not found on PATH")

// FreshnessFromFile populates the SourceMTimeNS / SourceSize
// fields on a JobSpec by stat-ing SourceAbsPath. Used by the CLI
// to capture "what version of the source did we convert from" at
// the moment of conversion, which the variant-resolve path later
// uses to detect drift.
func (j *JobSpec) FreshnessFromFile() error {
	info, err := os.Stat(j.SourceAbsPath)
	if err != nil {
		return err
	}
	j.SourceMTimeNS = info.ModTime().UnixNano()
	j.SourceSize = info.Size()
	return nil
}

// CreatedAtNow returns wall-clock UTC nanoseconds — the value
// callers stamp into `track_variants.created_at` on insert.
// Centralised here so test stubs can override (today: a const,
// good enough).
func CreatedAtNow() int64 { return time.Now().UnixNano() }
