// Mirror pins for the client-key helpers.
//
// EVERY expected value in this file is lifted VERBATIM from the iOS test
// suite — com.acoseac.dsdplayer/Tests/com_acoseac_dsdplayerTests/
// MetadataNormalizerTests.swift — so that an iOS-side rule change breaks
// a test HERE and the two partitions can be re-synchronised deliberately
// instead of drifting silently. When one of these fails after an iOS
// update, the fix is to mirror the iOS change, never to "improve" the Go
// side independently (see the package doc).
package dupes

import (
	"strings"
	"testing"
)

func TestNormalize_MirrorsSwiftPins(t *testing.T) {
	// test_normalizeLowercasesByDefault / test_normalizeCollapsesMixedWhitespace
	if got := normalize("  The  Band  "); got != "the band" {
		t.Fatalf("normalize: got %q", got)
	}
	if got := normalize("a\tb\nc   d"); got != "a b c d" {
		t.Fatalf("normalize mixed whitespace: got %q", got)
	}
}

func TestCleanDisplayName_MirrorsSwiftPins(t *testing.T) {
	cases := []struct{ in, want string }{
		{"[E] Song", "Song"},               // test_stripsExplicitMarker
		{"[Explicit] Song", "Song"},        //
		{"Song [90777496]", "Song"},        // test_stripsNumericBrackets
		{"Song [Deluxe]", "Song [Deluxe]"}, // test_preservesTextBrackets
		{"[E]", "[E]"},                     // test_returnsOriginalIfCleaningEmptiesString
		{"Live at Bangor Abbey [246014166] [2014]", "Live at Bangor Abbey"}, // test_stripsTaggerCatalogIDAndYearBrackets
		{"[E][246014166] Song", "Song"},                                     // test_stripsConsecutiveLeadingBrackets
		{"Album [9999999999] [2024] [Deluxe]", "Album [Deluxe]"},            // test_preservesTextBracketsAmongNumericStrips
		{"F***", "F***"},     // test_cleanDisplayName_preservesAsteriskForTitles
		{"Song *", "Song *"}, //
	}
	for _, c := range cases {
		if got := cleanDisplayName(c.in); got != c.want {
			t.Errorf("cleanDisplayName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestCleanArtistName_MirrorsSwiftPins(t *testing.T) {
	cases := []struct{ in, want string }{
		{"João Gilberto*", "João Gilberto"},                         // test_stripsTrailingDiscogsAsterisk
		{"Stan Getz & João Gilberto*", "Stan Getz & João Gilberto"}, // test_stripsAsteriskAtTokenBoundaries
		{"João Gilberto* & Stan Getz", "João Gilberto & Stan Getz"}, //
		{"A*, B", "A, B"},              //
		{"A*/B", "A/B"},                //
		{"A*B", "A*B"},                 // test_preservesMidTokenAsterisk
		{"Miles Davis", "Miles Davis"}, // test_cleanArtistName_isIdempotent…
		{"João Gilberto* [90777496]", "João Gilberto"}, // …AndComposesBracketStrip
	}
	for _, c := range cases {
		if got := cleanArtistName(c.in); got != c.want {
			t.Errorf("cleanArtistName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// Idempotence pin.
	if got := cleanArtistName(cleanArtistName("João Gilberto*")); got != "João Gilberto" {
		t.Errorf("cleanArtistName not idempotent: %q", got)
	}
}

func TestDiscNumberForFolderName_MirrorsSwiftPins(t *testing.T) {
	// test_matchesCDFolderVariants — NOTE the client rule includes Vol,
	// unlike the bridge's own discFolderRe. That difference is the point.
	for _, c := range []struct {
		in   string
		want int
	}{
		{"CD1", 1}, {"CD 2", 2}, {"Disc 3", 3}, {"disk04", 4}, {"Vol 5", 5},
	} {
		got, ok := discNumberForFolderName(c.in)
		if !ok || got != c.want {
			t.Errorf("discNumberForFolderName(%q) = (%d,%v), want (%d,true)", c.in, got, ok, c.want)
		}
	}
	// test_rejectsNonDiscFolders
	for _, in := range []string{"Album", "Mix CD1 extra"} {
		if _, ok := discNumberForFolderName(in); ok {
			t.Errorf("discNumberForFolderName(%q) matched, want no match", in)
		}
	}
}

func TestEffectiveAlbumPath_MirrorsSwiftPins(t *testing.T) {
	// test_stripsDiscFolderFromAlbumPath
	album, disc := effectiveAlbumPath("/Music/Artist/Album/CD2/track.flac")
	if album != "/Music/Artist/Album" || disc != 2 {
		t.Fatalf("disc-folder path: got (%q,%d)", album, disc)
	}
	// test_keepsRegularParentAsAlbum
	album, disc = effectiveAlbumPath("/Music/Artist/Album/track.flac")
	if album != "/Music/Artist/Album" || disc != 1 {
		t.Fatalf("regular path: got (%q,%d)", album, disc)
	}
}

// TestTrackNumberFallbackUsesTheSwiftRule pins the divergence the mirror
// must HONOUR, not fix: Swift's trackPrefixRegex accepts a bare space
// after the digits, so "07 Song.flac" yields 7 here. The bridge's own
// parseLeadingTrackNumber (internal/manifest/extractors.go) deliberately
// requires punctuation and returns 0 for the same input — it is the wrong
// helper for this mirror and must never be substituted in.
func TestTrackNumberFallbackUsesTheSwiftRule(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"03 - Song.flac", 3},               // test_trackNumberStripsLeadingDigits
		{"12.Song.flac", 12},                //
		{"07_Song.flac", 7},                 //
		{"Song.flac", 0},                    // test_trackNumberZeroWhenAbsent
		{"07 Song.flac", 7},                 // the bare-space divergence pin
		{"1234 Song.flac", 0},               // >3 digits cannot match \d{1,3} + separator
		{"450 de Oi (The Sheep).flac", 450}, // filename rule is NOT the title rule
	}
	for _, c := range cases {
		if got := trackNumberFromFilename(c.in); got != c.want {
			t.Errorf("trackNumberFromFilename(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestAlbumID_MirrorsSwiftPins(t *testing.T) {
	// test_albumIDIsStable
	if albumID("The Band", "Record", 2020) != albumID("  the  band  ", "Record", 2020) {
		t.Error("albumID not stable under whitespace/casing")
	}
	// test_albumID_treatsYearZeroAsAbsent / …NegativeYearAsAbsent
	if albumID("Beatles", "Abbey Road", 0) != albumID("Beatles", "Abbey Road", -1) {
		t.Error("year 0 and negative year must both read as absent")
	}
	// test_albumID_distinguishesRealYears
	if albumID("Beatles", "Abbey Road", 1969) == albumID("Beatles", "Abbey Road", 2019) {
		t.Error("real years must distinguish albums")
	}
	// test_albumID_multiValueAlbumArtist_collapsesToPrimary
	if albumID("Alphaville; Deutsches Filmorchester Babelsberg", "Eternally Yours Bonus-EP I", 2023) !=
		albumID("Alphaville", "Eternally Yours Bonus-EP I", 2023) {
		t.Error("multi-value albumArtist must collapse to its primary segment")
	}
	// test_albumID_variousArtists_preservedNotSliced
	if !strings.HasPrefix(albumID("Various Artists", "O Brother Where Art Thou", 2000), "various artists|") {
		t.Error("Various Artists must be preserved whole")
	}
	if !strings.HasPrefix(albumID("Various Artists; Alison Krauss", "O Brother Where Art Thou", 2000), "various artists") {
		t.Error("VA-led multi-value must stay VA-grouped")
	}
	// test_albumID_emptyAlbumArtist_notFragmented_unchanged
	if got := albumID("", "Mix", 2020); got != "|mix|2020" {
		t.Errorf("empty albumArtist: got %q, want |mix|2020", got)
	}
	if got := albumID("   ", "Mix", 2020); got != "|mix|2020" {
		t.Errorf("blank albumArtist: got %q, want |mix|2020", got)
	}
	// test_albumID_commaAndAmpersand_notSplit_noOverMerge
	if !strings.HasPrefix(albumID("Earth, Wind & Fire", "Gratitude", 1975), "earth, wind & fire|") {
		t.Error("Earth, Wind & Fire must never split")
	}
	if !strings.HasPrefix(albumID("Peter, Paul and Mary", "Around the Campfire", 1998), "peter, paul and mary|") {
		t.Error("Peter, Paul and Mary must never split")
	}
	// test_albumID_stripsEditionYearBracket / …ExplicitBracket / …preserves [Deluxe]
	if albumID("The Kelly Family", "Almost Heaven [1991]", 0) != albumID("The Kelly Family", "Almost Heaven", 0) {
		t.Error("[1991] edition bracket must strip")
	}
	if albumID("Some Artist", "Damn [Explicit]", 2017) != albumID("Some Artist", "Damn", 2017) {
		t.Error("[Explicit] bracket must strip")
	}
	if albumID("Some Artist", "Album [Deluxe]", 2020) == albumID("Some Artist", "Album", 2020) {
		t.Error("[Deluxe] must stay a distinct identity")
	}
}

func TestPrimaryAlbumArtistForGrouping_MirrorsSwiftPins(t *testing.T) {
	// test_primaryAlbumArtistForGrouping_directContract
	cases := []struct{ in, want string }{
		{"Alphaville; DFB", "Alphaville"},
		{"Alphaville", "Alphaville"},
		{"", ""},
		{"Various Artists", "Various Artists"},
		{"Soundtrack; John Williams", "Soundtrack; John Williams"},
		{"Earth, Wind & Fire", "Earth, Wind & Fire"},
	}
	for _, c := range cases {
		if got := primaryAlbumArtistForGrouping(c.in); got != c.want {
			t.Errorf("primaryAlbumArtistForGrouping(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSplitArtistDisplayName_MirrorsSwiftPins(t *testing.T) {
	eq := func(got, want []string) bool {
		if len(got) != len(want) {
			return false
		}
		for i := range got {
			if got[i] != want[i] {
				return false
			}
		}
		return true
	}
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil}, {"   ", nil}, {"\n\t", nil}, {"\n", nil}, // test_splitArtistDisplayName_emptyInput
		{"Abdullah Ibrahim", []string{"Abdullah Ibrahim"}},
		{"Abdullah Ibrahim; Ekaya", []string{"Abdullah Ibrahim", "Ekaya"}},
		{"A; B; C", []string{"A", "B", "C"}},
		{"  Abdullah Ibrahim ; Ekaya  ", []string{"Abdullah Ibrahim", "Ekaya"}},
		{"A; ; B", []string{"A", "B"}},                  // test_…_dropsEmptySegments
		{"A; ", []string{"A"}},                          //
		{"Bach;J.S.", []string{"Bach;J.S."}},            // test_…_doesNotSplitOnBareSemicolon
		{"Amy Grant; Amy Grant", []string{"Amy Grant"}}, // dedup
		{"Simon Goff; Katie Melua", []string{"Simon Goff", "Katie Melua"}},
		{"Amy Grant; AMY GRANT", []string{"Amy Grant"}},      // case-insensitive dedup, first casing
		{"AMY GRANT; amy grant", []string{"AMY GRANT"}},      //
		{"Beyoncé; Beyonce", []string{"Beyoncé", "Beyonce"}}, // diacritics stay distinct
	}
	for _, c := range cases {
		if got := splitArtistDisplayName(c.in); !eq(got, c.want) {
			t.Errorf("splitArtistDisplayName(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// --- KeyFor fallback-order pins (BridgeSyncActor.swift:693-724) ---

func TestKeyFor_EmptyAlbumArtistFallsBackToCleanedArtist(t *testing.T) {
	// 6.7% of the measured library has an empty albumArtist; the client
	// keys those on the CLEANED artist, so the mirror must too.
	withAA := Row{Path: "A/B/01 x.flac", Title: "x", Artist: "João Gilberto*", AlbumArtist: "João Gilberto", Album: "B"}
	without := Row{Path: "A/B/01 x.flac", Title: "x", Artist: "João Gilberto*", AlbumArtist: "", Album: "B"}
	if KeyFor(withAA) != KeyFor(without) {
		t.Fatalf("empty albumArtist must fall back to the cleaned artist:\n%+v\n%+v",
			KeyFor(withAA), KeyFor(without))
	}
}

func TestKeyFor_DiscFallsBackToFolderName(t *testing.T) {
	untagged := Row{Path: "Artist/Album/CD2/01 Song.flac", Title: "Song", Artist: "A", Album: "Album"}
	tagged := Row{Path: "Artist/Album/CD2/01 Song.flac", Title: "Song", Artist: "A", Album: "Album",
		Disc: 2, DiscTagged: true}
	if KeyFor(untagged) != KeyFor(tagged) {
		t.Fatalf("untagged disc must inherit the CD2 folder number")
	}
	if got := KeyFor(untagged).Disc; got != 2 {
		t.Fatalf("disc = %d, want 2", got)
	}
	// No disc folder → 1, never 0 (pathDefaults' floor).
	if got := KeyFor(Row{Path: "Artist/Album/01 Song.flac", Title: "Song"}).Disc; got != 1 {
		t.Fatalf("no-folder disc = %d, want 1", got)
	}
}

func TestKeyFor_TrackFallsBackToFilename_TaggedZeroIsAValue(t *testing.T) {
	untagged := Row{Path: "Artist/Album/07 Song.flac", Title: "Song"}
	if got := KeyFor(untagged).Track; got != 7 {
		t.Fatalf("untagged track = %d, want 7 (Swift bare-space rule)", got)
	}
	// An explicit Some(0) is a VALUE, not absence — the client uses it.
	zero := Row{Path: "Artist/Album/07 Song.flac", Title: "Song", Track: 0, TrackTagged: true}
	if got := KeyFor(zero).Track; got != 0 {
		t.Fatalf("tagged zero track = %d, want 0", got)
	}
}

func TestKeyFor_YearZeroEqualsAbsent(t *testing.T) {
	a := Row{Path: "A/B/01 x.flac", Title: "x", Album: "B", Artist: "A", Year: 0}
	b := Row{Path: "A/B/01 x.flac", Title: "x", Album: "B", Artist: "A", Year: -3}
	if KeyFor(a) != KeyFor(b) {
		t.Fatal("year 0 and negative must key identically (both absent)")
	}
}

func TestKeyFor_UntaggedFileDerivesEverythingFromPath(t *testing.T) {
	r := Row{Path: "Pink Floyd/The Wall/CD1/01 - In the Flesh.flac"}
	k := KeyFor(r)
	if k.Disc != 1 || k.Track != 1 {
		t.Fatalf("disc/track = %d/%d, want 1/1", k.Disc, k.Track)
	}
	if k.NormTitle != "01 - in the flesh" {
		// Fallback title is cleanDisplayName(basename-no-ext), which does
		// NOT strip track prefixes (that's cleanTrackTitle, which the sync
		// actor never applies) — then normalize lowercases it.
		t.Fatalf("normTitle = %q", k.NormTitle)
	}
	if !strings.HasPrefix(k.AlbumID, "pink floyd|the wall|") {
		t.Fatalf("albumID = %q", k.AlbumID)
	}
}

// --- Anti-unification tripwires ---

// TestClientKeyIsNotFoldForMatch pins a case where this mirror MUST
// differ from internal/enrich's foldForMatch: the enrichment fold
// NFKD-decomposes and strips diacritics ("Zdob și Zdub" → "zdob si
// zdub"), while the client's normalize keeps them. Unifying the two
// normalisers would silently change group membership.
func TestClientKeyIsNotFoldForMatch(t *testing.T) {
	if got := normalize("Zdob și Zdub"); got != "zdob și zdub" {
		t.Fatalf("normalize must PRESERVE diacritics (got %q) — it is not foldForMatch", got)
	}
}

// TestClientKeyIsNotReconcileNormTitle pins the difference from
// internal/manifest reconcile.go's normTitle (ToLower+TrimSpace, NO
// whitespace collapse): the client key collapses interior runs.
func TestClientKeyIsNotReconcileNormTitle(t *testing.T) {
	if got := normalize("a  b"); got != "a b" {
		t.Fatalf("normalize must collapse interior whitespace (got %q) — it is not reconcile's normTitle", got)
	}
}

// TestGroupKeyKeepsDiscAndTrack pins the two load-bearing key terms.
// Measured on the live 19,720-track library: WITHOUT disc+track the
// version-token false-positive rate is 19.3% (an acoustic session pairs
// with the album cut, a radio edit with the full version, the mono and
// stereo Pet Sounds mixes collapse); WITH them it is 2.2%. Dropping
// either term silently re-inflates the report and the suppression set.
func TestGroupKeyKeepsDiscAndTrack(t *testing.T) {
	base := Row{Path: "A/B/x.flac", Title: "Song", Album: "B", Artist: "A", Track: 1, TrackTagged: true, Disc: 1, DiscTagged: true}
	otherTrack := base
	otherTrack.Track = 2
	otherDisc := base
	otherDisc.Disc = 2
	if KeyFor(base) == KeyFor(otherTrack) {
		t.Fatal("track number must be part of the key")
	}
	if KeyFor(base) == KeyFor(otherDisc) {
		t.Fatal("disc number must be part of the key")
	}
}
