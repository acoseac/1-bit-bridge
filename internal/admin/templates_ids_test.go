package admin

import (
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// idAttrRe finds every literal id attribute in a template's SOURCE bytes.
//
// Deliberately naive, because that is exactly the view a static HTML analyser
// takes: it does not parse Go template actions, so it cannot tell that two
// occurrences sit in mutually exclusive {{if}}/{{else}} arms, and it does not
// treat {{/* ... */}} as a comment — to an HTML parser that is ordinary text,
// so markup quoted inside one is parsed as a real element.
//
// Both quote styles are accepted. Every id in the tree today is
// double-quoted, so the single-quoted arm matches nothing right now — it is
// there because the analyser this mirrors parses HTML properly and would
// catch `id=\'x\'`, and a guard that misses what the gate catches is worse
// than no guard: it reports green and the build still fails.
var idAttrRe = regexp.MustCompile(`\bid\s*=\s*(?:"([a-zA-Z0-9_:.-]+)"|'([a-zA-Z0-9_:.-]+)')`)

// idsIn returns every id attribute value in src, in order.
func idsIn(src string) []string {
	var out []string
	for _, m := range idAttrRe.FindAllStringSubmatch(src, -1) {
		// Exactly one of the two quote-style groups matches.
		if m[1] != "" {
			out = append(out, m[1])
		} else {
			out = append(out, m[2])
		}
	}
	return out
}

// TestNoTemplateRepeatsAnID is the local mirror of SonarCloud's Web:S7930
// ("Duplicate id found"), which is a RELIABILITY-rated bug and therefore fails
// the quality gate outright — the gate demands a new_reliability_rating of A.
//
// It reads template SOURCE rather than rendered output, and that is the whole
// point: the defect class this guards is an id repeated across exclusive
// template branches, of which exactly one ever renders. A test that fetched a
// page and scanned the served HTML would find nothing and pass forever while
// the gate stayed red. (A rendered-page check is still worth having for the
// different, cross-template composition case — see the sibling test below.)
//
// Two failure modes it has already caught, both of which cost a CI round:
//
//   - settings.html emitted id="update-latest" and id="update-notes" once per
//     branch across five arms of the update-status block.
//   - A template comment explaining that fix quoted the offending markup
//     literally, so the analyser parsed the prose as an element and counted
//     its id. Refer to ids in prose (update-latest), never as attributes.
//
// If a genuine need for a repeated id ever appears, it does not exist: ids are
// unique per document by definition, and every consumer here resolves them
// with getElementById, which returns only the first.
func TestNoTemplateRepeatsAnID(t *testing.T) {
	names, err := fs.Glob(templateFS, "templates/*.html")
	if err != nil {
		t.Fatal(err)
	}
	if len(names) == 0 {
		t.Fatal("no templates matched — the embed pattern moved and this guard is now vacuous")
	}

	for _, name := range names {
		src, err := fs.ReadFile(templateFS, name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		seen := map[string]int{}
		for _, id := range idsIn(string(src)) {
			seen[id]++
		}
		var dups []string
		for id, n := range seen {
			if n > 1 {
				dups = append(dups, fmt.Sprintf("%s (%d occurrences)", id, n))
			}
		}
		if len(dups) > 0 {
			sort.Strings(dups)
			t.Errorf("%s repeats an id: %s\n"+
				"Exclusive template branches do not help — a static HTML analyser "+
				"cannot see through them, and SonarCloud fails the build on it "+
				"(Web:S7930, a reliability bug). Emit the element once and make the "+
				"varying part a conditional attribute, as layout.html does for "+
				"aria-current. If an occurrence is inside a {{/* comment */}}, that "+
				"still counts: rewrite it to name the id in prose.",
				name, strings.Join(dups, ", "))
		}
	}
}

// TestRenderedPagesHaveNoDuplicateIDs is the composition half of the guard
// above. Each page is assembled from layout.html plus one content template, so
// two files that are individually clean can still collide once rendered
// together — a case the per-file scan structurally cannot see.
//
// It is deliberately NOT a replacement for the source scan: only one arm of an
// {{if}}/{{else}} ever renders, so the repeated-across-branches defect is
// invisible here. The two tests cover disjoint halves of the same rule.
func TestRenderedPagesHaveNoDuplicateIDs(t *testing.T) {
	srv, _, _ := newTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// Every page route registered in Handler(), plus the two player
	// sub-routes that render the same shell through a different path.
	pages := append([]string{
		"/", "/stats", "/library", "/library/inspector", "/library/duplicates",
		"/jobs", "/devices", "/upnp", "/data", "/smartmixes", "/settings",
		"/diagnostics", "/login",
	}, playerRoutes...)

	for _, p := range pages {
		resp, err := http.Get(ts.URL + p)
		if err != nil {
			t.Fatalf("GET %s: %v", p, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s: status %d, want 200", p, resp.StatusCode)
			continue
		}
		seen := map[string]int{}
		for _, id := range idsIn(string(body)) {
			seen[id]++
		}
		var dups []string
		for id, n := range seen {
			if n > 1 {
				dups = append(dups, fmt.Sprintf("%s (%d)", id, n))
			}
		}
		if len(dups) > 0 {
			sort.Strings(dups)
			t.Errorf("rendered %s contains duplicate ids: %s\n"+
				"Both halves of the page are individually clean, so this is a "+
				"collision between layout.html and the content template. "+
				"getElementById resolves only the first, so one of the two is dead.",
				p, strings.Join(dups, ", "))
		}
	}
}
