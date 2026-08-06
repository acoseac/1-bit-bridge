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
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
	"github.com/acoseac/1-bit-bridge/internal/fsutil"
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

// sidecarTmpSuffix terminates the atomic-rename temp file sox writes
// (SoxArgs output argument + RunSox's rename source). Kept as the visible
// trailing extension so a stale temp on disk still reads as a temp — the
// same operator-debuggability convention internal/atomicwrite documents.
const sidecarTmpSuffix = ".tmp"

// sidecarTmpTokenHexLen is the width of the per-job uniqueness token that
// sits between the sidecar path and sidecarTmpSuffix:
//
//	<sidecar>.<token>.tmp
//
// The token exists because the temp path used to be a pure function of the
// JobSpec — and two workers can legitimately hold the SAME spec at the same
// time. `DropInflight` frees a dedup key for a job that is still RUNNING (by
// design: DELETE /v1/upscale/variants calls it so a re-submit isn't coalesced
// against a worker about to write the sidecar the caller means to delete), and
// the delete also removes the `track_variants` row so `finalizeAndEnqueue`'s
// LookupVariant no longer refuses. With workers >= 2 (the default is
// min(NumCPU-1, 4)) job B then starts while job A's sox is still writing.
//
// On a shared deterministic temp path that corrupts the published variant:
// RunSox opens every job by `os.Remove`-ing its temp to clear crash debris, so
// B unlinks A's in-progress output; A's sox exits first (it started earlier)
// and A's rename therefore publishes B's still-being-written file, stats it,
// fsyncs it, and commits a track_variants row. `/v1/download?variant=` then
// serves a partially-written FLAC behind a committed row for as long as B keeps
// writing (serveVariant's freshness check compares the SOURCE's mtime/size, not
// the sidecar's), the row's SizeBytes is permanently wrong (it feeds freedBytes
// and the admin cached-bytes tile), and B is counted a failed job even though
// the variant exists. POSIX-only — on Windows both the unlink and the re-open
// fail with a sharing violation and B fails cleanly.
//
// 8 hex digits: see sidecarTmpCounter for why that is more than enough.
const sidecarTmpTokenHexLen = 8

// sidecarTmpReserve is the total number of bytes a temp basename adds over its
// final basename: "." + token + sidecarTmpSuffix. safeVariantFilename reserves
// exactly these bytes out of the 255-byte cap so the TEMP name fits too —
// **that reservation is the load-bearing part, not the literal suffix length**.
// If the temp shape ever changes again, change this with it.
const sidecarTmpReserve = 1 + sidecarTmpTokenHexLen + len(sidecarTmpSuffix)

// sidecarTmpCounter is seeded once from crypto/rand and incremented per
// nextSidecarTmpToken call. The random base makes two independently-started
// processes writing the same sidecar (the `bridge upscale` CLI alongside a
// running `bridge serve`) collide with probability ~2^-32 per pair; the
// monotonic increment makes an in-process collision impossible until 2^32
// jobs. A bare counter would be process-local and a bare random draw would
// need a call per job — this shape has neither problem.
//
// math/rand is deliberately NOT used (repo-wide convention).
var sidecarTmpCounter = newSidecarTmpCounter()

// newSidecarTmpCounter returns a counter seeded from crypto/rand. Split out of
// the package var so a test can build independent instances and assert the seed
// really is randomised — a constant seed would put two processes on identical
// temp paths for the same sidecar, which is precisely the collision the token
// exists to prevent (see sidecarTmpTokenHexLen).
//
// **The seed is unconditional on purpose — do NOT add an error branch here.**
// `crypto/rand.Read` never returns an error: since Go 1.24 it is documented to
// always fill b entirely, and it crashes the program irrecoverably rather than
// reporting a failure (go1.26's implementation is
// `if err != nil { fatal(…); panic("unreachable") }`, with `return len(b), nil`
// as its only return — and that fatal covers a replaced `rand.Reader` too).
// go.mod pins `go 1.26.4`, a hard minimum, so this holds for every build of
// this module.
//
// A fallback for that branch would therefore be unreachable code, and the
// obvious fallbacks are worse than nothing: a zero seed makes two processes
// start from the SAME counter and derive IDENTICAL temp paths — reaching the
// exact bug through a different door — while a time-based one implies the
// unreachable state is real and invites the next reader to trust it.
func newSidecarTmpCounter() *atomic.Uint64 {
	var b [8]byte
	_, _ = rand.Read(b[:])
	var c atomic.Uint64
	c.Store(binary.BigEndian.Uint64(b[:]))
	return &c
}

