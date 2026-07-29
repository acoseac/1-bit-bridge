// Query-shape ladders: when the tags as written don't match, retry with a
// broader query rather than giving up and stamping enriched_at.
//
// GOVERNING PRINCIPLE: relaxations belong in the QUERY, strictness belongs
// in the ACCEPTANCE. Every rung here issues a real request and its result
// still has to survive pickBestRelease / pickBestArtist unchanged. That is
// what makes a loose rung safe — it cannot lower the bar, only ask a
// different question.
//
// COST: each rung is one paced upstream request (MBMinInterval — 150ms
// self-hosted, 1100ms public), but the cost is per DISTINCT album/artist,
// not per track, because both ladders sit behind an LRU. ~1,450 albums on
// a 19.5k-track library, so 4 rungs -> 6 costs roughly +84s on a cold
// self-hosted pass.
//
// Both ladders are HARD-CAPPED. The cap is the structural defence against
// a future rung multiplying the fan-out: add a generator and the cap
// silently drops the tail rather than quietly tripling every cold pass.
package enrich

import (
	"context"
	"regexp"
	"strings"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

const (
	// maxReleaseAttempts bounds the album ladder (6 rungs today).
	maxReleaseAttempts = 6
	// maxArtistAttempts bounds the artist ladder (5 rungs today).
	maxArtistAttempts = 5
)

// --- album title shapes ---

// unbracketedEditionSuffixRE matches a trailing edition qualifier that
// carries NO brackets: "Abba Gold Anniversary Edition", "Heavy Flowers
// 10th Anniversary", "Songs Remastered".
//
// KEYWORD-ANCHORED, deliberately the opposite call from
// albumEditionSuffixRE — and the reason is worth stating, because the two
// sit next to each other and look inconsistent:
//
// A bracket is a SYNTACTIC MARKER. "(anything)" at the end of a title
// announces itself as a qualifier, so the bracketed regex can be
// content-blind and strip whatever it finds. Unbracketed text has no such
// marker: a generic "drop the trailing words" rule would mangle "Dark
// Side of the Moon" into "Dark Side of the". So this one must know the
// vocabulary.
//
// The prefix is `.+?` and REQUIRED, so an album literally titled
// "Anniversary" or "Remastered" is left alone.
var unbracketedEditionSuffixRE = regexp.MustCompile(
	`(?i)^(.+?)[\s,\-\x{2010}-\x{2015}:;/]+` +
		`(?:\d{1,3}(?:st|nd|rd|th)\s+)?` +
		`(?:anniversary|deluxe|expanded|limited|special|collector'?s?|legacy|` +
		`remaster(?:ed)?|bonus|extended|platinum|digital|japanese|` +
		`international|tour|reissue|mono|stereo)` +
		`(?:\s+(?:edition|version|remaster(?:ed)?|reissue|mix))?\s*$`)

// stripUnbracketedEditionSuffix removes one trailing unbracketed edition
// qualifier. Returns "" when there is nothing to strip.
func stripUnbracketedEditionSuffix(album string) string {
	m := unbracketedEditionSuffixRE.FindStringSubmatch(strings.TrimSpace(album))
	if m == nil {
		return ""
	}
	out := strings.TrimSpace(m[1])
	if out == "" || out == strings.TrimSpace(album) {
		return ""
	}
	return out
}

// stripArtistPrefix removes a leading artist name from an album title:
// "The Beatles 1962 – 1966" -> "1962 – 1966" (106 tracks), "Bon Jovi
// Greatest Hits" -> "Greatest Hits", "CAROLE KING Music" -> "Music".
//
// Works on TOKEN boundaries of the raw string, comparing FOLDED joins —
// you cannot map a folded offset back into the original, so the prefix is
// found by folding candidate token runs rather than by folding once and
// slicing. Longest matching prefix wins.
//
// Returns "" when nothing matches or when stripping would empty the
// title (a self-titled album — "Weezer" by Weezer — must be left alone).
func stripArtistPrefix(album string, artists []string) string {
	raw := strings.TrimSpace(album)
	tokens := strings.Fields(raw)
	if len(tokens) < 2 {
		return ""
	}
	var artistFolds []string
	for _, a := range artists {
		if a = strings.TrimSpace(a); a == "" {
			continue
		}
		if f := foldName(a); f != "" {
			artistFolds = append(artistFolds, f)
		}
		// Also accept the article-stripped form, so a tag reading
		// "Carpenters Singles 1969-1981" is caught for artist
		// "The Carpenters".
		if f := foldNameNoArticle(a); f != "" {
			artistFolds = append(artistFolds, f)
		}
	}
	if len(artistFolds) == 0 {
		return ""
	}
	// Longest prefix first: an artist whose name is itself several tokens
	// must win over a shorter accidental match.
	for k := len(tokens) - 1; k >= 1; k-- {
		prefixFold := foldName(strings.Join(tokens[:k], " "))
		if prefixFold == "" {
			continue
		}
		for _, af := range artistFolds {
			if prefixFold != af {
				continue
			}
			rest := strings.Join(tokens[k:], " ")
			if foldTitle(rest) == "" {
				return ""
			}
			return rest
		}
	}
	return ""
}

// --- artist name shapes ---

// headCreditSeparators split a multi-credit artist tag down to its first
// credit: "Ennio Morricone; Solisti e Orchestre" -> "Ennio Morricone".
//
// ═══ THE SET IS DELIBERATELY NARROW. DO NOT ADD '&' OR A BARE ','. ═══
//
// Measured: splitting "Peter, Paul & Mary" on '&' yields "Peter, Paul",
// which matches an UNRELATED MusicBrainz artist named "Peter Paul" at
// score 100 — 186 tracks would take a wrong MBID.
//
// And it is worse than a coincidence: foldName erases commas, so
// foldName("Peter, Paul") == foldName("Peter Paul") by construction. The
// acceptance layer therefore CANNOT catch this — it would happily accept
// the wrong artist as a folded-exact match. The only defence is never
// generating the query.
//
// '&' is legitimately INSIDE artist names: Simon & Garfunkel, Alison
// Krauss & Union Station, Earth, Wind & Fire, Peter, Paul & Mary. So is a
// bare comma: Crosby, Stills & Nash.
//
// The slash form is whitespace-delimited on purpose so "AC/DC" survives.
var headCreditSeparators = []string{
	";",
	" / ",
	" feat. ", " feat ", " ft. ", " ft ", " featuring ",
	" with ", " vs. ", " vs ",
}

// splitHeadCredit returns the first credit in a multi-credit tag, or ""
// when the tag carries only one credit.
func splitHeadCredit(artist string) string {
	raw := strings.TrimSpace(artist)
	if raw == "" {
		return ""
	}
	lower := strings.ToLower(raw)
	cut := -1
	for _, sep := range headCreditSeparators {
		if i := strings.Index(lower, sep); i >= 0 && (cut < 0 || i < cut) {
			cut = i
		}
	}
	if cut <= 0 {
		return ""
	}
	head := strings.TrimSpace(strings.Trim(raw[:cut], " ,;/"))
	if head == "" || strings.EqualFold(head, raw) {
		return ""
	}
	return head
}

// roleAnnotations is the CLOSED vocabulary of credit-role words that
// appear as comma-delimited annotations in tags exported by classical and
// jazz libraries: "ABDULLAH IBRAHIM, Composer feat. …", "Madeleine
// Peyroux, Guitar, Vocalist", "Rachel Podger, Conductor, Brecon Baroque,
// Ensemble".
//
// A CLOSED list is exactly what makes comma handling safe here. The rule
// below cuts at the first comma-delimited segment that IS one of these
// words — so "Peter, Paul & Mary" (segment "Paul & Mary"), "Crosby,
// Stills & Nash" and "Earth, Wind & Fire" are untouched, while a real
// role annotation is removed. Never widen this into "cut at the first
// comma".
var roleAnnotations = map[string]struct{}{
	"composer": {}, "soloist": {}, "conductor": {}, "performer": {},
	"orchestra": {}, "choir": {}, "vocalist": {}, "vocals": {},
	"guitar": {}, "piano": {}, "violin": {}, "cello": {}, "bass": {},
	"drums": {}, "arranger": {}, "producer": {}, "ensemble": {},
	"artist": {}, "lyricist": {}, "writer": {}, "featured artist": {},
}

// truncateAtFirstRole cuts an artist tag at the first comma-delimited
// segment that is a bare role word, returning the credit before it.
// Returns "" when no segment is a role.
func truncateAtFirstRole(artist string) string {
	raw := strings.TrimSpace(artist)
	if !strings.Contains(raw, ",") {
		return ""
	}
	segs := strings.Split(raw, ",")
	for i := 1; i < len(segs); i++ {
		key := foldName(segs[i])
		if _, isRole := roleAnnotations[key]; !isRole {
			continue
		}
		head := strings.TrimSpace(strings.Join(segs[:i], ","))
		head = strings.Trim(head, " ,;/")
		if head == "" || strings.EqualFold(head, raw) {
			return ""
		}
		return head
	}
	return ""
}

// --- ladder assembly ---

// releaseAttempt is one (artist, album) query shape.
type releaseAttempt struct{ artist, album string }

// buildReleaseLadder returns the ordered, deduped, capped query shapes for
// an album.
//
// Emission order is STRICTLY ADDITIVE — rungs 1-4 are byte-identical to
// what shipped before, so no album that resolves today can change its
// answer by taking a different rung:
//
//  1. (artist, album)                     — always
//  2. (albumArtist, album)                — compilations, junk artist tags
//  3. (artist, bracketStripped)           — "Goats Head Soup (2020 Deluxe)"
//  4. (albumArtist, bracketStripped)
//  5. (artist, unbracketedStripped)       — NEW: "Abba Gold Anniversary Edition"
//  6. (artist, artistPrefixStripped)      — NEW: "The Beatles 1962 – 1966"
func buildReleaseLadder(artist, albumArtist, album string) []releaseAttempt {
	artist = strings.TrimSpace(artist)
	album = strings.TrimSpace(album)
	albumArtist = strings.TrimSpace(albumArtist)

	// Folded compare, not EqualFold: it also suppresses a redundant rung
	// when the two differ only by accent or punctuation ("Yael Naim" vs
	// "Yael Naïm"), which used to cost a real request.
	useAlbumArtist := albumArtist != "" && foldName(albumArtist) != foldName(artist)

	var out []releaseAttempt
	seen := map[string]struct{}{}
	add := func(ar, al string) {
		if ar == "" || al == "" || len(out) >= maxReleaseAttempts {
			return
		}
		key := foldName(ar) + "\x00" + foldTitle(al)
		if _, dup := seen[key]; dup {
			return
		}
		seen[key] = struct{}{}
		out = append(out, releaseAttempt{ar, al})
	}

	add(artist, album)
	if useAlbumArtist {
		add(albumArtist, album)
	}
	if s := stripAlbumEditionSuffix(album); s != "" {
		add(artist, s)
		if useAlbumArtist {
			add(albumArtist, s)
		}
	}
	if s := stripUnbracketedEditionSuffix(album); s != "" {
		add(artist, s)
	}
	if s := stripArtistPrefix(album, []string{artist, albumArtist}); s != "" {
		add(artist, s)
	}
	return out
}

// buildArtistLadder returns the ordered, deduped, capped query shapes for
// an artist name.
//
//  1. t.Artist verbatim
//  2. head credit            — "Ennio Morricone; Solisti e Orchestre"
//  3. role-truncated         — "Madeleine Peyroux, Guitar, Vocalist"
//  4. head credit THEN role  — "ABDULLAH IBRAHIM, Composer feat. …"
//  5. t.AlbumArtist          — when it folds differently from all of the above
//
// Leading-article handling is deliberately NOT a rung: pickBestArtist's A3
// pass compares article-stripped forms on candidates it already has, so
// "The Carpenters" -> "Carpenters" costs zero extra requests.
//
// COST, measured on the 300 unresolved artists of the production library:
// this generates 1 rung for 79 of them, 2 for 214, 3 for 6 and 4 for 1 —
// a 1.47x request multiplier on a cold pass, which shrinks as artists
// resolve and get positively cached. That number is why resolveArtist
// still does NOT cache a clean no-match: the ladder is not expensive
// enough to justify trading away that documented invariant. Re-measure
// before concluding otherwise.
func buildArtistLadder(artist, albumArtist string) []string {
	artist = strings.TrimSpace(artist)
	albumArtist = strings.TrimSpace(albumArtist)

	var out []string
	seen := map[string]struct{}{}
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" || len(out) >= maxArtistAttempts {
			return
		}
		key := foldName(s)
		if key == "" {
			return
		}
		if _, dup := seen[key]; dup {
			return
		}
		seen[key] = struct{}{}
		out = append(out, s)
	}

	add(artist)
	head := splitHeadCredit(artist)
	add(head)
	// Role-truncate the NARROWEST name available. Applying it to the full
	// string when a head credit exists produces a useless hybrid rung —
	// "ABDULLAH IBRAHIM, Composer feat. Noah Jackson, Soloist" cut at its
	// LAST role segment gives "ABDULLAH IBRAHIM, Composer feat. Noah
	// Jackson", one wasted request on the way to the answer.
	base := artist
	if head != "" {
		base = head
	}
	add(truncateAtFirstRole(base))
	add(albumArtist)
	return out
}

