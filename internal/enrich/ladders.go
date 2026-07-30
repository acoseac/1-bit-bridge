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
	// maxReleaseAttempts bounds the album ladder. 8 rather than the 6
	// shapes listed in buildReleaseLadder: a title carrying BOTH a
	// bracketed and an unbracketed qualifier, on an album with a distinct
	// albumArtist, generates 7. The cap exists to stop a future generator
	// multiplying the fan-out, not to trim today's shapes — if it starts
	// truncating real rungs it has stopped doing its job.
	maxReleaseAttempts = 8
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
// Returns ("", "") when nothing matches or when stripping would empty the
// title (a self-titled album — "Weezer" by Weezer — must be left alone).
//
// The second return is WHICH of the supplied artists the prefix matched,
// and it is load-bearing rather than informational. pickBestRelease
// validates a candidate's artist credit against the artist that was
// QUERIED. On a split-credit album — track artist "John Lennon",
// albumArtist "The Beatles", album "The Beatles 1962 – 1966" — the prefix
// is found via the albumArtist, so querying the stripped title with the
// TRACK artist fails the credit check and the rung is wasted. The caller
// must query with the artist whose name was actually stripped.
func stripArtistPrefix(album string, artists []string) (rest, matched string) {
	raw := strings.TrimSpace(album)
	tokens := strings.Fields(raw)
	if len(tokens) < 2 {
		return "", ""
	}
	// Fold each candidate artist both plainly and article-stripped, so a
	// tag reading "Carpenters Singles 1969-1981" is caught for the artist
	// "The Carpenters". Keep the ORIGINAL string alongside each fold —
	// that is what the caller queries with.
	type artistFold struct{ fold, original string }
	var folds []artistFold
	for _, a := range artists {
		if a = strings.TrimSpace(a); a == "" {
			continue
		}
		if f := foldName(a); f != "" {
			folds = append(folds, artistFold{f, a})
		}
		if f := foldNameNoArticle(a); f != "" {
			folds = append(folds, artistFold{f, a})
		}
	}
	if len(folds) == 0 {
		return "", ""
	}
	// Longest prefix first: an artist whose name is itself several tokens
	// must win over a shorter accidental match.
	for k := len(tokens) - 1; k >= 1; k-- {
		prefixFold := foldName(strings.Join(tokens[:k], " "))
		if prefixFold == "" {
			continue
		}
		for _, af := range folds {
			if prefixFold != af.fold {
				continue
			}
			out := strings.Join(tokens[k:], " ")
			if foldTitle(out) == "" {
				return "", ""
			}
			return out, af.original
		}
	}
	return "", ""
}

// --- artist name shapes ---

// headCreditSeparators split a multi-credit artist tag down to its first
// credit: "Ennio Morricone; Solisti e Orchestre" -> "Ennio Morricone".
//
// ═══ THE SET IS DELIBERATELY NARROW. ═══
//
// The rule: a separator qualifies ONLY if it is an unambiguous credit
// delimiter. An English word that can appear INSIDE a name does not
// qualify, however often it also separates credits.
//
// Banned, with the reason each one is dangerous:
//
//   - '&'  — "Peter, Paul & Mary" splits to "Peter, Paul", which matches
//     an UNRELATED MusicBrainz artist named "Peter Paul" at score 100.
//     186 tracks would take a wrong MBID. Also inside Simon & Garfunkel,
//     Alison Krauss & Union Station, Earth, Wind & Fire.
//   - bare ',' — same collision by a different route (Crosby, Stills &
//     Nash).
//   - " with " — inside Sleeping with Sirens, Running with Scissors,
//     Girls with Guitars. Splitting yields "Sleeping" / "Running" /
//     "Girls", every one of which is a plausible real artist name.
//   - " vs " / " vs. " — same hazard, lower frequency.
//
// What makes these unrecoverable rather than merely risky: pickBestArtist
// validates a candidate against the QUERY THAT WAS SENT, not against the
// original tag. So once a bad rung is generated, an exact match for the
// truncated name is accepted as correct. And foldName erases commas, so
// foldName("Peter, Paul") == foldName("Peter Paul") by construction — the
// acceptance layer cannot catch that one even in principle. Never
// generating the query is the only defence.
//
// Dropping " with " and " vs " costs nothing measured: every artist
// recovery observed on the production library came through ';' or a role
// truncation.
//
// The slash form is whitespace-delimited on purpose so "AC/DC" survives.
var headCreditSeparators = []string{
	";",
	" / ",
	" feat. ", " feat ", " ft. ", " ft ", " featuring ",
}

