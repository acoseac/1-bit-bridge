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
	"sync"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/logging"
	"github.com/google/uuid"
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
//
// **v2 (this version)**: `-G` (gain-guard) added to SoxArgs. Sox
// pre-scans the input for the headroom needed to prevent clipping
// during the rate-conversion + dither pipeline and applies a
// matching attenuation. Fires only when the source has 0 dBFS
// peaks (most modern pop masters); the typical attenuation is
// well under 1 dB. The audio CONTENT shifts under guard, so the
// schema bump produces a fresh VariantID — operators run `bridge
// upscale` once after upgrade and the iOS client picks up the new
// guard-clean variants automatically. Pre-v2 sidecars stay served
// by their existing track_variants rows until the next
// `bridge upscale --gc` pass cleans them up.
const VariantSchemaVersion = "v2"

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

// OutputDirSubdir is the fixed subdirectory under `cfg.DataDir`
// where transcoded sidecar files land. Single source of truth so
// cmd/bridge (which wires the runtime pool's `OutputDir`) and the
// admin handlers (which surface "Stored at <path>" to operators)
// can't drift. Changing this value rehouses the cache; existing
// rows in `track_variants.sidecar_path` continue to point at the
// old location until they're rewritten by `bridge upscale --gc`.
const OutputDirSubdir = "transcoded"

// OutputDirFor returns the absolute on-disk directory where the
// pool writes converted sidecars given the bridge's `dataDir`.
// Mirrors the path the runtime pool is configured with at startup;
// safe to call from any goroutine (pure path arithmetic).
func OutputDirFor(dataDir string) string {
	return filepath.Join(dataDir, OutputDirSubdir)
}

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

	// BatchID groups this JobSpec into one operator-initiated batch
	// (v1.3 Coordinator). The zero-value uuid.UUID means the job was
	// submitted outside the batch path — legacy `POST /v1/upscale`
	// single-track requests and the `bridge upscale` CLI both leave
	// it zero. The Coordinator stamps it at Submit time; pool
	// callbacks (onJobComplete / onJobFailed) propagate it back so
	// the coordinator can attribute completion / failure to the
	// right `upscale_batches` row without a path-to-batch lookup.
	BatchID uuid.UUID
}

// VariantID returns the opaque identifier that uniquely names this
// JobSpec's output variant. Convention:
//
//	upscaled-<schemaVersion>-<targetRate>-<targetBits>
//
// e.g. `upscaled-v2-176400-24`. iOS keys on the `upscaled-` prefix
// to slot the variant into the share-level "prefer upscaled"
// resolution. Future variant kinds (e.g. PCM→DSD synthesis) get
// their own prefix.
//
// Hot path during manifest scan + every pool callback. The
// finite (rate × bits) cross-product across all real DACs makes
// memoization a clean O(1) win — see `variantIDCache`. Per-call
// cost goes from a `fmt.Sprintf` allocation to a map read.
func (j JobSpec) VariantID() string {
	if id, ok := lookupCachedVariantID(j.TargetSampleRate, j.TargetBits); ok {
		return id
	}
	return fmt.Sprintf("upscaled-%s-%d-%d", VariantSchemaVersion, j.TargetSampleRate, j.TargetBits)
}

// variantIDCache memoizes the `VariantID()` output for the
// finite (rate × bits) cross-product covering every real DAC.
// Built once via sync.Once on first VariantID call (lazy
// initialization beats init()-time so test binaries that don't
// touch VariantID never pay the construction cost). Sub-mics
// O(1) lookup vs ~150 ns fmt.Sprintf + alloc per call.
//
// Out-of-set combinations fall through to the live fmt.Sprintf
// path (forward-compat with future rates / bit depths). Don't
// pre-seed every conceivable combination — keep the cache tight
// to what real hardware uses; an unknown (rate, bits) pair pays
// the unmemoized cost once and that's correct.
var (
	variantIDCacheOnce sync.Once
	variantIDCache     map[[2]int]string
)