// nextSidecarTmpToken returns a fresh fixed-width token. Always exactly
// sidecarTmpTokenHexLen hex digits (%08x of a uint32), which is what lets
// sidecarTmpReserve be a compile-time constant.
func nextSidecarTmpToken() string {
	return fmt.Sprintf("%0*x", sidecarTmpTokenHexLen, uint32(sidecarTmpCounter.Add(1)))
}

// Variant ID prefixes. Two transcode classes coexist today:
//
//   - "upscaled-" — render a hi-res target (e.g. 192/24) FROM a
//     lower-res source, for home-DAC playback. Operator-driven
//     (POST /v1/upscale + `bridge upscale --all`).
//
//   - "optimized-" — render a CarPlay-compatible 16/44.1 or 16/48
//     target FROM a hi-res source. iOS auto-routes to these when
//     CarPlay is the active audio output; the sample-rate-family
//     selector (`TargetRateForOptimize`) keeps SoX on the integer-
//     ratio fast path.
//
// **Don't share the variant ID cache between kinds** — the
// (44100, 16) entry exists in BOTH spaces and the upscale memo
// returns `upscaled-v2-44100-16` for either kind's lookup, silently
// corrupting variant routing at the SQL persistence boundary. Two
// parallel memos (this file: `variantIDCache` + `optimizeIDCache`)
// keep the prefix semantics tight.
const (
	VariantPrefixUpscaled  = "upscaled"
	VariantPrefixOptimized = "optimized"
)

// JobKind discriminates upscale (default) from optimize jobs across
// the full pool / coordinator / handler / CLI surface. Zero-value
// `JobKindUpscale` preserves legacy behavior for every existing
// JobSpec construction site that didn't yet learn about the field.
type JobKind string

