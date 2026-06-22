package smartplaylist

import (
	"math"
	"testing"
	"time"
)

func iptr(v int) *int { return &v }

func feat(path, genre string, bpm, keyRoot int, mode string, dur float64) TrackFeature {
	f := TrackFeature{Path: path, Title: "T-" + path, Artist: "A-" + path, Genre: genre, Duration: dur}
	if bpm > 0 {
		f.BPM = iptr(bpm)
	}
	if keyRoot >= 0 {
		f.KeyRoot = iptr(keyRoot)
		f.KeyMode = mode
	}
	return f
}

func stat(path string, plays int) PlayStat { return PlayStat{Path: path, Plays: plays} }

// --- Camelot ---

func TestToCamelot(t *testing.T) {
	cases := []struct {
		root int
		mode string
		want Camelot
		ok   bool
	}{
		{0, "major", Camelot{8, false}, true},  // C major = 8B
		{9, "minor", Camelot{8, true}, true},   // A minor = 8A (relative of C)
		{7, "major", Camelot{9, false}, true},  // G major = 9B
		{11, "major", Camelot{1, false}, true}, // B major = 1B
		{0, "minor", Camelot{5, true}, true},   // C minor = 5A
		{12, "major", Camelot{}, false},        // out of range
		{0, "", Camelot{}, false},              // unknown mode
	}
	for _, c := range cases {
		got, ok := ToCamelot(c.root, c.mode)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("ToCamelot(%d,%q) = %v,%v want %v,%v", c.root, c.mode, got, ok, c.want, c.ok)
		}
	}
}

func TestCompatibilityCost(t *testing.T) {
	b8 := Camelot{8, false} // 8B
	cases := []struct {
		other Camelot
		want  int
		desc  string
	}{
		{Camelot{8, false}, 0, "identical"},
		{Camelot{8, true}, 1, "relative"},
		{Camelot{9, false}, 1, "adjacent +1"},
		{Camelot{7, false}, 1, "adjacent -1"},
		{Camelot{10, false}, 2, "energy +2 same letter"},
		{Camelot{9, true}, 2, "diagonal opposite letter +1"},
		{Camelot{4, true}, 3, "incompatible"},
		{Camelot{1, false}, 3, "far same letter"},
	}
	// wheel wrap: 1 and 12 are adjacent
	if compatibilityCost(Camelot{12, false}, Camelot{1, false}) != 1 {
		t.Errorf("12B↔1B should be adjacent (cost 1)")
	}
	for _, c := range cases {
		if got := compatibilityCost(b8, c.other); got != c.want {
			t.Errorf("compatibilityCost(8B, %v) [%s] = %d want %d", c.other, c.desc, got, c.want)
		}
	}
}

// --- BPM half/double-time ---

func TestNormalizedBPMDistance(t *testing.T) {
	cases := []struct {
		a, b, want float64
	}{
		{75, 150, 0},   // double-time
		{150, 75, 0},   // half-time
		{120, 122, 2},  // close
		{0, 120, 0},    // unknown a
		{120, 0, 0},    // unknown b
		{120, 180, 60}, // not a clean multiple
	}
	for _, c := range cases {
		if got := normalizedBPMDistance(c.a, c.b); math.Abs(got-c.want) > 1e-9 {
			t.Errorf("normalizedBPMDistance(%v,%v) = %v want %v", c.a, c.b, got, c.want)
		}
	}
}

// --- harmonic sequencing ---

func TestSequenceHarmonic_DeterministicFlow(t *testing.T) {
	a := feat("a", "", 120, 0, "major", 0) // 8B
	b := feat("b", "", 122, 7, "major", 0) // 9B (adjacent to 8B)
	c := feat("c", "", 121, 9, "minor", 0) // 8A (relative of 8B)
	d := feat("d", "", 200, 5, "minor", 0) // 4A (incompatible from 8B/8A/9B)
	pool := []TrackFeature{a, b, c, d}

	seq := sequenceHarmonic(a, pool, 10)
	if len(seq) != 4 {
		t.Fatalf("want all 4 tracks sequenced, got %d", len(seq))
	}
	order := []string{seq[0].Path, seq[1].Path, seq[2].Path, seq[3].Path}
	// a(8B) → c(8A, relative, bpm 121 closest) → b(9B, diagonal) → d(reset jump)
	want := []string{"a", "c", "b", "d"}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("sequence order = %v want %v", order, want)
		}
	}
}

