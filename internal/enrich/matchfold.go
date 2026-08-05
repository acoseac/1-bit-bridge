// Unicode folding for MATCH-TIME COMPARISON ONLY.
//
// The problem this solves: MusicBrainz hands back the right answer and the
// bridge throws it away because two strings that a human reads as identical
// differ by a byte. Measured against the production library, all of these
// were rejected while scoring 96–100:
//
//	local tag                        MusicBrainz               why
//	What's Up?                       What’s Up?                U+2019 vs '
//	Australia's Favourite …          Australia’s Favourite …   U+2019 vs '
//	Songs 2003-2013                  Songs 2003–2013           en dash vs -
//	II - Yo Te Voy A Amar            II: Yo te voy a amar       - vs :
//	Abba Gold Anniversary Edition    Gold (anniversary edition) parentheses
//	Zdob si Zdub                     Zdob și Zdub               U+0219
//	Yael Naim                        Yael Naïm                  U+00EF
//
// NOTE the containment in pickBestRelease was ALREADY symmetric
// (`Contains(a,b) || Contains(b,a)`) before this change — every failure
// above is byte-literalness, not direction. Folding is the whole fix.
//
// ─────────────────────────────────────────────────────────────────────
// DO NOT UNIFY this with the three sibling normalisers. They look like
// duplication and are not — each one's output is load-bearing somewhere
// this one's is not:
//
//	unicodeLowerScalar      internal/manifest/sqlfunc.go
//	  A deterministic SQL scalar backing three functional indexes.
//	  Changing its output needs a migration; v26 already rebuilt them once.
//
//	ArtistImagePathByName   internal/enrich/enricher.go
//	  Its output is a SHA-256 that NAMES FILES ON DISK. Unifying orphans
//	  every cached artist portrait — a silent mass re-fetch from Deezer
//	  that no test would catch. TestFoldForMatchIsNotTheArtistImageCacheKey
//	  is the tripwire.
//
//	normTitle               internal/manifest/reconcile.go
//	  Deliberately weak (ToLower+TrimSpace). It groups albums for a pass
//	  that REWRITES TAGS; over-folding there merges distinct albums.
//
//	clientkey normalize     internal/dupes/clientkey.go
//	  A VERBATIM MIRROR of the iOS MetadataNormalizer, so its output must
//	  equal the client's byte-for-byte — it may never be "improved". It
//	  deliberately KEEPS diacritics that this fold strips ("Zdob și Zdub"
//	  stays și there); TestClientKeyIsNotFoldForMatch is the tripwire.
//
// The output of foldTitle/foldName must never be persisted, hashed into a
// filename, used as a cache key, or embedded in SQL.
package enrich

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// matchFoldCaser is constructed once at package level. cases.Fold()
// returns a transformer; building one per comparison would allocate on
// the enricher's hot path. Same rationale as the shared caser in
// enricher.go.
var matchFoldCaser = cases.Fold()

// shortTitleExactRunes is the length at or below which folded containment
// is downgraded to folded EQUALITY.
//
// It blocks `Go` ⊂ `Go West`, `Us` ⊂ `Let Us Pray`, `IV` ⊂ `IV Symphonies`.
//
// LENGTH ONLY — there is deliberately no "or a single token" clause. An
// earlier draft had one and it is a real bug: canonical release-group
// titles are very often a single token (Thriller, Gold, Nevermind,
// Rumours, Unplugged, Animals) and local tags routinely hang an edition
// suffix off them, so `Thriller 25th Anniversary Edition` vs `Thriller`
// would be rejected — the exact superset class this change exists to fix.
//
// Measured in RUNES, not bytes: a 3-character Cyrillic or CJK title is
// 6–9 bytes and must not silently take the strict path.
//
// Against Atlas this rule almost never fires, because Atlas's score IS
// `int(pg_trgm_similarity*100)+bonus` and trigram similarity dilutes with
// length mismatch — a 4-trigram query against a 22-trigram title scores
// ~18 and dies at the score floor first. It is here for the public
// MusicBrainz configuration, whose Lucene relevance is not length-aware
// the same way.
const shortTitleExactRunes = 3

