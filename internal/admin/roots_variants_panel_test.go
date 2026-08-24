package admin

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The Roots page's transcoded-cache panel is server-rendered markup
// driven entirely by ids resolved at runtime in app.js. Nothing in Go
// links the two, so a rename on either side fails silently: the JS
// resolves null, every guard returns early, and the panel simply does
// nothing — no error, no console noise, no failing test.
//
// That is the same failure the settings-form mapping test exists for,
// and it has already happened once on this page: the Inspector's
// storage bar and its JS drifted through several renames before this
// code moved here.
// Matching any `"variants-…"` STRING LITERAL, not just the argument of
// a getElementById call, is load-bearing. An earlier version of this
// regexp anchored on getElementById and therefore covered a handful of
// the panel's ids while appearing to cover all of them: most of the
// wiring goes through the shared setText(id, …) helper, so renaming
// `variants-free` in the template left this test green. A guard that
// reports on a fraction of its subject is worse than no guard.
var variantsPanelIDRe = regexp.MustCompile(`"(variants-[a-z0-9-]+)"`)

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// TestVariantsPanelIDsExistInTheTemplate pins the JS → template
// direction: every id app.js reaches for must be in library.html.
func TestVariantsPanelIDsExistInTheTemplate(t *testing.T) {
	js := readFile(t, "static/app.js")
	tmpl := readFile(t, "templates/library.html")

	present := map[string]bool{}
	for _, id := range idsIn(tmpl) {
		present[id] = true
	}

	var missing []string
	for _, m := range variantsPanelIDRe.FindAllStringSubmatch(js, -1) {
		if !present[m[1]] {
			missing = append(missing, m[1])
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("app.js resolves ids that library.html does not define: %v\n"+
			"The panel fails SILENTLY when this drifts — every guard returns early "+
			"and nothing reports an error.", missing)
	}
}

// TestVariantsPanelTemplateIDsAreUsed pins the other direction. A
// control rendered but never wired is worse than one that is absent:
// it looks operable, and clicking it does nothing at all.
func TestVariantsPanelTemplateIDsAreUsed(t *testing.T) {
	js := readFile(t, "static/app.js")
	tmpl := readFile(t, "templates/library.html")

	// Ids that exist for CSS or for the accessible name rather than for
	// JS. Each needs a reason, so that adding to this list is a
	// decision rather than a way to silence the test. Empty today.
	exempt := map[string]string{}

	var unused []string
	for _, id := range idsIn(tmpl) {
		if !strings.HasPrefix(id, "variants-") {
			continue
		}
		if _, ok := exempt[id]; ok {
			continue
		}
		if !strings.Contains(js, `"`+id+`"`) {
			unused = append(unused, id)
		}
	}
	if len(unused) > 0 {
		sort.Strings(unused)
		t.Errorf("library.html defines variant-panel ids nothing in app.js touches: %v\n"+
			"A control that renders but is never wired reads as broken, not as absent.", unused)
	}
}

// TestVariantsClearRequiresAnExactPhrase reads the guard out of the
// source, because it is the only thing standing between a click and
// every generated file on the bridge.
//
// A prefix or case-insensitive match would defeat the point — that is
// exactly what made the old bare [y/N] uninstall prompt a fat-finger
// hazard, and why actInstallService and actUninstall both demand a
// literal typed phrase.
func TestVariantsClearRequiresAnExactPhrase(t *testing.T) {
	js := readFile(t, "static/app.js")
	for _, want := range []string{
		`phrase.value !== "CLEAR"`, // the input handler's gate
	} {
		if !strings.Contains(js, want) {
			t.Errorf("the whole-library clear no longer gates on %s — a looser match "+
				"turns a typed confirmation into a fat-finger hazard", want)
		}
	}
	// The click handler must re-check rather than trusting the button's
	// disabled state: disabled is a UI affordance, and a stray
	// programmatic click bypasses it entirely.
	if strings.Count(js, `phrase.value !== "CLEAR"`) < 2 {
		t.Error("only one CLEAR check found; the click handler must re-verify the " +
			"phrase rather than trusting the button's disabled attribute")
	}
}
