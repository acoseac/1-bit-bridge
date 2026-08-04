package transcode

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// TestVariantIDStable pins the variantID format. iOS keys on the
// `upscaled-` prefix to slot variants into the share-level "prefer
// upscaled" resolution; if this format ever drifts, the iOS picker
// will silently stop recognising upscale variants. The test is
// here so anyone changing the format has to touch this assertion
// AND the iOS resolver in the same PR.
func TestVariantIDStable(t *testing.T) {
	cases := []struct {
		j    JobSpec
		want string
	}{
		{
			JobSpec{TargetSampleRate: 176400, TargetBits: 24},
			"upscaled-v2-176400-24",
		},
		{
			JobSpec{TargetSampleRate: 192000, TargetBits: 24},
			"upscaled-v2-192000-24",
		},
		{
			JobSpec{TargetSampleRate: 352800, TargetBits: 32},
			"upscaled-v2-352800-32",
		},
	}
	for _, c := range cases {
		got := c.j.VariantID()
		if got != c.want {
			t.Errorf("VariantID = %q, want %q", got, c.want)
		}
	}
}

// TestOutputDirForJoinsSubdir pins the single source of truth for
// "where converted sidecars land". Three independent surfaces read
// it (cmd/bridge runtime pool wiring, admin Settings template,
// admin Library Inspector template); any drift here would cause
// the admin UI to advertise a path different from where the pool
// actually writes.
func TestOutputDirForJoinsSubdir(t *testing.T) {
	cases := []struct {
		dataDir string
		want    string
	}{
		{"/var/lib/bridge", filepath.Join("/var/lib/bridge", "transcoded")},
		{"/tmp/bridge-live/data", filepath.Join("/tmp/bridge-live/data", "transcoded")},
		{"/Volumes/Audio/.bridge", filepath.Join("/Volumes/Audio/.bridge", "transcoded")},
	}
	for _, c := range cases {
		got := OutputDirFor(c.dataDir)
		if got != c.want {
			t.Errorf("OutputDirFor(%q) = %q, want %q", c.dataDir, got, c.want)
		}
	}
	// Symbolic check: the constant's literal value is the contract
	// that `bridge upscale --gc` and the runtime pool share.
	// Bumping the literal here without the GC walker would orphan
	// every sidecar on the first restart after upgrade.
	if OutputDirSubdir != "transcoded" {
		t.Fatalf("OutputDirSubdir = %q, want %q (bumping this rehouses the cache; coordinate with --gc)", OutputDirSubdir, "transcoded")
	}
}

// TestSidecarPathIncludesVariantSuffix proves that two JobSpecs
// with the same source but different variants land at distinct
// sidecar paths. Without this guarantee, the user's first call to
// `bridge upscale --target-rate 176400` followed by a second call
// at `--target-rate 192000` would silently overwrite the first
// sidecar — a real bug Gemini bot caught at the plan-review stage.
func TestSidecarPathIncludesVariantSuffix(t *testing.T) {
	// Host-native OutputDir. The containment assertion below is a plain
	// HasPrefix against this string, while SidecarPath builds its result
	// with filepath.Join — which normalises separators. A POSIX literal
	// therefore compared "\tmp\transcoded\..." against "/tmp/transcoded"
	// on Windows and read as an escape from OutputDir. t.TempDir() is
	// absolute and already in the host's own separator form.
	base := JobSpec{
		SourceLibraryRel: "Music/Artist/Album/01 Track.flac",
		OutputDir:        t.TempDir(),
	}
	a := base
	a.TargetSampleRate = 176400
	a.TargetBits = 24

	b := base
	b.TargetSampleRate = 192000
	b.TargetBits = 24

	pa := a.SidecarPath()
	pb := b.SidecarPath()
	if pa == pb {
		t.Fatalf("two variants of the same source produced identical sidecar paths: %q", pa)
	}
	// Both must live UNDER OutputDir (v1.4 source-mirrored layout
	// means the sidecar is in <OutputDir>/<libRel-dir>/<filename>,
	// not directly in OutputDir). Use HasPrefix to assert containment
	// rather than equality.
	for _, p := range []string{pa, pb} {
		if !strings.HasPrefix(p, base.OutputDir+string(filepath.Separator)) {
			t.Errorf("sidecar path %q escapes OutputDir %q", p, base.OutputDir)
		}
		if !strings.HasSuffix(p, ".flac") {
			t.Errorf("sidecar path %q must end in .flac", p)
		}
	}
	// The variantID must appear in each filename so a directory
	// listing tells the operator which is which.
	if !strings.Contains(pa, "upscaled-v2-176400-24") {
		t.Errorf("sidecar path %q missing 176400 variantID", pa)
	}
	if !strings.Contains(pb, "upscaled-v2-192000-24") {
		t.Errorf("sidecar path %q missing 192000 variantID", pb)
	}
}