// lookupCachedVariantID returns the memoized VariantID string for
// the given (rate, bits) pair, or `_, false` on miss. The caller
// falls through to the live fmt.Sprintf path on miss.
//
// **Lossless cache key** (CodeRabbit Major on PR #211): the prior
// uint64-pack form `(rate << 8) | (bits & 0xff)` truncated bits
// to 8 bits, so `bits=272` (256+16) silently aliased to the
// cached `bits=16` entry and returned the wrong VariantID. A
// `[2]int` key compares byte-identical to the lookup tuple with
// zero allocation (arrays are value types) and admits the full
// `int` range on both axes — out-of-set inputs miss cleanly and
// fall through to the unmemoized path.
func lookupCachedVariantID(rate, bits int) (string, bool) {
	variantIDCacheOnce.Do(initVariantIDCache)
	if rate < 0 || bits < 0 {
		return "", false
	}
	id, ok := variantIDCache[[2]int{rate, bits}]
	return id, ok
}

func initVariantIDCache() {
	rates := []int{44100, 48000, 88200, 96000, 176400, 192000}
	bitsList := []int{16, 24}
	variantIDCache = make(map[[2]int]string, len(rates)*len(bitsList))
	for _, r := range rates {
		for _, b := range bitsList {
			variantIDCache[[2]int{r, b}] =
				fmt.Sprintf("upscaled-%s-%d-%d", VariantSchemaVersion, r, b)
		}
	}
}

// SidecarPath returns the absolute filesystem path the converted
// FLAC will land at. Filename pattern (v1.4):
//
//	<OutputDir>/<libRel-dirname>/<libRel-basename>.<VariantID>.flac
//
// Example: SourceLibraryRel `Diana Krall/The Look of Love/01 Love Letters.flac`
// + VariantID `upscaled-v2-176400-24` →
//
//	<OutputDir>/Diana Krall/The Look of Love/01 Love Letters.flac.upscaled-v2-176400-24.flac
//
// **Why source-path-mirrored layout**: operators with write access to
// their library can `mv` the OutputDir contents directly into the
// library and slot variants alongside source files. The variantID
// suffix on the filename is load-bearing (multiple targets coexist:
// `01 Love Letters.flac.upscaled-v2-176400-24.flac` next to
// `01 Love Letters.flac.upscaled-v2-352800-24.flac`).
//
// **Why keep the original extension** (`.flac`) in the basename: a
// folder with `Track1.flac` AND `Track1.wav` (legit A/B testing case)
// would otherwise both write to `Track1.upscaled-…flac` — zero
// collision tolerance for that exact case is structural.
//
// **Migration**: existing hash-flat sidecars on disk stay served by
// their existing `track_variants.sidecar_path` rows (absolute paths;
// DB lookup is the only resolver — never filesystem-walked). Only
// newly-transcoded variants use the new layout. Mixed-layout state
// is fine; `bridge variants relayout` (separate CLI) opt-in
// renames legacy variants to match.
//
// **Filesystem safety**: long basenames + Windows-illegal characters
// + reserved names route through `safeVariantFilename` which:
//   - replaces `: * ? " < > |` with `_` (deterministic),
//   - middle-truncates the basename + appends a short SHA8 of the
//     original full basename for uniqueness when total would exceed
//     255 bytes (ext4 / NTFS / exFAT cap).
//
// Dirname segments are NOT sanitised — they came from the source
// filesystem which already accepts them.
func (j JobSpec) SidecarPath() string {
	dir := filepath.Dir(j.SourceLibraryRel)
	base := filepath.Base(j.SourceLibraryRel)
	filename := safeVariantFilename(base, j.VariantID())
	if dir == "" || dir == "." {
		return filepath.Join(j.OutputDir, filename)
	}
	return filepath.Join(j.OutputDir, dir, filename)
}

