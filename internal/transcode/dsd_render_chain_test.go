package transcode

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/dsdtone"
)

// --- argv goldens ---------------------------------------------------------

func TestFFmpegDSDDecodeArgs(t *testing.T) {
	got := strings.Join(ffmpegDSDDecodeArgs("/lib/a.dsf"), " ")
	want := "-nostdin -hide_banner -loglevel error -i /lib/a.dsf -map 0:a:0 -af volume=0.5 -f f32le -"
	if got != want {
		t.Errorf("ffmpegDSDDecodeArgs =\n  %s\nwant\n  %s", got, want)
	}
	// The PCM pipe's argv is untouched: the pre-attenuation is DSD-only.
	if strings.Contains(strings.Join(ffmpegDecodeArgs("/lib/a.m4a"), " "), "volume") {
		t.Error("the PCM fallback must not be attenuated")
	}
}

func TestDSDStageAArgs_PerTier(t *testing.T) {
	geo := sourceGeometry{SampleRate: 352800, Channels: 2, Duration: 3}
	pcm := JobSpec{Kind: JobKindPCMRender, Quality: QualityVeryHigh, TargetSampleRate: 176400, TargetBits: 24}
	opt := JobSpec{Kind: JobKindOptimize, SourceIsDSD: true, Quality: QualityVeryHigh, TargetSampleRate: 44100, TargetBits: 16}

	gotPCM := strings.Join(pcm.dsdStageAArgs(geo, "/scratch", "/scratch/tok.stageA.sox"), " ")
	wantPCM := "--temp /scratch -t raw -e float -b 32 -L -r 352800 -c 2 - -e signed -b 32 -t sox /scratch/tok.stageA.sox rate -v -L 176400 sinc -a 110 -t 10000 -35000"
	if gotPCM != wantPCM {
		t.Errorf("faithful stage A =\n  %s\nwant\n  %s", gotPCM, wantPCM)
	}
	gotOpt := strings.Join(opt.dsdStageAArgs(geo, "/scratch", "/scratch/tok.stageA.sox"), " ")
	wantOpt := "--temp /scratch -t raw -e float -b 32 -L -r 352800 -c 2 - -e signed -b 32 -t sox /scratch/tok.stageA.sox rate -v -L 44100"
	if gotOpt != wantOpt {
		t.Errorf("compact stage A =\n  %s\nwant\n  %s", gotOpt, wantOpt)
	}
	for name, args := range map[string][]string{"pcm": pcm.dsdStageAArgs(geo, "/s", "/s/a"), "opt": opt.dsdStageAArgs(geo, "/s", "/s/a")} {
		if slices.Contains(args, "-G") {
			t.Errorf("%s: no -G on the DSD chain — the ×0.5 pre-attenuation IS the headroom", name)
		}
	}
	// The pipe rate is ffprobe's fs/8 VERBATIM — never divided again.
	if !strings.Contains(gotPCM, "-r 352800 ") {
		t.Error("stage A must describe the pipe at the decoder's fs/8 rate")
	}
	// Quality presets ride through.
	high := JobSpec{Kind: JobKindPCMRender, Quality: QualityHigh, TargetSampleRate: 176400, TargetBits: 24}
	if !strings.Contains(strings.Join(high.dsdStageAArgs(geo, "/s", "/s/a"), " "), "rate -h -L 176400") {
		t.Error("the quality preset must select sox's rate flag")
	}
}

