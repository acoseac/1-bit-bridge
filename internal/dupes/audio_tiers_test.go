package dupes

import "testing"

// TestClassify_MD5RefinesSameFormat pins the evidence ladder: full MD5
// coverage refines a same-format group into identical-audio (all equal)
// or different-audio (any difference); PARTIAL coverage keeps the
// same-format inference — no group asserts certainty on half evidence.
func TestClassify_MD5RefinesSameFormat(t *testing.T) {
	withMD5 := func(path, md5 string) Row {
		r := flacRow(path, 44100, 16, 200, 1000)
		r.AudioMD5 = md5
		return r
	}
	identical := []Row{withMD5("A/B/x.flac", "aaaa"), withMD5("C/B/x.flac", "aaaa")}
	if got := classify(identical); got != TierIdenticalAudio {
		t.Fatalf("equal MD5s: got %s, want identical-audio", got)
	}
	remasters := []Row{withMD5("A/B/x.flac", "aaaa"), withMD5("C/B/x.flac", "bbbb")}
	if got := classify(remasters); got != TierDifferentAudio {
		t.Fatalf("differing MD5s: got %s, want different-audio", got)
	}
	partial := []Row{withMD5("A/B/x.flac", "aaaa"), withMD5("C/B/x.flac", "")}
	if got := classify(partial); got != TierSameFormat {
		t.Fatalf("partial coverage: got %s, want same-format (md5-partial)", got)
	}
	// The refinement never rescues a group that failed an earlier gate:
	// different geometry stays different-format even with equal MD5s
	// (equal MD5s across different geometry would be a tag/rip anomaly,
	// and the geometry verdict is the safer claim).
	crossGeo := []Row{withMD5("A/B/x.flac", "aaaa"),
		func() Row { r := withMD5("C/B/x.flac", "aaaa"); r.SampleRate = 96000; return r }()}
	if got := classify(crossGeo); got != TierDifferentFormat {
		t.Fatalf("cross-geometry: got %s, want different-format", got)
	}
}

// TestPlanSuppression_AudioTiers: identical-audio suppresses like
// same-format (redundancy is now a FACT); different-audio is NEVER
// suppressed under any mode — the evidence's safety payoff.
func TestPlanSuppression_AudioTiers(t *testing.T) {
	withMD5 := func(path, md5 string, size int64) Row {
		r := flacRow(path, 44100, 16, 200, size)
		r.AudioMD5 = md5
		return r
	}
	identical := []Row{withMD5("A/B/x.flac", "aaaa", 900), withMD5("C/B/x.flac", "aaaa", 1000)}
	g := Group{Key: KeyFor(identical[0]), Tier: classify(identical), Members: identical}
	got := PlanSuppression(g, Policy{Mode: FilterSameFormat})
	if len(got) != 1 || got[0] != "A/B/x.flac" {
		t.Fatalf("identical-audio must suppress the non-winner, got %v", got)
	}

	remasters := []Row{withMD5("A/B/x.flac", "aaaa", 900), withMD5("C/B/x.flac", "bbbb", 1000)}
	g = Group{Key: KeyFor(remasters[0]), Tier: classify(remasters), Members: remasters}
	for _, mode := range []FilterMode{FilterSameFormat, FilterHighestQuality} {
		if got := PlanSuppression(g, Policy{Mode: mode}); len(got) != 0 {
			t.Fatalf("mode %s: different-audio (proven remasters) must never suppress, got %v", mode, got)
		}
	}
}

func TestMD5Coverage(t *testing.T) {
	g := Group{Members: []Row{
		{Path: "a", AudioMD5: "aaaa"},
		{Path: "b"},
		{Path: "c", AudioMD5: "cccc"},
	}}
	known, total := g.MD5Coverage()
	if known != 2 || total != 3 {
		t.Fatalf("coverage = %d/%d, want 2/3", known, total)
	}
}
