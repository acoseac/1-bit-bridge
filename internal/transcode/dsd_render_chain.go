package transcode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/analyze"
	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

// The DSD → PCM render chain — the branch of Run that a routeFFmpegDSDPipe
// source takes. Three stages, all inside Run's atomic-publish contract:
//
//	A  ffmpeg decodes the DSD at UNITY and hands sox a float pipe at fs/8
//	   (ffprobe reports the decoder's fs/8 rate directly, so soxStdinInputArgs
//	   describes the pipe verbatim — 352 800 Hz for DSD64). The pipe carries
//	   `-af volume=0.5`: a float exponent decrement, measured bit-exact, that
//	   gives sox's int32 domain 6.02 dB of headroom so a hot source cannot
//	   clip at the float→int input or inside `rate` / `sinc`. It MUST stay on
//	   the ffmpeg side — `sox -v 0.5` was measured to clip a hot float pipe at
//	   the input conversion BEFORE applying -v. sox decimates to the target
//	   rate (and low-passes at 30/40 kHz for the faithful tier) into an
//	   UNCOMPRESSED SoX-native int32 scratch on local disk: sox's own sample
//	   domain, no FLAC encode+decode+decode on a CPU-bound host, no RIFF 4 GB
//	   ceiling. No `-G`: the pre-attenuation is the headroom, and a linear-
//	   phase overshoot is bounded by the taps' ℓ1 norm well under 6 dB.
//	B  the scratch's BS.1770 true peak (4× oversampled at its native rate)
//	   → TP_unity = TP + 6.0206 (the pre-attenuation removed)
//	   → G = clamp(0, 6, −TP_unity − 1 dBTP), rounded to 0.1 dB — the
//	   track's own guard. When the job carries an AlbumGainer, G is the
//	   album's shared boost instead, never above the track's own guard
//	   (album_gain.go).
//	C  sox reads the scratch and writes the FLAC sidecar with
//	   `gain 6.0206 + G` — exactly the pre-attenuation undone PLUS the
//	   clip-guarded gain, so the rendition sits at unity + G and its true
//	   peak lands at or below −1 dBTP for any source that peaks at or below
//	   −1 dBTP at unity. The guard cannot attenuate (its clamp stops at 0),
//	   so a hotter source keeps its own peak, and one above 0 dBTP clips in
//	   this stage and is refused (ErrDSDClipped; plan-2026-09-09-loupe-w5.md
//	   P0d, escalated) — then `dither -s`.
//
// Both sox stages FAIL on any `clipped` line in sox's stderr: the arithmetic
// says it cannot happen, and the check is the belt. Measured (and pinned by
// the TestRunFFmpegPipe_*Clipping* pair): sox warns `input clipped N
// samples` when a hot float pipe hits its int32 input conversion — with or
// without a `-v` on that input, since -v applies AFTER the conversion — and
// `rate clipped N samples` when an effect overshoots; both exit 0, so the
// warning is the only signal and the check must read it. The ×0.5
// pre-attenuation is what PREVENTS the input clip; the check is what would
// catch a regression that removed it. The applied gain is returned
// (RunResult.AppliedGainDB) and recorded in the settings blob; the iOS
// client attenuates by exactly that value when the user asked for 0 dB.
//
// An earlier draft wrote Stage C's gain as `6.0206 + G − 6`, which leaves the
// file 6 dB quiet and silently discards the SACD +6 dB compensation; the
// real-toolchain test pins RMS = −23.01 + 6 dB on a −20 dBFS tone so it
// cannot recur.

// RunResult is what Run produces for one job.
type RunResult struct {
	SizeBytes int64
	// Settings is the opaque JSON the pool stores in track_variants.sox_settings.
	Settings string
	// AppliedGainDB is the clip-guarded gain baked into a DSD rendition
	// (0 ≤ G ≤ 6). Nil for every PCM job — the wire and the column both
	// treat absence as "not a DSD rendition".
	AppliedGainDB *float64
	// TruePeakDBTP is the intermediate's true peak at UNITY decode, i.e.
	// before the applied gain; nil for PCM jobs and for a digitally silent
	// source (nothing measured).
	TruePeakDBTP *float64
	// PeakProfile is the DSDPeakProfile TruePeakDBTP was measured on, so
	// the writer can record the peak for the album-level gain beside the
	// rendition (manifest.VariantRow.PeakProfile). Empty for PCM jobs.
	PeakProfile string
}

