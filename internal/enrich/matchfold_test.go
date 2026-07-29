package enrich

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestFoldForMatchPinsTheOrderedPipeline covers each stage of the fold
// independently, so a reordering shows up as a specific failure rather
// than a vague one.
func TestFoldForMatchPinsTheOrderedPipeline(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		// NFKD compatibility decomposition.
		{"ligature", "ﬁnale", "finale"},
		{"fullwidth", "ＡＢＣ", "abc"},
		{"nbsp", "Kind of Blue", "kind of blue"},
		{"numero", "№ 5", "no 5"},
		// Accent stripping (via NFKD + Mn removal).
		{"diaeresis", "Yael Naïm", "yael naim"},
		{"comma-below", "Zdob și Zdub", "zdob si zdub"},
		{"acute", "José González", "jose gonzalez"},
		// DOCUMENTED GAP, not an oversight: ø / ł / đ / æ are atomic
		// letters, not base+combining, so NFKD leaves them intact and the
		// Mn filter never sees them. `Bjornstad` therefore does NOT fold
		// equal to `Bjørnstad`, while `Zdob si Zdub` DOES equal
		// `Zdob și Zdub` (ș decomposes). See TestFoldLeavesStrokeLetters.
		{"nordic-o survives", "Ketil Bjørnstad", "ketil bjørnstad"},
		// Case folding — not ToLower.
		{"eszett", "Straße", "strasse"},
		{"turkish-dotted-I", "İSTANBUL", "istanbul"},
		// Apostrophe family: DELETED, never spaced.
		{"curly apostrophe", "What’s Up?", "whats up"},
		{"ascii apostrophe", "What's Up?", "whats up"},
		{"missing apostrophe", "Whats Up", "whats up"},
		{"quotes", "“Heroes”", "heroes"},
		// Dash family: SPACE, never deleted.
		{"en dash", "Songs 2003–2013", "songs 2003 2013"},
		{"hyphen", "Songs 2003-2013", "songs 2003 2013"},
		{"em dash", "Live — Berlin", "live berlin"},
		// Colon / slash: SPACE.
		{"colon", "II: Yo te voy a amar", "ii yo te voy a amar"},
		{"colon no space", "II:Yo", "ii yo"},
		{"slash", "Poor Boy / Lucky Man", "poor boy lucky man"},
		// Brackets: deleted, inner words KEPT.
		{"parens", "Gold (anniversary edition)", "gold anniversary edition"},
		{"square", "[A] What's Up-", "a whats up"},
		// Ampersand: space-padded " and ".
		{"tight ampersand", "R&B", "r and b"},
		{"spaced ampersand", "R & B", "r and b"},
		{"spelled and", "R and B", "r and b"},
		{"name ampersand", "Simon & Garfunkel", "simon and garfunkel"},
		// Whitespace collapse.
		{"runs", "  The   Wall  ", "the wall"},
		{"empty", "", ""},
		{"punctuation only", "!!!", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := foldTitle(tc.in); got != tc.want {
				t.Errorf("foldTitle(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestFoldForMatchIsIdempotent — folding a folded string must be a no-op,
// or any caller that folds twice silently gets a different answer.
func TestFoldForMatchIsIdempotent(t *testing.T) {
	for _, in := range []string{
		"What’s Up?", "Songs 2003–2013", "R&B", "Gold (anniversary edition)",
		"Zdob și Zdub", "İSTANBUL", "Straße", "  spaced  out  ", "", "!!!",
		"Peter, Paul & Mary", "II: Yo te voy a amar", "ﬁnale",
	} {
		once := foldTitle(in)
		twice := foldTitle(once)
		if once != twice {
			t.Errorf("foldTitle not idempotent for %q: %q -> %q", in, once, twice)
		}
	}
}

// TestFoldForMatchRecoversTheMeasuredCases is the regression lock for the
// actual production strings. Each pair was observed being rejected while
// MusicBrainz scored it 96–100.
func TestFoldForMatchRecoversTheMeasuredCases(t *testing.T) {
	for _, tc := range []struct{ local, mb string }{
		{"What's Up?", "What’s Up?"},
		{"Australia's Favourite Nursery Rhymes", "Australia’s Favourite Nursery Rhymes"},
		{"Ain't Nobody Worryin'", "Ain’t Nobody Worryin’"},
		{"Don't Explain", "Don’t Explain"},
		{"I'll Get By", "I’ll Get By"},
		{"Songs 2003-2013", "Songs 2003–2013"},
		{"II - Yo Te Voy A Amar", "II: Yo te voy a amar"},
		{"Eternally Yours Bonus-EP I", "Eternally Yours, Bonus-EP I"},
		{"Zdob si Zdub", "Zdob și Zdub"},
		{"Yael Naim", "Yael Naïm"},
	} {
		if a, b := foldTitle(tc.local), foldTitle(tc.mb); a != b {
			t.Errorf("%q and %q must fold equal, got %q vs %q", tc.local, tc.mb, a, b)
		}
	}
	// Superset relations — the local tag is LONGER than the canonical.
	for _, tc := range []struct{ local, mb string }{
		{"Abba Gold Anniversary Edition", "Gold (anniversary edition)"},
		{"Thriller 25th Anniversary Edition", "Thriller"},
		{"Carnegie Hall Concert - June 18, 1971", "The Carnegie Hall Concert: June 18, 1971"},
	} {
		if !foldedTokenContains(foldTitle(tc.local), foldTitle(tc.mb)) {
			t.Errorf("%q must token-contain %q (folded: %q vs %q)",
				tc.local, tc.mb, foldTitle(tc.local), foldTitle(tc.mb))
		}
	}
}

// TestFoldForMatchDoesNotOverMatch is the other half — the pairs that
// must stay DISTINCT.
func TestFoldForMatchDoesNotOverMatch(t *testing.T) {
	// Deleting dashes instead of spacing them would collapse these two
	// genuinely different Metallica albums.
	if foldTitle("Re-Load") == foldTitle("Reload") {
		t.Error("Re-Load and Reload must not fold equal — deleting dashes instead of spacing them")
	}
	for _, tc := range []struct {
		name, hay, needle string
	}{
		{"substring not token", "Goldfinger", "Gold"},
		{"prefix not token", "Ariadne auf Naxos", "Aria"},
		{"suffix not token", "Download", "Load"},
		{"inside word", "Believe", "Live"},
		{"short title", "Go West", "Go"},
		{"short title 2", "Let Us Pray", "Us"},
		{"short roman", "IV Symphonies", "IV"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if foldedTokenContains(foldTitle(tc.hay), foldTitle(tc.needle)) {
				t.Errorf("%q must NOT token-contain %q", tc.hay, tc.needle)
			}
		})
	}
	// An empty needle would otherwise make Contains trivially true and
	// accept every candidate.
	if foldedTokenContains("some album", "") {
		t.Error("an empty needle must never match")
	}
	if foldedTokenContains("", "some album") {
		t.Error("an empty haystack must never match")
	}
}

// TestFoldedTokenContainsIsSymmetric — the direction of the arguments
// must not change the verdict. The production containment has always been
// symmetric; the fold must not quietly make it directional.
func TestFoldedTokenContainsIsSymmetric(t *testing.T) {
	for _, tc := range [][2]string{
		{"abba gold anniversary edition", "gold anniversary edition"},
		{"thriller", "thriller 25th anniversary edition"},
		{"goldfinger", "gold"},
		{"go west", "go"},
	} {
		a := foldedTokenContains(tc[0], tc[1])
		b := foldedTokenContains(tc[1], tc[0])
		if a != b {
			t.Errorf("asymmetric verdict for %q / %q: %v vs %v", tc[0], tc[1], a, b)
		}
	}
}

// TestShortTitleRuleIsLengthOnly guards against reintroducing the
// "or a single token" clause.
//
// An earlier draft had one. It is a real bug: canonical release-group
// titles are very often a single token, and local tags routinely hang an
// edition suffix off them, so the clause rejects exactly the superset
// class this change exists to fix.
func TestShortTitleRuleIsLengthOnly(t *testing.T) {
	singleTokenCanonicals := []struct{ local, canonical string }{
		{"Thriller 25th Anniversary Edition", "Thriller"},
		{"Nevermind Deluxe Edition", "Nevermind"},
		{"Rumours 35th Anniversary", "Rumours"},
		{"Unplugged Live Edition", "Unplugged"},
		{"Animals 2018 Remix", "Animals"},
	}
	for _, tc := range singleTokenCanonicals {
		if !foldedTokenContains(foldTitle(tc.local), foldTitle(tc.canonical)) {
			t.Errorf("%q must match single-token canonical %q — the short-title rule "+
				"must be LENGTH-only, never 'or a single token'", tc.local, tc.canonical)
		}
	}
	// And the length rule still bites where it should.
	if foldedTokenContains(foldTitle("Go West"), foldTitle("Go")) {
		t.Error("a <=3-rune title must require exact equality")
	}
	// Measured in runes, not bytes: a 3-character Cyrillic title is 6
	// bytes and must take the same strict path as a 3-byte ASCII one.
	if foldedTokenContains(foldTitle("Мир во всем мире"), foldTitle("Мир")) {
		t.Error("short-title rule must measure runes, not bytes")
	}
}

// TestFoldLeavesStrokeLetters documents a deliberate limitation so a
// future reader doesn't mistake it for a bug — or "fix" it without
// evidence.
//
// Accent stripping works by NFKD-decomposing a letter into base +
// combining mark and dropping the mark. Letters with a STROKE or BAR
// (ø ł đ æ ð þ œ) are atomic code points with no decomposition, so they
// survive. The fold is therefore inconsistent by construction: it
// equates `si`/`și` but not `Bjornstad`/`Bjørnstad`.
//
// Measured before accepting this: 8 of the 300 unresolved artists on the
// production library carry such a letter, and in every one of them the
// letter is present on BOTH sides (`Ketil Bjørnstad, Composer` vs MB's
// `Ketil Bjørnstad`). They fail on the role suffix, not the letter — so a
// stroke-folding table would recover nothing here while adding unmeasured
// collision surface. Revisit only with numbers.
func TestFoldLeavesStrokeLetters(t *testing.T) {
	for _, tc := range []struct{ a, b string }{
		{"Ketil Bjørnstad", "Ketil Bjornstad"},
		{"Susanne Sundfør", "Susanne Sundfor"},
	} {
		if foldName(tc.a) == foldName(tc.b) {
			t.Errorf("%q and %q now fold equal — stroke folding was added. "+
				"That may be an improvement, but it is UNMEASURED: update this "+
				"test deliberately and record the evidence.", tc.a, tc.b)
		}
	}
	// The decomposable counterpart still works, which is the contrast.
	if foldName("Zdob si Zdub") != foldName("Zdob și Zdub") {
		t.Error("decomposable diacritics must still fold equal")
	}
}

// TestFoldNameStripsArticlesOnlySafely pins the artist-side article rule.
func TestFoldNameStripsArticlesOnlySafely(t *testing.T) {
	for _, tc := range []struct{ name, in, plain, noArticle string }{
		{"the carpenters", "The Carpenters", "the carpenters", "carpenters"},
		{"bare carpenters", "Carpenters", "carpenters", "carpenters"},
		{"the band", "The Band", "the band", "band"},
		{"an artist", "An Emotional Fish", "an emotional fish", "emotional fish"},
		// The collision guard: stripping would leave a bare article.
		{"the the", "The The", "the the", "the the"},
		{"bare the", "The", "the", "the"},
		{"a", "A", "a", "a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := foldName(tc.in); got != tc.plain {
				t.Errorf("foldName(%q) = %q, want %q", tc.in, got, tc.plain)
			}
			if got := foldNameNoArticle(tc.in); got != tc.noArticle {
				t.Errorf("foldNameNoArticle(%q) = %q, want %q", tc.in, got, tc.noArticle)
			}
		})
	}
	// The Carpenters case must actually resolve: this is 81 tracks.
	if foldNameNoArticle("The Carpenters") != foldNameNoArticle("Carpenters") {
		t.Error("The Carpenters must fold-equal Carpenters after article stripping")
	}
	// And The The must NOT collapse onto an artist literally named "The".
	if foldNameNoArticle("The The") == foldNameNoArticle("The") {
		t.Error("The The must not collapse onto The")
	}
}

// TestFoldForMatchIsNotTheArtistImageCacheKey is a tripwire, not a
// behaviour test.
//
// ArtistImagePathByName hashes the artist name into a FILENAME. If a
// future refactor unifies it with foldForMatch, every cached artist
// portrait is orphaned and silently re-fetched from Deezer — a failure
// mode invisible in CI and expensive in production. This test fails
// loudly instead, and the reader lands on the do-not-unify table in
// matchfold.go.
func TestFoldForMatchIsNotTheArtistImageCacheKey(t *testing.T) {
	dir := t.TempDir()
	a := ArtistImagePathByName(dir, "Yael Naïm")
	b := ArtistImagePathByName(dir, "Yael Naim")
	if a == b {
		t.Fatalf("ArtistImagePathByName has been unified with the match fold — "+
			"accent-different names now hash to the same file (%s). "+
			"See the do-not-unify table in matchfold.go: this orphans every "+
			"cached portrait on deploy.", filepath.Base(a))
	}
	// Meanwhile the MATCH fold does treat them as the same artist, which
	// is the whole point of keeping the two separate.
	if foldName("Yael Naïm") != foldName("Yael Naim") {
		t.Error("the match fold should consider these the same artist")
	}
}

// TestFoldForMatchNeverPanicsOnInvalidUTF8 mirrors the guard on the SQL
// scalar in internal/manifest/sqlfunc.go — tag data is arbitrary bytes.
func TestFoldForMatchNeverPanicsOnInvalidUTF8(t *testing.T) {
	for _, in := range []string{
		"\xff\xfe invalid",
		string([]byte{0xC3, 0x28}),
		"\x00null",
		strings.Repeat("\xed\xa0\x80", 4), // lone surrogates
	} {
		_ = foldTitle(in)
		_ = foldName(in)
		_ = foldNameNoArticle(in)
	}
}
