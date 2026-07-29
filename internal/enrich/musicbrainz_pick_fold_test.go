package enrich

import "testing"

func rc(id, title string, score int, credit string) releaseCandidate {
	return releaseCandidate{
		ID: id, Title: title, Score: score,
		ArtistCredit: []artistCredit{{Name: credit}},
	}
}

// TestPickBestReleaseAcceptsFoldedTitleVariants covers the measured
// rejections — every one of these scored 96-100 and was discarded because
// two strings differed by a byte.
func TestPickBestReleaseAcceptsFoldedTitleVariants(t *testing.T) {
	for _, tc := range []struct {
		name, localAlbum, localArtist, mbTitle, mbCredit string
		score                                            int
	}{
		{"curly apostrophe", "What's Up?", "4 Non Blondes", "What’s Up?", "4 Non Blondes", 100},
		{"en dash", "Songs 2003-2013", "Ane Brun", "Songs 2003–2013", "Ane Brun", 100},
		{"colon vs dash", "II - Yo Te Voy A Amar", "Boyz II Men", "II: Yo te voy a amar", "Boyz II Men", 100},
		{"local is superset", "Abba Gold Anniversary Edition", "Abba", "Gold (anniversary edition)", "ABBA", 96},
		{"comma variant", "Eternally Yours Bonus-EP I", "Alphaville", "Eternally Yours, Bonus-EP I", "Alphaville", 100},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := pickBestRelease(
				[]releaseCandidate{rc("id-1", tc.mbTitle, tc.score, tc.mbCredit)},
				tc.localAlbum, tc.localArtist)
			if got == nil {
				t.Fatalf("rejected %q against %q (score %d) — this is the bug",
					tc.mbTitle, tc.localAlbum, tc.score)
			}
		})
	}
}

// TestPickBestReleaseMatchesOnReleaseGroupTitle covers the free-recall
// arm. Atlas's trigram plan MATCHES on release_group.name but RETURNS
// release.name as `title`; Atlas's own analysis records the two differing
// for 4.4% of releases. Rejecting on a title the upstream never claimed
// to match is simply wrong.
func TestPickBestReleaseMatchesOnReleaseGroupTitle(t *testing.T) {
	c := rc("id-1", "Halcyon Days", 92, "Ellie Goulding")
	c.ReleaseGroup = &releaseGroup{ID: "rg-1", Title: "Halcyon"}
	if got := pickBestRelease([]releaseCandidate{c}, "Halcyon", "Ellie Goulding"); got == nil {
		t.Fatal("release-group title arm did not fire")
	}
	// The release-title arm must still work on its own.
	c2 := rc("id-2", "Halcyon", 92, "Ellie Goulding")
	if got := pickBestRelease([]releaseCandidate{c2}, "Halcyon", "Ellie Goulding"); got == nil {
		t.Fatal("release-title arm regressed")
	}
	// And a candidate matching on NEITHER title is still rejected.
	c3 := rc("id-3", "Bright Lights", 92, "Ellie Goulding")
	c3.ReleaseGroup = &releaseGroup{ID: "rg-3", Title: "Lights"}
	if got := pickBestRelease([]releaseCandidate{c3}, "Halcyon", "Ellie Goulding"); got != nil {
		t.Fatalf("accepted an unrelated release via the release-group arm: %s", got.Title)
	}
}

// TestPickBestReleaseStillRejectsBelowScoreFloor locks R1. The folding
// work must NOT have relaxed the >=80 floor — that floor is the bound
// every other relaxation here leans on.
func TestPickBestReleaseStillRejectsBelowScoreFloor(t *testing.T) {
	// Folded-EXACT title and credit, but score 79.
	c := rc("id-1", "What’s Up?", 79, "4 Non Blondes")
	if got := pickBestRelease([]releaseCandidate{c}, "What's Up?", "4 Non Blondes"); got != nil {
		t.Fatal("the >=80 release score floor was relaxed — it must not be")
	}
	c.Score = 80
	if got := pickBestRelease([]releaseCandidate{c}, "What's Up?", "4 Non Blondes"); got == nil {
		t.Fatal("score 80 must be accepted (the floor is >=, not >)")
	}
}

// TestPickBestReleaseRawExactOutranksFoldedExact pins the bonus ordering:
// a byte-exact match must always beat a merely folded-equal one.
func TestPickBestReleaseRawExactOutranksFoldedExact(t *testing.T) {
	folded := rc("folded", "What’s Up?", 100, "4 Non Blondes") // +5 title
	raw := rc("raw", "What's Up?", 96, "4 Non Blondes")        // +10 title
	// folded: 100 + 5 + 10(credit exact) = 115
	// raw:     96 + 10 + 10             = 116
	got := pickBestRelease([]releaseCandidate{folded, raw}, "What's Up?", "4 Non Blondes")
	if got == nil || got.ID != "raw" {
		t.Fatalf("got %v, want the byte-exact candidate to win", got)
	}
}

// TestPickBestReleaseKeepsFirstAtTopScore locks R5's strict `>`.
func TestPickBestReleaseKeepsFirstAtTopScore(t *testing.T) {
	a := rc("first", "Greatest Hits", 100, "Queen")
	b := rc("second", "Greatest Hits", 100, "Queen")
	got := pickBestRelease([]releaseCandidate{a, b}, "Greatest Hits", "Queen")
	if got == nil || got.ID != "first" {
		t.Fatalf("got %v, want the FIRST candidate at the top score", got)
	}
}