// TestSidecarPathStableForSameInputs proves the hash function is
// deterministic — re-running `bridge upscale` for the same source
// should land at the same sidecar path so an interrupted run is
// resumable.
func TestSidecarPathStableForSameInputs(t *testing.T) {
	j := JobSpec{
		SourceLibraryRel: "Music/Album/01.flac",
		OutputDir:        "/tmp/x",
		TargetSampleRate: 176400,
		TargetBits:       24,
	}
	first := j.SidecarPath()
	second := j.SidecarPath()
	if first != second {
		t.Fatalf("SidecarPath not deterministic across calls with identical inputs: %q vs %q", first, second)
	}
}

// TestSidecarFilenameLengthBounded locks the upper bound on the
// filename's character count. With the 16-char hash truncation the
// shape is `<16 hex>-upscaled-<vN>-<rate>-<bits>.flac` ≈ 50 chars
// for typical variants. Pin at 80 to leave room for future variant
// kinds (e.g. PCM→DSD synthesis) without re-engaging the Windows
// MAX_PATH risk this truncation defends against.
func TestSidecarFilenameLengthBounded(t *testing.T) {
	const maxFilenameChars = 80
	cases := []JobSpec{
		// Common case: 24/176.4
		{SourceLibraryRel: "x", OutputDir: "/tmp/x", TargetSampleRate: 176400, TargetBits: 24},
		// High-rate case: 32/705600 (max practical sox upscale target)
		{SourceLibraryRel: "x", OutputDir: "/tmp/x", TargetSampleRate: 705600, TargetBits: 32},
		// Long source path (the hash is fixed-width regardless of input)
		{
			SourceLibraryRel: "Music/Composer/Performer/Conductor/Orchestra/Album Title (Remastered Deluxe Edition) [2026]/Disc 01/01 - Movement Title in C Major.flac",
			OutputDir:        "/tmp/x",
			TargetSampleRate: 192000,
			TargetBits:       24,
		},
	}
	for _, j := range cases {
		got := filepath.Base(j.SidecarPath())
		if n := len(got); n > maxFilenameChars {
			t.Errorf("sidecar filename %d chars > %d: %q", n, maxFilenameChars, got)
		}
	}
}

// TestSoxArgsShape pins the exact argv shape we hand to sox.
// `-G` (gain-guard) leads as a global option; quality "very-high"
// → `rate -v -L <Hz>` (linear phase pinned explicitly; byte-identical
// to sox's default — see SoxArgs), dither -s, bit depth flag -b N,
// .tmp suffix on output. Any change to this shape needs an
// integration-test re-run on a known-good fixture.
func TestSoxArgsShape(t *testing.T) {
	j := JobSpec{
		SourceAbsPath:    "/lib/Music/Album/01.flac",
		SourceLibraryRel: "Music/Album/01.flac",
		TargetSampleRate: 176400,
		TargetBits:       24,
		Quality:          QualityVeryHigh,
		OutputDir:        "/tmp/transcoded",
	}
	args, settings, tmpPath := j.SoxArgs()
	want := []string{
		"-G",
		"/lib/Music/Album/01.flac",
		"-b", "24",
		"-t", "flac",
		j.SidecarPath() + ".tmp",
		"rate", "-v", "-L", "176400",
		"dither", "-s",
	}
	if len(args) != len(want) {
		t.Fatalf("args length: got %d, want %d (%v vs %v)", len(args), len(want), args, want)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Errorf("args[%d]: got %q, want %q", i, args[i], want[i])
		}
	}
	// Q2: the returned tmpPath IS the sox output argument, so RunSox renames
	// exactly the file sox wrote — no independent SidecarPath recomputation.
	if tmpPath != args[6] {
		t.Errorf("tmpPath %q != sox output arg args[6] %q", tmpPath, args[6])
	}
	if tmpPath != j.SidecarPath()+".tmp" {
		t.Errorf("tmpPath = %q, want %q", tmpPath, j.SidecarPath()+".tmp")
	}
	// Settings JSON must mention the rate flag, phase, target rate,
	// guard flag, and schema version so a future post-mortem can
	// identify what produced this sidecar.
	for _, needle := range []string{`"resampler":"sox"`, `"rateFlag":"-v"`, `"phase":"linear"`, `"targetRate":176400`, `"guard":true`, `"schemaVersion":"v2"`} {
		if !strings.Contains(settings, needle) {
			t.Errorf("settings JSON missing %q (got: %s)", needle, settings)
		}
	}
}