func TestDSDStageCArgs(t *testing.T) {
	pcm := JobSpec{Kind: JobKindPCMRender, TargetSampleRate: 176400, TargetBits: 24}
	got := strings.Join(pcm.dsdStageCArgs("/scratch", "/scratch/tok.stageA.sox", "/v/a.dsf.pcm-v1-176400-24.flac.tok.tmp", dsdPreAttenuationDB+5.0), " ")
	want := "--temp /scratch /scratch/tok.stageA.sox -b 24 -t flac /v/a.dsf.pcm-v1-176400-24.flac.tok.tmp gain 11.0206 dither -s"
	if got != want {
		t.Errorf("stage C =\n  %s\nwant\n  %s", got, want)
	}
	if strings.Contains(got, "-G") {
		t.Error("no -G on stage C either")
	}
	opt := JobSpec{Kind: JobKindOptimize, SourceIsDSD: true, TargetSampleRate: 44100, TargetBits: 16}
	if !strings.Contains(strings.Join(opt.dsdStageCArgs("/s", "/s/a", "/v/t", dsdPreAttenuationDB), " "), "-b 16 -t flac /v/t gain 6.0206 dither -s") {
		t.Error("compact stage C must write 16-bit and undo exactly the pre-attenuation when G is 0")
	}
}

// --- pure decisions -------------------------------------------------------

func TestClipGuardedGainDB(t *testing.T) {
	cases := []struct {
		tpUnity float64
		want    float64
	}{
		{-9, 6.0},    // quiet: full nominal gain
		{-7, 6.0},    // exactly at the ceiling
		{-6.99, 6.0}, // 5.99 rounds up to the ceiling
		{-4.2, 3.2},
		{-1.05, 0.1},
		{-1.0, 0},
		{-0.5, 0}, // hot master: no gain, never negative
		{0, 0},
		{1, 0},
		{math.NaN(), 0},
		{math.Inf(1), 0},
		{math.Inf(-1), 0},
	}
	for _, tc := range cases {
		if got := ClipGuardedGainDB(tc.tpUnity); got != tc.want {
			t.Errorf("ClipGuardedGainDB(%v) = %v, want %v", tc.tpUnity, got, tc.want)
		}
	}
}

func TestSoxReportedClipping(t *testing.T) {
	for stderr, want := range map[string]bool{
		"sox WARN rate: rate clipped 12 samples; decrease volume?": true,
		"sox WARN dither: dither clipped 3 samples":                true,
		"sox WARN sox: `raw' input clipped 23600 samples":          true,
		"": false,
		"sox WARN formats: MAGIC IDs do not match": false,
	} {
		if got := soxReportedClipping(stderr); got != want {
			t.Errorf("soxReportedClipping(%q) = %v, want %v", stderr, got, want)
		}
	}
}

func TestDSDExpectedDurationFallsBackToManifest(t *testing.T) {
	if got := dsdExpectedDurationSec(0, 60); got != 60 {
		t.Errorf("probe 0 must fall back to the manifest: got %v", got)
	}
	if got := dsdExpectedDurationSec(61, 60); got != 61 {
		t.Errorf("a reported probe wins: got %v", got)
	}
	if got := dsdExpectedDurationSec(0, 0); got != 0 {
		t.Errorf("both unknown → 0: got %v", got)
	}
	// The guard is only as good as its reference: with the fallback a
	// half-decoded render is refused; without it the check is vacuous.
	if !decodeLengthDisagrees(dsdExpectedDurationSec(0, 60), 30) {
		t.Error("ffprobe 0, manifest 60 s, produced 30 s must be refused")
	}
	if decodeLengthDisagrees(dsdExpectedDurationSec(0, 0), 30) {
		t.Error("with no reference at all the guard is skipped (documented), not inverted")
	}
}