const (
	JobKindUpscale  JobKind = "upscale"
	JobKindOptimize JobKind = "optimize"
)

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
	// SourceBits is the source bit depth (16/24/32), 0 if unknown.
	// Display-only — feeds the live worker grid's signal-chain string
	// ("44.1/16 ➔ 176.4/24"); never used in target-rate/bits selection.
	SourceBits       int
	TargetSampleRate int // Hz; e.g. 176400 / 192000
	TargetBits       int // 16/24/32
	Quality          Quality
	OutputDir        string // <dataDir>/transcoded

	// Kind discriminates the transcode class — upscale (default,
	// zero-value preserves legacy behavior) or optimize (CarPlay-
	// targeted downsample to 16/44.1 or 16/48). Drives the variant
	// ID prefix in `VariantID()` and the eligibility-gate branch in
	// `Coordinator.Submit`.
	Kind JobKind

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
//	upscaled-<schemaVersion>-<targetRate>-<targetBits>   // JobKindUpscale (default)
//	optimized-<schemaVersion>-<targetRate>-<targetBits>  // JobKindOptimize
//
// e.g. `upscaled-v2-176400-24` or `optimized-v2-44100-16`. iOS keys
// on the prefix to slot the variant into the share-level "prefer
// upscaled" toggle vs. the runtime CarPlay-routing path. Future
// variant kinds (e.g. PCM→DSD synthesis) get their own prefix.
//
// Hot path during manifest scan + every pool callback. The finite
// (rate × bits) cross-product across all real DACs makes memoization
// a clean O(1) win — two parallel caches keep the prefix discriminator
// strict at the SQL boundary (see the docblock on the prefix constants).
//
// **Don't share `variantIDCache` between kinds** — pre-seeded with
// `upscaled-v2-*` for `(44100, 16)`, returning that for an optimize
// job would silently emit the wrong variant ID into `track_variants`.
func (j JobSpec) VariantID() string {
	if j.Kind == JobKindOptimize {
		if id, ok := lookupCachedOptimizeVariantID(j.TargetSampleRate, j.TargetBits); ok {
			return id
		}
		return fmt.Sprintf("optimized-%s-%d-%d", VariantSchemaVersion, j.TargetSampleRate, j.TargetBits)
	}
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

// optimizeIDCache memoizes the optimize-kind VariantID strings. Tiny
// space: 16-bit × {44100, 48000} — the only family-aligned targets
// `TargetRateForOptimize` ever picks. Out-of-set inputs fall through
// to the live `fmt.Sprintf` path in `VariantID()`.
//
// **Strictly disjoint from `variantIDCache`** — sharing the map
// would silently corrupt variant IDs at the SQL persistence
// boundary (the upscale memo's `(44100, 16) → "upscaled-v2-..."`
// entry would win for optimize-kind lookups too).
var (
	optimizeIDCacheOnce sync.Once
	optimizeIDCache     map[[2]int]string
)

func lookupCachedOptimizeVariantID(rate, bits int) (string, bool) {
	optimizeIDCacheOnce.Do(initOptimizeIDCache)
	if rate < 0 || bits < 0 {
		return "", false
	}
	id, ok := optimizeIDCache[[2]int{rate, bits}]
	return id, ok
}

func initOptimizeIDCache() {
	optimizeIDCache = map[[2]int]string{
		{44100, 16}: fmt.Sprintf("optimized-%s-44100-16", VariantSchemaVersion),
		{48000, 16}: fmt.Sprintf("optimized-%s-48000-16", VariantSchemaVersion),
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
//   - appends a `~<sha8>.` disambiguation segment derived from the
//     RAW (pre-sanitization) basename bytes whenever sanitization
//     touched the name OR the total would exceed 255 bytes (ext4 /
//     NTFS / exFAT cap). The raw-bytes hash is load-bearing: two
//     source files differing only in FAT-illegal characters
//     (`Track:A.flac` + `Track*A.flac`) sanitize to identical bytes,
//     and pre-fix the hash was computed over the sanitized form so
//     it collapsed too — silently overwriting one variant with the
//     other via `os.Rename`. The current shape keeps distinct raw
//     inputs distinct on disk; DO NOT move the hash computation
//     past `sanitiseForFAT` at any new call site.
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

// VariantSidecarBasename builds the variant FLAC's basename for the
// source-path-mirrored layout — the single source of truth shared by the
// runtime writer (JobSpec.SidecarPath, above) and the `bridge variants
// move` CLI (cmd/bridge computeNewSidecarPath), so the two can't drift.
// srcBase is the source file's basename (filepath.Base of its library-
// relative path); variantID is the persisted variant ID. See
// safeVariantFilename for the FAT-sanitization + 255-byte-cap trade-offs.
func VariantSidecarBasename(srcBase, variantID string) string {
	return safeVariantFilename(srcBase, variantID)
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
	// 255 is the ext4 / NTFS / exFAT basename limit — minus everything the
	// atomic-rename temp name adds on top of the final one (sidecarTmpReserve
	// = "." + per-job token + sidecarTmpSuffix). Without the reserve, a
	// basename at exactly 255 bytes makes the `<sidecar>.flac.<token>.tmp`
	// write target longer still and sox fails with ENAMETOOLONG — precisely
	// for the long-classical-filename inputs this sanitizer exists to handle.
	const fsBasenameCap = 255 - sidecarTmpReserve
	raw := srcBase
	sanitized := fsutil.SanitiseForFAT(srcBase)

	// Clean path: name didn't trip FAT sanitization AND fits the cap.
	// Pre-fix this branch ran even when sanitization rewrote the name,
	// which let two distinct raw inputs (`Track:A.flac` + `Track*A.flac`)
	// collapse onto the same sanitized output and silently overwrite each
	// other's variant via os.Rename. The `sanitized == raw` clause closes
	// that hole — any name that touched sanitization falls through to the
	// raw-bytes SHA8 suffix path so distinct sources stay distinct on disk.
	//
	// The `sanitized == raw` byte-compare is a cheap O(len(srcBase))
	// pre-check; it gates the more expensive `fmt.Sprintf` so names that
	// DID get rewritten (classical-music libraries with colons + question
	// marks are the common case) skip the string allocation entirely
	// (Gemini medium on PR #280).
	if sanitized == raw {
		candidate := fmt.Sprintf("%s.%s.flac", sanitized, variantID)
		if len(candidate) <= fsBasenameCap {
			return candidate
		}
	}
	// Disambiguation path. Hash the RAW (pre-sanitization) bytes — hashing
	// the sanitized form was the original bug (`Track:A.flac` and
	// `Track*A.flac` sanitize identically, so they hashed identically too).
	// Note: Unicode normalisation drift (NFC vs NFD of the same character)
	// does NOT collapse here because sanitiseForFAT only rewrites ASCII
	// target bytes and leaves multi-byte sequences alone — the raw bytes
	// of the two forms differ, so the SHA8 differs even when the visible
	// glyph matches. A future refactor that adds Unicode normalisation
	// INSIDE sanitiseForFAT would re-open the collision; a regression test
	// guards this.
	sum := sha256.Sum256([]byte(raw))
	sha8 := hex.EncodeToString(sum[:])[:8]
	// Reserved budget for: "~<sha8>." + variantID + ".flac".
	suffix := fmt.Sprintf("~%s.%s.flac", sha8, variantID)
	budget := fsBasenameCap - len(suffix)
	if budget < 8 {
		// Pathological: variantID alone consumes the budget. Fall back to
		// a fully-hash-named filename — losing the source-mirror property
		// but guaranteeing a valid, bounded name. It MUST still encode the
		// variant: the prior `v.<sha8>` + oversized suffix form both blew
		// the budget (it re-appended the too-long suffix) AND — had the
		// suffix been dropped — would have collided two variants of the
		// same source (violating the VariantID-suffix invariant). Hash the
		// variantID too. In practice variantID is ~25 chars, so this
		// branch is dead under realistic configs.
		vsum := sha256.Sum256([]byte(variantID))
		variantHash8 := hex.EncodeToString(vsum[:])[:8]
		return fmt.Sprintf("v.%s.%s.flac", sha8, variantHash8)
	}
	if len(sanitized) <= budget {
		return sanitized + suffix
	}
	// Middle-truncate: keep `head + ".." + tail`, then append suffix.
	// UTF-8-safe rune-boundary clip: byte-level slicing could land in
	// the middle of a multi-byte rune ("Dvořák" mid-truncated at byte
	// 2 would corrupt the `ř`). Use `truncateUTF8AtMost` which scans
	// rune boundaries up to the byte budget.
	half := (budget - 2) / 2
	if half < 1 {
		return fsutil.TruncateUTF8AtMost(sanitized, budget) + suffix
	}
	// On an odd budget, give the leftover byte to the head so
	// `head + ".." + tail` uses the full budget instead of dropping a
	// byte of naming context (matters for long classical filenames).
	head := fsutil.TruncateUTF8AtMost(sanitized, budget-2-half)
	tail := fsutil.TruncateUTF8FromEnd(sanitized, half)
	return head + ".." + tail + suffix
}

// The FAT-sanitize + UTF-8-truncate primitives this file used to define
// (sanitiseForFAT / truncateUTF8AtMost / truncateUTF8FromEnd) now live in
// internal/fsutil, shared with internal/analyze's waveform sidecars.

// SoxArgs builds the argv for the sox invocation. Returns the
// exact slice exec.Command will receive (including the leading
// input/output sentinels), a JSON-friendly settings string for
// `track_variants.sox_settings` (forensic record of what produced
// this sidecar), and the two paths the run works with: the FINAL
// sidecar path and the temp path sox is told to write.
//
// **Not idempotent in its temp path**: each call mints a fresh
// per-job token (see sidecarTmpTokenHexLen). A caller that needs the
// temp path MUST use the value returned by the SAME call that produced
// the argv it runs — never re-derive it from a second call. Returning
// finalPath alongside is what lets RunSox honour that: it re-derives
// nothing, so the rename target is exactly the path sox was told to
// write, minus a token RunSox never has to know about. The two
// settings-only callers (`Pool.processJob`, `cmd/bridge` upscale) discard
// both paths.
//
// Everything except the temp token is a pure function of the JobSpec, so
// unit tests can still pin the exact command shape without invoking sox.
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
//
// `rate … -L` pins LINEAR phase response on the resampler. This is
// already sox's default for the `rate` effect, so the pinned form is
// byte-identical to the prior unpinned `rate -v <Hz>` output — verified
// with deterministic `sox -R` runs (the signal-path md5 matches; only
// the always-present dither PRNG differs run-to-run). Pinning it is pure
// defense: a future sox release that changed the default phase could
// otherwise silently flip our cached variants to minimum/intermediate
// phase, and it lets the admin worker grid label the chain "linear
// phase" truthfully. Because the deterministic output is unchanged, NO
// VariantSchemaVersion bump / regeneration is required — existing
// sidecars stay valid.
func (j JobSpec) SoxArgs() (args []string, settings string, finalPath string, tmpPath string) {
	rateFlag := "-v"
	switch j.Quality {
	case QualityHigh:
		rateFlag = "-h"
	case QualityMedium:
		rateFlag = "-m"
	}
	// Output goes to `<sidecar>.<token>.tmp` so the rename(2) at the
	// end of `RunSox` is atomic. Sox normally picks the encoder
	// from the output filename's last extension — which here is
	// `.tmp`, not `.flac`, and sox bombs with
	//   `sox FAIL formats: no handler for file extension 'tmp'`.
	// Force the format explicitly via `-t flac` on the output
	// argument so sox ignores the filename and writes FLAC. (Bug
	// found post-merge of PR #126: enqueue worked but every sox
	// invocation failed during the worker pool's actual run.)
	//
	// Compute BOTH paths from the SINGLE SidecarPath() call and return
	// them: RunSox re-derives nothing, so it never re-hashes an over-cap /
	// FAT-sanitised filename and the rename target is GUARANTEED to match
	// the exact path sox was told to write — no independent recomputation
	// to drift (Q2). The token is what keeps two workers holding the same
	// spec off one temp path (see sidecarTmpTokenHexLen); it is also why
	// RunSox can no longer recover finalPath by trimming the suffix.
	finalPath = j.SidecarPath()
	tmpPath = finalPath + "." + nextSidecarTmpToken() + sidecarTmpSuffix
	args = []string{
		"-G",
		j.SourceAbsPath,
		"-b", strconv.Itoa(j.TargetBits),
		"-t", "flac",
		tmpPath,
		"rate", rateFlag, "-L", strconv.Itoa(j.TargetSampleRate),
		"dither", "-s",
	}
	settings = fmt.Sprintf(
		`{"resampler":"sox","quality":%q,"rateFlag":%q,"phase":"linear","targetRate":%d,"targetBits":%d,"guard":true,"schemaVersion":%q}`,
		j.Quality, rateFlag, j.TargetSampleRate, j.TargetBits, VariantSchemaVersion)
	return args, settings, finalPath, tmpPath
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

// OptimizeEligible returns true when the source is structurally
// higher fidelity than any CarPlay route accepts. Pre-filters at
// the eligibility layer so the downstream rate-resolver isn't
// called on no-op candidates.
//
// Carries `sourcePath` AND `codec` so legacy DB rows scanned before
// the codec column was populated (codec == "") can fall back to
// the on-disk extension. Without that fallback, `bridge optimize
// --all` on a pre-upgrade library would silently skip every track
// until the operator runs a destructive full re-scan to backfill
// the codec column.
//
// Eligibility:
//   - PCM-only (DSD / lossy MP3/AAC / unknown rejected).
//     .m4a is treated as PCM-candidate because ALAC-in-M4A is the
//     common lossless case; AAC-in-M4A fails the rate/bits gate at
//     the bottom (lossy is always 44.1/16 in practice).
//   - Higher than CarPlay floor: sourceRate > 48000 OR sourceBits > 16.
//     Already-at-floor 44.1/16 and 48/16 sources return false.
func OptimizeEligible(sourcePath, codec string, sourceRate, sourceBits int) bool {
	c := strings.ToUpper(strings.TrimSpace(codec))
	isPCM := c == "FLAC" || c == "ALAC" || c == "WAV" || c == "AIFF" || c == "PCM"

	if c == "" {
		ext := strings.ToLower(filepath.Ext(sourcePath))
		isPCM = ext == ".flac" || ext == ".wav" ||
			ext == ".aif" || ext == ".aiff" || ext == ".m4a"
	}

	if !isPCM {
		return false
	}
	return sourceRate > 48000 || sourceBits > 16
}

// TargetRateForOptimize picks the CarPlay-compatible downsample
// target while preserving sample-rate-family alignment so SoX
// takes the integer-ratio fast path:
//
//	44.1k family (44100 / 88200 / 176400) → 44100  (exact /1, /2, /4)
//	48k family   (48000 / 96000 / 192000) → 48000  (exact /1, /2, /4)
//	Anything else (rare/exotic) → 48000  (broader compatibility floor)
//
// Wired CarPlay accepts up to 16-bit / 48 kHz uncompressed LPCM,
// wireless CarPlay encodes through a 16-bit / 48 kHz AAC pipeline,
// so 48 kHz is just as valid a CarPlay target as 44.1 kHz — picking
// the same family as the source avoids the fractional-resample CPU
// cost (`96000/44100 = 2.176…`) and the resampling artifacts a
// variable-rate filter has to defend against.
func TargetRateForOptimize(sourceRate int) int {
	if sourceRate%48000 == 0 {
		return 48000
	}
	if sourceRate%44100 == 0 {
		return 44100
	}
	return 48000
}

// ResolveTargetRateForOptimize is the optimize-kind analog of
// `ResolveTargetRate`. Pure pick — returns the family-preserving
// target rate unconditionally. Eligibility is the authoritative
// gate (see `OptimizeEligible`); this function does NOT
// re-evaluate "is the source already at the target".
//
// **Why no rate-only skip**: a 44.1/24 source is at the target
// rate (44.1k) but `OptimizeEligible` correctly returns true
// (bits > 16 → needs 24→16 downsample). A previous version of
// this function returned 0 when `sourceRate <= target`, which
// silently skipped legitimate bit-only optimize candidates at
// every call site that checked `if target == 0 { skip }`. The
// gate AND the resolver must agree on eligibility for the bit-
// only case to flow through. Per Gemini bot review on PR #270.
//
// **Don't reuse `ResolveTargetRate`** — its `target <= sourceRate`
// branch is correct for upscale (refuse downsampling), but wrong
// for optimize.
func ResolveTargetRateForOptimize(sourceRate int) (int, error) {
	if sourceRate <= 0 {
		return 0, fmt.Errorf("source rate must be positive, got %d", sourceRate)
	}
	return TargetRateForOptimize(sourceRate), nil
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
// sox output goes to `<sidecar>.<token>.tmp`, then we `os.Rename` to
// the final path on success — a crash mid-conversion leaves at most a
// `.tmp` file behind (cleaned up by `bridge upscale --gc`, which reaps
// by "not a known sidecar path" and so is name-shape-agnostic).
//
// The token makes the temp path per-JOB rather than per-SPEC, which is
// what lets two workers legitimately holding the same spec (DropInflight
// + re-submit) run concurrently without unlinking or publishing each
// other's in-progress output. See sidecarTmpTokenHexLen.
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
	// Both paths come from SoxArgs's single SidecarPath() computation (Q2) —
	// no re-hash here, and the rename target below is exactly the path sox
	// wrote. tmpPath carries a per-job token, so it MUST be the value from
	// THIS call (a second SoxArgs() would mint a different one).
	args, _, finalPath, tmpPath := j.SoxArgs()
	// v1.4 source-mirrored layout: sidecars land under
	// <OutputDir>/<libRel-dirname>/<filename>. The parent of
	// finalPath may NOT exist yet (first variant in a new album
	// folder) — MkdirAll on the parent covers both the OutputDir
	// root case AND the per-album subdir case. Pre-v1.4 only
	// needed MkdirAll(j.OutputDir); the per-file form is now
	// the load-bearing call (CodeRabbit CRITICAL on PR D1).
	if err := os.MkdirAll(filepath.Dir(finalPath), 0o755); err != nil {
		return 0, fmt.Errorf("mkdir sidecar dir: %w", err)
	}
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
	if err := atomicwrite.RenameWithRetryCtx(ctx, tmpPath, finalPath); err != nil {
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

// SoxInfo is the result of ProbeSox: where sox lives, its version, and —
// crucially for the bridge's pipeline — whether the build has FLAC support.
// The bridge forces `-t flac` for every conversion (see SoxArgs), so a
// FLAC-less sox passes the bare runnable check but fails EVERY job at
// runtime. FormatsKnown lets callers stay conservative: act on a confirmed
// FLAC-absence only, never on an unparseable help output.
type SoxInfo struct {
	Path         string
	Version      string   // e.g. "v14.4.2"; "" if the banner couldn't be parsed
	Formats      []string // lowercased AUDIO FILE FORMATS tokens
	FormatsKnown bool     // true iff the format block was found+parsed
	HasFLAC      bool     // Formats contains "flac"
}

// soxLookPath / soxProbeCommand are the test seams for ProbeSox —
// production points them at exec.LookPath / exec.CommandContext; tests
// inject a fake binary path + canned `sox --help` output so the full probe
// flow runs deterministically WITHOUT a real sox on the host (mirrors
// tailscale.commandContext, manifest.renameFunc). Tests MUST restore both
// via t.Cleanup; production code MUST NOT mutate them.
var (
	soxLookPath     = exec.LookPath
	soxProbeCommand = exec.CommandContext
)

// soxSectionHeaderRE matches a sox --help section header line —
// "AUDIO FILE FORMATS:", "AUDIO DEVICE DRIVERS:", "EFFECTS:", etc. Headers
// are an ALL-CAPS word group ending in a colon at column 0; the lowercase
// format tokens never match, so we terminate the format block on the NEXT
// header regardless of its name (robust to sox builds that reorder or omit
// later sections).
var soxSectionHeaderRE = regexp.MustCompile(`^[A-Z][A-Z0-9 /]*:`)

// soxAudioFileFormatsRE locates the start of the formats block. Matching the
// ORIGINAL text case-insensitively (rather than strings.ToUpper(text) +
// Index) avoids a full-output allocation per call AND is byte-accurate: a
// non-ASCII char before the header whose uppercase form differs in byte
// length would shift a ToUpper-derived index and corrupt the slice.
var soxAudioFileFormatsRE = regexp.MustCompile(`(?i)AUDIO FILE FORMATS:`)

// ProbeSox locates sox on PATH and inspects it with a SINGLE `sox --help`
// spawn — the help output carries both the version banner and the
// "AUDIO FILE FORMATS:" block, so one process call yields everything. The
// error contract matches the old PrecheckSox exactly (ErrSoxMissing when
// the binary is absent; a wrapped error on timeout / can't-start) so all
// existing callers' errors.Is checks keep working.
//
// **Bounded by a 2 s timeout** (PR #108) wrapped around the INCOMING ctx:
// a parent cancellation (CLI ^C, server shutdown) aborts the spawn early,
// while the 2 s cap still applies when called via PrecheckSox(Background())
// so a wedged PATH wrapper / hung sox can't deadlock startup.
//
// **Locale-pinned** (LC_ALL/LANG/LANGUAGE=C) so a translated help text
// can't defeat the header match; CombinedOutput because some builds print
// --help to stderr. A non-zero --help exit is NOT treated as failure (sox
// --help legitimately exits non-zero on some builds) — only an empty
// result or a timeout is.
func ProbeSox(ctx context.Context) (SoxInfo, error) {
	var info SoxInfo
	path, err := soxLookPath("sox")
	if err != nil {
		return info, fmt.Errorf("%w: %v", ErrSoxMissing, err)
	}
	info.Path = path

	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	cmd := soxProbeCommand(ctx, path, "--help")
	cmd.Env = append(os.Environ(), "LC_ALL=C", "LANG=C", "LANGUAGE=C")
	out, runErr := cmd.CombinedOutput()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return info, fmt.Errorf("sox --help timed out after 2s; broken PATH wrapper or hung process")
	}
	text := string(out)
	if strings.TrimSpace(text) == "" {
		// No output at all — couldn't actually run sox. Surface the
		// underlying error (matches the old --version failure path).
		if runErr != nil {
			return info, fmt.Errorf("sox --help failed: %w", runErr)
		}
		return info, fmt.Errorf("sox --help produced no output")
	}
	info.Version = parseSoxVersion(text)
	info.Formats, info.FormatsKnown = parseSoxFileFormats(text)
	for _, f := range info.Formats {
		if f == "flac" {
			info.HasFLAC = true
			break
		}
	}
	return info, nil
}

// PrecheckSox returns nil if `sox` is on PATH and runnable. It is a thin
// wrapper over ProbeSox (one probe implementation, no duplication); the
// FLAC-aware callers use ProbeSox directly while the boolean-only sites
// (e.g. the public /v1/upscale/stats adapter) keep this signature.
func PrecheckSox() error {
	_, err := ProbeSox(context.Background())
	return err
}

// parseSoxVersion extracts the sox version token ("v14.4.2") from the
// "sox: SoX v14.4.2" banner present in --help / --version output.
// Best-effort and cosmetic. Returns "" when absent OR when the token has no
// digit — some builds (notably Homebrew HEAD) print a bare "SoX v" with no
// number, which is not a useful version to surface.
func parseSoxVersion(text string) string {
	const marker = "SoX "
	i := strings.Index(text, marker)
	if i < 0 {
		return ""
	}
	rest := text[i+len(marker):]
	end := strings.IndexFunc(rest, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\n' || r == '\r'
	})
	tok := rest
	if end >= 0 {
		tok = rest[:end]
	}
	tok = strings.TrimSpace(tok)
	if strings.IndexFunc(tok, func(r rune) bool { return r >= '0' && r <= '9' }) < 0 {
		return "" // no digit (e.g. bare "v") — not a usable version
	}
	return tok
}

// parseSoxFileFormats extracts the audio file-format tokens from sox --help.
// The block starts at "AUDIO FILE FORMATS:" and runs until the next ALL-CAPS
// section header (soxSectionHeaderRE) or EOF — so it survives single-line,
// flush-left-wrapped, and indented-wrapped layouts without depending on the
// next section's name. Returns (formats, true) when the block is found;
// (nil, false) otherwise (callers then conservatively assume FLAC present).
func parseSoxFileFormats(text string) ([]string, bool) {
	loc := soxAudioFileFormatsRE.FindStringIndex(text)
	if loc == nil {
		return nil, false
	}
	rest := text[loc[1]:] // remainder of the header line + everything after
	var formats []string
	for _, line := range strings.Split(rest, "\n") {
		line = strings.TrimRight(line, "\r")
		if soxSectionHeaderRE.MatchString(strings.TrimSpace(line)) {
			break // next section header terminates the format block
		}
		for _, tok := range strings.Fields(line) {
			formats = append(formats, strings.ToLower(tok))
		}
	}
	return formats, true
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

// soxFormatsForExt maps a source extension to the tokens sox prints in its
// AUDIO FILE FORMATS block that would decode it — ANY match means readable.
//
// A list rather than one token because a format's handler name and its
// extension are not reliably the same word, and builds differ: WAV shows up
// as "wav" and sometimes "wavpcm", and the AIFF family's three extensions
// are covered by overlapping tokens.
//
// The MP4 entries are the point of the whole guard, and are why this must
// be a MAP MISS that fails open rather than an absent key. `.m4a` IS a
// shape the upstream gate forwards (ALAC is lossless, so nothing above
// excludes it), so leaving it unmapped would fail open and allow exactly
// the case this refuses — which is what the first draft did, and what
// TestSoxInfoCanDecode caught. Listing candidate tokens instead means
// every stock build refuses it (none carry them), while a build that grows
// MP4 support is allowed automatically with no code change here.
//
// Only shapes the pipeline can actually be handed are listed: lossy
// sources (manifest.IsLossyCodec) and DSD are already excluded upstream.
var soxFormatsForExt = map[string][]string{
	".flac": {"flac"},
	".wav":  {"wav", "wavpcm"},
	".aif":  {"aif", "aiff"},
	".aiff": {"aiff", "aif"},
	".aifc": {"aifc", "aiff"},
	".m4a":  {"mp4", "m4a"},
	".mp4":  {"mp4"},
	".m4b":  {"mp4", "m4a"},
	".m4p":  {"mp4", "m4a"},
}

// CanDecode reports whether this sox build can read the given source file.
//
// It exists because the eligibility gate and the decoder disagreed. ALAC
// clears every check upstream — manifest.IsLossyCodec doesn't list it (it
// is lossless), canSetBitsPerSample allowlists it, and OptimizeEligible
// names "ALAC" outright — so an .m4a reached sox, which has no MP4
// demuxer in any stock build. The job then failed after being advertised
// to the client as eligible: on iOS the wand renders enabled, the user
// taps it, and the work fails downstream.
//
// That path only became reachable when PR #440 started extracting PCM
// geometry for M4A. Before it, SampleRate was nil and the gate refused
// early — which is the honest answer this restores.
//
// Fail-OPEN in two cases, both deliberate:
//
//   - !FormatsKnown — an unparseable `sox --help` must never disable a
//     working install. Same posture as ProbeSox's HasFLAC contract.
//   - an extension absent from soxFormatsForExt — the map covers what the
//     upstream gate lets through; anything else is a shape this guard was
//     not written to judge, and refusing it here would silently narrow
//     the pipeline as a side effect of an unrelated change. (MP4
//     extensions are therefore listed rather than omitted — see the map.)
//
// The check is against the LIVE build's format list, so it also covers
// the minimal-install case ProbeSox's HasFLAC field handles globally: an
// apt sox without libsox-fmt-all can't read FLAC either, and this refuses
// those per-source instead of only at feature-gate time.
func (i SoxInfo) CanDecode(sourcePath string) bool {
	if !i.FormatsKnown {
		return true
	}
	candidates, mapped := soxFormatsForExt[strings.ToLower(filepath.Ext(sourcePath))]
	if !mapped {
		return true
	}
	for _, want := range candidates {
		if slices.Contains(i.Formats, want) {
			return true
		}
	}
	return false
}
