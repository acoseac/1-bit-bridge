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
//	   → G = clamp(0, 6, −TP_unity − 1 dBTP), rounded to 0.1 dB.
//	C  sox reads the scratch and writes the FLAC sidecar with
//	   `gain 6.0206 + G` — exactly the pre-attenuation undone PLUS the
//	   clip-guarded gain, so the rendition sits at unity + G and its true
//	   peak lands at or below −1 dBTP by construction — then `dither -s`.
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
func sameRateFamily(a, b int) bool {
	return (a%44100 == 0) == (b%44100 == 0) && (a%48000 == 0) == (b%48000 == 0)
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
	Lowpass              string   `json:"lowpass,omitempty"`
}

func (j JobSpec) dsdSettings(geo sourceGeometry, appliedGainDB float64, truePeakUnity *float64) (string, error) {
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

// publishSidecar is the atomic publish both chains share: rename the temp
// onto the final path (same filesystem, so rename(2)) and stat the result.
func publishSidecar(ctx context.Context, tmpPath, finalPath string) (int64, error) {
	if err := atomicwrite.RenameWithRetryCtx(ctx, tmpPath, finalPath); err != nil {
		return 0, fmt.Errorf("rename sidecar: %w", err)
	}
	info, err := os.Stat(finalPath)
	if err != nil {
		return 0, fmt.Errorf("stat sidecar: %w", err)
	}
	return info.Size(), nil
}

// renderDSD is the DSD branch of Run — see the chain docblock at the top of
// this file. It owns its scratch and its temp sidecar and reaps both on every
// exit; only a successful publish keeps the final file.
func (j JobSpec) renderDSD(ctx context.Context) (RunResult, error) {
	geo, err := probeSourceGeometry(ctx, j.SourceAbsPath)
	if err != nil {
		return RunResult{}, err
	}
	if err := validateDSDGeometry(geo, j); err != nil {
		return RunResult{}, err
	}
	if j.TargetSampleRate <= 0 || j.TargetBits <= 0 {
		return RunResult{}, fmt.Errorf("dsd render: target %d Hz / %d bit is not a rendition (%s)",
			j.TargetSampleRate, j.TargetBits, j.SourceLibraryRel)
	}

	finalPath := j.SidecarPath()
	token := nextSidecarTmpToken()
	tmpPath := finalPath + "." + token + sidecarTmpSuffix
	if err := os.MkdirAll(filepath.Dir(finalPath), 0o755); err != nil {
		return RunResult{}, fmt.Errorf("mkdir sidecar dir: %w", err)
	}
	_ = os.Remove(tmpPath)
	scratchDir := renderScratchDir(j.TempDir)
	if err := os.MkdirAll(scratchDir, 0o700); err != nil {
		return RunResult{}, fmt.Errorf("mkdir render scratch dir: %w", err)
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

	// Stage A — decode at unity (×0.5 on the pipe), decimate into the scratch.
	soxStderr, err := runFFmpegPipe(ctx, j.dsdStageAArgs(geo, scratchDir, scratchPath), ffmpegDSDDecodeArgs(j.SourceAbsPath))
	if err != nil {
		return RunResult{}, err
	}
	if soxReportedClipping(soxStderr) {
		return RunResult{}, fmt.Errorf("%w in stage A (%s): %s", ErrDSDClipped, j.SourceLibraryRel, firstLine(soxStderr))
	}
	expected := dsdExpectedDurationSec(geo.Duration, j.SourceDurationSec)
	if produced := soxFileDuration(ctx, scratchPath); decodeLengthDisagrees(expected, produced) {
		return RunResult{}, fmt.Errorf("%w: source %.3fs, produced %.3fs (%s)",
			ErrFFmpegDecodeIncomplete, expected, produced, j.SourceLibraryRel)
	}

	// Stage B — true peak of the scratch, at unity, → the clip-guarded gain.
	tp, measured, err := analyze.TruePeakDBTP(ctx, scratchPath, geo.Channels)
	if err != nil {
		return RunResult{}, fmt.Errorf("dsd render: true peak of %s: %w", j.SourceLibraryRel, err)
	}
	gain := dsdNominalGainDB
	var truePeakUnity *float64
	if measured {
		u := tp + dsdPreAttenuationDB
		truePeakUnity = &u
		gain = ClipGuardedGainDB(u)
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
	settings, err := j.dsdSettings(geo, gain, truePeakUnity)
	if err != nil {
		return RunResult{}, fmt.Errorf("dsd render: settings: %w", err)
	}
	size, err := publishSidecar(ctx, tmpPath, finalPath)
	if err != nil {
		return RunResult{}, err
	}
	cleanup = false
	logger.Debug("dsd render ok",
		"path", j.SourceLibraryRel,
		"variant", j.VariantID(),
		"applied_gain_db", gain,
		"sidecar_bytes", size)
	return RunResult{SizeBytes: size, Settings: settings, AppliedGainDB: &gain, TruePeakDBTP: truePeakUnity}, nil
}
