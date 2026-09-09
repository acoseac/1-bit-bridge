package atlasharvest

import "testing"

func rt(medium, pos int, title string, lengthMS int64, mbid string) ReleaseTrack {
	return ReleaseTrack{MediumPosition: medium, Position: pos, Title: title, LengthMS: lengthMS, RecordingMBID: mbid}
}

// The Beatles release the brief names, shortened. Two discs so the medium
// actually matters.
func twoDiscRelease() []ReleaseTrack {
	return []ReleaseTrack{
		rt(1, 1, "Love Me Do", 143000, "rec-d1t1"),
		rt(1, 2, "Please Please Me", 123000, "rec-d1t2"),
		rt(1, 3, "From Me to You", 117000, "rec-d1t3"),
		rt(2, 1, "A Hard Day's Night", 154000, "rec-d2t1"),
		rt(2, 2, "Eight Days a Week", 164000, "rec-d2t2"),
	}
}

func TestMatchRelease(t *testing.T) {
	single := []ReleaseTrack{
		rt(1, 1, "Alpha", 100000, "rec-1"),
		rt(1, 2, "Beta", 200000, "rec-2"),
		rt(1, 3, "Intro", 30000, "rec-3"),
		rt(1, 4, "Intro", 31000, "rec-4"), // the duplicate-title case, 1,275 albums here have one
	}
	for _, tc := range []struct {
		name      string
		entries   []ReleaseTrack
		title     string
		disc, num int
		durMS     int64
		wantMBID  string
		wantTier  MatchTier
	}{
		{"position and title agree", single, "Alpha", 1, 1, 100000, "rec-1", MatchCorroborated},
		{"corroboration ignores a wild duration", single, "Alpha", 1, 1, 999000, "rec-1", MatchCorroborated},
		{"unique title when the position is wrong", single, "Beta", 1, 9, 200000, "rec-2", MatchTitle},
		{"position when the title disagrees", single, "Renamed By Me", 1, 2, 200000, "rec-2", MatchPosition},

		// A duplicate title names nothing on its own...
		{"ambiguous title alone resolves nothing", single, "Intro", 0, 0, 30000, "", MatchNone},
		// ...but the position still corroborates it, because the entry the
		// position names is one of the ones carrying that title.
		{"ambiguous title is still corroborated by position", single, "Intro", 1, 4, 31000, "rec-4", MatchCorroborated},

		// The multi-disc trap: a file with no disc number must NOT be assumed
		// to be on medium 1, or disc 2 track 1 silently takes disc 1 track 1.
		{"no disc on a multi-medium release refuses to position",
			twoDiscRelease(), "Unknown Title", 0, 1, 154000, "", MatchNone},
		{"no disc on a SINGLE-medium release still positions",
			single, "Renamed By Me", 0, 2, 200000, "rec-2", MatchPosition},
		{"a tagged disc positions on a multi-medium release",
			twoDiscRelease(), "Renamed", 2, 2, 164000, "rec-d2t2", MatchPosition},

		// The veto, which applies only to the uncorroborated tiers.
		{"a wildly wrong duration vetoes a position-only match",
			single, "Renamed By Me", 1, 2, 20000, "", MatchNone},
		{"a wildly wrong duration vetoes a title-only match",
			single, "Beta", 1, 9, 20000, "", MatchNone},
		{"an unknown local duration abstains rather than refusing",
			single, "Renamed By Me", 1, 2, 0, "rec-2", MatchPosition},

		{"an empty release resolves nothing", nil, "Alpha", 1, 1, 100000, "", MatchNone},
		{"nothing to match on", single, "", 0, 0, 0, "", MatchNone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, tier := MatchRelease(tc.entries, tc.title, tc.disc, tc.num, tc.durMS)
			if tier != tc.wantTier {
				t.Errorf("tier = %v, want %v", tier, tc.wantTier)
			}
			if got.RecordingMBID != tc.wantMBID {
				t.Errorf("mbid = %q, want %q", got.RecordingMBID, tc.wantMBID)
			}
		})
	}
}

// The veto must be OFF for a corroborated match and ON for the others. That
// split is the measurement, not a preference: over 752 tracks, corroborated
// matches agreed on duration 100% of the time within 30 s, while the
// uncorroborated tiers were at 82% and 87% — so a veto there discriminates and
// a veto on corroborated matches could only ever be wrong.
func TestTheDurationVetoAppliesOnlyToUncorroboratedMatches(t *testing.T) {
	entries := []ReleaseTrack{rt(1, 1, "Alpha", 100000, "rec-1"), rt(1, 2, "Beta", 200000, "rec-2")}
	// Corroborated, absurd duration: still accepted.
	if _, tier := MatchRelease(entries, "Alpha", 1, 1, 5_000_000); tier != MatchCorroborated {
		t.Errorf("a corroborated match was vetoed on duration: %v", tier)
	}
	// Position-only, same absurd duration: refused.
	if _, tier := MatchRelease(entries, "Not The Title", 1, 1, 5_000_000); tier != MatchNone {
		t.Errorf("an uncorroborated match ignored the duration: %v", tier)
	}
	// And just inside the threshold it is accepted, so the bound is not
	// rejecting everything.
	if _, tier := MatchRelease(entries, "Not The Title", 1, 1, 100000+maxDurationDeltaMS-1); tier != MatchPosition {
		t.Errorf("a duration just inside the threshold was refused: %v", tier)
	}
}

func TestFoldTitle(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		// The real pair from this library: the local tag is unaccented.
		{"Hasta mañana", "hasta manana"},
		{"Hasta Manana", "hasta manana"},
		{"  The   Long  Song  ", "the long song"},
		{"Rock & Roll!", "rock roll"},
		{"Track (Remastered 2009)", "track remastered 2009"},
		{"ÄÖÜ", "aou"},
		{"", ""},
		{"—", ""},
	} {
		if got := foldTitle(tc.in); got != tc.want {
			t.Errorf("foldTitle(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// A title that folds to nothing must not match every other such title —
// otherwise a release with two untitled entries resolves one of them at random.
func TestAnEmptyFoldNeverMatches(t *testing.T) {
	entries := []ReleaseTrack{rt(1, 1, "—", 100000, "rec-1"), rt(1, 2, "Beta", 200000, "rec-2")}
	if got, tier := MatchRelease(entries, "!!!", 0, 0, 100000); tier != MatchNone {
		t.Errorf("an empty fold matched %q via %v", got.RecordingMBID, tier)
	}
}

func TestSoleMedium(t *testing.T) {
	if m, ok := soleMedium([]ReleaseTrack{rt(1, 1, "a", 0, "x"), rt(1, 2, "b", 0, "y")}); !ok || m != 1 {
		t.Errorf("single medium: got %d,%v", m, ok)
	}
	if _, ok := soleMedium(twoDiscRelease()); ok {
		t.Error("a two-disc release reported a sole medium")
	}
	// A release whose only medium is not numbered 1 still has a sole medium.
	if m, ok := soleMedium([]ReleaseTrack{rt(2, 1, "a", 0, "x")}); !ok || m != 2 {
		t.Errorf("sole medium 2: got %d,%v", m, ok)
	}
}