const (
	// dsdPreAttenuationLinear is the float multiplier ffmpeg applies in Stage
	// A; dsdPreAttenuationDB is the same number as sox's `gain` sees it
	// (20·log10(2), four decimals — what the harness measured RMS parity
	// against).
	dsdPreAttenuationLinear = 0.5
	dsdPreAttenuationDB     = 6.0206
	// dsdNominalGainDB is the SACD +6 dB convention: DSD masters are cut ~6 dB
	// below PCM full scale, and this is the ceiling the clip guard works down
	// from. It matches the phone converter's default.
	dsdNominalGainDB = 6.0
	// dsdClipGuardHeadroomDBTP is how far below full scale the rendition's
	// true peak must land after the gain.
	dsdClipGuardHeadroomDBTP = 1.0
	// renderScratchSubdir is the bridge-owned directory under TempDir where
	// Stage A scratch lives; the startup / --gc purge sweeps ONLY this
	// directory, never a shared temp root.
	renderScratchSubdir = "1-bit-bridge-render"
	// renderScratchSuffix names a Stage A scratch file; the purge matches on
	// it so a foreign file in the directory is never touched.
	renderScratchSuffix = ".stageA.sox"
	// renderScratchMaxAge is how old an orphaned scratch (a crash between
	// Stage A and the deferred remove) must be before the purge reclaims it —
	// comfortably past the 4 h job-timeout cap.
	renderScratchMaxAge = 12 * time.Hour
)

// dsdLowpassArgs is the faithful tier's finishing low-pass: the phone
// converter's 30 kHz pass / 40 kHz stop at ≥100 dB, expressed as sox's
// `sinc` with a 10 kHz transition band centred on 35 kHz. Applied AFTER
// `rate` (cheaper at 176.4 kHz than at 352.8 kHz); the compact tier needs
// none — `rate`'s own anti-alias filter removes the DSD noise shelf on the
// way to 44.1 / 48 kHz.
var dsdLowpassArgs = []string{"sinc", "-a", "110", "-t", "10000", "-35000"}

// ErrDSDGeometryMismatch is returned when ffprobe's decoder geometry does not
// describe the source the manifest recorded — a pipe declared at the wrong
// rate renders a half- or double-speed variant, so this fails CLOSED.
var ErrDSDGeometryMismatch = errors.New("dsd render: decoder geometry disagrees with the source")

// ErrDSDClipped is returned when a sox stage reported clipping. The chain's
// arithmetic makes that unreachable; the check is the belt.
var ErrDSDClipped = errors.New("dsd render: sox reported clipping")

// ErrDSDDecodeUnavailable refuses a DSD job that did NOT route to the DSD
// chain. Run picks its route from the source's EXTENSION plus the decoder
// probe, while the eligibility gates admit a row on its CODEC — so two
// shapes reach Run with SourceIsDSD set and a non-DSD route: a host whose
// ffmpeg lacks the dsd_* decoders (routeNone), and a row the scanner
// stamped DSF/DFF whose filename says otherwise. Both used to fall
// through to sox-direct, where sox either fails with its own unrelated
// diagnostic or — for a shape it happens to accept — publishes a file
// under a DSD variant id with none of the DSD semantics: no clip guard,
// no measured true peak, no appliedGainDB. Refusing is the fail-closed
// answer and it names the real cause (CodeRabbit on PR #863).
//
// The two shapes are different facts, and the pool strikes only one: Run
// marks the first as this host's (markToolUnavailable), since installing a
// DSD-capable ffmpeg is its remedy, and leaves the second, a row whose file
// no toolchain can fix, to strike as before.
var ErrDSDDecodeUnavailable = errors.New("dsd render: source is DSD but the DSD decode route is unavailable")

// ffmpegDSDDecodeArgs is ffmpegDecodeArgs plus the ×0.5 pre-attenuation —
// see the chain docblock for why the attenuation lives here and not in sox.
func ffmpegDSDDecodeArgs(srcAbs string) []string {
	return []string{
		"-nostdin", "-hide_banner", "-loglevel", "error",
		"-i", srcAbs,
		"-map", "0:a:0",
		"-af", "volume=" + strconv.FormatFloat(dsdPreAttenuationLinear, 'f', -1, 64),
		"-f", "f32le",
		"-",
	}
}