// safeVariantFilename builds the variant FLAC's basename for the
// source-path-mirrored layout. Trade-offs documented in
// `SidecarPath`'s docblock.
//
// The output shape is:
//
//	<srcBase>.<variantID>.flac
//
// — with srcBase optionally middle-truncated + SHA8-suffixed when
// the full filename would exceed 255 bytes (the ext4 / NTFS /
// exFAT / encrypted-overlay basename cap).
//
// Characters illegal on FAT-family filesystems (`: * ? " < > |`)
// are replaced with `_` deterministically. The replacement is
// path-wide rather than condition-on-target-filesystem because the
// transcoder doesn't know the destination filesystem at write time
// AND the deterministic mapping is friendly to GC + admin migration
// (re-running with the same source produces the same filename,
// keeping `track_variants.sidecar_path` rows valid).
//
// Pure helper — testable in isolation; table-tested across cases.
func safeVariantFilename(srcBase, variantID string) string {
	const fsBasenameCap = 255
	srcBase = sanitiseForFAT(srcBase)
	candidate := fmt.Sprintf("%s.%s.flac", srcBase, variantID)
	if len(candidate) <= fsBasenameCap {
		return candidate
	}
	// Over-length: compute a stable SHA8 of the ORIGINAL full
	// basename for uniqueness, then middle-truncate srcBase to fit.
	sum := sha256.Sum256([]byte(srcBase))
	sha8 := hex.EncodeToString(sum[:])[:8]
	// Reserved budget for: "~<sha8>." + variantID + ".flac".
	suffix := fmt.Sprintf("~%s.%s.flac", sha8, variantID)
	budget := fsBasenameCap - len(suffix)
	if budget < 8 {
		// Pathological: variantID alone consumes the budget. Fall
		// back to a fully-hash-named filename — losing the
		// source-mirror property but guaranteeing a valid filename.
		// In practice the variantID is ~25 chars; this branch is
		// dead under realistic configs.
		return fmt.Sprintf("v.%s%s", sha8, suffix)
	}
	if len(srcBase) <= budget {
		return srcBase + suffix
	}
	// Middle-truncate: keep `head + ".." + tail`, then append suffix.
	half := (budget - 2) / 2
	if half < 1 {
		return srcBase[:budget] + suffix
	}
	head := srcBase[:half]
	tail := srcBase[len(srcBase)-half:]
	return head + ".." + tail + suffix
}

// sanitiseForFAT replaces characters that FAT-family filesystems
// (FAT32 / exFAT / NTFS) reject in filenames with `_`. The
// substitution is deterministic so re-runs of the same source
// produce identical output — the DB lookup contract depends on it.
//
// Forward slash isn't included: the caller has already split the
// path; this operates on a basename only. Backslash (`\`) IS
// included because some sources have it embedded in a single path
// segment (rare; cross-OS rip tools sometimes do this) and FAT
// rejects it.
func sanitiseForFAT(s string) string {
	const bad = `:*?"<>|\`
	out := []byte(s)
	for i, b := range out {
		if strings.ContainsRune(bad, rune(b)) {
			out[i] = '_'
		}
	}
	return string(out)
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
//
// `-G` (gain-guard, leading global option) instructs sox to pre-
// scan the input for the headroom needed to prevent clipping
// through the rate-conversion + dither pipeline and apply a
// matching attenuation. Required for upscaling 0 dBFS-mastered
// material (most modern pop / loudness-war masters): without it,
// intersample peaks the rate filter reconstructs above 0 dBFS
// would clip on the integer FLAC output. The attenuation is
// computed from the actual peak — typically well under 1 dB and
// inaudible — and only fires when the source has the headroom
// problem. Cost is one extra peak-scan pass; invisible against
// the rate-conversion CPU dominating the run.
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
		"-G",
		j.SourceAbsPath,
		"-b", strconv.Itoa(j.TargetBits),
		"-t", "flac",
		j.SidecarPath() + ".tmp",
		"rate", rateFlag, strconv.Itoa(j.TargetSampleRate),
		"dither", "-s",
	}
	settings := fmt.Sprintf(
		`{"resampler":"sox","quality":%q,"rateFlag":%q,"targetRate":%d,"targetBits":%d,"guard":true,"schemaVersion":%q}`,
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
		"path", j.SourceLibraryRel,
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