// foldTitle folds an album/release title for comparison.
//
// Pipeline order is load-bearing and pinned by
// TestFoldForMatchPinsTheOrderedPipeline.
func foldTitle(s string) string { return foldForMatch(s, false) }

// foldName folds an artist name for comparison. Identical to foldTitle
// today — the article strip lives in foldNameNoArticle, not here.
//
// Kept as a separate name because the CALL SITES differ in a way that
// must never converge: a title can never be article-stripped (`The Wall`
// must not fold to `Wall`), so the title path can never be handed the
// flag, while the name path legitimately has both forms. One function
// serving both would make that a per-call decision instead of a
// per-domain one.
func foldName(s string) string { return foldForMatch(s, false) }

// foldNameNoArticle is foldName with a leading article removed, which is
// what lets `The Carpenters` match MusicBrainz's `Carpenters` (81 tracks
// on the production library; the candidate scores 73, below the
// release-side floor).
//
// Article stripping is the loosest rule in this file, so it is bounded
// three ways. Names only, never titles. Refused when the remainder is
// empty or is ITSELF an article, which is what protects `The The` —
// NOT a token-count rule; see stripLeadingArticle, which records that
// "only strip when more than one token remains" was considered and
// rejected because it would refuse `The Carpenters` -> `carpenters`,
// the single-token case the feature exists for. And it is applied as a
// SEPARATE acceptance pass in pickBestArtist, so a non-article match
// always wins first.
//
// Split from foldName rather than folded into it so pickBestArtist can
// try the strict form first — see A2/A3 there. That pass derives this
// from foldName via stripLeadingArticle rather than calling it again,
// an identity that holds only while the strip is the LAST stage of the
// pipeline and is pinned by TestFoldNameNoArticleIsStripOverFoldName.
func foldNameNoArticle(s string) string { return foldForMatch(s, true) }

// foldForMatch is the shared pipeline.
//
//  1. NFKD — compatibility DECOMPOSITION. Not NFKC: decomposing is what
//     separates a base letter from its accent so step 2 can drop the
//     accent. `Zdob și Zdub` and `Yael Naïm` are recovered here and
//     nowhere else. NFKD also folds ﬁ→fi, fullwidth Latin→ASCII, №, NBSP.
//  2. Drop nonspacing marks (Mn). This is accent-stripping, and it also
//     removes the U+0307 that case-folding Turkish İ leaves behind.
//     Aggressive for scripts where marks are semantic (Arabic, Indic),
//     but those are not what a Latin-script music library matches on, and
//     both sides of every comparison get the same treatment.
//  3. Case-fold — not ToLower: it handles ß→ss and the Turkish İ/ı pair.
//  4. `&` → " and ", SPACE-PADDED and BEFORE punctuation mapping. Padding
//     is load-bearing: a bare `&`→`and` turns `R&B` into `randb`, which
//     then fails against `R and B` and `R & B`. Padded, all three become
//     `r and b`.
//  5. Punctuation: apostrophes/quotes DELETED, every other non-alphanumeric
//     mapped to SPACE. The asymmetry is deliberate — see below.
//  6. Collapse whitespace runs.
//
// Why apostrophes are deleted rather than spaced: taggers routinely drop
// them entirely, so `Ain't` must fold equal to `Aint`. Mapping to space
// would give `ain t` and fail.
//
// Why dashes become space rather than being deleted: deleting collapses
// `Re-Load` into `Reload`, two different Metallica albums. Spacing keeps
// them two tokens vs one.
func foldForMatch(s string, stripArticle bool) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	s = norm.NFKD.String(s)

	// Steps 2–5 in one pass over the decomposed runes.
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case unicode.Is(unicode.Mn, r):
			// accent / combining mark — drop
		case r == '&':
			b.WriteString(" and ")
		case isApostropheLike(r):
			// drop entirely
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
		default:
			// dashes, colons, slashes, brackets, dots, commas, whitespace
			b.WriteByte(' ')
		}
	}
	out := matchFoldCaser.String(b.String())
	out = strings.Join(strings.Fields(out), " ")

	if stripArticle {
		out = stripLeadingArticle(out)
	}
	return out
}

