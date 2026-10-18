package smartplaylist

import "testing"

func pathsIn(items []Item) map[string]bool {
	out := map[string]bool{}
	for _, it := range items {
		out[it.Path] = true
	}
	return out
}

func keyedPool(paths ...string) []TrackFeature {
	out := make([]TrackFeature, len(paths))
	for i, p := range paths {
		out[i] = feat(p, "Jazz", 120, 0, "major", 200)
	}
	return out
}

// A path any discovery shelf served in the last 7 days stays out of the
// discovery slice. The familiar slice of Daily Mix is not part of that skip.
func TestDailyDiscoverySkipsAPathServedThisWeek(t *testing.T) {
	opts := testOpts(true)
	opts.MaxItems = 4
	opts.DailyDiscoveryRatio = 0.5
	opts.MinDailyFamiliar = 2
	famA := feat("fam-a", "Jazz", 120, 0, "major", 100)
	famB := feat("fam-b", "Jazz", 120, 0, "major", 100)
	served := feat("served", "Jazz", 120, 0, "major", 100)
	fresh := feat("fresh", "Jazz", 119, 0, "major", 100)
	in := Inputs{
		Familiar:       []PlayStat{stat(famA.Path, 5), stat(famB.Path, 4)},
		Features:       map[string]TrackFeature{famA.Path: famA, famB.Path: famB},
		AnalyzedPool:   []TrackFeature{famA, famB, served, fresh},
		PlayedPaths:    map[string]bool{famA.Path: true, famB.Path: true},
		ServedRecently: map[string]bool{served.Path: true, famA.Path: true},
	}
	pl, ok := buildDailyMix(in, opts)
	if !ok {
		t.Fatal("daily mix should still build from the familiar slice")
	}
	have := pathsIn(pl.Items)
	if have[served.Path] {
		t.Fatal("a path served this week was placed in the discovery slice")
	}
	if !have[fresh.Path] {
		t.Fatal("an unserved unplayed track should fill the discovery slice")
	}
	if !have[famA.Path] {
		t.Fatal("the familiar slice must keep a track even when it was served")
	}
	if pl.ExclusionRelaxed {
		t.Fatal("an empty-able discovery slice must not drop the exclusion; the family minimum is the familiar set")
	}
}

// Favorites, Heavy Rotation and Recently Played are skipped by the
// discovery shelves even when the served table does not mention them.
func TestDiscoverySkipsTheFamiliarShelves(t *testing.T) {
	// Each familiar shelf has to clear its own minimum or it is not shown,
	// and a path that is not on the shelf is not excluded.
	neutrals := []string{"n1", "n2", "n3", "n4", "n5", "n6"}
	hr := []string{"hr1", "hr2"}
	fav := []string{"fav1", "fav2", "fav3", "fav4", "fav5"}
	recent := []string{"recent1", "recent2"}
	var names []string
	names = append(names, neutrals...)
	names = append(names, hr...)
	names = append(names, fav...)
	names = append(names, recent...)
	pool := keyedPool(names...)
	by := map[string]TrackFeature{}
	for _, f := range pool {
		by[f.Path] = f
	}
	favFeats := make([]TrackFeature, len(fav))
	for i, p := range fav {
		favFeats[i] = by[p]
	}
	in := Inputs{
		AnalyzedPool:  pool,
		LoudFastPool:  pool,
		HeavyRotation: []PlayStat{stat("hr1", 9), stat("hr2", 8)},
		Favorites:     favFeats,
		Recent:        []PlayStat{stat("recent1", 2), stat("recent2", 1)},
		Features:      by,
		WeekSeed:      1,
	}
	opts := testOpts(true)
	opts.MinAutoMixPool = 3
	opts.MaxItems = 20
	opts.MinMoodBand = 4
	opts.MaxMoodBandItems = 20
	opts.MinFavorites = 5
	opts.MaxFavoritesItems = 100

	auto, ok := buildAutoMix(in, opts)
	if !ok {
		t.Fatal("auto mix should build from the tracks that are not on a familiar shelf")
	}
	if auto.ExclusionRelaxed {
		t.Fatal("the remaining pool meets the minimum; the exclusion should hold")
	}
	have := pathsIn(auto.Items)
	for _, p := range []string{"hr1", "hr2", "fav1", "recent1", "recent2"} {
		if have[p] {
			t.Errorf("auto mix included familiar-shelf path %s", p)
		}
	}
	if !have["n1"] {
		t.Fatal("a track on no familiar shelf should remain in auto mix")
	}

	lift, ok := buildLiftOff(in, opts)
	if !ok {
		t.Fatal("lift off should build")
	}
	have = pathsIn(lift.Items)
	if have["recent1"] || have["hr1"] || have["fav1"] {
		t.Fatalf("lift off included a familiar shelf: %+v", lift.Items)
	}
}

// Dropping the exclusion, not the minimum, when the skip would hide the family.
func TestDiscoveryKeepsAFamilyThatTheExclusionWouldHide(t *testing.T) {
	pool := keyedPool("a", "b", "c", "d", "e", "f")
	in := Inputs{
		AnalyzedPool:   pool,
		ServedRecently: map[string]bool{"a": true, "b": true, "c": true, "d": true},
		WeekSeed:       2,
	}
	opts := testOpts(true)
	opts.MinAutoMixPool = 3
	opts.MaxItems = 10
	pl, ok := buildAutoMix(in, opts)
	if !ok {
		t.Fatal("auto mix must stay when the exclusion would leave it under its minimum")
	}
	if !pl.ExclusionRelaxed {
		t.Fatal("the exclusion was kept even though the filtered pool cannot fill the mix")
	}
	if len(pl.Items) < 5 {
		t.Fatalf("relaxed mix has %d items, want the unfiltered chain", len(pl.Items))
	}
	have := pathsIn(pl.Items)
	if !have["a"] {
		t.Fatal("relaxing the exclusion should bring the served paths back")
	}

	cuts := make([]PlayStat, len(pool))
	feats := map[string]TrackFeature{}
	for i, f := range pool {
		cuts[i] = stat(f.Path, 1)
		feats[f.Path] = f
	}
	in = Inputs{
		ArtistDeepCuts: cuts,
		Features:       feats,
		ServedRecently: map[string]bool{"a": true, "b": true, "c": true},
		WeekSeed:       2,
	}
	opts.MinArtistDeepCuts = 4
	opts.MaxArtistDeepCutsItems = 10
	deep, ok := buildArtistDeepCuts(in, opts)
	if !ok || !deep.ExclusionRelaxed {
		t.Fatalf("deep cuts should relax to stay at its minimum, ok=%v relaxed=%v items=%d",
			ok, deep.ExclusionRelaxed, len(deep.Items))
	}
	if len(deep.Items) < 4 {
		t.Fatalf("relaxed deep cuts has %d items", len(deep.Items))
	}
}
