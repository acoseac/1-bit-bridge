package enrich

import (
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// TestIsJunkArtistTagNeverClassifiesRealArtistsAsJunk is the tripwire.
//
// The junk list disables the local-artist veto — the ONLY check in the whole
// fingerprint path that uses information the pipeline did not produce. An
// over-eager classifier is therefore worse than none: it silently removes the
// last independent witness on exactly the tracks where a wrong answer is
// hardest to notice.
//
// Same spirit as TestSplitHeadCreditNeverSplitsOnAmpersandOrBareComma: the
// dangerous direction is over-matching, so that is what gets the long list.
func TestIsJunkArtistTagNeverClassifiesRealArtistsAsJunk(t *testing.T) {
	real := []string{
		"Various Production",       // starts with "various"
		"Unknown Mortal Orchestra", // starts with "unknown"
		"The Unknown",
		"Artist vs Poet",
		"Artist Name - Unknown", // folds to "artist name unknown"
		"Untitled Artist",
		"None More Black",
		"VA Bank",
		"Nona Reeves",
		"Trackspotters",
		"CD Project",
		"Disco Inferno",
		"Peter, Paul & Mary",
		"AC/DC",
		"Zdob și Zdub",
		"a-ha",
		"3 Doors Down", // digits, but not ONLY digits
		"Blink-182",
		"Sunn O)))",
	}
	for _, name := range real {
		if isJunkArtistTag(name) {
			t.Errorf("%q classified as junk — that silently drops the veto on a real artist", name)
		}
	}
}

// TestArtistNamesThatFoldToNothingAreNotWitnesses covers a real artist that
// still cannot serve as a witness. "!!!" is a genuine band, but every
// character is punctuation, so folding leaves nothing and
// foldedTokenContains refuses an empty needle — there is nothing to compare a
// fingerprint answer against in either direction.
//
// Classifying it as "no witness" is therefore correct rather than a
// misclassification: the consequence is a higher submission-count bar at the
// gate, which is the conservative outcome. Recorded as its own test so the
// distinction is not mistaken for a bug in the junk list.
func TestArtistNamesThatFoldToNothingAreNotWitnesses(t *testing.T) {
	for _, name := range []string{"!!!", "()", "...", "- -"} {
		if HasUsableArtistWitness(name) {
			t.Errorf("%q folds to nothing, so it cannot contradict anything", name)
		}
		// And it must not veto, in either direction.
		m := AcousticMatch{ArtistMBID: "x", ArtistName: "Some Artist"}
		if acousticMatchContradictsTag(name, m) {
			t.Errorf("%q must not veto — there is nothing to compare", name)
		}
	}
}

func TestIsJunkArtistTagCatchesTheGenericOnes(t *testing.T) {
	junk := []string{
		"", "   ",
		"Unknown Artist", "unknown artist", "UNKNOWN ARTIST",
		"An Unknown Artist",
		"[Unknown Artist]", // brackets fold to spaces
		"(no artist)",
		"Unknown",
		"Various Artists", "Various", "VA",
		"Untitled",
		"None",
		"N/A", // the slash folds to a space
		"Artist",
		"12345",
		"CD 01", "cd 1", "Disc 2", "Track 03",
	}
	for _, name := range junk {
		if !isJunkArtistTag(name) {
			t.Errorf("%q should be junk — it carries no artist information to veto with", name)
		}
	}
}

// TestAcousticVetoOnlySubtracts pins the property that makes the veto safe to
// add at all: it can refuse a fingerprint answer but can never promote one.
func TestAcousticVetoOnlySubtracts(t *testing.T) {
	m := AcousticMatch{ArtistMBID: "6d7b7cd4-254b-4c25-83f6-dd20f98ceacd", ArtistName: "M83"}

	t.Run("a contradicting tag vetoes", func(t *testing.T) {
		if !acousticMatchContradictsTag("Completely Different Band", m) {
			t.Fatal("a real, disagreeing tag must veto")
		}
	})

	t.Run("an agreeing tag does not veto", func(t *testing.T) {
		if acousticMatchContradictsTag("M83", m) {
			t.Error("an agreeing tag must not veto")
		}
	})

	t.Run("junk and blank tags cannot veto", func(t *testing.T) {
		// No witness is not the same as a disagreeing witness. These tracks
		// are the population fingerprinting exists for; the gate compensates
		// by demanding more independent submissions.
		for _, tag := range []string{"", "An Unknown Artist", "CD 03", "Various Artists"} {
			if acousticMatchContradictsTag(tag, m) {
				t.Errorf("%q must not veto — it carries no information", tag)
			}
		}
	})

	t.Run("token containment tolerates the usual credit shapes", func(t *testing.T) {
		// Real tags routinely carry more or less of the credit than
		// MusicBrainz's canonical name. Those are not contradictions.
		wider := AcousticMatch{ArtistName: "Bill Withers", ArtistMBID: "x"}
		for _, tag := range []string{"Bill Withers", "bill withers", "Bill Withers & Friends"} {
			if acousticMatchContradictsTag(tag, wider) {
				t.Errorf("%q should not read as a contradiction of %q", tag, wider.ArtistName)
			}
		}
	})

	t.Run("an empty fingerprint name cannot veto", func(t *testing.T) {
		if acousticMatchContradictsTag("Some Artist", AcousticMatch{ArtistMBID: "x"}) {
			t.Error("with no fingerprint name there is nothing to compare")
		}
	})
}

// fakeLookup is a canned AcousticLookup.
type fakeLookup map[string]AcousticMatch

func (f fakeLookup) LookupPath(p string) (AcousticMatch, bool) {
	m, ok := f[p]
	return m, ok
}

func TestApplyAcousticFallback(t *testing.T) {
	const goodMBID = "6d7b7cd4-254b-4c25-83f6-dd20f98ceacd"
	const recMBID = "cd2e7c47-16f5-46c6-a37c-a1eb7bf599ff"

	t.Run("nil lookup means the feature is off", func(t *testing.T) {
		e := &Enricher{}
		if _, ok := e.applyAcousticFallback(&manifest.Track{Path: "a.flac"}); ok {
			t.Fatal("a nil lookup must never recover anything")
		}
	})

	t.Run("writes the artist and recording, never a release", func(t *testing.T) {
		e := &Enricher{acoustic: fakeLookup{"a.flac": {
			ArtistMBID: goodMBID, ArtistName: "M83", RecordingMBID: recMBID,
			AlbumHint: "Before the Dawn Heals Us", AcoustID: "acid-1",
		}}}
		tr := &manifest.Track{Path: "a.flac", Artist: "An Unknown Artist"}
		m, ok := e.applyAcousticFallback(tr)
		if !ok {
			t.Fatal("expected a recovery")
		}
		if tr.ArtistMBID != goodMBID {
			t.Errorf("ArtistMBID = %q", tr.ArtistMBID)
		}
		if tr.MusicBrainzTrackID != recMBID {
			t.Errorf("MusicBrainzTrackID = %q", tr.MusicBrainzTrackID)
		}
		// The load-bearing assertion: an album cue is a QUERY term, and must
		// not have been stored as an identifier.
		if tr.MusicBrainzAlbumID != "" {
			t.Errorf("MusicBrainzAlbumID = %q — a fingerprint cannot justify a release", tr.MusicBrainzAlbumID)
		}
		if tr.ArtworkMBID != "" {
			t.Errorf("ArtworkMBID = %q — downstream of a release, equally unjustified", tr.ArtworkMBID)
		}
		if m.ArtistName != "M83" {
			t.Errorf("the caller needs the canonical name for the portrait lookup, got %q", m.ArtistName)
		}
	})

	t.Run("a contradicting tag refuses the whole match", func(t *testing.T) {
		e := &Enricher{acoustic: fakeLookup{"a.flac": {
			ArtistMBID: goodMBID, ArtistName: "M83", RecordingMBID: recMBID,
		}}}
		tr := &manifest.Track{Path: "a.flac", Artist: "Some Other Band"}
		if _, ok := e.applyAcousticFallback(tr); ok {
			t.Fatal("the veto must refuse the match")
		}
		if tr.ArtistMBID != "" || tr.MusicBrainzTrackID != "" {
			t.Errorf("a vetoed match must write nothing at all: %+v", tr)
		}
	})

	t.Run("a non-UUID artist MBID is refused", func(t *testing.T) {
		// AcoustID is a third-party JSON source and this value reaches
		// ArtistImagePath's filepath.Join as a leading component.
		e := &Enricher{acoustic: fakeLookup{"a.flac": {
			ArtistMBID: "../../evil", ArtistName: "M83",
		}}}
		tr := &manifest.Track{Path: "a.flac"}
		if _, ok := e.applyAcousticFallback(tr); ok {
			t.Fatal("a non-UUID MBID must be refused before it can reach a path")
		}
		if tr.ArtistMBID != "" {
			t.Errorf("ArtistMBID = %q", tr.ArtistMBID)
		}
	})

	t.Run("a non-UUID recording MBID drops only that field", func(t *testing.T) {
		e := &Enricher{acoustic: fakeLookup{"a.flac": {
			ArtistMBID: goodMBID, ArtistName: "M83", RecordingMBID: "not-a-uuid",
		}}}
		tr := &manifest.Track{Path: "a.flac"}
		if _, ok := e.applyAcousticFallback(tr); !ok {
			t.Fatal("a bad recording MBID must not cost the artist")
		}
		if tr.ArtistMBID != goodMBID {
			t.Errorf("ArtistMBID = %q", tr.ArtistMBID)
		}
		if tr.MusicBrainzTrackID != "" {
			t.Errorf("MusicBrainzTrackID = %q, want it dropped", tr.MusicBrainzTrackID)
		}
	})

	t.Run("no verdict for this path", func(t *testing.T) {
		e := &Enricher{acoustic: fakeLookup{}}
		if _, ok := e.applyAcousticFallback(&manifest.Track{Path: "a.flac"}); ok {
			t.Fatal("an absent verdict must not recover anything")
		}
	})
}

// TestHasUsableArtistWitness is the sweeper-facing view of the same
// classification: the gate raises its submission-count bar when a track has no
// tag that could contradict the answer, and that decision is made where the
// fingerprint is taken.
func TestHasUsableArtistWitness(t *testing.T) {
	if HasUsableArtistWitness("An Unknown Artist") {
		t.Error("a junk tag is not a witness")
	}
	if HasUsableArtistWitness("") {
		t.Error("a blank tag is not a witness")
	}
	if !HasUsableArtistWitness("Peter, Paul & Mary") {
		t.Error("a real artist is a witness")
	}
}

func TestIsJunkAlbumTag(t *testing.T) {
	junk := []string{"", "  ", "CD 01", "cd 3", "Disc 2", "Unknown Album", "Untitled", "None", "N/A"}
	for _, a := range junk {
		if !isJunkAlbumTag(a) {
			t.Errorf("%q should be junk — there is nothing to search MusicBrainz by", a)
		}
	}
	// Real album titles must survive: misclassifying one discards the
	// operator's own title in favour of a fingerprint hint, which is the
	// wrong way round.
	real := []string{
		"Load", "CD Project Red", "Disc-Overy", "Untitled Unmastered",
		"None So Vile", "Unknown Pleasures", "Album of the Year",
		"Track and Field",
		// Numeric album titles are common and must survive — see the
		// asymmetry note on isJunkAlbumTag. Misclassifying one would
		// substitute a fingerprint's title for the operator's own.
		"1", "4", "21", "1989", "90125",
	}
	for _, a := range real {
		if isJunkAlbumTag(a) {
			t.Errorf("%q classified as junk — a real title must win over a fingerprint hint", a)
		}
	}
}

// TestAlbumTermPrefersTheLocalTag pins the ordering in the album hop: the
// operator's own album title is used when it is usable, and the fingerprint's
// hint only fills in when there is nothing to search by.
//
// Backwards, this would let a fingerprint override a perfectly good album
// title with whichever release group AcoustID happened to name.
func TestAlbumTermPrefersTheLocalTag(t *testing.T) {
	// A real local tag beats a hint.
	if isJunkAlbumTag("Kind of Blue") {
		t.Fatal("precondition: a real title must not be junk")
	}
	// A junk local tag yields to the hint.
	if !isJunkAlbumTag("CD 03") {
		t.Fatal("precondition: a disc-number title must be junk")
	}
}