// lowerASCII maps A-Z to a-z and leaves every other byte alone.
//
// UNLIKE strings.ToLower this is byte-length preserving, which is what makes
// splitHeadCredit's `raw[:cut]` safe. Go's case table shortens İ (U+0130, 2
// bytes → 1), U+212A KELVIN (3 → 1) and ẞ (U+1E9E, 3 → 2), so an offset found
// in the lowered string is not an offset into the original: every such rune
// before the separator shifts the cut left, truncating the head credit, and a
// multi-byte rune straddling the shifted offset makes the slice invalid UTF-8
// — which then flows through escapeLucene into the query URL.
//
// stripArtistPrefix's docblock states the rule this was breaking: "you cannot
// map a folded offset back into the original".
//
// Lowering only ASCII loses no matches here because every separator is ASCII,
// and an ASCII byte in UTF-8 is always a standalone rune (continuation bytes
// are >= 0x80). Returns s unchanged when there is nothing to lower, so the
// common case allocates nothing — strictly cheaper than ToLower, not merely
// safer.
func lowerASCII(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] < 'A' || s[i] > 'Z' {
			continue
		}
		// First uppercase byte. Everything before it is already lower, so the
		// copy starts here rather than rescanning from 0.
		b := []byte(s)
		for j := i; j < len(b); j++ {
			if b[j] >= 'A' && b[j] <= 'Z' {
				b[j] += 'a' - 'A'
			}
		}
		return string(b)
	}
	return s
}