// apostropheLike is every character a tagger might use where a human
// reads an apostrophe or a quote. All are deleted, never spaced.
func isApostropheLike(r rune) bool {
	switch r {
	case '\'', '‘', '’', '‚', '‛', // ' ‘ ’ ‚ ‛
		'`', '´', 'ʹ', 'ʻ', 'ʼ', 'ʽ', 'ʾ', 'ʿ',
		'"', '“', '”', '„', '‟', // " “ ” „ ‟
		'′', '″': // ′ ″
		return true
	}
	return false
}

// leadingArticles are stripped from ARTIST names only.
var leadingArticles = []string{"the ", "a ", "an "}

// stripLeadingArticle removes a leading article, refusing when the
// remainder is empty or is ITSELF an article.
//
// That guard is what protects `The The` — it folds to `the the`, and
// stripping would give `the`, which compares equal to any artist named
// "The". The remainder-is-an-article test refuses precisely that.
//
// A reviewer proposed the more obvious "only strip when more than one
// token remains" instead. That rule is wrong here: it would also refuse
// `The Carpenters` → `carpenters`, which is a single token and is the
// 81-track case this exists for. The collision to defend against is an
// article-only remainder, not a short one.
func stripLeadingArticle(folded string) string {
	for _, art := range leadingArticles {
		if !strings.HasPrefix(folded, art) {
			continue
		}
		rest := folded[len(art):]
		if rest == "" || isBareArticle(rest) {
			return folded
		}
		return rest
	}
	return folded
}

// isBareArticle reports whether a folded string is nothing but an
// article ("the", "a", "an").
func isBareArticle(folded string) bool {
	for _, art := range leadingArticles {
		if folded == strings.TrimSpace(art) {
			return true
		}
	}
	return false
}

// foldedTokenContains reports whether two ALREADY-FOLDED strings overlap,
// symmetrically, on whitespace-token boundaries.
//
// Token alignment is what stops `gold` matching inside `goldfinger`,
// `aria` inside `ariadne auf naxos`, and `load` inside `download`. It
// costs nothing in recall — every measured recovery is a whole-title or
// whole-token-prefix relation.
//
// Both arguments MUST already be folded; this does no folding of its own
// so the caller can hoist the query-side fold out of the candidate loop.
func foldedTokenContains(a, b string) bool {
	if a == "" || b == "" {
		// An empty needle makes strings.Contains trivially true, which
		// would accept every candidate.
		return false
	}
	if a == b {
		return true
	}
	// Short titles demand exact equality — see shortTitleExactRunes.
	// utf8.RuneCountInString rather than len([]rune(s)): this is the
	// candidate-comparison hot path and the conversion would allocate a
	// rune slice per call just to read its length.
	shorter, longer := a, b
	if utf8.RuneCountInString(longer) < utf8.RuneCountInString(shorter) {
		shorter, longer = longer, shorter
	}
	if utf8.RuneCountInString(shorter) <= shortTitleExactRunes {
		return false // equality was already checked above
	}
	return tokenAlignedContains(longer, shorter)
}

// tokenAlignedContains reports whether `needle` appears in `hay` starting
// and ending on a token boundary. Both are folded (single-space-separated).
func tokenAlignedContains(hay, needle string) bool {
	switch {
	case hay == needle:
		return true
	case strings.HasPrefix(hay, needle+" "):
		return true
	case strings.HasSuffix(hay, " "+needle):
		return true
	default:
		return strings.Contains(hay, " "+needle+" ")
	}
}