func TestValidateDSDGeometry(t *testing.T) {
	pcm64 := JobSpec{Kind: JobKindPCMRender, SourceSampleRate: 2822400, SourceChannels: 2, TargetSampleRate: 176400, SourceLibraryRel: "a.dsf"}
	cases := []struct {
		name string
		geo  sourceGeometry
		spec JobSpec
		ok   bool
	}{
		{"DSD64 stereo at fs/8", sourceGeometry{SampleRate: 352800, Channels: 2}, pcm64, true},
		{"channels unknown in the manifest are accepted", sourceGeometry{SampleRate: 352800, Channels: 2},
			JobSpec{Kind: JobKindPCMRender, SourceSampleRate: 2822400, TargetSampleRate: 176400}, true},
		{"rate unknown in the manifest is accepted when the family agrees", sourceGeometry{SampleRate: 352800, Channels: 2},
			JobSpec{Kind: JobKindPCMRender, TargetSampleRate: 176400}, true},
		{"48k family", sourceGeometry{SampleRate: 384000, Channels: 2},
			JobSpec{Kind: JobKindPCMRender, SourceSampleRate: 3072000, SourceChannels: 2, TargetSampleRate: 192000}, true},
		{"compact tier at 44.1k", sourceGeometry{SampleRate: 352800, Channels: 2},
			JobSpec{Kind: JobKindOptimize, SourceIsDSD: true, SourceSampleRate: 2822400, TargetSampleRate: 44100}, true},

		{"pipe at the nominal rate (fs, not fs/8) refuses", sourceGeometry{SampleRate: 2822400, Channels: 2}, pcm64, false},
		{"pipe at fs/64 refuses", sourceGeometry{SampleRate: 44100, Channels: 2}, pcm64, false},
		{"channel mismatch refuses", sourceGeometry{SampleRate: 352800, Channels: 1}, pcm64, false},
		{"zero geometry refuses", sourceGeometry{}, pcm64, false},
		{"family mismatch refuses (48k pipe, 44.1k target)", sourceGeometry{SampleRate: 384000, Channels: 2},
			JobSpec{Kind: JobKindPCMRender, TargetSampleRate: 176400}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateDSDGeometry(tc.geo, tc.spec)
			if (err == nil) != tc.ok {
				t.Errorf("validateDSDGeometry = %v, want ok=%v", err, tc.ok)
			}
			if err != nil && !isErr(err, ErrDSDGeometryMismatch) {
				t.Errorf("refusal must be typed ErrDSDGeometryMismatch, got %v", err)
			}
		})
	}
}

func isErr(err, target error) bool {
	for e := err; e != nil; {
		if e == target {
			return true
		}
		u, ok := e.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		e = u.Unwrap()
	}
	return false
}

func TestTempBytesForRender(t *testing.T) {
	// Sized at the TARGET rate: the same numbers whatever the source's DSD
	// rate, which is why the function has no source-rate parameter at all.
	if got := TempBytesForRender(2, 176400, 3600); got != 5_080_320_000 {
		t.Errorf("stereo hour at 176.4k = %d, want 5,080,320,000", got)
	}
	if got := TempBytesForRender(2, 44100, 3600); got != 1_270_080_000 {
		t.Errorf("stereo hour at 44.1k = %d, want 1,270,080,000", got)
	}
	if got := TempBytesForRender(2, 176400, 3); got != 4_233_600 {
		t.Errorf("3 s stereo at 176.4k = %d, want 4,233,600 (the measured scratch size)", got)
	}
	for _, bad := range []struct {
		ch, rate int
		s        float64
	}{{0, 176400, 60}, {2, 0, 60}, {2, 176400, 0}, {2, 176400, math.NaN()}, {2, 176400, math.Inf(1)}} {
		if got := TempBytesForRender(bad.ch, bad.rate, bad.s); got != 0 {
			t.Errorf("TempBytesForRender(%d, %d, %v) = %d, want 0", bad.ch, bad.rate, bad.s, got)
		}
	}
}

func TestRenderScratchDir(t *testing.T) {
	if got := renderScratchDir("/mnt/tmp"); got != filepath.Join("/mnt/tmp", renderScratchSubdir) {
		t.Errorf("renderScratchDir = %q", got)
	}
	if got := renderScratchDir(""); got != filepath.Join(os.TempDir(), renderScratchSubdir) {
		t.Errorf("empty TempDir must resolve to the OS temp dir: %q", got)
	}
}