// ClipGuardedGainDB is the Stage B decision: the gain that lands a source
// whose true peak at unity is truePeakUnityDBTP at −1 dBTP, clamped to
// [0, 6] and rounded to 0.1 dB. A non-finite peak (a silent or unreadable
// measurement) yields 0 — the safe direction when nothing was measured.
func ClipGuardedGainDB(truePeakUnityDBTP float64) float64 {
	if math.IsNaN(truePeakUnityDBTP) || math.IsInf(truePeakUnityDBTP, 0) {
		return 0
	}
	g := -truePeakUnityDBTP - dsdClipGuardHeadroomDBTP
	g = math.Max(0, math.Min(dsdNominalGainDB, g))
	return math.Round(g*10) / 10
}

// soxReportedClipping recognises sox's clip warnings (`rate clipped N
// samples`, `dither clipped`, `input clipped`) in a stage's stderr.
func soxReportedClipping(stderr string) bool {
	return strings.Contains(strings.ToLower(stderr), "clipped")
}

// dsdExpectedDurationSec is the completeness guard's reference: ffprobe's
// container duration when it reported one, else the manifest's. Without the
// fallback a probe that reports 0 makes decodeLengthDisagrees vacuous, and a
// half-decoded DSD would publish.
func dsdExpectedDurationSec(probed, manifest float64) float64 {
	if probed > 0 {
		return probed
	}
	if manifest > 0 {
		return manifest
	}
	return 0
}

// sameRateFamily reports whether two rates share a 44.1k / 48k family.
//
// Membership on BOTH sides, not agreement about membership. The equality form
// answered true for a pair in NEITHER family — (32000, 32000) is
// `false == false` twice — which is not what the name says and is not what
// validateDSDGeometry wants from it. Not reachable today, since every pipe
// rate is fs/8 of a DSD rate and every target is a 44.1/48 multiple, so this
// is the contract being made to match the doc rather than a live fix.
func sameRateFamily(a, b int) bool {
	return (a%44100 == 0 && b%44100 == 0) || (a%48000 == 0 && b%48000 == 0)
}

// validateDSDGeometry pins the relationship between what ffprobe reports
// (the decoder's fs/8 pipe) and what the manifest recorded (the nominal DSD
// rate, e.g. 2 822 400 for DSD64): pipe × 8 == nominal, the channel count
// agrees when the spec carries one, and the pipe is in the target's rate
// family. A wrong pipe rate is not a slower render — it is a variant at the
// wrong pitch — so any disagreement fails closed.
func validateDSDGeometry(geo sourceGeometry, j JobSpec) error {
	if geo.SampleRate <= 0 || geo.Channels <= 0 {
		return fmt.Errorf("%w: ffprobe reported rate=%d channels=%d (%s)",
			ErrDSDGeometryMismatch, geo.SampleRate, geo.Channels, j.SourceLibraryRel)
	}
	nominal := j.SourceSampleRate
	if nominal > 0 && geo.SampleRate*8 != nominal {
		return fmt.Errorf("%w: decoder pipe %d Hz × 8 ≠ manifest rate %d Hz (%s)",
			ErrDSDGeometryMismatch, geo.SampleRate, nominal, j.SourceLibraryRel)
	}
	if j.SourceChannels > 0 && geo.Channels != j.SourceChannels {
		return fmt.Errorf("%w: decoder reports %d channels, manifest %d (%s)",
			ErrDSDGeometryMismatch, geo.Channels, j.SourceChannels, j.SourceLibraryRel)
	}
	if nominal == 0 {
		nominal = geo.SampleRate * 8
	}
	if j.TargetSampleRate > 0 && !sameRateFamily(nominal, j.TargetSampleRate) {
		return fmt.Errorf("%w: DSD rate %d Hz and target %d Hz are in different rate families (%s)",
			ErrDSDGeometryMismatch, nominal, j.TargetSampleRate, j.SourceLibraryRel)
	}
	return nil
}

// TempBytesForRender is the Stage A scratch size for a render: int32
// samples at the TARGET rate for the source's duration — 4 × channels ×
// targetRate × seconds. The target rate, never the decoder's fs/8 pipe rate:
// the scratch is written AFTER `rate`, and sizing it at the pipe rate would
// over-estimate by 2–8× and refuse valid jobs on a tight temp volume. So the
// figure is the same whatever the source's DSD rate: an hour of stereo →
// 176.4 kHz is 5.08 GB, → 44.1 kHz is 1.27 GB.
func TempBytesForRender(channels, targetRate int, durationSec float64) int64 {
	if channels <= 0 || targetRate <= 0 || durationSec <= 0 || math.IsNaN(durationSec) || math.IsInf(durationSec, 0) {
		return 0
	}
	return int64(math.Ceil(4 * float64(channels) * float64(targetRate) * durationSec))
}

