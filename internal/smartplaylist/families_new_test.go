package smartplaylist

import (
	"testing"
)

// Builder-level tests for the five 2026-06-22 families: Drive Mix, On Repeat,
// From Artists You Love, Wind Down, Lift Off.
//
// The engine is pure — these tests build minimal Inputs + Options literals
// and never touch SQLite. The manifest queries that feed each family have
// their own integration tests in internal/manifest.

// newOpts mirrors testOpts (engine_test.go) but is local so a future test
// running before testOpts is initialised doesn't depend on init order.
func newOpts() Options {
	return Options{
		AnalysisEnabled:        true,
		MaxItems:               10,
		MinDriveMix:            10,
		MaxDriveMixItems:       30,
		OnRepeatEnterFloor:     12,
		OnRepeatExitFloor:      8,
		MaxOnRepeatItems:       25,
		MinArtistDeepCuts:      9,
		MaxArtistDeepCutsItems: 20,
		MinMoodBand:            15,
		MaxMoodBandItems:       25,
	}
}

// featurePool builds a [path]TrackFeature map for a list of (path) labels —
// every track gets the same title/artist so itemsFromPaths can hydrate.
func featurePool(paths ...string) map[string]TrackFeature {
	out := make(map[string]TrackFeature, len(paths))
	for _, p := range paths {
		out[p] = TrackFeature{Path: p, Title: "T-" + p, Artist: "A-" + p}
	}
	return out
}

func makePlayStats(paths []string) []PlayStat {
	out := make([]PlayStat, len(paths))
	for i, p := range paths {
		out[i] = PlayStat{Path: p, Plays: len(paths) - i}
	}
	return out
}

// --- Drive Mix ---

func TestBuildDriveMix_PopulatedAboveThreshold(t *testing.T) {
	paths := []string{"/c1.flac", "/c2.flac", "/c3.flac", "/c4.flac", "/c5.flac",
		"/c6.flac", "/c7.flac", "/c8.flac", "/c9.flac", "/c10.flac"}
	in := Inputs{
		Drive:    makePlayStats(paths),
		Features: featurePool(paths...),
	}
	got, ok := buildDriveMix(in, newOpts())
	if !ok {
		t.Fatal("Drive Mix should fire when count ≥ MinDriveMix")
	}
	if got.Slug != "drive-mix" || got.Kind != KindDriveMix {
		t.Fatalf("slug/kind mismatch: %+v", got)
	}
	if got.Title != "Drive Mix" || got.Subtitle != "Your road favorites" {
		t.Fatalf("title/subtitle: %+v", got)
	}
	if len(got.Items) != 10 {
		t.Fatalf("want 10 items, got %d", len(got.Items))
	}
}

func TestBuildDriveMix_BelowThresholdDropped(t *testing.T) {
	paths := []string{"/c1.flac", "/c2.flac"} // < MinDriveMix
	in := Inputs{Drive: makePlayStats(paths), Features: featurePool(paths...)}
	if _, ok := buildDriveMix(in, newOpts()); ok {
		t.Fatal("Drive Mix should drop below MinDriveMix")
	}
}

// --- On Repeat hysteresis truth table ---

func TestBuildOnRepeat_HysteresisTruthTable(t *testing.T) {
	cases := []struct {
		name     string
		count    int
		visible  bool
		expected bool
	}{
		{"7 + cold = drop", 7, false, false},
		{"8 + cold = drop (below enter floor)", 8, false, false},
		{"11 + cold = drop", 11, false, false},
		{"12 + cold = emit (at enter floor)", 12, false, true},
		{"7 + visible = drop (below exit floor)", 7, true, false},
		{"8 + visible = emit (at exit floor)", 8, true, true},
		{"11 + visible = emit (above exit floor)", 11, true, true},
		{"12 + visible = emit (above both)", 12, true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			paths := make([]string, c.count)
			for i := 0; i < c.count; i++ {
				paths[i] = "/r" + string(rune('A'+(i%26))) + "-" + string(rune('A'+(i/26))) + ".flac"
			}
			in := Inputs{
				OnRepeat: makePlayStats(paths),
				Features: featurePool(paths...),
			}
			if c.visible {
				in.PreviouslyVisible = map[string]bool{onRepeatSlug: true}
			}
			_, ok := buildOnRepeat(in, newOpts())
			if ok != c.expected {
				t.Fatalf("count=%d visible=%v: got ok=%v want %v",
					c.count, c.visible, ok, c.expected)
			}
		})
	}
}

