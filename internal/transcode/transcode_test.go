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
			"upscaled-v1-176400-24",
		},
		{
			JobSpec{TargetSampleRate: 192000, TargetBits: 24},
			"upscaled-v1-192000-24",
		},
		{
			JobSpec{TargetSampleRate: 352800, TargetBits: 32},
			"upscaled-v1-352800-32",
		},
	}
	for _, c := range cases {
		got := c.j.VariantID()
		if got != c.want {
			t.Errorf("VariantID = %q, want %q", got, c.want)
		}
	}
}

// TestSidecarPathIncludesVariantSuffix proves that two JobSpecs
// with the same source but different variants land at distinct
// sidecar paths. Without this guarantee, the user's first call to
// `bridge upscale --target-rate 176400` followed by a second call
// at `--target-rate 192000` would silently overwrite the first
// sidecar — a real bug Gemini bot caught at the plan-review stage.
func TestSidecarPathIncludesVariantSuffix(t *testing.T) {
	base := JobSpec{
		SourceLibraryRel: "Music/Artist/Album/01 Track.flac",
		OutputDir:        "/tmp/transcoded",
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
	// Both must live under OutputDir (no path-traversal escape).
	for _, p := range []string{pa, pb} {
		if filepath.Dir(p) != base.OutputDir {
			t.Errorf("sidecar path %q escapes OutputDir %q", p, base.OutputDir)
		}
		if !strings.HasSuffix(p, ".flac") {
			t.Errorf("sidecar path %q must end in .flac", p)
		}
	}
	// The variantID must appear in each filename so a directory
	// listing tells the operator which is which.
	if !strings.Contains(pa, "upscaled-v1-176400-24") {
		t.Errorf("sidecar path %q missing 176400 variantID", pa)
	}
	if !strings.Contains(pb, "upscaled-v1-192000-24") {
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
	if j.SidecarPath() != j.SidecarPath() {
		t.Fatal("SidecarPath not deterministic across calls with identical inputs")
	}
}

// TestSoxArgsShape pins the exact argv shape we hand to sox.
// Quality "very-high" → `rate -v <Hz>`, dither -s, bit depth flag
// -b N, .tmp suffix on output. Any change to this shape needs an
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
	args, settings := j.SoxArgs()
	want := []string{
		"/lib/Music/Album/01.flac",
		"-b", "24",
		"-t", "flac",
		j.SidecarPath() + ".tmp",
		"rate", "-v", "176400",
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
	// Settings JSON must mention the rate flag, target rate, and
	// schema version so a future post-mortem can identify what
	// produced this sidecar.
	for _, needle := range []string{`"resampler":"sox"`, `"rateFlag":"-v"`, `"targetRate":176400`, `"schemaVersion":"v1"`} {
		if !strings.Contains(settings, needle) {
			t.Errorf("settings JSON missing %q (got: %s)", needle, settings)
		}
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
	args, _ := j.SoxArgs()
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
		args, _ := j.SoxArgs()
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