func TestPurgeStaleRenderScratch(t *testing.T) {
	tempDir := t.TempDir()
	dir := renderScratchDir(tempDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	touch := func(path string, age time.Duration) {
		t.Helper()
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		mt := now.Add(-age)
		if err := os.Chtimes(path, mt, mt); err != nil {
			t.Fatal(err)
		}
	}
	fresh := filepath.Join(dir, "aaaa0001.stageA.sox")
	stale := filepath.Join(dir, "aaaa0002.stageA.sox")
	other := filepath.Join(dir, "notes.txt")
	foreign := filepath.Join(tempDir, "bbbb0003.stageA.sox") // in the PARENT, not ours
	touch(fresh, 1*time.Hour)
	touch(stale, 13*time.Hour)
	touch(other, 30*time.Hour)
	touch(foreign, 30*time.Hour)
	if err := os.MkdirAll(filepath.Join(dir, "sub.stageA.sox"), 0o700); err != nil {
		t.Fatal(err)
	}

	removed, err := purgeStaleRenderScratch(tempDir, renderScratchMaxAge, now)
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if removed != 1 {
		t.Errorf("removed %d, want exactly the one stale scratch", removed)
	}
	for _, keep := range []string{fresh, other, foreign, filepath.Join(dir, "sub.stageA.sox")} {
		if _, err := os.Stat(keep); err != nil {
			t.Errorf("%s must survive the purge: %v", filepath.Base(keep), err)
		}
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("the 13 h scratch must be gone (stat err=%v)", err)
	}
	// Missing directory is not an error.
	if n, err := purgeStaleRenderScratch(filepath.Join(tempDir, "never-created"), renderScratchMaxAge, now); n != 0 || err != nil {
		t.Errorf("missing dir → (0, nil), got (%d, %v)", n, err)
	}
}

func TestDSDSettingsShape(t *testing.T) {
	geo := sourceGeometry{SampleRate: 352800, Channels: 2}
	pcm := JobSpec{Kind: JobKindPCMRender, Quality: QualityVeryHigh, TargetSampleRate: 176400, TargetBits: 24}
	tp := -3.4
	blob, err := pcm.dsdSettings(geo, 2.4, &tp)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(blob), &m); err != nil {
		t.Fatalf("settings must be JSON: %v\n%s", err, blob)
	}
	want := map[string]any{
		"resampler": "sox", "decoder": "ffmpeg-dsd+sox", "quality": "very-high", "rateFlag": "-v", "phase": "linear",
		"targetRate": float64(176400), "targetBits": float64(24), "guard": false, "schemaVersion": "v1", "kind": "pcm",
		"dsdRate": float64(2822400), "pipeRate": float64(352800), "channels": float64(2),
		"preAttenuationLinear": 0.5, "nominalGainDB": 6.0, "appliedGainDB": 2.4, "truePeakDBTP": -3.4,
		"lowpass": "sinc -a 110 -t 10000 -35000",
	}
	for k, v := range want {
		if m[k] != v {
			t.Errorf("settings[%q] = %v (%T), want %v", k, m[k], m[k], v)
		}
	}
	// A measured 0.0 gain ships as 0, and an unmeasured peak as null — the
	// compact tier carries no lowpass key at all.
	opt := JobSpec{Kind: JobKindOptimize, SourceIsDSD: true, Quality: QualityVeryHigh, TargetSampleRate: 44100, TargetBits: 16}
	blob, err = opt.dsdSettings(geo, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(blob, `"appliedGainDB":0`) || !strings.Contains(blob, `"truePeakDBTP":null`) || strings.Contains(blob, "lowpass") {
		t.Errorf("compact/silent blob shape wrong: %s", blob)
	}
}

func TestParseSoxSettings_LegacyAndDSDBlobs(t *testing.T) {
	legacy := `{"resampler":"sox","quality":"very-high","rateFlag":"-v","targetRate":48000,"targetBits":16,"guard":true,"schemaVersion":"v2"}`
	v, ok := ParseSoxSettings(legacy)
	if !ok || v.SchemaVersion != "v2" || v.TargetRate != 48000 || !v.Guard || v.AppliedGainDB != nil || v.Decoder != "" {
		t.Errorf("legacy blob parsed wrong: ok=%v %+v", ok, v)
	}
	geo := sourceGeometry{SampleRate: 352800, Channels: 2}
	blob, _ := JobSpec{Kind: JobKindPCMRender, TargetSampleRate: 176400, TargetBits: 24}.dsdSettings(geo, 5.0, nil)
	v, ok = ParseSoxSettings(blob)
	if !ok || v.Decoder != "ffmpeg-dsd+sox" || v.AppliedGainDB == nil || *v.AppliedGainDB != 5.0 || v.TruePeakDBTP != nil || v.Kind != "pcm" {
		t.Errorf("DSD blob parsed wrong: ok=%v %+v", ok, v)
	}
	if _, ok := ParseSoxSettings("sox -G a.flac -b 24 -t flac out rate -v -L 176400 dither -s"); ok {
		t.Error("a non-JSON blob must report ok=false so a reader shows it raw")
	}
}

// --- real toolchain --------------------------------------------------------

// requireSox gates a test that shells out to sox and nothing else. Kept
// separate from requireSoxAndFFmpeg so a sox-only measurement is not
// silently skipped on a host that simply has no ffmpeg.
func requireSox(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("sox"); err != nil {
		t.Skip("real sox not on PATH")
	}
}

func requireSoxAndFFmpeg(t *testing.T) {
	t.Helper()
	requireSox(t)
	for _, bin := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("real %s not on PATH", bin)
		}
	}
}