// searchArtistWithFallbacks resolves an artist, retrying with a narrower
// credit when the tag as written doesn't match.
//
// Measured on the production library, these are what the extra rungs buy
// (per-track counts):
//
//	Ennio Morricone; Solisti e Orchestre del Cinema Italiano   37   head credit
//	Rachel Podger, Conductor, Brecon Baroque, Ensemble         34   role cut
//	Emmylou Harris; Mark Knopfler                              31   head credit
//	Damien Jurado, Artist                                      31   role cut
//	ABDULLAH IBRAHIM, Composer feat. Noah Jackson, Soloist     26   head + role
//	Madeleine Peyroux, Guitar, Vocalist                        24   role cut
//
// ERROR SEMANTICS MIRROR THE ALBUM LADDER EXACTLY: a rung runs only after
// a clean (nil, nil) "no plausible match". Any error — transient or
// persistent — returns immediately, so resolveArtist's transient-retry,
// ctx-cancel and negative-cache branches all still see what they saw when
// this was a single call.
func (e *Enricher) searchArtistWithFallbacks(ctx context.Context, t *manifest.Track) (*SearchResult, error) {
	attempts := buildArtistLadder(t.Artist, t.AlbumArtist)
	for i, name := range attempts {
		// Pace EVERY rung — the politeness contract is per-request.
		if !sleepCtx(ctx, e.MBMinInterval) {
			return nil, ctx.Err()
		}
		res, err := e.mb.SearchArtist(ctx, name)
		if err != nil {
			return nil, err
		}
		if res != nil {
			if i > 0 {
				logger.Info("MB artist search matched on a fallback query",
					"path", t.Path, "attempt", i,
					"tagged", t.Artist, "searched", name)
			}
			return res, nil
		}
	}
	return nil, nil
}
