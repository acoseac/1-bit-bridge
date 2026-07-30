package enrich

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestStripUnbracketedEditionSuffix(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		// Real production tags.
		{"Abba Gold Anniversary Edition", "Abba Gold"},
		{"Heavy Flowers 10th Anniversary", "Heavy Flowers"},
		{"The Best of Chicago, 40th Anniversary Edition", "The Best of Chicago"},
		{"Some Album Deluxe Edition", "Some Album"},
		{"Some Album - Remastered", "Some Album"},
		{"Some Album 2011 Remaster", "Some Album 2011"},
		{"Album Limited Edition", "Album"},
		{"Album: Japanese Version", "Album"},
		// Must NOT strip — the qualifier IS the title.
		{"Anniversary", ""},
		{"Deluxe", ""},
		{"Remastered", ""},
		// Must NOT strip — no qualifier at all.
		{"Dark Side of the Moon", ""},
		{"The Complete Warner Years", ""},
		{"The Singles", ""},
		{"Songs 2003-2013", ""},
		{"", ""},
		// Bracketed forms are the OTHER stripper's job; this one must
		// leave them alone so the two don't both fire on one title.
		{"Goats Head Soup (2020 Deluxe)", ""},
		{"Album [Deluxe Edition]", ""},
	} {
		t.Run(tc.in, func(t *testing.T) {
			if got := stripUnbracketedEditionSuffix(tc.in); got != tc.want {
				t.Errorf("stripUnbracketedEditionSuffix(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestStripArtistPrefix(t *testing.T) {
	for _, tc := range []struct {
		name, album string
		artists     []string
		want        string
		wantMatched string
	}{
		{"beatles", "The Beatles 1962 – 1966", []string{"The Beatles"}, "1962 – 1966", "The Beatles"},
		{"bon jovi", "Bon Jovi Greatest Hits", []string{"Bon Jovi"}, "Greatest Hits", "Bon Jovi"},
		{"case differs", "CAROLE KING Music", []string{"Carole King"}, "Music", "Carole King"},
		{"via albumArtist", "Queen Greatest Hits", []string{"", "Queen"}, "Greatest Hits", "Queen"},
		{"article on artist", "Carpenters Singles 1969-1981", []string{"The Carpenters"}, "Singles 1969-1981", "The Carpenters"},
		// The split-credit case: the prefix belongs to the albumArtist, so
		// that is who the caller must query with — querying "John Lennon"
		// for a Beatles release fails pickBestRelease's credit check.
		{"split credit", "The Beatles 1962 – 1966", []string{"John Lennon", "The Beatles"}, "1962 – 1966", "The Beatles"},
		// Self-titled: stripping would empty the title.
		{"self titled", "Weezer", []string{"Weezer"}, "", ""},
		{"exactly the artist", "Bon Jovi", []string{"Bon Jovi"}, "", ""},
		// No prefix relation at all.
		{"unrelated", "Dark Side of the Moon", []string{"Pink Floyd"}, "", ""},
		{"no artists", "Some Album", nil, "", ""},
		{"empty album", "", []string{"Someone"}, "", ""},
		// Longest prefix wins: don't stop at "Peter".
		{"longest wins", "Peter Gabriel So", []string{"Peter Gabriel"}, "So", "Peter Gabriel"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, matched := stripArtistPrefix(tc.album, tc.artists)
			if got != tc.want {
				t.Errorf("stripArtistPrefix(%q, %v) = %q, want %q", tc.album, tc.artists, got, tc.want)
			}
			if matched != tc.wantMatched {
				t.Errorf("stripArtistPrefix(%q, %v) matched %q, want %q — the caller "+
					"queries with this, and the wrong one fails the credit check",
					tc.album, tc.artists, matched, tc.wantMatched)
			}
		})
	}
}

// TestBuildReleaseLadderQueriesTheStrippedArtist pins the split-credit
// case end-to-end: track artist "John Lennon", albumArtist "The Beatles",
// album "The Beatles 1962 – 1966". The prefix is found via the
// albumArtist, so a rung querying "1962 – 1966" must carry "The Beatles"
// — with "John Lennon" it would fail pickBestRelease's artist gate and
// the rung would be a wasted request.
func TestBuildReleaseLadderQueriesTheStrippedArtist(t *testing.T) {
	got := buildReleaseLadder("John Lennon", "The Beatles", "The Beatles 1962 – 1966")
	if !ladderHas(got, "The Beatles", "1962 – 1966") {
		t.Fatalf("ladder must query the stripped title with the artist it was "+
			"stripped from: %v", got)
	}
}

// TestBuildReleaseLadderTriesAlbumArtistOnTheUnbracketedRung — a
// compilation whose title carries an unbracketed edition suffix needs the
// albumArtist for the same reason the bracketed rung always has.
func TestBuildReleaseLadderTriesAlbumArtistOnTheUnbracketedRung(t *testing.T) {
	got := buildReleaseLadder("Bon Jovi", "Various Artists", "All Time Rock Ballads Deluxe Edition")
	if !ladderHas(got, "Various Artists", "All Time Rock Ballads") {
		t.Fatalf("unbracketed rung must also try the albumArtist: %v", got)
	}
}

// TestSplitHeadCreditNeverSplitsOnAmpersandOrBareComma is the single most
// important test in this file.
//
// Splitting "Peter, Paul & Mary" on '&' yields "Peter, Paul", which
// matches an unrelated MusicBrainz artist "Peter Paul" at score 100 — 186
// tracks would take a wrong MBID. And because foldName erases commas,
// fold("Peter, Paul") == fold("Peter Paul"), so the ACCEPTANCE layer
// cannot catch it either. Never generating the query is the only defence.
func TestSplitHeadCreditNeverSplitsOnAmpersandOrBareComma(t *testing.T) {
	for _, name := range []string{
		"Peter, Paul & Mary",
		"Simon & Garfunkel",
		"Alison Krauss & Union Station",
		"Earth, Wind & Fire",
		"Crosby, Stills & Nash",
		"Emerson, Lake & Palmer",
		"Jamie MacDougall & Haydn Eisenstadt Trio",
		"Hall & Oates",
		// English words that appear INSIDE band names. Splitting these
		// yields "Sleeping" / "Running" / "Girls" / "Us", every one of
		// which is a plausible real artist on MusicBrainz — and
		// pickBestArtist validates against the QUERY that was sent, not
		// the original tag, so an exact match for the truncated name is
		// accepted as correct.
		"Sleeping with Sirens",
		"Running with Scissors",
		"Girls with Guitars",
		"Dancing with Wolves",
		"Us vs Them",
		"Spy vs. Spy",
	} {
		if got := splitHeadCredit(name); got != "" {
			t.Errorf("splitHeadCredit(%q) = %q — a separator must be an UNAMBIGUOUS "+
				"credit delimiter. '&', bare commas, ' with ' and ' vs ' all appear "+
				"inside real artist names; this is the 186-track wrong-MBID class",
				name, got)
		}
	}
}

func TestSplitHeadCredit(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"Ennio Morricone; Solisti e Orchestre del Cinema Italiano", "Ennio Morricone"},
		{"Emmylou Harris; Mark Knopfler", "Emmylou Harris"},
		{"Paul Simon; Art Garfunkel", "Paul Simon"},
		{"Asaf Avidan / The Mojos", "Asaf Avidan"},
		{"Someone feat. Another", "Someone"},
		{"Someone ft. Another", "Someone"},
		{"Someone featuring Another", "Someone"},
		// ' vs ' and ' with ' are NOT separators — they appear inside real
		// band names, and the cost of getting that wrong is a confidently
		// wrong MBID. A genuine "A vs B" collaboration simply falls back
		// to the other rungs.
		{"Beyoncé vs. Someone", ""},
		{"Someone with Another", ""},
		// AC/DC must survive — the slash rung is whitespace-delimited.
		{"AC/DC", ""},
		{"AC/DC; Someone", "AC/DC"},
		// Single credit — nothing to split.
		{"Metallica", ""},
		{"", ""},
		// A leading separator must not yield an empty head.
		{"; Someone", ""},
	} {
		t.Run(tc.in, func(t *testing.T) {
			if got := splitHeadCredit(tc.in); got != tc.want {
				t.Errorf("splitHeadCredit(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestSplitHeadCreditSurvivesRunesToLowerWouldShorten covers the offset bug.
//
// `cut` is found in the lowercased string and used to slice the ORIGINAL, so
// the two have to agree byte for byte. strings.ToLower does not preserve
// length: Go's case table shortens İ (U+0130, 2 bytes → 1), U+212A KELVIN
// (3 → 1) and ẞ (U+1E9E, 3 → 2). Each such rune before the separator drags the
// cut left.
//
// The UTF-8 assertion is the half that matters most, and the half a want-string
// comparison alone would miss: a cut landing mid-sequence produces a slice that
// is not valid UTF-8, and that is what reaches the network.
func TestSplitHeadCreditSurvivesRunesToLowerWouldShorten(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"turkish dotted I", "İbrahim Tatlıses feat. Sezen Aksu", "İbrahim Tatlıses"},
		{"two of them", "İsmail İpek; Someone", "İsmail İpek"},
		{"kelvin sign", "KKeith; Someone", "KKeith"},
		{"capital eszett", "Straẞe; Someone", "Straẞe"},
		{"multibyte straddling the shifted offset", "İé; Someone", "İé"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := splitHeadCredit(tc.in)
			if !utf8.ValidString(got) {
				t.Fatalf("splitHeadCredit(%q) = %q — not valid UTF-8; the cut landed "+
					"mid-rune, and this string goes into the query URL", tc.in, got)
			}
			if got != tc.want {
				t.Errorf("splitHeadCredit(%q) = %q, want %q — an offset found in the "+
					"lowercased string was used to slice the original", tc.in, got, tc.want)
			}
		})
	}
}

// TestLowerASCIIIsByteLengthPreserving pins the property splitHeadCredit
// depends on, directly — including for the runes that break ToLower.
func TestLowerASCIIIsByteLengthPreserving(t *testing.T) {
	for _, s := range []string{
		"", "Metallica", "ALL CAPS", "already lower",
		"İbrahim", "KKelvin", "Straẞe", "Zdob și Zdub", "日本",
		"Peter, Paul & Mary; Someone",
	} {
		got := lowerASCII(s)
		if len(got) != len(s) {
			t.Errorf("lowerASCII(%q) changed length %d -> %d", s, len(s), len(got))
		}
		if !utf8.ValidString(got) {
			t.Errorf("lowerASCII(%q) = %q is not valid UTF-8", s, got)
		}
		// Same answer as ToLower wherever ToLower is length-preserving, which
		// is the only place splitHeadCredit's separators can match anyway.
		if lower := strings.ToLower(s); len(lower) == len(s) && got != lower {
			t.Errorf("lowerASCII(%q) = %q, ToLower = %q", s, got, lower)
		}
	}
}

func TestTruncateAtFirstRole(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		// Real production tags.
		{"ABDULLAH IBRAHIM, Composer", "ABDULLAH IBRAHIM"},
		{"Madeleine Peyroux, Guitar, Vocalist", "Madeleine Peyroux"},
		{"Rachel Podger, Conductor, Brecon Baroque, Ensemble", "Rachel Podger"},
		{"Damien Jurado, Artist", "Damien Jurado"},
		{"Ketil Bjørnstad, Composer", "Ketil Bjørnstad"},
		{"Susanne Sundfør, Composer, Lyricist, Producer, Artist", "Susanne Sundfør"},
		// MUST NOT fire — the segment after the comma is not a bare role.
		// These are exactly the names a "cut at the first comma" rule
		// would destroy.
		{"Peter, Paul & Mary", ""},
		{"Crosby, Stills & Nash", ""},
		{"Earth, Wind & Fire", ""},
		{"Emerson, Lake & Palmer", ""},
		{"Zubin Mehta,London Philharmonic Orchestra", ""},
		{"Metallica", ""},
		{"", ""},
		// A leading role would empty the name.
		{"Composer, Someone", ""},
	} {
		t.Run(tc.in, func(t *testing.T) {
			if got := truncateAtFirstRole(tc.in); got != tc.want {
				t.Errorf("truncateAtFirstRole(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestBuildReleaseLadderIsAdditiveAndCapped pins that rungs 1-4 are
// byte-identical to what shipped before, so no album that resolves today
// can change its answer by taking a different rung.
func TestBuildReleaseLadderIsAdditiveAndCapped(t *testing.T) {
	got := buildReleaseLadder("Artist", "AlbumArtist", "Album (Deluxe)")
	want := []releaseAttempt{
		{"Artist", "Album (Deluxe)"},
		{"AlbumArtist", "Album (Deluxe)"},
		{"Artist", "Album"},
		{"AlbumArtist", "Album"},
	}
	if len(got) != len(want) {
		t.Fatalf("ladder = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("rung %d = %v, want %v (order must stay additive)", i, got[i], want[i])
		}
	}

	// The new rungs appear only when their generator fires.
	unbr := buildReleaseLadder("Abba", "", "Abba Gold Anniversary Edition")
	if !ladderHas(unbr, "Abba", "Abba Gold") {
		t.Errorf("unbracketed-suffix rung missing: %v", unbr)
	}
	pref := buildReleaseLadder("The Beatles", "", "The Beatles 1962 – 1966")
	if !ladderHas(pref, "The Beatles", "1962 – 1966") {
		t.Errorf("artist-prefix rung missing: %v", pref)
	}

	// Cap holds even when every generator fires.
	big := buildReleaseLadder("Some Artist", "Another Artist",
		"Some Artist Big Album Name (Deluxe Edition) Anniversary Edition")
	if len(big) > maxReleaseAttempts {
		t.Fatalf("ladder has %d rungs, cap is %d", len(big), maxReleaseAttempts)
	}
}

// TestBuildReleaseLadderDedupsFoldedDuplicates — a rung that folds the
// same as an earlier one is a wasted upstream request.
func TestBuildReleaseLadderDedupsFoldedDuplicates(t *testing.T) {
	// albumArtist differs only by accent → must not add a rung.
	got := buildReleaseLadder("Yael Naim", "Yael Naïm", "Album")
	if len(got) != 1 {
		t.Errorf("ladder = %v, want a single rung (albumArtist folds the same)", got)
	}
	// Exact duplicate strings likewise.
	got2 := buildReleaseLadder("Artist", "Artist", "Album")
	if len(got2) != 1 {
		t.Errorf("ladder = %v, want a single rung", got2)
	}
}

func TestBuildArtistLadder(t *testing.T) {
	for _, tc := range []struct {
		name, artist, albumArtist string
		want                      []string
	}{
		{
			"head credit then role",
			"ABDULLAH IBRAHIM, Composer feat. Noah Jackson, Soloist", "",
			[]string{
				"ABDULLAH IBRAHIM, Composer feat. Noah Jackson, Soloist",
				"ABDULLAH IBRAHIM, Composer",
				"ABDULLAH IBRAHIM",
			},
		},
		{
			"semicolon credit",
			"Ennio Morricone; Solisti e Orchestre del Cinema Italiano", "",
			[]string{
				"Ennio Morricone; Solisti e Orchestre del Cinema Italiano",
				"Ennio Morricone",
			},
		},
		{
			"role only",
			"Madeleine Peyroux, Guitar, Vocalist", "",
			[]string{"Madeleine Peyroux, Guitar, Vocalist", "Madeleine Peyroux"},
		},
		{
			"ampersand name stays whole",
			"Peter, Paul & Mary", "",
			[]string{"Peter, Paul & Mary"},
		},
		// The rung MusicBrainz cannot answer is dropped; the one it can
		// survives. This case used to expect BOTH — which read as correct
		// and was, for this function, while the guard one level up in
		// resolveArtist meant production never reached either of them.
		{
			"unanswerable head rung dropped, albumArtist survives",
			"CD 01", "Abdullah Ibrahim",
			[]string{"Abdullah Ibrahim"},
		},
		{
			"every rung unanswerable yields no ladder",
			"CD 01", "Disc 2",
			nil,
		},
		{
			"albumArtist folds the same",
			"Metallica", "metallica",
			[]string{"Metallica"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := buildArtistLadder(tc.artist, tc.albumArtist)
			if len(got) != len(tc.want) {
				t.Fatalf("ladder = %v, want %v", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("rung %d = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestBuildArtistLadderIsCapped(t *testing.T) {
	got := buildArtistLadder(
		"One, Composer; Two, Soloist feat. Three, Vocalist / Four", "Five")
	if len(got) > maxArtistAttempts {
		t.Fatalf("ladder has %d rungs, cap is %d: %v", len(got), maxArtistAttempts, got)
	}
	// The first rung is always the tag verbatim.
	if !strings.HasPrefix(got[0], "One, Composer") {
		t.Errorf("first rung must be the tag verbatim, got %q", got[0])
	}
}

func ladderHas(l []releaseAttempt, artist, album string) bool {
	for _, a := range l {
		if a.artist == artist && a.album == album {
			return true
		}
	}
	return false
}