func requireDSDToolchain(t *testing.T) {
	t.Helper()
	requireSoxAndFFmpeg(t)
	resetFFmpegSnapshotForTest()
	if ff := FFmpegSnapshot(); !ff.HasDSD {
		t.Skipf("host ffmpeg lacks the DSD decoders (known=%v)", ff.DecodersKnown)
	}
}

// TestRunFFmpegPipe_SoxFailureLeadsTheError pins the error composition the
// DSD chain relies on: sox is waited on first and its diagnostic comes
// before ffmpeg's trailing EPIPE noise.
func TestRunFFmpegPipe_SoxFailureLeadsTheError(t *testing.T) {
	requireSoxAndFFmpeg(t)
	ffArgs := []string{"-nostdin", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "anullsrc=r=44100:cl=stereo", "-t", "0.2", "-f", "f32le", "-"}
	// `gain` with a non-numeric argument is a usage error sox raises while
	// parsing the effects chain, before it reads a byte of input. (An
	// UNKNOWN effect name would not do: sox's grammar takes any unknown
	// trailing word as the output file and the intended output as an input.)
	soxArgs := append(soxStdinInputArgs(sourceGeometry{SampleRate: 44100, Channels: 2}),
		"-t", "sox", filepath.Join(t.TempDir(), "out.sox"), "gain", "notanumber")
	stderr, err := runFFmpegPipe(context.Background(), soxArgs, ffArgs)
	if err == nil {
		t.Fatal("a sox usage error must fail the pipe")
	}
	msg := err.Error()
	if !strings.HasPrefix(msg, "sox:") {
		t.Errorf("error must lead with sox's failure: %q", msg)
	}
	si, fi := strings.Index(msg, "(sox stderr:"), strings.Index(msg, "(ffmpeg stderr:")
	if si < 0 || fi < 0 || si > fi {
		t.Errorf("sox's stderr must precede ffmpeg's: %q", msg)
	}
	if !strings.Contains(strings.ToLower(stderr), "gain") {
		t.Errorf("sox's stderr must be returned to the caller: %q", stderr)
	}
}