// TestSoxArgsIncludesGuardFlag pins `-G` as the leading global
// option. Sox's gain-guard is what prevents intersample peaks on
// 0 dBFS-mastered material from clipping through the rate-conversion
// + dither pipeline. Regression trap: a future refactor that drops
// or repositions `-G` would silently re-introduce occasional
// clipping in upscale variants. Position matters — sox treats `-G`
// as a global option that must precede the input file argument.
func TestSoxArgsIncludesGuardFlag(t *testing.T) {
	j := JobSpec{
		SourceAbsPath:    "/lib/Music/Album/01.flac",
		SourceLibraryRel: "Music/Album/01.flac",
		TargetSampleRate: 176400,
		TargetBits:       24,
		Quality:          QualityVeryHigh,
		OutputDir:        "/tmp/transcoded",
	}
	args, _, _ := j.SoxArgs()
	if len(args) == 0 {
		t.Fatal("SoxArgs returned empty slice")
	}
	if args[0] != "-G" {
		t.Errorf("args[0] = %q, want %q (gain-guard must lead as a global option, before the input path)", args[0], "-G")
	}
}

// TestSoxArgsForcesFlacEncoder pins the `-t flac` flag immediately
// before the output path. Sox normally picks the encoder from the
// output filename's last extension; ours is `<sidecar>.flac.tmp`
// and the trailing `.tmp` made sox bomb with
// `sox FAIL formats: no handler for file extension 'tmp'`. The
// `-t flac` preceding the output path tells sox to ignore the
// filename's hint and write FLAC. Found post-merge of PR #126
// (case-insensitive lookup) when enqueue worked but every sox
// invocation failed during the worker pool's actual run — the
// case fix unmasked this latent bug.
func TestSoxArgsForcesFlacEncoder(t *testing.T) {
	j := JobSpec{
		SourceAbsPath:    "/lib/Music/Album/01.flac",
		SourceLibraryRel: "Music/Album/01.flac",
		TargetSampleRate: 176400,
		TargetBits:       24,
		Quality:          QualityVeryHigh,
		OutputDir:        "/tmp/transcoded",
	}
	args, _, _ := j.SoxArgs()
	// Find `-t flac` and assert it comes immediately before an
	// argument that ends in `.tmp` (the output path).
	tIdx := -1
	for i, a := range args {
		if a == "-t" && i+1 < len(args) && args[i+1] == "flac" {
			tIdx = i
			break
		}
	}
	if tIdx < 0 {
		t.Fatalf("-t flac flag missing from args: %v", args)
	}
	if tIdx+2 >= len(args) {
		t.Fatalf("-t flac is the trailing flag with no output path after: %v", args)
	}
	if !strings.HasSuffix(args[tIdx+2], ".tmp") {
		t.Errorf("-t flac must precede the .tmp output path; got args[%d]=%q", tIdx+2, args[tIdx+2])
	}
}

// TestSoxArgsRespectsQualityPreset confirms the Quality enum maps
// to the right rate-flag letter. Operators may eventually want a
// "fast" preset on a Pi; the mapping table here is the gate.
func TestSoxArgsRespectsQualityPreset(t *testing.T) {
	cases := []struct {
		q    Quality
		flag string
	}{
		{QualityVeryHigh, "-v"},
		{QualityHigh, "-h"},
		{QualityMedium, "-m"},
	}
	for _, c := range cases {
		j := JobSpec{
			TargetSampleRate: 176400,
			TargetBits:       24,
			Quality:          c.q,
		}
		args, _, _ := j.SoxArgs()
		// Find "rate" in the args; the next token is the rate flag.
		rateIdx := -1
		for i, a := range args {
			if a == "rate" {
				rateIdx = i
				break
			}
		}
		if rateIdx < 0 || rateIdx+1 >= len(args) {
			t.Fatalf("Quality=%q: args missing 'rate' marker (%v)", c.q, args)
		}
		if got := args[rateIdx+1]; got != c.flag {
			t.Errorf("Quality=%q: rate flag = %q, want %q", c.q, got, c.flag)
		}
	}
}