func TestSequenceHarmonic_SkipsKeylessTracks(t *testing.T) {
	a := feat("a", "", 120, 0, "major", 0)
	noKey := TrackFeature{Path: "nokey", BPM: iptr(120)} // no KeyRoot
	seq := sequenceHarmonic(a, []TrackFeature{a, noKey}, 10)
	if len(seq) != 1 || seq[0].Path != "a" {
		t.Fatalf("keyless track should be excluded; got %+v", seq)
	}
}

// --- session segmentation ---

func TestAverageSessionDuration(t *testing.T) {
	s := func(sec int64) int64 { return sec * int64(time.Second) }
	events := []Event{
		{StartedAt: s(0), DurationUsed: 100},   // session 1
		{StartedAt: s(110), DurationUsed: 100}, // +10s gap → same session
		{StartedAt: s(220), DurationUsed: 100},
		{StartedAt: s(4320), DurationUsed: 100}, // +4000s gap → new session
		{StartedAt: s(4430), DurationUsed: 100},
	}
	avg, sessions := averageSessionDuration(events, 3600)
	if sessions != 2 {
		t.Fatalf("sessions = %d want 2", sessions)
	}
	if math.Abs(avg-250) > 1e-9 { // (300 + 200) / 2
		t.Fatalf("avg = %v want 250", avg)
	}
	if _, n := averageSessionDuration(nil, 3600); n != 0 {
		t.Errorf("empty events should yield 0 sessions")
	}
}

// --- interleave ---

func TestInterleave_Ratio(t *testing.T) {
	fam := []TrackFeature{feat("f0", "", 0, -1, "", 0), feat("f1", "", 0, -1, "", 0), feat("f2", "", 0, -1, "", 0),
		feat("f3", "", 0, -1, "", 0), feat("f4", "", 0, -1, "", 0), feat("f5", "", 0, -1, "", 0), feat("f6", "", 0, -1, "", 0)}
	disc := []TrackFeature{feat("d0", "", 0, -1, "", 0), feat("d1", "", 0, -1, "", 0), feat("d2", "", 0, -1, "", 0)}
	out := interleave(fam, disc)
	if len(out) != 10 {
		t.Fatalf("want 10, got %d", len(out))
	}
	if out[0].Path != "f0" {
		t.Errorf("first slot should be familiar, got %s", out[0].Path)
	}
	nDisc := 0
	for _, x := range out {
		if x.Path[0] == 'd' {
			nDisc++
		}
	}
	if nDisc != 3 {
		t.Errorf("discovery count = %d want 3", nDisc)
	}
}

// --- Generate end-to-end ---

func richInputs() Inputs {
	features := map[string]TrackFeature{}
	reg := func(f TrackFeature) { features[f.Path] = f }
	// played + analyzed favourites
	reg(feat("a", "Jazz", 120, 0, "major", 100))
	reg(feat("b", "Jazz", 122, 7, "major", 100))
	reg(feat("c", "Jazz", 121, 9, "minor", 100))
	reg(feat("d", "Rock", 130, -1, "", 100))
	reg(feat("e", "Rock", 140, -1, "", 100))
	// unplayed analyzed discovery candidates
	reg(feat("f", "Jazz", 119, 2, "major", 100))
	reg(feat("g", "Jazz", 123, 4, "minor", 100))

	s := func(sec int64) int64 { return sec * int64(time.Second) }
	return Inputs{
		HeavyRotation: []PlayStat{stat("a", 9), stat("b", 8), stat("c", 7)},
		Recent:        []PlayStat{stat("c", 1), stat("b", 1)},
		Forgotten:     []PlayStat{stat("d", 5), stat("e", 4)},
		Familiar:      []PlayStat{stat("a", 20), stat("b", 18), stat("c", 16)},
		HourBuckets:   []HourPath{{Hour: 8, Path: "a", Plays: 3}, {Hour: 8, Path: "b", Plays: 2}},
		Events: []Event{
			// session 1 (3×100s), big gap, session 2 (2×100s) → avg 250s,
			// so the Finish Line greedy needs 3 tracks (above its 3-track floor).
			{StartedAt: s(0), DurationUsed: 100}, {StartedAt: s(110), DurationUsed: 100},
			{StartedAt: s(220), DurationUsed: 100},
			{StartedAt: s(5000), DurationUsed: 100}, {StartedAt: s(5110), DurationUsed: 100},
		},
		AnalyzedPool: []TrackFeature{features["a"], features["b"], features["c"], features["f"], features["g"]},
		PlayedPaths:  map[string]bool{"a": true, "b": true, "c": true, "d": true, "e": true},
		Features:     features,
	}
}