// TestRunFFmpegPipe_ClippingWarningIsReturnedOnSuccess measures the belt:
// a 0.99 square wave through `rate` overshoots (Gibbs) and sox clips it
// while still exiting 0 — the warning must reach the caller through the
// returned stderr, which is what the DSD chain's stage checks read.
func TestRunFFmpegPipe_ClippingWarningIsReturnedOnSuccess(t *testing.T) {
	requireSoxAndFFmpeg(t)
	ffArgs := []string{"-nostdin", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "aevalsrc=0.99*sgn(sin(2*PI*1000*t)):s=44100", "-t", "0.2",
		"-f", "f32le", "-"}
	soxArgs := append(soxStdinInputArgs(sourceGeometry{SampleRate: 44100, Channels: 1}),
		"-e", "signed", "-b", "32", "-t", "sox", filepath.Join(t.TempDir(), "out.sox"),
		"rate", "-v", "-L", "22050")
	stderr, err := runFFmpegPipe(context.Background(), soxArgs, ffArgs)
	if err != nil {
		t.Fatalf("sox exits 0 on a clip warning: %v", err)
	}
	if !soxReportedClipping(stderr) {
		t.Errorf("a near-full-scale square through rate must trip `rate clipped`; stderr=%q", stderr)
	}
}

// TestRunFFmpegPipe_InputClippingIsReported measures the other half of the
// belt: a genuinely hot float pipe (ffmpeg's `sine` source is fixed at
// −18 dBFS, so it needs aevalsrc) is clipped at sox's int32 INPUT with a
// `input clipped N samples` warning and exit 0 — with or without a `-v`
// on the input, since -v applies AFTER the conversion (the S6 measurement
// behind keeping the ×0.5 on the ffmpeg side). The chain's stage check
// reads exactly this line, so a regression that let a hot pipe reach sox
// would fail the render rather than publish a clipped rendition.
func TestRunFFmpegPipe_InputClippingIsReported(t *testing.T) {
	requireSoxAndFFmpeg(t)
	out := filepath.Join(t.TempDir(), "out.sox")
	ffArgs := []string{"-nostdin", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "aevalsrc=1.5*sin(2*PI*1000*t):s=44100", "-t", "0.2",
		"-f", "f32le", "-"}
	soxArgs := append(soxStdinInputArgs(sourceGeometry{SampleRate: 44100, Channels: 1}),
		"-e", "signed", "-b", "32", "-t", "sox", out)
	stderr, err := runFFmpegPipe(context.Background(), soxArgs, ffArgs)
	if err != nil {
		t.Fatalf("sox exits 0 on an input clip warning: %v", err)
	}
	if !soxReportedClipping(stderr) {
		t.Fatalf("a +3.5 dBFS float pipe must trip sox's `input clipped` warning; stderr=%q", stderr)
	}
	if peak, _ := soxStats(t, out); peak < -0.05 {
		t.Errorf("the pipe must have been clipped to full scale; peak = %.2f dB", peak)
	}
}

// soxStats reads sox's `stats` peak and RMS (dBFS) for a file.
func soxStats(t *testing.T, path string) (peakDB, rmsDB float64) {
	t.Helper()
	out, err := exec.Command("sox", path, "-n", "stats").CombinedOutput()
	if err != nil {
		t.Fatalf("sox stats: %v (%s)", err, out)
	}
	peakDB, rmsDB = math.NaN(), math.NaN()
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) >= 4 && f[0] == "Pk" && f[1] == "lev" {
			peakDB, _ = strconv.ParseFloat(f[3], 64)
		}
		if len(f) >= 4 && f[0] == "RMS" && f[1] == "lev" {
			rmsDB, _ = strconv.ParseFloat(f[3], 64)
		}
	}
	if math.IsNaN(peakDB) || math.IsNaN(rmsDB) {
		t.Fatalf("could not parse sox stats:\n%s", out)
	}
	return peakDB, rmsDB
}

func soxInfoInt(t *testing.T, path, flag string) int {
	t.Helper()
	out, err := exec.Command("sox", "--i", flag, path).Output()
	if err != nil {
		t.Fatalf("sox --i %s: %v", flag, err)
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatalf("sox --i %s = %q", flag, out)
	}
	return n
}

