package enrich

import (
	"context"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/lrucache"
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
	realArtists := []string{
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
	for _, name := range realArtists {
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

// vetoSubject is the match every veto test compares against.
func vetoSubject() AcousticMatch {
	return AcousticMatch{ArtistMBID: "6d7b7cd4-254b-4c25-83f6-dd20f98ceacd", ArtistName: "M83"}
}

// TestAcousticVetoRefusesAContradiction — the veto's whole job.
func TestAcousticVetoRefusesAContradiction(t *testing.T) {
	if !acousticMatchContradictsTag("Completely Different Band", vetoSubject()) {
		t.Fatal("a real, disagreeing tag must veto")
	}
}

// TestAcousticVetoOnlySubtracts pins the property that makes the veto safe to
// add at all: it can refuse a fingerprint answer but can never promote one.
// Everything below must NOT veto.
func TestAcousticVetoOnlySubtracts(t *testing.T) {
	m := vetoSubject()

	if acousticMatchContradictsTag("M83", m) {
		t.Error("an agreeing tag must not veto")
	}

	// No witness is not the same as a disagreeing witness. These tracks are
	// the population fingerprinting exists for; the gate compensates by
	// demanding more independent submissions.
	for _, tag := range []string{"", "An Unknown Artist", "CD 03", "Various Artists"} {
		if acousticMatchContradictsTag(tag, m) {
			t.Errorf("%q must not veto — it carries no information", tag)
		}
	}

	// Real tags routinely carry more or less of the credit than MusicBrainz's
	// canonical name. Those are not contradictions.
	wider := AcousticMatch{ArtistName: "Bill Withers", ArtistMBID: "x"}
	for _, tag := range []string{"Bill Withers", "bill withers", "Bill Withers & Friends"} {
		if acousticMatchContradictsTag(tag, wider) {
			t.Errorf("%q should not read as a contradiction of %q", tag, wider.ArtistName)
		}
	}

	if acousticMatchContradictsTag("Some Artist", AcousticMatch{ArtistMBID: "x"}) {
		t.Error("with no fingerprint name there is nothing to compare")
	}
}

// fakeLookup is a canned AcousticLookup.
type fakeLookup map[string]AcousticMatch

func (f fakeLookup) LookupPath(p string) (AcousticMatch, bool) {
	m, ok := f[p]
	return m, ok
}

const (
	fbArtistMBID = "6d7b7cd4-254b-4c25-83f6-dd20f98ceacd"
	fbRecMBID    = "cd2e7c47-16f5-46c6-a37c-a1eb7bf599ff"
)

func TestApplyAcousticFallbackOffWhenNoLookup(t *testing.T) {
	e := &Enricher{}
	if _, o := e.applyAcousticFallback(&manifest.Track{Path: "a.flac"}); o != acousticNoVerdict {
		t.Fatalf("outcome = %v — a nil lookup must never recover anything", o)
	}
}

// TestApplyAcousticFallbackWritesArtistNeverRelease is the behavioural twin of
// the gate's structural pin: even a maximally favourable verdict, carrying an
// album cue, must leave the release and artwork fields empty. The cue is a
// QUERY term, and storing it as an identifier is the one thing a fingerprint
// cannot justify.
func TestApplyAcousticFallbackWritesArtistNeverRelease(t *testing.T) {
	e := &Enricher{acoustic: fakeLookup{"a.flac": {
		ArtistMBID: fbArtistMBID, ArtistName: "M83", RecordingMBID: fbRecMBID,
		AlbumHint: "Before the Dawn Heals Us", AcoustID: "acid-1",
	}}}
	tr := &manifest.Track{Path: "a.flac", Artist: "An Unknown Artist"}

	m, o := e.applyAcousticFallback(tr)
	if o != acousticApplied {
		t.Fatalf("outcome = %v, want applied", o)
	}
	if tr.ArtistMBID != fbArtistMBID {
		t.Errorf("ArtistMBID = %q", tr.ArtistMBID)
	}
	if tr.MusicBrainzTrackID != fbRecMBID {
		t.Errorf("MusicBrainzTrackID = %q", tr.MusicBrainzTrackID)
	}
	if tr.MusicBrainzAlbumID != "" {
		t.Errorf("MusicBrainzAlbumID = %q — a fingerprint cannot justify a release", tr.MusicBrainzAlbumID)
	}
	if tr.ArtworkMBID != "" {
		t.Errorf("ArtworkMBID = %q — downstream of a release, equally unjustified", tr.ArtworkMBID)
	}
	if m.ArtistName != "M83" {
		t.Errorf("the caller needs the canonical name for the portrait lookup, got %q", m.ArtistName)
	}
}

// TestApplyAcousticFallbackRefusals — each of these must be REFUSED rather
// than reported as "no verdict": a verdict existed and this layer rejected it,
// and the skip-reason counters depend on telling those apart.
func TestApplyAcousticFallbackRefusals(t *testing.T) {
	t.Run("a contradicting tag refuses the whole match", func(t *testing.T) {
		e := &Enricher{acoustic: fakeLookup{"a.flac": {
			ArtistMBID: fbArtistMBID, ArtistName: "M83", RecordingMBID: fbRecMBID,
		}}}
		tr := &manifest.Track{Path: "a.flac", Artist: "Some Other Band"}
		if _, o := e.applyAcousticFallback(tr); o != acousticRefused {
			t.Fatalf("outcome = %v, want refused", o)
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
		if _, o := e.applyAcousticFallback(tr); o != acousticRefused {
			t.Fatalf("outcome = %v — a non-UUID MBID must not reach a path", o)
		}
		if tr.ArtistMBID != "" {
			t.Errorf("ArtistMBID = %q", tr.ArtistMBID)
		}
	})
}

func TestApplyAcousticFallbackPartialData(t *testing.T) {
	t.Run("a non-UUID recording MBID drops only that field", func(t *testing.T) {
		e := &Enricher{acoustic: fakeLookup{"a.flac": {
			ArtistMBID: fbArtistMBID, ArtistName: "M83", RecordingMBID: "not-a-uuid",
		}}}
		tr := &manifest.Track{Path: "a.flac"}
		if _, o := e.applyAcousticFallback(tr); o != acousticApplied {
			t.Fatalf("outcome = %v — a bad recording MBID must not cost the artist", o)
		}
		if tr.ArtistMBID != fbArtistMBID {
			t.Errorf("ArtistMBID = %q", tr.ArtistMBID)
		}
		if tr.MusicBrainzTrackID != "" {
			t.Errorf("MusicBrainzTrackID = %q, want it dropped", tr.MusicBrainzTrackID)
		}
	})

	t.Run("no verdict for this path", func(t *testing.T) {
		e := &Enricher{acoustic: fakeLookup{}}
		if _, o := e.applyAcousticFallback(&manifest.Track{Path: "a.flac"}); o != acousticNoVerdict {
			t.Fatalf("outcome = %v, want no-verdict", o)
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
	realAlbums := []string{
		"Load", "CD Project Red", "Disc-Overy", "Untitled Unmastered",
		"None So Vile", "Unknown Pleasures", "Album of the Year",
		"Track and Field",
		// Numeric album titles are common and must survive — see the
		// asymmetry note on isJunkAlbumTag. Misclassifying one would
		// substitute a fingerprint's title for the operator's own.
		"1", "4", "21", "1989", "90125",
	}
	for _, a := range realAlbums {
		if isJunkAlbumTag(a) {
			t.Errorf("%q classified as junk — a real title must win over a fingerprint hint", a)
		}
	}
}

// TestAlbumSearchTermPrefersTheLocalTitle pins the one function both the
// release search and the artwork chain's iTunes fallback consult.
//
// Computing it twice is how the two drift, and the failure is silent: a junk
// album name reaching iTunes loses that fallback without any error.
func TestAlbumSearchTermPrefersTheLocalTitle(t *testing.T) {
	hint := AcousticMatch{AlbumHint: "Before the Dawn Heals Us"}

	if got := albumSearchTerm(&manifest.Track{Album: "Kind of Blue"}, hint); got != "Kind of Blue" {
		t.Errorf("got %q — a real local title must outrank the fingerprint's hint", got)
	}
	if got := albumSearchTerm(&manifest.Track{Album: "CD 03"}, hint); got != hint.AlbumHint {
		t.Errorf("got %q — a junk local title must yield to the hint", got)
	}
	if got := albumSearchTerm(&manifest.Track{Album: ""}, hint); got != hint.AlbumHint {
		t.Errorf("got %q — a blank title must yield to the hint", got)
	}
	// Numeric titles are real albums; see the asymmetry note on isJunkAlbumTag.
	if got := albumSearchTerm(&manifest.Track{Album: "1989"}, hint); got != "1989" {
		t.Errorf("got %q — a numeric album title must not be replaced", got)
	}
	// Nothing usable on either side.
	if got := albumSearchTerm(&manifest.Track{Album: "CD 03"}, AcousticMatch{}); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

// TestAcousticFallbackDoesNotOverwriteAResolvedArtist — reaching the fallback
// with an artist already set means pickBestArtist accepted a real tag, which
// is at least as trustworthy as audio. The fingerprint is being consulted for
// the ALBUM in that case, so it must leave the artist alone.
func TestAcousticFallbackDoesNotOverwriteAResolvedArtist(t *testing.T) {
	const fromText = "11111111-1111-1111-1111-111111111111"
	const fromAudio = "6d7b7cd4-254b-4c25-83f6-dd20f98ceacd"

	e := &Enricher{acoustic: fakeLookup{"a.flac": {
		ArtistMBID: fromAudio, ArtistName: "M83", AlbumHint: "Before the Dawn Heals Us",
	}}}
	tr := &manifest.Track{Path: "a.flac", Artist: "M83", Album: "CD 01", ArtistMBID: fromText}

	m, o := e.applyAcousticFallback(tr)
	if o != acousticApplied {
		t.Fatalf("outcome = %v — the fallback must still run, the album hint is the new information", o)
	}
	if tr.ArtistMBID != fromText {
		t.Errorf("ArtistMBID = %q, want the text-resolved value preserved", tr.ArtistMBID)
	}
	// The match is still returned so the caller can run the album hop.
	if m.AlbumHint == "" {
		t.Error("the caller needs the hint to search the album by")
	}
}

// TestAlbumHopSharesTheTextPathCache pins the sibling-track saving.
//
// Every track under one junk-tagged folder produces an identical
// (artistName, album) query. Without a shared cache each pays its own
// SearchRelease plus a full MBMinInterval sleep — 1.1s each against public
// MusicBrainz, on exactly the population this feature targets. A 15-track
// "CD 01" folder would spend a quarter of a minute asking the same question
// fifteen times.
func TestAlbumHopSharesTheTextPathCache(t *testing.T) {
	e := &Enricher{albumCache: lrucache.New[string, albumResolution](8)}
	m := AcousticMatch{ArtistName: "M83", AlbumHint: "Before the Dawn Heals Us"}
	tr := &manifest.Track{Path: "a.flac", Album: "CD 01"}

	// Seed as though the text path had already resolved this exact query.
	const release = "11111111-1111-1111-1111-111111111111"
	const group = "22222222-2222-2222-2222-222222222222"
	e.albumCache.Set(cacheKey(m.ArtistName, m.AlbumHint),
		albumResolution{ReleaseMBID: release, ReleaseGroupMBID: group})

	// e.mb is nil: reaching the network here would panic, which is precisely
	// the assertion — the cache must answer without a round-trip.
	gotRelease, gotGroup, err := e.resolveAlbumFromAcoustic(context.Background(), tr, m)
	if err != nil {
		t.Fatalf("resolveAlbumFromAcoustic: %v", err)
	}
	if gotRelease != release || gotGroup != group {
		t.Errorf("got (%q, %q), want the cached resolution", gotRelease, gotGroup)
	}
}

// TestAlbumHopUsesTheSameKeyAsTheTextPath — sharing only pays off if both
// sides key identically. A divergence here would silently halve the hit rate
// rather than fail anything.
func TestAlbumHopUsesTheSameKeyAsTheTextPath(t *testing.T) {
	// The hop keys on the fingerprint's canonical artist name and the resolved
	// album term — the same two inputs the text path uses, in the same order.
	m := AcousticMatch{ArtistName: "M83", AlbumHint: "Hint"}
	tr := &manifest.Track{Album: "Real Album"}
	if got, want := cacheKey(m.ArtistName, albumSearchTerm(tr, m)), cacheKey("M83", "Real Album"); got != want {
		t.Errorf("key = %q, want %q", got, want)
	}
}

// TestArtistTagPredicatesDiffer is the guard against merging two predicates
// that look like duplicates.
//
// One table rather than three loops, because the contrast IS the subject: each
// row states what both predicates must say about the same name, so a reader
// can see at a glance where they diverge and why.
//
// isJunkArtistTag can afford to be broad — a false positive there only removes
// the local witness, and the gate compensates by demanding more submissions.
// isUnsearchableArtistTag cannot: a false positive there means the MusicBrainz
// query is never sent and the track permanently loses a text match it would
// have had. Anyone collapsing the two fails here, named row by row, rather than
// quietly costing real bands their metadata.
func TestArtistTagPredicatesDiffer(t *testing.T) {
	for _, tc := range []struct {
		name         string
		junk         bool // no usable witness for the fingerprint veto
		unsearchable bool // MusicBrainz cannot answer; do not send the query
		why          string
	}{
		// Broad for the veto, searchable here. These are the rows that make
		// the two sets different, and the reason they cannot be merged.
		{"311", true, false, "real band, all digits"},
		{"112", true, false, "real band, all digits"},
		{"Various Artists", true, false, "a real MusicBrainz special-purpose artist"},
		{"VA", true, false, "abbreviation of the same real entity"},
		{"!!!", true, false, "real band; folds to nothing, so no witness is possible"},
		{"unknown", true, false, "plausible band name on its own"},
		{"None", true, false, "plausible band name on its own"},
		{"Untitled", true, false, "plausible band name on its own"},

		// What isUnsearchableArtistTag exists for: folder labels that landed
		// in the artist field, and explicit placeholders.
		{"CD 01", true, true, "disc folder label"},
		{"cd 2", true, true, "disc folder label"},
		{"Disc 1", true, true, "disc folder label"},
		{"disk 03", true, true, "disc folder label"},
		{"Track 7", true, true, "track label"},
		{"An Unknown Artist", true, true, "explicit placeholder"},
		{"Unknown Artist", true, true, "explicit placeholder"},
		{"No Artist", true, true, "explicit placeholder"},

		// Real artists that shade toward the patterns above. Each one that
		// slipped through would lose its MusicBrainz identity for the life of
		// the library.
		{"CD Projekt", false, false, "starts with the cd prefix, not a label"},
		{"Discharge", false, false, "starts with disc"},
		{"Disclosure", false, false, "starts with disc"},
		{"Track and Field", false, false, "starts with track"},
		{"CD 01 Orchestra", false, false, "label-shaped prefix, but continues"},
		{"Unknown Mortal Orchestra", false, false, "starts with unknown"},
		{"The Unknown Artist Collective", false, false, "contains the placeholder phrase"},
		{"Artist vs Poet", false, false, "starts with artist"},
		{"Peter, Paul and Mary", false, false, "ordinary name"},
		{"Simon & Garfunkel", false, false, "ordinary name"},
		{"Zdob \u0219i Zdub", false, false, "non-ASCII, ordinary name"},
	} {
		if got := isJunkArtistTag(tc.name); got != tc.junk {
			t.Errorf("isJunkArtistTag(%q) = %v, want %v — %s", tc.name, got, tc.junk, tc.why)
		}
		if got := isUnsearchableArtistTag(tc.name); got != tc.unsearchable {
			t.Errorf("isUnsearchableArtistTag(%q) = %v, want %v — %s\n"+
				"\ta wrong true here stops MusicBrainz ever being asked about this name",
				tc.name, got, tc.unsearchable, tc.why)
		}
	}
}

// TestResolveArtistSkipsTheRequestForUnsearchableTags pins the behaviour, not
// just the predicate: the request must not be sent at all.
//
// The counter is the whole point. A version that sent the query and discarded
// the answer would satisfy every assertion about the track, and would still
// leave the tracks this exists for spinning against a 5xx forever — a transient
// error returns without stamping, so a futile query retries on every batch and
// never reaches the fingerprint fallback.
func TestResolveArtistSkipsTheRequestForUnsearchableTags(t *testing.T) {
	e, calls := newCountingArtistEnricher(t)

	tr := &manifest.Track{Path: "CD 01/Album/01.flac", Artist: "CD 01", Album: "Some Album"}
	if err := e.resolveArtist(context.Background(), tr); err != nil {
		t.Fatalf("resolveArtist: %v", err)
	}
	if got := calls.Load(); got != 0 {
		t.Errorf("upstream saw %d requests, want 0 — a folder label was searched as an artist", got)
	}
	if tr.ArtistMBID != "" {
		t.Errorf("ArtistMBID = %q, want empty", tr.ArtistMBID)
	}

	// A real name on the same enricher still goes out, so the skip is scoped
	// rather than a blanket disable.
	realArtist := &manifest.Track{Path: "a/b/c.flac", Artist: "Nobody At All", Album: "Some Album"}
	if err := e.resolveArtist(context.Background(), realArtist); err != nil {
		t.Fatalf("resolveArtist(real): %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("upstream saw %d requests after a real artist, want 1", got)
	}
}
