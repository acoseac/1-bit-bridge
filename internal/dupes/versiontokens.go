package dupes

import (
	"sort"
	"strings"
	"unicode"
)

// versionTokens is a CLOSED lowercase set of words that mark a different
// VERSION of the same nominal track/album — the things that make two
// same-keyed rows not-duplicates (an acoustic session vs the album cut,
// the mono vs stereo mix). Matched as whole letter-run tokens across each
// member's path segments ∪ album ∪ title; an ASYMMETRY between members
// (one carries a token the other doesn't) demotes the group to
// TierInconclusive.
//
// Bias INCLUSIVE when extending: an over-eager token only costs a real
// duplicate its tier (it degrades to inconclusive, which is never
// suppressed) — the safe direction. A symmetric hit (both members inside
// "The Mix"/CD folders named "Live …") never demotes, because the sets
// compare equal.
var versionTokens = map[string]struct{}{
	"acoustic": {}, "alternate": {}, "alternative": {}, "anniversary": {},
	"bonus": {}, "deluxe": {}, "demo": {}, "edit": {}, "edition": {},
	"expanded": {}, "extended": {}, "instrumental": {}, "karaoke": {},
	"live": {}, "mix": {}, "mono": {}, "radio": {}, "redux": {},
	"remaster": {}, "remastered": {}, "remix": {}, "remixes": {},
	"reprise": {}, "rework": {}, "session": {}, "sessions": {},
	"single": {}, "stereo": {}, "unplugged": {}, "version": {},
}

// versionTokenSet returns the sorted version tokens found in the row's
// path segments, album and title. Tokens are maximal LETTER runs
// lowercased — "mono" does not hit "Monomania" (one run), but does hit
// "(Mono)" and "Mono2009"-style tagger suffixes where the digits break
// the run.
func versionTokenSet(r Row) []string {
	found := map[string]struct{}{}
	scan := func(s string) {
		lower := strings.ToLower(s)
		start := -1
		flush := func(end int) {
			if start < 0 {
				return
			}
			tok := lower[start:end]
			if _, ok := versionTokens[tok]; ok {
				found[tok] = struct{}{}
			}
			start = -1
		}
		for i, run := range lower {
			if unicode.IsLetter(run) {
				if start < 0 {
					start = i
				}
				continue
			}
			flush(i)
		}
		flush(len(lower))
	}
	for _, seg := range strings.FieldsFunc(r.Path, func(c rune) bool { return c == '/' || c == '\\' }) {
		scan(seg)
	}
	scan(r.Album)
	scan(r.Title)
	out := make([]string, 0, len(found))
	for tok := range found {
		out = append(out, tok)
	}
	sort.Strings(out)
	return out
}

// versionTokensSymmetric reports whether every member carries the SAME
// version-token set. Asymmetry is the demotion signal; a shared token is
// fine.
func versionTokensSymmetric(members []Row) bool {
	if len(members) < 2 {
		return true
	}
	base := strings.Join(versionTokenSet(members[0]), "\x1f")
	for _, m := range members[1:] {
		if strings.Join(versionTokenSet(m), "\x1f") != base {
			return false
		}
	}
	return true
}