// TestPickBestReleaseShortTitleGuard — a <=3-rune album title must not
// swallow a longer candidate through containment.
func TestPickBestReleaseShortTitleGuard(t *testing.T) {
	c := rc("id-1", "Go West", 100, "Pet Shop Boys")
	if got := pickBestRelease([]releaseCandidate{c}, "Go", "Pet Shop Boys"); got != nil {
		t.Fatalf("short title 'Go' matched %q via containment", got.Title)
	}
	// Exact still works at that length.
	c2 := rc("id-2", "Go", 100, "Pet Shop Boys")
	if got := pickBestRelease([]releaseCandidate{c2}, "Go", "Pet Shop Boys"); got == nil {
		t.Fatal("an exact short title must still match")
	}
}

// --- artist ---

func ac(id, name string, score int) artistCandidate {
	return artistCandidate{ID: id, Name: name, Score: score}
}

// TestPickBestArtistNormalisedEqualityAtAnyScore is the headline fix,
// encoded with the SCORES ACTUALLY MEASURED against the production
// library. Every one of these was discarded unread because the >=80 floor
// ran before the name comparison.
func TestPickBestArtistNormalisedEqualityAtAnyScore(t *testing.T) {
	for _, tc := range []struct {
		local, mbName string
		score, tracks int
	}{
		{"Peter, Paul & Mary", "Peter, Paul and Mary", 78, 186},
		{"The Carpenters", "Carpenters", 73, 81},
		{"Oscar Peterson Trio", "The Oscar Peterson Trio", 86, 68},
		{"Zdob si Zdub", "Zdob și Zdub", 57, 66},
		{"Yael Naim", "Yael Naïm", 53, 25},
		{"Simon And Garfunkel", "Simon & Garfunkel", 80, 12},
	} {
		t.Run(tc.local, func(t *testing.T) {
			got := pickBestArtist([]artistCandidate{ac("id-1", tc.mbName, tc.score)}, tc.local)
			if got == nil {
				t.Fatalf("rejected %q for %q at score %d — %d tracks lost",
					tc.mbName, tc.local, tc.score, tc.tracks)
			}
		})
	}
}

// TestPickBestArtistPassOrder — raw exact must beat folded exact even
// when the raw match sits later in the candidate list, because raw is the
// strictly stronger signal.
func TestPickBestArtistPassOrder(t *testing.T) {
	cands := []artistCandidate{
		ac("folded", "Zdob și Zdub", 100),
		ac("raw", "Zdob si Zdub", 40),
	}
	got := pickBestArtist(cands, "Zdob si Zdub")
	if got == nil || got.ID != "raw" {
		t.Fatalf("got %v, want the raw-exact candidate regardless of position/score", got)
	}
	// And article-stripped equality must lose to plain folded equality.
	cands2 := []artistCandidate{
		ac("stripped", "Carpenters", 100),
		ac("plain", "The Carpenters", 30),
	}
	got2 := pickBestArtist(cands2, "The Carpenters")
	if got2 == nil || got2.ID != "plain" {
		t.Fatalf("got %v, want the non-article-stripped match to win", got2)
	}
}

// TestPickBestArtistFuzzyFallbackUnchanged — A4 was left exactly as it
// was, because zero measured resolutions came from it.
func TestPickBestArtistFuzzyFallbackUnchanged(t *testing.T) {
	// No name match at all; top score >= 90 → accepted.
	if got := pickBestArtist([]artistCandidate{ac("id-1", "Something Else", 90)}, "Nothing Alike"); got == nil {
		t.Error("the >=90 fuzzy fallback must still fire")
	}
	// Below 90 → nil.
	if got := pickBestArtist([]artistCandidate{ac("id-1", "Something Else", 89)}, "Nothing Alike"); got != nil {
		t.Errorf("fuzzy fallback fired at 89: %v", got)
	}
	if got := pickBestArtist(nil, "Anyone"); got != nil {
		t.Error("empty candidate list must yield nil")
	}
}

// TestPickBestArtistCommaFoldingIsWhyTheLadderMustNotSplitOnCommas
// documents the interaction that makes PR-C's split ban load-bearing
// rather than merely prudent.
//
// The fold erases commas, so fold("Peter, Paul") == fold("Peter Paul").
// A ladder rung that split "Peter, Paul & Mary" on '&' or on a bare comma
// would query "Peter, Paul", and this acceptance would then HAPPILY
// accept an unrelated MusicBrainz artist named "Peter Paul" at score 100
// — 186 tracks with a wrong MBID.
//
// The defence is that the rung is never generated (PR-C), not that the
// acceptance would catch it. This test exists so nobody assumes otherwise.
func TestPickBestArtistCommaFoldingIsWhyTheLadderMustNotSplitOnCommas(t *testing.T) {
	if foldName("Peter, Paul") != foldName("Peter Paul") {
		t.Fatal("precondition changed: the fold no longer erases commas")
	}
	got := pickBestArtist([]artistCandidate{ac("wrong", "Peter Paul", 100)}, "Peter, Paul")
	if got == nil {
		t.Skip("acceptance rejected it — the hazard is gone and PR-C's ban could be revisited")
	}
	// Expected: it IS accepted. That is precisely why the query must never
	// be built.
	if got.ID != "wrong" {
		t.Fatalf("unexpected candidate %v", got)
	}
	// The full string, however, resolves correctly — which is why the
	// ladder tries it FIRST and never needs the dangerous split.
	full := pickBestArtist([]artistCandidate{ac("right", "Peter, Paul and Mary", 78)}, "Peter, Paul & Mary")
	if full == nil || full.ID != "right" {
		t.Fatalf("the full-string match must resolve without any split: %v", full)
	}
}