// RenderScratchDir is renderScratchDir for callers outside the package —
// the sweeper's scratch-volume probe and the CLI's `--gc` purge — so every
// consumer agrees on where scratch lives.
func RenderScratchDir(tempDir string) string { return renderScratchDir(tempDir) }

// PurgeStaleRenderScratch is purgeStaleRenderScratch at the standard purge
// age (renderScratchMaxAge), for `bridge serve` startup and `--gc`: the
// crash case (SIGKILL, power loss) the deferred remove cannot cover.
func PurgeStaleRenderScratch(tempDir string) (int, error) {
	return purgeStaleRenderScratch(tempDir, renderScratchMaxAge, time.Now())
}

// renderScratchDir is where Stage A scratch lives: a bridge-owned
// subdirectory of the configured temp dir (the OS temp dir when unset). On
// the VPS the variants dir is a B2 FUSE mount, so scratch must never derive
// from OutputDir.
func renderScratchDir(tempDir string) string {
	if strings.TrimSpace(tempDir) == "" {
		tempDir = os.TempDir()
	}
	return filepath.Join(tempDir, renderScratchSubdir)
}

// purgeStaleRenderScratch removes Stage A scratch files older than olderThan
// from the bridge-owned scratch directory — the crash case the deferred
// remove cannot cover (SIGKILL, power loss). Only files carrying
// renderScratchSuffix inside renderScratchDir are candidates; a foreign file
// in the parent, a subdirectory, or a fresh scratch a live job is writing is
// never touched. A missing directory is not an error.
func purgeStaleRenderScratch(tempDir string, olderThan time.Duration, now time.Time) (removed int, err error) {
	dir := renderScratchDir(tempDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	var firstErr error
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), renderScratchSuffix) {
			continue
		}
		info, ierr := e.Info()
		if ierr != nil {
			continue
		}
		if now.Sub(info.ModTime()) <= olderThan {
			continue
		}
		if rerr := os.Remove(filepath.Join(dir, e.Name())); rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
			if firstErr == nil {
				firstErr = rerr
			}
			continue
		}
		removed++
	}
	return removed, firstErr
}

// rateFlag maps the quality preset onto sox's `rate` quality flag.
func (j JobSpec) rateFlag() string {
	switch j.Quality {
	case QualityHigh:
		return "-h"
	case QualityMedium:
		return "-m"
	default:
		return "-v"
	}
}

// dsdStageAArgs is Stage A's sox argv: the float pipe in, the int32 scratch
// out, `rate` to the target, and the faithful tier's finishing low-pass.
// `--temp` keeps sox's own temporaries (the `rate` effect buffers nothing,
// but `--temp` costs nothing and a future effect that does must not land on
// the FUSE mount) beside the scratch. No `-G`.
func (j JobSpec) dsdStageAArgs(geo sourceGeometry, scratchDir, scratchPath string) []string {
	args := []string{"--temp", scratchDir}
	args = append(args, soxStdinInputArgs(geo)...)
	args = append(args,
		"-e", "signed", "-b", "32", "-t", "sox", scratchPath,
		"rate", j.rateFlag(), "-L", strconv.Itoa(j.TargetSampleRate),
	)
	if j.Kind == JobKindPCMRender {
		args = append(args, dsdLowpassArgs...)
	}
	return args
}

// dsdStageCArgs is Stage C's sox argv: the scratch in, the FLAC sidecar out
// at the tier's bit depth, `gain` = the pre-attenuation undone plus the
// clip-guarded gain, then shaped dither. No `-G`.
func (j JobSpec) dsdStageCArgs(scratchDir, scratchPath, tmpPath string, totalGainDB float64) []string {
	return []string{
		"--temp", scratchDir,
		scratchPath,
		"-b", strconv.Itoa(j.TargetBits),
		"-t", "flac",
		tmpPath,
		"gain", strconv.FormatFloat(totalGainDB, 'f', 4, 64),
		"dither", "-s",
	}
}

