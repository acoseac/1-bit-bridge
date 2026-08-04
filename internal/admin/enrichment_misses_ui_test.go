package admin

import (
	"regexp"
	"strings"
	"testing"
)

// enrichMissesElementIDs are the DOM ids the misses drill-down couples
// across two files: dashboard.html declares them, app.js looks them up by
// getElementById.
//
// That coupling is silent in both directions. Rename the id in the
// template and the lookup returns null, so every render function early-
// returns and the panel does nothing — no console error, no failing
// build, no failing test. It is the same shape as the /inspector href
// that 404'd for its whole life: a string contract between two files that
// nothing checks.
var enrichMissesElementIDs = []string{
	"enrich-misses-toggle", // the button that opens the panel
	"enrich-misses",        // the panel itself (hidden until opened)
	"enrich-misses-status", // the summary line / error surface
	"enrich-misses-body",   // where the per-facet sections are appended
}

// TestEnrichMissesElementsExistInTemplate pins the template half.
func TestEnrichMissesElementsExistInTemplate(t *testing.T) {
	b, err := templateFS.ReadFile("templates/dashboard.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(b)
	for _, id := range enrichMissesElementIDs {
		if !strings.Contains(html, `id="`+id+`"`) {
			t.Errorf("dashboard.html has no element with id=%q; app.js looks it up by that id "+
				"and silently does nothing when it is absent", id)
		}
	}
}

// TestEnrichMissesElementsReferencedInJS pins the other half — so
// deleting the markup without deleting its reader, or vice versa, is
// caught from whichever side the change starts on.
func TestEnrichMissesElementsReferencedInJS(t *testing.T) {
	b, err := staticFS.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(b)
	for _, id := range enrichMissesElementIDs {
		if !strings.Contains(js, `"`+id+`"`) {
			t.Errorf("app.js never references id %q, but dashboard.html declares it — "+
				"either the markup is dead or its reader was renamed", id)
		}
	}
}

// TestEnrichMissesFacetLabelsCoverThePredicate pins the JS label maps
// against the facet keys the SERVER emits.
//
// manifest.MissFacets is a lockstep mirror of enrichmentMissPredicateSQL,
// and the drill-down iterates its own label map to decide which sections
// to draw. So a new facet added server-side would be counted in the
// totals and silently omitted from the breakdown — the panel would say
// "6,909 tracks are short" and then account for fewer than that, which is
// precisely the "one aggregate number nobody can decompose" problem this
// whole surface exists to fix.
func TestEnrichMissesFacetLabelsCoverThePredicate(t *testing.T) {
	b, err := staticFS.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(b)
	// The server-side truth. Kept as literals rather than importing the
	// constants so a rename shows up here as a failure to reconcile
	// rather than compiling away silently.
	for _, facet := range []string{"artwork", "artist", "release"} {
		for _, m := range []string{"ENRICH_MISS_FACET_LABELS", "ENRICH_MISS_FACET_NOUNS"} {
			block, ok := jsObjectLiteral(js, m)
			if !ok {
				t.Fatalf("could not find the %s object literal in app.js", m)
			}
			if !strings.Contains(block, facet+":") {
				t.Errorf("%s has no entry for facet %q; the server counts it, so the panel "+
					"would report a total it then fails to account for", m, facet)
			}
		}
	}
}

// jsObjectLiteral returns the body of `const <name> = { … }`.
//
// Whitespace-tolerant on the declaration and brace-counted to the close,
// rather than scanning for a literal `const X = {` and the next `};`.
// The strict form failed on reformatting — an extra space, or the
// declaration wrapped across lines — and while that surfaced as a loud
// t.Fatalf rather than a silent pass, a guard test that breaks on
// unrelated formatting gets deleted rather than fixed.
func jsObjectLiteral(js, name string) (string, bool) {
	decl := regexp.MustCompile(`(?s)\bconst\s+` + regexp.QuoteMeta(name) + `\s*=\s*\{`)
	loc := decl.FindStringIndex(js)
	if loc == nil {
		return "", false
	}
	// Count from the opening brace the declaration ended on. These maps
	// hold flat string values with no braces of their own; a nested
	// object would still be balanced, and a brace inside a string
	// literal would not be — which is why the doc above says what shape
	// this expects.
	depth := 0
	for i := loc[1] - 1; i < len(js); i++ {
		switch js[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return js[loc[1]:i], true
			}
		}
	}
	return "", false
}

// TestJSObjectLiteralToleratesFormatting pins the helper against the
// reformattings that broke its first version, so "whitespace-tolerant"
// is a tested property rather than a claim in a comment.
func TestJSObjectLiteralToleratesFormatting(t *testing.T) {
	tests := []struct {
		name string
		js   string
	}{
		{"canonical", `const M = {
  a: "x",
};`},
		{"extra spaces around =", `const  M   =   {
  a: "x",
};`},
		{"declaration wrapped across lines", `const M =
{
  a: "x",
};`},
		{"single line", `const M = { a: "x" };`},
		{"no trailing semicolon", `const M = { a: "x" }`},
		{"preceded by a similarly-named const", `const MOTHER = { b: "y" };
const M = { a: "x" };`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body, ok := jsObjectLiteral(tc.js, "M")
			if !ok {
				t.Fatalf("jsObjectLiteral did not find M in:\n%s", tc.js)
			}
			if !strings.Contains(body, `a:`) {
				t.Errorf("body %q does not contain the entry it should", body)
			}
			if strings.Contains(body, `b:`) {
				t.Errorf("body %q leaked an entry from a different literal", body)
			}
		})
	}
	if _, ok := jsObjectLiteral(`const M = { a: "x" };`, "NOPE"); ok {
		t.Error("found a literal that isn't there; a false positive here would make the facet test vacuous")
	}
	// Unbalanced input must report not-found rather than running to the
	// end of the file and returning everything after the declaration.
	if _, ok := jsObjectLiteral(`const M = { a: "x"`, "M"); ok {
		t.Error("an unterminated literal must not be reported as found")
	}
}