// TestRunDSD_RealToolchain renders first-order-SDM fixtures through the
// shipping chain and checks the contract the whole design rests on: the
// container geometry, the completeness, the clip-guarded gain per tier, and
// LEVEL PARITY — a −20 dBFS tone lands at RMS −23.01 + 6 dB, which is what
// pins Stage C's `6.0206 + G` (a `− 6` would leave every rendition 6 dB
// quiet and read as a silent failure of the SACD compensation).
func TestRunDSD_RealToolchain(t *testing.T) {
	requireDSDToolchain(t)
	root := t.TempDir()
	lib := filepath.Join(root, "lib", "Album")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	const seconds = 3.0
	mint := func(name string, ampDB float64, dff bool) string {
		t.Helper()
		p := filepath.Join(lib, name)
		var err error
		if dff {
			_, err = dsdtone.MintDFF(p, dsdtone.Tone{RateHz: 2822400, Seconds: seconds, AmplitudeDBFS: ampDB})
		} else {
			_, err = dsdtone.MintDSF(p, dsdtone.Tone{RateHz: 2822400, Seconds: seconds, AmplitudeDBFS: ampDB})
		}
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	m20 := mint("tone-m20.dsf", -20, false)
	m6 := mint("tone-m6.dsf", -6, false)
	dff20 := mint("tone-m20.dff", -20, true)
	outDir := filepath.Join(root, "variants")
	tempDir := filepath.Join(root, "tmp")

	spec := func(src string, kind JobKind, rate, bits int) JobSpec {
		return JobSpec{
			SourceAbsPath: src, SourceLibraryRel: "Album/" + filepath.Base(src),
			SourceSampleRate: 2822400, SourceIsDSD: true, SourceChannels: 2, SourceDurationSec: seconds,
			TargetSampleRate: rate, TargetBits: bits, Quality: QualityVeryHigh,
			OutputDir: outDir, TempDir: tempDir, Kind: kind,
		}
	}
	cases := []struct {
		name             string
		spec             JobSpec
		wantVariant      string
		gainMin, gainMax float64
		wantRMS          float64 // NaN = don't check
		peakMax          float64 // NaN = don't check
	}{
		{"DSF −20 faithful", spec(m20, JobKindPCMRender, 176400, 24), "pcm-v1-176400-24", 6.0, 6.0, -17.01, math.NaN()},
		{"DSF −20 compact", spec(m20, JobKindOptimize, 44100, 16), "optimized-dsd-v1-44100-16", 6.0, 6.0, -17.01, math.NaN()},
		{"DSF −6 faithful", spec(m6, JobKindPCMRender, 176400, 24), "pcm-v1-176400-24", 4.6, 5.1, math.NaN(), -0.8},
		{"DSF −6 compact", spec(m6, JobKindOptimize, 44100, 16), "optimized-dsd-v1-44100-16", 4.9, 5.1, math.NaN(), -0.8},
		{"DFF −20 faithful", spec(dff20, JobKindPCMRender, 176400, 24), "pcm-v1-176400-24", 6.0, 6.0, -17.01, math.NaN()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.spec.VariantID(); got != tc.wantVariant {
				t.Fatalf("VariantID = %q, want %q", got, tc.wantVariant)
			}
			r, err := Run(context.Background(), tc.spec)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			final := tc.spec.SidecarPath()
			info, err := os.Stat(final)
			if err != nil {
				t.Fatalf("sidecar not published: %v", err)
			}
			if info.Size() != r.SizeBytes || r.SizeBytes == 0 {
				t.Errorf("SizeBytes = %d, stat = %d", r.SizeBytes, info.Size())
			}
			if r.AppliedGainDB == nil {
				t.Fatal("a DSD rendition must report its applied gain")
			}
			if g := *r.AppliedGainDB; g < tc.gainMin || g > tc.gainMax {
				t.Errorf("applied gain = %.2f dB, want in [%.1f, %.1f]", g, tc.gainMin, tc.gainMax)
			}
			if r.TruePeakDBTP == nil {
				t.Error("a tone has a measurable true peak")
			}
			if rate := soxInfoInt(t, final, "-r"); rate != tc.spec.TargetSampleRate {
				t.Errorf("rendition rate = %d, want %d", rate, tc.spec.TargetSampleRate)
			}
			if bits := soxInfoInt(t, final, "-b"); bits != tc.spec.TargetBits {
				t.Errorf("rendition bits = %d, want %d", bits, tc.spec.TargetBits)
			}
			if d := soxFileDuration(context.Background(), final); math.Abs(d-seconds)/seconds > durationTolerance {
				t.Errorf("rendition duration = %.3f s, want %.1f within %.0f%%", d, seconds, durationTolerance*100)
			}
			peak, rms := soxStats(t, final)
			if !math.IsNaN(tc.wantRMS) && math.Abs(rms-tc.wantRMS) > 0.05 {
				t.Errorf("RMS = %.2f dB, want %.2f ± 0.05 (level parity: −23.01 + 6)", rms, tc.wantRMS)
			}
			if !math.IsNaN(tc.peakMax) && (peak > tc.peakMax || peak < -1.6) {
				t.Errorf("peak = %.2f dB, want in [−1.6, %.1f] (the clip guard lands hot sources at ≈ −1 dBTP)", peak, tc.peakMax)
			}
			v, ok := ParseSoxSettings(r.Settings)
			if !ok || v.Decoder != "ffmpeg-dsd+sox" || v.SchemaVersion != DSDRenditionSchemaVersion || v.AppliedGainDB == nil || *v.AppliedGainDB != *r.AppliedGainDB || v.Kind != string(tc.spec.Kind) {
				t.Errorf("settings blob wrong: ok=%v %+v", ok, v)
			}
			if (v.Lowpass != "") != (tc.spec.Kind == JobKindPCMRender) {
				t.Errorf("only the faithful tier records a lowpass: %+v", v)
			}
		})
	}
	// Every scratch and temp was reaped; only the published sidecars remain.
	if entries, err := os.ReadDir(renderScratchDir(tempDir)); err != nil || len(entries) != 0 {
		t.Errorf("scratch dir must be empty after the runs: %v (err=%v)", entries, err)
	}
	if strays, _ := filepath.Glob(filepath.Join(outDir, "*", "*"+sidecarTmpSuffix)); len(strays) != 0 {
		t.Errorf("temp sidecars left behind: %v", strays)
	}
}