// dsdRenderSettings is the sox_settings blob for a DSD rendition. It is a
// SUPERSET of the PCM chain's blob (same first seven keys), marshalled AFTER
// Stage B because the gain is not known before it. `appliedGainDB` /
// `truePeakDBTP` are pointers so a measured 0.0 ships `0` and an unmeasured
// (silent) source ships `null`. The column is opaque TEXT; the rule for any
// reader is ParseSoxSettings's — every field optional, an unparseable blob is
// displayed raw.
type dsdRenderSettings struct {
	Resampler            string   `json:"resampler"`
	Decoder              string   `json:"decoder"`
	Quality              Quality  `json:"quality"`
	RateFlag             string   `json:"rateFlag"`
	Phase                string   `json:"phase"`
	TargetRate           int      `json:"targetRate"`
	TargetBits           int      `json:"targetBits"`
	Guard                bool     `json:"guard"`
	SchemaVersion        string   `json:"schemaVersion"`
	Kind                 JobKind  `json:"kind"`
	DSDRate              int      `json:"dsdRate"`
	PipeRate             int      `json:"pipeRate"`
	Channels             int      `json:"channels"`
	PreAttenuationLinear float64  `json:"preAttenuationLinear"`
	NominalGainDB        float64  `json:"nominalGainDB"`
	AppliedGainDB        *float64 `json:"appliedGainDB"`
	TruePeakDBTP         *float64 `json:"truePeakDBTP"`
	// GainScope says which rule decided AppliedGainDB: GainScopeTrack (the
	// track's own clip guard) or GainScopeAlbum (the album's shared boost,
	// bounded by that guard). TrackGainDB is the track's own guard, recorded
	// either way, so an album-scoped row still says what the album cost it.
	GainScope   string   `json:"gainScope"`
	TrackGainDB *float64 `json:"trackGainDB"`
	Lowpass     string   `json:"lowpass,omitempty"`
}

