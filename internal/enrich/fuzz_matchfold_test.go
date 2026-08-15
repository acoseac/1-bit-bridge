// Fuzz coverage for the match-folding vocabulary and the Retry-After parser.
//
// # The fold invariant is the point
//
// `foldNameNoArticle` and `foldName` are documented to satisfy
// `foldNameNoArticle(x) == stripLeadingArticle(foldName(x))`, and that identity
// is LOAD-BEARING: `pickBestArtist` derives one from the other so each
// candidate is folded once, and the ordered A1→A4 passes rely on the derived
// value meaning what the direct call would have meant. The identity holds only
// while the article strip stays the LAST stage of the pipeline, so it is
// exactly the kind of property a future reordering breaks silently.
//
// `TestFoldNameNoArticleIsStripOverFoldName` already pins it on a table; this
// pins it over arbitrary Unicode, which is where the pipeline actually gets
// interesting (NFKD decomposition, mark stripping, case folding, and the
// `&` → " and " rewrite all run before the strip).
//
// # Retry-After
//
// Attacker-adjacent in the practical sense: it is a header from a third-party
// upstream, and mis-parsing it either parks the enricher (the `maxRetryAfter`
// cap exists for that) or drops the backoff entirely. The overflow and
// fractional-seconds branches are hand-rolled, which is what makes them worth
// fuzzing rather than table-testing alone.
package enrich

import (
	"testing"
	"time"
)

func FuzzParseRetryAfter(f *testing.F) {
	f.Add("120")
	f.Add("Wed, 21 Oct 2015 07:28:00 GMT")
	f.Add("99999999999999999999999") // ErrRange → the maxRetryAfter cap
	f.Add("86400.5")                 // fractional delta-seconds
	f.Add("-5")
	f.Fuzz(func(t *testing.T, s string) {
		d := parseRetryAfter(s, time.Unix(0, 0))
		// The cap is the whole point of the function: a hostile or
		// misconfigured upstream must never be able to park the enricher.
		if d < 0 || d > maxRetryAfter {
			t.Fatalf("parseRetryAfter(%q) = %v, outside [0, %v]", s, d, maxRetryAfter)
		}
	})
}

func FuzzFoldForMatch(f *testing.F) {
	f.Add("The Beatles")
	f.Add("Zdob și Zdub")
	f.Add("R&B")
	f.Add("Ain't Misbehavin'")
	f.Add("!!!")
	f.Fuzz(func(t *testing.T, s string) {
		name := foldName(s)
		noArticle := foldNameNoArticle(s)
		_ = foldTitle(s)
		if got := stripLeadingArticle(name); got != noArticle {
			t.Fatalf("fold invariant broken for %q:\n stripLeadingArticle(foldName) = %q\n foldNameNoArticle           = %q",
				s, got, noArticle)
		}
	})
}

func FuzzFoldedTokenContains(f *testing.F) {
	f.Add("bill withers friends", "bill withers")
	f.Fuzz(func(t *testing.T, a, b string) { _ = foldedTokenContains(a, b) })
}

func FuzzParseSize(f *testing.F) {
	f.Add("500")
	f.Add("")
	f.Fuzz(func(t *testing.T, s string) { _, _ = ParseSize(s) })
}
