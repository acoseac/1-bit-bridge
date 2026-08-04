package admin

import (
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

// jsObjectLiteral returns the text between `const <name> = {` and its
// closing brace. Good enough for a flat literal of string values, which
// is all these maps are.
func jsObjectLiteral(js, name string) (string, bool) {
	start := strings.Index(js, "const "+name+" = {")
	if start < 0 {
		return "", false
	}
	rest := js[start:]
	end := strings.Index(rest, "};")
	if end < 0 {
		return "", false
	}
	return rest[:end], true
}
