package dupes

import "testing"

// TestSortName — literals lifted from MetadataNormalizer.sortName's
// own docstring, so an iOS rule change trips a red test here instead
// of silently de-synchronising the two clients' alphabetical order.
func TestSortName(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"The Beatles", "Beatles"},
		{"The Cars", "Cars"},
		{"the beatles", "beatles"},
		{"THE BEATLES", "BEATLES"},
		// Strips ONLY when a non-empty remainder follows. Note "The "
		// yields "The", not "The ": the twin TRIMS before it tests the
		// prefix, so the trailing space is gone by the time the guard
		// runs and "the" != "the ". The Swift docstring's "returned
		// unchanged" means un-STRIPPED, not un-trimmed — this test
		// asserted the looser reading and went red, which is the test
		// doing its job.
		{"The", "The"},
		{"The ", "The"},
		{"the", "the"},
		// No trailing space after the article → not an article.
		{"Theremin", "Theremin"},
		{"Therapy?", "Therapy?"},
		// Only "The" — never A/An or non-English articles.
		{"A Perfect Circle", "A Perfect Circle"},
		{"An Emerald City", "An Emerald City"},
		{"Los Lobos", "Los Lobos"},
		{"Die Ärzte", "Die Ärzte"},
		// Surrounding whitespace is trimmed; inner spacing is not touched.
		{"  The Beatles  ", "Beatles"},
		{"  Adele  ", "Adele"},
		{"", ""},
		{"   ", ""},
		// Non-"T" first char short-circuits before any prefix work.
		{"Adele", "Adele"},
		{"2Pac", "2Pac"},
	} {
		if got := SortName(tc.in); got != tc.want {
			t.Errorf("SortName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestSortNameIsIdempotent — the docstring claims it, and the catalog
// relies on it: a sort key is recomputed on every rebuild, so a
// non-idempotent strip would reshuffle buckets between rebuilds.
func TestSortNameIsIdempotent(t *testing.T) {
	for _, in := range []string{
		"The Beatles", "The The", "The", "Theremin", "  The Cars ", "Adele", "",
	} {
		once := SortName(in)
		if twice := SortName(once); twice != once {
			t.Errorf("SortName not idempotent for %q: %q → %q", in, once, twice)
		}
	}
}

// TestArtistIDUsesPrimarySegment mirrors MetadataNormalizer.artistID:
// the FIRST "; " segment owns the identity. Splitting on "&" or a bare
// comma is catastrophic here for the same reason it is in the enricher
// ("Peter, Paul & Mary" → "Peter, Paul"), so pin that it doesn't.
func TestArtistIDUsesPrimarySegment(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"Alphaville", "alphaville"},
		{"Alphaville; Deutsches Filmorchester Babelsberg", "alphaville"},
		{"  Alphaville  ", "alphaville"},
		// Never split on these — they are internal to single acts.
		{"Earth, Wind & Fire", "earth, wind & fire"},
		{"Peter, Paul and Mary", "peter, paul and mary"},
		{"Simon & Garfunkel", "simon & garfunkel"},
		{"AC/DC", "ac/dc"},
		{"", ""},
	} {
		if got := ArtistID(tc.in); got != tc.want {
			t.Errorf("ArtistID(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestResolveMatchesKeyFor is the extraction's structural pin: KeyFor
// must remain exactly keyFrom(Resolve(...)). If someone re-inlines the
// ladder into KeyFor and changes one branch, this catches the half
// that Resolve's own callers would otherwise silently disagree with.
func TestResolveMatchesKeyFor(t *testing.T) {
	rows := []Row{
		{Path: "Artist/Album/01 - Song.flac", Title: "Song", Artist: "Artist", Album: "Album", Year: 2001},
		{Path: "Artist/Album/CD 02/07 Other.flac"},
		{Path: "2go/Music/Aerosmith/O, YEAH!/01-Mama Kin.dsf"},
		{Path: "x.flac", Title: "T", Artist: "A", AlbumArtist: "AA", Album: "AL",
			Year: 1999, Disc: 3, DiscTagged: true, Track: 9, TrackTagged: true},
		{Path: "Various/Comp [2014]/03. Track.flac", AlbumArtist: "Various Artists"},
	}
	for _, r := range rows {
		res := Resolve(r)
		want := KeyFor(r)
		got := Key{
			AlbumID:   AlbumIDOf(res),
			Disc:      res.Disc,
			Track:     res.Track,
			NormTitle: Normalize(res.Title),
		}
		if got != want {
			t.Errorf("path %q: Resolve-derived key %+v != KeyFor %+v", r.Path, got, want)
		}
	}
}

// TestResolveRecordsInference pins the flags the catalog orders albums
// by. With discNumber tagged on well under 1% of a real library, the
// INFERRED value is what actually sorts a multi-disc release — so a
// regression that stopped inferring would flatten every box set.
func TestResolveRecordsInference(t *testing.T) {
	inferred := Resolve(Row{Path: "A/Album/CD 02/07 Song.flac"})
	if inferred.Disc != 2 || !inferred.DiscInferred {
		t.Errorf("folder disc: got disc=%d inferred=%v, want 2/true",
			inferred.Disc, inferred.DiscInferred)
	}
	if inferred.Track != 7 || !inferred.TrackInferred {
		t.Errorf("filename track: got track=%d inferred=%v, want 7/true",
			inferred.Track, inferred.TrackInferred)
	}

	tagged := Resolve(Row{Path: "A/Album/CD 02/07 Song.flac",
		Disc: 5, DiscTagged: true, Track: 11, TrackTagged: true})
	if tagged.Disc != 5 || tagged.DiscInferred {
		t.Errorf("tagged disc must win: got disc=%d inferred=%v, want 5/false",
			tagged.Disc, tagged.DiscInferred)
	}
	if tagged.Track != 11 || tagged.TrackInferred {
		t.Errorf("tagged track must win: got track=%d inferred=%v, want 11/false",
			tagged.Track, tagged.TrackInferred)
	}

	// An explicit tagged ZERO is a value, not an absence — the whole
	// reason Row carries DiscTagged/TrackTagged rather than using 0 as
	// a sentinel.
	zero := Resolve(Row{Path: "A/Album/07 Song.flac", Track: 0, TrackTagged: true})
	if zero.Track != 0 || zero.TrackInferred {
		t.Errorf("explicit zero track: got track=%d inferred=%v, want 0/false",
			zero.Track, zero.TrackInferred)
	}
}
