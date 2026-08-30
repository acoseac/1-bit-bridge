package admin

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
)

// A class in a template that no stylesheet defines renders as nothing.
//
// It has now happened three times, and each one shipped and was found by
// looking at the page rather than by a test:
//
//	.error        four elements across three templates, no rule anywhere,
//	              so every error message read as ordinary body text
//	.hint.warn    an inert class COMBINATION — `.hint` existed, `.hint.warn`
//	              did not — so the upload warning read as a normal hint
//	.page-lead    upload.html was the only page not using `.lede`, so the
//	              Add-music intro rendered in body ink at body size while
//	              every sibling page's intro was muted and spaced
//
// The first two got a test each. This is the generalisation, because a
// fourth one-off test would still not have caught the third: they assert
// specific classes someone already knew about.
//
// The rule: every class a template renders must either have a rule in
// app.css / player.css, or be listed below as a non-visual hook with the
// reason. A class with no rule is not automatically a defect — plenty are
// pure JS query targets or page-scope markers — but it has to be a
// DECISION, which is exactly what was missing all three times.
//
// Both stylesheets count, because layout.html loads player.css on every
// page (the now-playing bar persists across all of them), so a class
// defined there is live everywhere. Comments are stripped first: this
// repo's CSS comments name the classes they discuss, and a substring scan
// would read prose as a definition — the trap that made an earlier version
// of the sibling warn-hint test pass against the very bug it was written
// for.
var templateClassNonVisualHooks = map[string]string{
	// Page-scope markers on <section class="page …">. They exist so a
	// per-page rule CAN be written and so handlers_partial_test.go can
	// assert which template rendered; several have no rule today.
	"dashboard":        "page-scope marker",
	"devices":          "page-scope marker",
	"library":          "page-scope marker",
	"composition":      "page-scope marker + JS hook (app.js reads it)",
	"diagnostics-page": "page-scope marker; handlers_partial_test.go asserts on it",
	"duplicates-page":  "page-scope marker + JS hook",
	"page-upload":      "page-scope marker",

	// JS query hooks — app.js / the player modules find elements by these.
	"analysis-stats":   "JS hook: refreshAnalysisStats",
	"expires-cell":     "JS hook: token expiry rendering",
	"export-history":   "JS hook: history export buttons",
	"remove-root":      "JS hook: delegated click on the roots list",
	"revoke-token":     "JS hook: delegated click on the devices list",
	"rotate-token":     "JS hook: delegated click on the devices list",
	"set-expiry-token": "JS hook: delegated click on the devices list",
	"player-view":      "JS hook: the player shell's view mount",
	"player-toolbar":   "JS hook: the player shell's toolbar mount",

	// Grouping elements whose styling comes from a sibling class on the
	// same element, or from the parent's layout.
	"kv":                 "grouping div; .kv-label/.kv-row/.kv-note are styled and .pair-fields grids them",
	"jobs-meta":          `paired with "panel", which carries the styling`,
	"telemetry-panel":    `paired with "panel", which carries the styling`,
	"telemetry-group":    "wrapper around .panel children",
	"settings-sections":  "wrapper around the settings tab panes",
	"upscale-target-row": `paired with "field", which carries the styling`,
	"upscale-clear-row":  "block wrapper around a button + hint",
}

var templateClassAttrRe = regexp.MustCompile(`class="([^"{}]*)"`)

// cssDefinedClasses returns every class name any selector in the given
// stylesheets mentions, with comments stripped first.
func cssDefinedClasses(t *testing.T, paths ...string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		css := uploadCSSCommentRe.ReplaceAllString(string(b), "")
		for _, m := range regexp.MustCompile(`\.([A-Za-z][A-Za-z0-9_-]*)`).
			FindAllStringSubmatch(css, -1) {
			out[m[1]] = true
		}
	}
	if len(out) < 100 {
		t.Fatalf("only %d classes scraped from the stylesheets — the scan has "+
			"stopped matching, which would make this pass while checking nothing",
			len(out))
	}
	return out
}

func TestEveryTemplateClassIsStyledOrADeclaredHook(t *testing.T) {
	defined := cssDefinedClasses(t, "static/app.css", "static/player.css")

	names, err := fs.Glob(templateFS, "templates/*.html")
	if err != nil {
		t.Fatal(err)
	}
	if len(names) < 10 {
		t.Fatalf("only %d templates found — the glob has stopped matching", len(names))
	}

	// class -> the templates that render it, for a failure message that
	// says where to look.
	where := map[string][]string{}
	for _, name := range names {
		b, err := templateFS.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for _, m := range templateClassAttrRe.FindAllStringSubmatch(string(b), -1) {
			for _, cls := range strings.Fields(m[1]) {
				if !regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]*$`).MatchString(cls) {
					continue // a Go-template expression, not a literal class
				}
				if defined[cls] || templateClassNonVisualHooks[cls] != "" {
					continue
				}
				base := filepath.Base(name)
				if !slices.Contains(where[cls], base) {
					where[cls] = append(where[cls], base)
				}
			}
		}
	}

	var missing []string
	for cls := range where {
		missing = append(missing, cls)
	}
	sort.Strings(missing)
	for _, cls := range missing {
		sort.Strings(where[cls])
		t.Errorf("templates render class=%q (%s) but neither app.css nor player.css "+
			"defines it, and it is not a declared hook.\n"+
			"If it is meant to be seen, it is inert and renders as nothing — that is "+
			"the .error / .hint.warn / .page-lead defect a third time.\n"+
			"If it is a JS query target or a page-scope marker, add it to "+
			"templateClassNonVisualHooks WITH the reason.",
			cls, strings.Join(where[cls], ", "))
	}
}