func (j JobSpec) dsdSettings(geo sourceGeometry, appliedGainDB float64, truePeakUnity *float64, gainScope string, trackGainDB float64) (string, error) {
	s := dsdRenderSettings{
		Resampler:            "sox",
		Decoder:              routeFFmpegDSDPipe.String(),
		Quality:              j.Quality,
		RateFlag:             j.rateFlag(),
		Phase:                "linear",
		TargetRate:           j.TargetSampleRate,
		TargetBits:           j.TargetBits,
		Guard:                false,
		SchemaVersion:        DSDRenditionSchemaVersion,
		Kind:                 j.Kind,
		DSDRate:              geo.SampleRate * 8,
		PipeRate:             geo.SampleRate,
		Channels:             geo.Channels,
		PreAttenuationLinear: dsdPreAttenuationLinear,
		NominalGainDB:        dsdNominalGainDB,
		AppliedGainDB:        &appliedGainDB,
		TruePeakDBTP:         truePeakUnity,
		GainScope:            gainScope,
		TrackGainDB:          &trackGainDB,
	}
	if j.Kind == JobKindPCMRender {
		s.Lowpass = strings.Join(dsdLowpassArgs, " ")
	}
	b, err := json.Marshal(s)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// SoxSettingsView is the tolerant read model for track_variants.sox_settings:
// every field optional, so the PCM chain's blob, a DSD rendition's blob and
// a future shape all decode without error. ParseSoxSettings reports ok=false
// only when the text is not a JSON object at all — a reader then shows the
// raw text rather than failing.
type SoxSettingsView struct {
	Decoder       string   `json:"decoder"`
	SchemaVersion string   `json:"schemaVersion"`
	Kind          string   `json:"kind"`
	TargetRate    int      `json:"targetRate"`
	TargetBits    int      `json:"targetBits"`
	Guard         bool     `json:"guard"`
	AppliedGainDB *float64 `json:"appliedGainDB"`
	TruePeakDBTP  *float64 `json:"truePeakDBTP"`
	GainScope     string   `json:"gainScope"`
	TrackGainDB   *float64 `json:"trackGainDB"`
	Lowpass       string   `json:"lowpass"`
}

// ParseSoxSettings decodes a sox_settings blob into the tolerant view.
func ParseSoxSettings(blob string) (SoxSettingsView, bool) {
	var v SoxSettingsView
	if err := json.Unmarshal([]byte(blob), &v); err != nil {
		return SoxSettingsView{}, false
	}
	return v, true
}

// soxFileDuration reads a file's duration through sox itself (`sox --i -D`),
// which is the reader that wrote the `.sox` scratch. 0 on any failure, which
// the completeness guard treats as "unknown".
func soxFileDuration(ctx context.Context, path string) float64 {
	out, err := exec.CommandContext(ctx, resolveBin(func() (string, error) { return soxLookPath("sox") }, "sox"),
		"--i", "-D", path).Output()
	if err != nil {
		return 0
	}
	return parseProbeDuration(strings.TrimSpace(string(out)))
}

// soxFileSamples reads how many samples per channel a `.sox` scratch holds,
// through sox (`sox --i -s`). sox rewrites the scratch's length when it
// closes it, so this is what Stage A wrote, a failed write included
// (measured, backlog B264). 0 on any failure: unknown.
func soxFileSamples(ctx context.Context, path string) uint64 {
	out, err := exec.CommandContext(ctx, resolveBin(func() (string, error) { return soxLookPath("sox") }, "sox"),
		"--i", "-s", path).Output()
	if err != nil {
		return 0
	}
	n, err := strconv.ParseUint(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// publishSidecar is the atomic publish both chains share: rename the temp
// onto the final path (same filesystem, so rename(2)) and stat the result.
//
// It asks first whether the source is still the version the spec records,
// and publishes nothing when it is not (ErrSourceChanged): a file that
// changed while it rendered was read, in part or whole, as bytes the stamp
// does not describe, and the serve path would refuse the result anyway. The
// caller's deferred cleanup removes the temp. A method, and the only publish
// helper, so a chain cannot publish without the check.
//
// The rename and the stat after it touch the variants directory alone, so
// each marks its failure as the output side's (output_fault.go).
func (j JobSpec) publishSidecar(ctx context.Context, tmpPath, finalPath string) (int64, error) {
	if err := j.sourceChanged("it changed while it rendered, so the rendition was discarded"); err != nil {
		return 0, err
	}
	sidecarDir := filepath.Dir(finalPath)
	if err := atomicwrite.RenameWithRetryCtx(ctx, tmpPath, finalPath); err != nil {
		return 0, markOutputFault(outputVariants, sidecarDir, fmt.Errorf("rename sidecar: %w", err))
	}
	info, err := os.Stat(finalPath)
	if err != nil {
		return 0, markOutputFault(outputVariants, sidecarDir, fmt.Errorf("stat sidecar: %w", err))
	}
	return info.Size(), nil
}

// dsdGeometry probes the decoder's geometry and checks it, and the job's
// target, before any stage runs — the checks a render and a measurement
// share.
func (j JobSpec) dsdGeometry(ctx context.Context) (sourceGeometry, error) {
	geo, err := probeSourceGeometry(ctx, j.SourceAbsPath)
	if err != nil {
		return sourceGeometry{}, err
	}
	if err := validateDSDGeometry(geo, j); err != nil {
		return sourceGeometry{}, err
	}
	if j.TargetSampleRate <= 0 || j.TargetBits <= 0 {
		return sourceGeometry{}, fmt.Errorf("dsd render: target %d Hz / %d bit is not a rendition (%s)",
			j.TargetSampleRate, j.TargetBits, j.SourceLibraryRel)
	}
	return geo, nil
}

// decodeAndMeasure runs Stage A into scratchPath and Stage B over it,
// returning the true peak at UNITY decode — nil for a digitally silent
// source — and the samples per channel the scratch holds (0 when sox could
// not say), which Stage C's rendition must hold too. The caller owns
// scratchPath and removes it on every exit; a render keeps it for Stage C, a
// measurement discards it.
//
// A Stage A that did not leave a whole scratch is asked of the scratch's
// volume (scratchOutput): sox exits 0 after a write it could not make, and
// stops reading, so a full scratch volume ends in ffmpeg's broken pipe or a
// short scratch, which a source that decoded short also leaves.
func (j JobSpec) decodeAndMeasure(ctx context.Context, geo sourceGeometry, scratchDir, scratchPath string) (*float64, uint64, error) {
	// Stage A — decode at unity (×0.5 on the pipe), decimate into the scratch.
	soxStderr, err := runFFmpegPipe(ctx, j.dsdStageAArgs(geo, scratchDir, scratchPath), ffmpegDSDDecodeArgs(j.SourceAbsPath))
	if err != nil {
		if ctx.Err() != nil {
			return nil, 0, err
		}
		return nil, 0, scratchOutput(scratchDir, scratchPath, err)
	}
	if soxReportedClipping(soxStderr) {
		return nil, 0, fmt.Errorf("%w in stage A (%s): %s", ErrDSDClipped, j.SourceLibraryRel, firstLine(soxStderr))
	}
	expected := dsdExpectedDurationSec(geo.Duration, j.SourceDurationSec)
	samples := soxFileSamples(ctx, scratchPath)
	if produced := float64(samples) / float64(j.TargetSampleRate); decodeLengthDisagrees(expected, produced) {
		return nil, 0, scratchOutput(scratchDir, scratchPath, fmt.Errorf("%w: source %.3fs, produced %.3fs (%s)",
			ErrFFmpegDecodeIncomplete, expected, produced, j.SourceLibraryRel))
	}

	// Stage B — true peak of the scratch, back at unity.
	tp, measured, err := analyze.TruePeakDBTP(ctx, scratchPath, geo.Channels)
	if err != nil {
		return nil, 0, fmt.Errorf("dsd render: true peak of %s: %w", j.SourceLibraryRel, err)
	}
	if !measured {
		return nil, samples, nil
	}
	u := tp + dsdPreAttenuationDB
	return &u, samples, nil
}

// MeasureDSDPeak runs Stages A and B for j and returns the source's true
// peak at unity decode — nil for a digitally silent source — publishing
// nothing. It is how the album survey measures a track that has no rendition
// yet: the render's own decode, scratch and meter, so the number is the one
// a render of j would measure. Its scratch is removed before it returns.
//
// It refuses what Run refuses: a source that is not DSD, or one that does not
// route to the DSD chain (ErrDSDDecodeUnavailable).
func MeasureDSDPeak(ctx context.Context, j JobSpec) (*float64, error) {
	if !j.SourceIsDSD {
		return nil, fmt.Errorf("measure dsd peak: %s is not a DSD source", j.SourceLibraryRel)
	}
	if !needsDecodeRouting(j.SourceAbsPath) ||
		decodeRouteFor(SnapshotOrOpen(func() (SoxInfo, error) { return ProbeSox(ctx) }),
			FFmpegSnapshot(), j.SourceAbsPath) != routeFFmpegDSDPipe {
		return nil, fmt.Errorf("%w (source %q)", ErrDSDDecodeUnavailable, filepath.Base(j.SourceAbsPath))
	}
	geo, err := j.dsdGeometry(ctx)
	if err != nil {
		return nil, err
	}
	scratchDir := renderScratchDir(j.TempDir)
	if err := j.mkdirScratch(scratchDir); err != nil {
		return nil, markOutputFault(outputScratch, scratchDir, fmt.Errorf("mkdir render scratch dir: %w", err))
	}
	scratchPath := filepath.Join(scratchDir, nextSidecarTmpToken()+renderScratchSuffix)
	_ = os.Remove(scratchPath)
	defer func() { _ = os.Remove(scratchPath) }()
	if err := createOutput(scratchPath, scratchDir); err != nil {
		return nil, markOutputFault(outputScratch, scratchDir, fmt.Errorf("create render scratch: %w", err))
	}
	peak, _, err := j.decodeAndMeasure(ctx, geo, scratchDir, scratchPath)
	return peak, err
}

// mkdirScratch creates the Stage A scratch directory. Its default home is
// the OS temp dir, shared by every user, where a `sudo bridge render` (or
// `optimize`) left it root's and 0700, so every DSD render of the service
// then failed to create its scratch there. Run as root, a directory it
// creates in such a shared directory takes the owner of the variants
// directory the render publishes into (fsutil.MkdirAllShared); anywhere
// else, its parent's, which a configured temp dir the service owns gives.
func (j JobSpec) mkdirScratch(scratchDir string) error {
	return fsutil.MkdirAllShared(scratchDir, 0o700, j.OutputDir)
}

// renderDSD is the DSD branch of Run — see the chain docblock at the top of
// this file. It owns its scratch and its temp sidecar and reaps both on every
// exit; only a successful publish keeps the final file.
func (j JobSpec) renderDSD(ctx context.Context) (RunResult, error) {
	// Claim this track's peak before anything can fail, and resolve the
	// claim on every exit: an album-mate's survey waiting on it must never
	// wait on a render that already gave up.
	var resolveClaim func(*float64, error)
	if j.AlbumGain != nil {
		resolveClaim = j.AlbumGain.Claim(ctx, j)
		defer func() {
			if resolveClaim != nil {
				resolveClaim(nil, fmt.Errorf("dsd render of %s ended before its peak was measured", j.SourceLibraryRel))
			}
		}()
	}

	geo, err := j.dsdGeometry(ctx)
	if err != nil {
		return RunResult{}, err
	}

	finalPath := j.SidecarPath()
	sidecarDir := filepath.Dir(finalPath)
	token := nextSidecarTmpToken()
	tmpPath := finalPath + "." + token + sidecarTmpSuffix
	// Every step that writes to the variants directory or the render
	// scratch marks its own failure, as in Run (output_fault.go).
	if err := fsutil.MkdirAll(sidecarDir, 0o755); err != nil {
		return RunResult{}, markOutputFault(outputVariants, sidecarDir, fmt.Errorf("mkdir sidecar dir: %w", err))
	}
	_ = os.Remove(tmpPath)
	scratchDir := renderScratchDir(j.TempDir)
	if err := j.mkdirScratch(scratchDir); err != nil {
		return RunResult{}, markOutputFault(outputScratch, scratchDir, fmt.Errorf("mkdir render scratch dir: %w", err))
	}
	scratchPath := filepath.Join(scratchDir, token+renderScratchSuffix)
	_ = os.Remove(scratchPath)
	cleanup := true
	defer func() {
		_ = os.Remove(scratchPath)
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()
	// Stage C's sox writes the rendition and Stage A's the scratch: both
	// created here, before Stage A, so a directory that refuses new files
	// fails now, by type, and not after the decode, in a sox message. The
	// rendition takes the install's owner, as in Run, so a
	// `sudo bridge render` leaves one the service can replace; the scratch
	// takes its directory's. Nothing on Windows (createOutput).
	if err := createOutput(tmpPath, finalPath); err != nil {
		return RunResult{}, markOutputFault(outputVariants, sidecarDir, fmt.Errorf("create sidecar: %w", err))
	}
	if err := createOutput(scratchPath, scratchDir); err != nil {
		return RunResult{}, markOutputFault(outputScratch, scratchDir, fmt.Errorf("create render scratch: %w", err))
	}

	// Stages A and B — decode into the scratch and measure it at unity.
	truePeakUnity, scratchSamples, err := j.decodeAndMeasure(ctx, geo, scratchDir, scratchPath)
	if err != nil {
		return RunResult{}, err
	}
	if resolveClaim != nil {
		resolveClaim(truePeakUnity, nil)
		resolveClaim = nil
	}
	trackGain := dsdNominalGainDB
	if truePeakUnity != nil {
		trackGain = ClipGuardedGainDB(*truePeakUnity)
	}
	gain, gainScope := trackGain, GainScopeTrack
	if j.AlbumGain != nil {
		albumGain, ok, aerr := j.AlbumGain.AlbumGainDB(ctx, j, truePeakUnity)
		if aerr != nil {
			return RunResult{}, fmt.Errorf("dsd render: album gain for %s: %w", j.SourceLibraryRel, aerr)
		}
		if ok {
			gain, gainScope = albumBoundedGain(trackGain, albumGain), GainScopeAlbum
		}
	}

	// Stage C — undo the pre-attenuation, apply the gain, dither, publish.
	stageC := exec.CommandContext(ctx, resolveBin(func() (string, error) { return soxLookPath("sox") }, "sox"),
		j.dsdStageCArgs(scratchDir, scratchPath, tmpPath, dsdPreAttenuationDB+gain)...)
	out, err := stageC.CombinedOutput()
	if err != nil {
		return RunResult{}, fmt.Errorf("sox: %w (stderr: %s)", err, strings.TrimSpace(string(out)))
	}
	if soxReportedClipping(string(out)) {
		return RunResult{}, fmt.Errorf("%w in stage C (%s): %s", ErrDSDClipped, j.SourceLibraryRel, firstLine(string(out)))
	}
	// Stage C exits 0 after a write it could not make, as every sox does,
	// so the rendition is read (rendition_complete.go). Its gain and dither
	// keep the scratch's length to the sample, so a whole rendition holds
	// exactly what the scratch held (measured, backlog B264).
	whole, err := j.wholeRendition(tmpPath, sidecarDir)
	if err != nil {
		return RunResult{}, err
	}
	if scratchSamples > 0 && whole.held != scratchSamples {
		return RunResult{}, fmt.Errorf("%w: the scratch holds %d samples, the rendition %d (%s)",
			ErrRenditionIncomplete, scratchSamples, whole.held, j.SourceLibraryRel)
	}
	settings, err := j.dsdSettings(geo, gain, truePeakUnity, gainScope, trackGain)
	if err != nil {
		return RunResult{}, fmt.Errorf("dsd render: settings: %w", err)
	}
	size, err := j.publishSidecar(ctx, tmpPath, finalPath)
	if err != nil {
		return RunResult{}, err
	}
	cleanup = false
	logger.Debug("dsd render ok",
		"path", j.SourceLibraryRel,
		"variant", j.VariantID(),
		"applied_gain_db", gain,
		"gain_scope", gainScope,
		"sidecar_bytes", size)
	return RunResult{SizeBytes: size, Settings: settings, AppliedGainDB: &gain,
		TruePeakDBTP: truePeakUnity, PeakProfile: j.DSDPeakProfile()}, nil
}