func TestBuildOnRepeat_SlugMatchesPreviouslyVisibleKey(t *testing.T) {
	// Sanity guard against future kebab/camel slug drift — the regenerator
	// keys PreviouslyVisible by SLUG (StoredSmartPlaylist.Slug), not Kind.
	if onRepeatSlug != "on-repeat" {
		t.Errorf("onRepeatSlug = %q, want %q", onRepeatSlug, "on-repeat")
	}
	if string(KindOnRepeat) != "onRepeat" {
		t.Errorf("KindOnRepeat = %q, want %q (camelCase)", KindOnRepeat, "onRepeat")
	}
}

// --- From Artists You Love (weekly shuffle determinism) ---

func TestBuildArtistDeepCuts_WeeklyShuffleStableAndRotates(t *testing.T) {
	paths := []string{"/a/1.flac", "/a/2.flac", "/a/3.flac",
		"/b/1.flac", "/b/2.flac", "/b/3.flac",
		"/c/1.flac", "/c/2.flac", "/c/3.flac"}
	in := Inputs{
		ArtistDeepCuts: makePlayStats(paths),
		Features:       featurePool(paths...),
		WeekSeed:       42,
	}
	opts := newOpts()

	got1, ok := buildArtistDeepCuts(in, opts)
	if !ok || len(got1.Items) != 9 {
		t.Fatalf("first build: ok=%v items=%d", ok, len(got1.Items))
	}
	// Same seed → same order (determinism).
	got2, _ := buildArtistDeepCuts(in, opts)
	for i := range got1.Items {
		if got1.Items[i].Path != got2.Items[i].Path {
			t.Fatalf("same seed must produce same order at i=%d: %s vs %s",
				i, got1.Items[i].Path, got2.Items[i].Path)
		}
	}
	// Different seed → order changes (rotation).
	in.WeekSeed = 9001
	got3, _ := buildArtistDeepCuts(in, opts)
	identical := true
	for i := range got1.Items {
		if got1.Items[i].Path != got3.Items[i].Path {
			identical = false
			break
		}
	}
	if identical {
		t.Fatal("different WeekSeed should rotate the order")
	}
}

func TestBuildArtistDeepCuts_BelowThresholdDropped(t *testing.T) {
	paths := []string{"/a/1.flac", "/a/2.flac"} // < MinArtistDeepCuts
	in := Inputs{
		ArtistDeepCuts: makePlayStats(paths),
		Features:       featurePool(paths...),
		WeekSeed:       1,
	}
	if _, ok := buildArtistDeepCuts(in, newOpts()); ok {
		t.Fatal("ArtistDeepCuts should drop below MinArtistDeepCuts")
	}
}

// --- Mood bands ---

func TestBuildWindDown_PopulatedAboveThreshold(t *testing.T) {
	pool := make([]TrackFeature, 20)
	for i := range pool {
		pool[i] = TrackFeature{Path: "/q" + string(rune('A'+(i%26))) + ".flac",
			Title: "Quiet", Artist: "Slowdive"}
	}
	in := Inputs{QuietSlowPool: pool, WeekSeed: 12345}
	got, ok := buildWindDown(in, newOpts())
	if !ok {
		t.Fatal("Wind Down should fire when pool ≥ MinMoodBand")
	}
	if got.Slug != "wind-down" || got.Kind != KindWindDown {
		t.Fatalf("slug/kind mismatch: %+v", got)
	}
}