// TestRunDSD_RealToolchain_RefusesGeometryMismatch drives the fail-closed
// geometry check through the real ffprobe: a manifest rate that disagrees
// with what the decoder emits must refuse, publish nothing, and leave no
// scratch behind.
func TestRunDSD_RealToolchain_RefusesGeometryMismatch(t *testing.T) {
	requireDSDToolchain(t)
	root := t.TempDir()
	src := filepath.Join(root, "tone.dsf")
	if _, err := dsdtone.MintDSF(src, dsdtone.Tone{RateHz: 2822400, Seconds: 0.2, AmplitudeDBFS: -20}); err != nil {
		t.Fatal(err)
	}
	tempDir := filepath.Join(root, "tmp")
	j := JobSpec{
		SourceAbsPath: src, SourceLibraryRel: "tone.dsf",
		SourceSampleRate: 5644800, // lies: the file is DSD64
		SourceIsDSD:      true, TargetSampleRate: 176400, TargetBits: 24, Quality: QualityVeryHigh,
		OutputDir: filepath.Join(root, "variants"), TempDir: tempDir, Kind: JobKindPCMRender,
	}
	_, err := Run(context.Background(), j)
	if !isErr(err, ErrDSDGeometryMismatch) {
		t.Fatalf("want ErrDSDGeometryMismatch, got %v", err)
	}
	if _, serr := os.Stat(j.SidecarPath()); !os.IsNotExist(serr) {
		t.Error("a refused render must publish nothing")
	}
	if entries, _ := os.ReadDir(renderScratchDir(tempDir)); len(entries) != 0 {
		t.Errorf("no scratch may remain: %v", entries)
	}
}