// splitHeadCredit returns the first credit in a multi-credit tag, or ""
// when the tag carries only one credit.
func splitHeadCredit(artist string) string {
	raw := strings.TrimSpace(artist)
	if raw == "" {
		return ""
	}
	// lowerASCII, NOT strings.ToLower — see its docblock. `cut` below is an
	// offset into this string and is used to slice `raw`, so the two must
	// agree byte for byte.
	lower := lowerASCII(raw)
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

// --- cache keys ---
//
// A memo key must name every input its ladder reads. These two live here, next
// to the builders, because that is the only place the two can be checked
// against each other by eye.
//
// They did not, and it was live. cacheKey(artist, album) has been the album
// memo since the first enrichment PR, when buildReleaseLadder did not exist
// and the query WAS (artist, album). The ladders later grew an albumArtist
// rung — and on a track whose artist tag is junk, that rung is the one that
// answers. So two tracks agreeing on (artist, album) and differing in
// albumArtist shared one entry and got whichever answer ran first: a wrong
// release MBID, and with it the wrong cover.
//
// The shape it bites is ordinary. A classical library tags artist with the
// performer and albumArtist with the composer, so "Berliner Philharmoniker" /
// "Symphony No. 5" is one key for Beethoven's and Mahler's alike.
//
// Both helpers collapse to the historic key when albumArtist is absent or
// folds to the artist — the same useAlbumArtist test buildReleaseLadder uses
// to decide whether the rung exists at all. So a library where the two agree
// keeps byte-identical keys and the sibling-track sharing these caches exist
// for is untouched; only the tracks that could get a wrong answer get a
// narrower key.

// releaseCacheKey keys the album cache on every input buildReleaseLadder
// reads.
func releaseCacheKey(artist, albumArtist, album string) string {
	if !laddersUseAlbumArtist(artist, albumArtist) {
		return cacheKey(artist, album)
	}
	return artist + "\x00" + albumArtist + "\x00" + album
}

// artistCacheKey keys the artist cache on every input buildArtistLadder
// reads.
func artistCacheKey(artist, albumArtist string) string {
	if !laddersUseAlbumArtist(artist, albumArtist) {
		return "artist\x00" + artist
	}
	return "artist\x00" + artist + "\x00" + albumArtist
}

// laddersUseAlbumArtist reports whether albumArtist can contribute a rung the
// artist alone would not produce. Shared by both builders and both keys, so a
// key can never disagree with the ladder it is memoizing.
func laddersUseAlbumArtist(artist, albumArtist string) bool {
	albumArtist = strings.TrimSpace(albumArtist)
	return albumArtist != "" && foldName(albumArtist) != foldName(strings.TrimSpace(artist))
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
	useAlbumArtist := laddersUseAlbumArtist(artist, albumArtist)

	var out []releaseAttempt
	seen := map[string]struct{}{}
	add := func(ar, al string) {
		if ar == "" || al == "" || len(out) >= maxReleaseAttempts {
			return
		}
		// A name MusicBrainz cannot answer as an artist cannot answer as half
		// of an (artist, album) query either, and this is the query that runs
		// FIRST — before resolveArtist is reached at all. Gating only the
		// artist search left the louder half of the traffic in place.
		//
		// Per-rung, so rung 2's albumArtist — the rung that rescues exactly
		// these tracks, and which simply becomes rung 1 — is untouched.
		//
		// Waste, not danger: pickBestRelease requires an artist-credit match,
		// so "CD 01" could never have accepted a wrong release. What it could
		// do is spin. A 5xx is transient, so the track retries on every batch
		// without ever being able to succeed.
		if isUnsearchableArtistTag(ar) {
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
		if useAlbumArtist {
			add(albumArtist, s)
		}
	}
	// Query the stripped title with the artist whose name was actually
	// stripped off it — see stripArtistPrefix. Also try the track artist
	// when it differs, for the ordinary case where the album is credited
	// to the track artist but the tag repeats it in the title.
	if s, matched := stripArtistPrefix(album, []string{artist, albumArtist}); s != "" {
		add(matched, s)
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
		// A tag MusicBrainz cannot answer is not worth asking about — but
		// the judgement belongs to the RUNG, not to the tag the track
		// happened to carry.
		//
		// It was applied one level up, at the top of resolveArtist, which
		// returned before the ladder was ever built. That skipped the
		// albumArtist rung too — the rung whose entire purpose is to
		// recover an artist when the artist tag is a folder label. The tag
		// that triggered the guard was the tag the rung existed for, so the
		// guard removed the answer along with the pointless question.
		//
		// buildArtistLadder's own test names the case: artist "CD 01",
		// albumArtist "Abdullah Ibrahim". It kept passing because it drives
		// this function directly; production never got here.
		if isUnsearchableArtistTag(s) {
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
//
// Takes the ladder rather than building it, so resolveArtist can decide what
// an empty one means before recording a cache miss for a track that will
// issue no request.
func (e *Enricher) searchArtistWithFallbacks(ctx context.Context, attempts []string, t *manifest.Track) (*SearchResult, error) {
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
			// Gate on "we asked something other than the tag", not on the
			// rung INDEX. Dropping an unanswerable rung can make the
			// albumArtist rung index 0, and an index-gated log would go
			// quiet on exactly the recoveries worth knowing about — this
			// line is the operator's only measure of what the ladder buys.
			if foldName(name) != foldName(t.Artist) {
				logger.Info("MB artist search matched on a fallback query",
					"path", t.Path, "attempt", i,
					"tagged", t.Artist, "searched", name)
			}
			return res, nil
		}
	}
	return nil, nil
}