func testOpts(analysis bool) Options {
	return Options{
		AnalysisEnabled: analysis, MaxItems: 10,
		MinHeavyRotation: 2, MinRecentlyPlayed: 2, MinForgotten: 2,
		MinAutoMixPool: 3, MinTimeOfDayPlays: 2, MinDailyFamiliar: 2, MinSessions: 2,
		SessionGapSeconds: 3600, DailyDiscoveryRatio: 0.30,
		// New families' thresholds — kept ≥ 1 so an unpopulated fixture (empty
		// Drive / OnRepeat / ArtistDeepCuts / QuietSlowPool / LoudFastPool)
		// doesn't trip the family-emits-with-zero-items edge. Per-family
		// fixture-driven tests live in families_new_test.go.
		MinDriveMix: 2, MaxDriveMixItems: 10,
		OnRepeatEnterFloor: 12, OnRepeatExitFloor: 8, MaxOnRepeatItems: 10,
		MinArtistDeepCuts: 2, MaxArtistDeepCutsItems: 10,
		MinMoodBand: 2, MaxMoodBandItems: 10,
	}
}

func TestGenerate_AllFamiliesInOrder(t *testing.T) {
	out := Generate(richInputs(), testOpts(true))
	want := []Kind{
		KindHeavyRotation, KindAutoMix, KindDailyMix, KindTimeOfDay,
		KindRecentlyPlayed, KindForgottenFavorites, KindFinishLine,
	}
	if len(out) != len(want) {
		t.Fatalf("got %d families, want %d: %+v", len(out), len(want), kinds(out))
	}
	for i, k := range want {
		if out[i].Kind != k {
			t.Fatalf("family order = %v want %v", kinds(out), want)
		}
		if out[i].Slug == "" || out[i].Title == "" {
			t.Errorf("%s missing slug/title", out[i].Kind)
		}
	}
	// time-of-day carries hourly pools, not a flat list
	tod := byKind(out, KindTimeOfDay)
	if tod == nil || len(tod.HourlyItems) == 0 || len(tod.Items) != 0 {
		t.Errorf("time-of-day should use HourlyItems, not Items: %+v", tod)
	}
	if len(tod.HourlyItems[8]) == 0 {
		t.Errorf("hour-8 pool should be populated")
	}
}

func TestGenerate_AnalysisOffGatesHarmonic(t *testing.T) {
	out := Generate(richInputs(), testOpts(false))
	if byKind(out, KindAutoMix) != nil {
		t.Errorf("Auto Mix must be omitted when analysis is off")
	}
	// Daily Mix survives (familiar-only, no discovery) and stays within played set.
	dm := byKind(out, KindDailyMix)
	if dm == nil {
		t.Fatalf("Daily Mix should still generate from the familiar set")
	}
	for _, it := range dm.Items {
		if it.Path == "f" || it.Path == "g" {
			t.Errorf("Daily Mix discovery leaked with analysis off: %s", it.Path)
		}
	}
}

func TestGenerate_OmitsBelowThreshold(t *testing.T) {
	in := richInputs()
	in.HeavyRotation = []PlayStat{stat("a", 9)} // 1 < MinHeavyRotation(2)
	out := Generate(in, testOpts(true))
	if byKind(out, KindHeavyRotation) != nil {
		t.Errorf("Heavy Rotation should be omitted below threshold")
	}
}

func TestGenerate_DropsUnresolvablePaths(t *testing.T) {
	in := richInputs()
	// Two of the three heavy-rotation paths aren't in Features → only 1
	// resolves, below MinHeavyRotation(2) → omitted.
	in.HeavyRotation = []PlayStat{stat("a", 9), stat("ghost1", 8), stat("ghost2", 7)}
	out := Generate(in, testOpts(true))
	if byKind(out, KindHeavyRotation) != nil {
		t.Errorf("Heavy Rotation should be omitted when paths don't resolve")
	}
}

func kinds(ps []GeneratedPlaylist) []Kind {
	out := make([]Kind, len(ps))
	for i, p := range ps {
		out[i] = p.Kind
	}
	return out
}

func byKind(ps []GeneratedPlaylist, k Kind) *GeneratedPlaylist {
	for i := range ps {
		if ps[i].Kind == k {
			return &ps[i]
		}
	}
	return nil
}