func TestBuildLiftOff_PopulatedAboveThreshold(t *testing.T) {
	pool := make([]TrackFeature, 20)
	for i := range pool {
		pool[i] = TrackFeature{Path: "/l" + string(rune('A'+(i%26))) + ".flac",
			Title: "Loud", Artist: "Bringer"}
	}
	in := Inputs{LoudFastPool: pool, WeekSeed: 12345}
	got, ok := buildLiftOff(in, newOpts())
	if !ok {
		t.Fatal("Lift Off should fire when pool ≥ MinMoodBand")
	}
	if got.Slug != "lift-off" || got.Kind != KindLiftOff {
		t.Fatalf("slug/kind mismatch: %+v", got)
	}
}

func TestBuildMoodBands_ShuffleSeparately(t *testing.T) {
	// Given the SAME pool fed to both bands, the deterministic shuffle must
	// produce DIFFERENT orderings — otherwise Wind Down and Lift Off would
	// always pick the same first N tracks for overlapping pools.
	pool := make([]TrackFeature, 30)
	for i := range pool {
		pool[i] = TrackFeature{Path: "/x" + string(rune('A'+(i%26))) + "-" + string(rune('A'+(i/26))) + ".flac",
			Title: "T", Artist: "A"}
	}
	in := Inputs{QuietSlowPool: pool, LoudFastPool: pool, WeekSeed: 12345}
	opts := newOpts()
	wd, _ := buildWindDown(in, opts)
	lo, _ := buildLiftOff(in, opts)

	if len(wd.Items) == 0 || len(lo.Items) == 0 {
		t.Fatalf("both bands should populate: wd=%d lo=%d", len(wd.Items), len(lo.Items))
	}
	identical := true
	for i := range wd.Items {
		if wd.Items[i].Path != lo.Items[i].Path {
			identical = false
			break
		}
	}
	if identical {
		t.Fatal("Wind Down and Lift Off must produce different orderings on the same pool")
	}
}

func TestBuildMoodBands_BelowThresholdDropped(t *testing.T) {
	smallPool := []TrackFeature{
		{Path: "/q1.flac", Title: "x", Artist: "a"},
		{Path: "/q2.flac", Title: "x", Artist: "a"},
	}
	in := Inputs{QuietSlowPool: smallPool, LoudFastPool: smallPool, WeekSeed: 1}
	if _, ok := buildWindDown(in, newOpts()); ok {
		t.Fatal("Wind Down should drop below MinMoodBand")
	}
	if _, ok := buildLiftOff(in, newOpts()); ok {
		t.Fatal("Lift Off should drop below MinMoodBand")
	}
}

// --- shuffle helper determinism guards ---

func TestShufflePathsByWeek_DeterministicAndSeedSensitive(t *testing.T) {
	paths := []string{"/a", "/b", "/c", "/d", "/e", "/f"}
	a := shufflePathsByWeek(paths, 1)
	b := shufflePathsByWeek(paths, 1)
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("same seed must produce same order: i=%d %s vs %s", i, a[i], b[i])
		}
	}
	c := shufflePathsByWeek(paths, 99)
	identical := true
	for i := range a {
		if a[i] != c[i] {
			identical = false
			break
		}
	}
	if identical {
		t.Fatal("different seeds should produce different orderings")
	}
}

func TestSeedFromISOWeek_StableAcrossCalls(t *testing.T) {
	// Store-then-compare so a static-analyzer (SA4000) doesn't fold the two
	// calls into the same expression — and the test actually exercises
	// determinism instead of comparing a value to itself (CodeRabbit / Sonar
	// on PR #431).
	first := SeedFromISOWeek(2026, 25)
	second := SeedFromISOWeek(2026, 25)
	if first != second {
		t.Fatalf("SeedFromISOWeek must be deterministic for the same input: %d != %d", first, second)
	}
	if SeedFromISOWeek(2026, 25) == SeedFromISOWeek(2026, 26) {
		t.Fatal("adjacent weeks must produce distinct seeds")
	}
	if SeedFromISOWeek(2025, 25) == SeedFromISOWeek(2026, 25) {
		t.Fatal("adjacent years must produce distinct seeds")
	}
}
