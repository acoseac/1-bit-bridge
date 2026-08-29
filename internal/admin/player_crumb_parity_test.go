package admin

import (
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestCrumbRootsAreRealSections pins every breadcrumb root in views.js to a
// section the router actually serves.
//
// A crumb's whole job is to be clicked. The click is intercepted by the
// delegated data-route handler in boot.js, which pushes the href and calls
// route() — so an href the player does not own is dispatched by route()'s
// silent `routes[section] || routes.albums` fallback and quietly renders the
// album grid, or (for a path outside PLAYER_HEADS entirely) falls through to a
// full page load, which stops playback. Both failures look like a working link
// until someone presses it.
//
// The roots live in one table (CRUMB_ROOTS) precisely so this test can read
// them; the risk it guards is a section route being renamed in SECTIONS
// without the table following, which nothing else in the suite would notice.
func TestCrumbRootsAreRealSections(t *testing.T) {
	views, err := staticFS.ReadFile("static/player/views.js")
	if err != nil {
		t.Fatal(err)
	}
	boot, err := staticFS.ReadFile("static/player/boot.js")
	if err != nil {
		t.Fatal(err)
	}

	roots := parseCrumbRoots(t, string(views))
	if len(roots) == 0 {
		t.Fatal("no CRUMB_ROOTS entries parsed from views.js — the table moved or " +
			"changed shape, and this test is no longer checking anything")
	}
	sections := parseSectionHrefs(t, string(boot))
	if len(sections) == 0 {
		t.Fatal("no SECTIONS entries parsed from boot.js")
	}

	for _, key := range crumbRootKeys(roots) {
		if !sections[roots[key]] {
			t.Errorf("CRUMB_ROOTS.%s points at %q, which is not a section route in "+
				"boot.js's SECTIONS (%s). A crumb link to a path the router does not "+
				"own renders the album grid or hard-loads the page, either of which "+
				"reads as a working link until it is pressed.",
				key, roots[key], sortedKeys(sections))
		}
	}
}

// crumbRootRe matches one `key: { label: "…", href: "/…" },` entry.
var crumbRootRe = regexp.MustCompile(`(?m)^\s*(\w+):\s*\{\s*label:\s*"[^"]*",\s*href:\s*"([^"]*)"\s*\}`)

func parseCrumbRoots(t *testing.T, src string) map[string]string {
	t.Helper()
	body, ok := literalBlock(src, "const CRUMB_ROOTS = {")
	if !ok {
		t.Fatal("CRUMB_ROOTS literal not found in views.js")
	}
	out := map[string]string{}
	for _, m := range crumbRootRe.FindAllStringSubmatch(body, -1) {
		out[m[1]] = m[2]
	}
	return out
}

// sectionHrefRe matches the third element of a `["key", "Label", "/href"]`
// SECTIONS row.
var sectionHrefRe = regexp.MustCompile(`\[\s*"[^"]*"\s*,\s*"[^"]*"\s*,\s*"([^"]*)"\s*\]`)

func parseSectionHrefs(t *testing.T, src string) map[string]bool {
	t.Helper()
	body, ok := literalBlock(src, "const SECTIONS = [")
	if !ok {
		t.Fatal("SECTIONS literal not found in boot.js")
	}
	out := map[string]bool{}
	for _, m := range sectionHrefRe.FindAllStringSubmatch(body, -1) {
		out[m[1]] = true
	}
	return out
}

// literalBlock returns the source between `open` and the line that closes it
// at column zero — enough structure for these two flat literals, and it stops
// a stray brace elsewhere in the file from being read as part of the table.
func literalBlock(src, open string) (string, bool) {
	i := strings.Index(src, open)
	if i < 0 {
		return "", false
	}
	rest := src[i+len(open):]
	closer := "\n};"
	if strings.HasSuffix(open, "[") {
		closer = "\n];"
	}
	j := strings.Index(rest, closer)
	if j < 0 {
		return "", false
	}
	return rest[:j], true
}

// crumbRootKeys is the local sibling of the package's existing
// sortedKeys(map[string]bool): same job, different map value type.
func crumbRootKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