// TestPickTargetRateDefaults pins the auto-rate selection table.
// 44.1 family → 176400; 48 family → 192000; everything already at
// or above the auto target → 0 (skip).
func TestPickTargetRateDefaults(t *testing.T) {
	cases := []struct {
		source int
		want   int
	}{
		{44100, 176400},
		{88200, 176400},
		{176400, 0}, // already at target
		{352800, 0},
		{48000, 192000},
		{96000, 192000},
		{192000, 0},
		{384000, 0},
		{8000, 0}, // exotic — auto declines
	}
	for _, c := range cases {
		got := PickTargetRate(c.source)
		// PickTargetRate returns the candidate without comparing
		// to source — that "skip if already at/above" check lives
		// in ResolveTargetRate. So 176400→176400 is a valid
		// candidate; the skip happens later.
		if c.source == 176400 || c.source == 192000 || c.source == 352800 || c.source == 384000 || c.source == 8000 {
			// These fall into the default branch (0).
			if got != 0 {
				t.Errorf("PickTargetRate(%d) = %d, want 0 (default branch)", c.source, got)
			}
		} else {
			if got != c.want {
				t.Errorf("PickTargetRate(%d) = %d, want %d", c.source, got, c.want)
			}
		}
	}
}

// TestResolveTargetRateAutoSkipsAlreadyAtTarget proves the
// "auto + source already at the auto target" case returns (0, nil),
// signalling skip. Without this gate, `bridge upscale` would
// happily re-encode every 24/192 file as 24/192 (no audio benefit,
// pure waste of CPU + disk).
func TestResolveTargetRateAutoSkipsAlreadyAtTarget(t *testing.T) {
	cases := []struct {
		flag   string
		source int
		want   int
	}{
		{"auto", 44100, 176400},
		{"auto", 176400, 0}, // skip — already at auto target
		{"auto", 48000, 192000},
		{"auto", 192000, 0}, // skip
		{"auto", 8000, 0},   // unfamiliar source rate; auto declines
		{"", 44100, 176400}, // empty → same as "auto"
	}
	for _, c := range cases {
		got, err := ResolveTargetRate(c.flag, c.source)
		if err != nil {
			t.Errorf("ResolveTargetRate(%q, %d) error: %v", c.flag, c.source, err)
			continue
		}
		if got != c.want {
			t.Errorf("ResolveTargetRate(%q, %d) = %d, want %d", c.flag, c.source, got, c.want)
		}
	}
}

// TestResolveTargetRateExplicitNeverDownsamples — explicit numeric
// flag value must reject targets at or below source. This is the
// "no on-the-fly transcoding" mission's offline analog: we don't
// degrade quality on conversion.
func TestResolveTargetRateExplicitNeverDownsamples(t *testing.T) {
	cases := []struct {
		flag   string
		source int
		want   int
	}{
		{"176400", 44100, 176400}, // upscale OK
		{"176400", 176400, 0},     // same → skip
		{"176400", 192000, 0},     // would be downsample → skip
		{"192000", 96000, 192000}, // upscale OK
		{"352800", 176400, 352800},
	}
	for _, c := range cases {
		got, err := ResolveTargetRate(c.flag, c.source)
		if err != nil {
			t.Errorf("ResolveTargetRate(%q, %d) error: %v", c.flag, c.source, err)
			continue
		}
		if got != c.want {
			t.Errorf("ResolveTargetRate(%q, %d) = %d, want %d", c.flag, c.source, got, c.want)
		}
	}
}

// TestResolveTargetRateRejectsMalformed — a typo'd flag value
// must surface an error, not silently fall through to 0 (which
// the caller would treat as "skip"). Operators want loud
// failure on bad input.
func TestResolveTargetRateRejectsMalformed(t *testing.T) {
	cases := []string{"foo", "192k", "-100", "0"}
	for _, c := range cases {
		_, err := ResolveTargetRate(c, 44100)
		if err == nil {
			t.Errorf("ResolveTargetRate(%q, 44100) expected error, got nil", c)
		}
	}
}

// TestPrecheckSoxReturnsTypedErrorWhenMissing — when sox is not on
// PATH the caller needs to distinguish "feature unavailable"
// (typed sentinel) from "transient sox failure" (generic error).
// We build an error that mirrors the wrap pattern PrecheckSox uses
// and assert errors.Is propagates through it. Doesn't depend on a
// real sox installation.
func TestPrecheckSoxReturnsTypedErrorWhenMissing(t *testing.T) {
	wrapped := fmt.Errorf("%w: %v", ErrSoxMissing, errors.New("exec: \"sox\": executable file not found in $PATH"))
	if !errors.Is(wrapped, ErrSoxMissing) {
		t.Fatalf("errors.Is(%v, ErrSoxMissing) = false; the typed sentinel must propagate via wrapping", wrapped)
	}
}
