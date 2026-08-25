package admin

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The player module builds every node in JavaScript, so nothing connects
// a class it emits to a rule that styles it. A typo, a rename in either
// stylesheet, or a borrowed class disappearing from app.css all fail the
// same silent way: the markup is correct, the page just renders wrong,
// and only a human looking at the right screen notices.
//
// This is not hypothetical. `.small` rode ~25 nodes across the module
// from the day it was written — every stat line, every variant note, the
// About attribution — and had a rule in NEITHER stylesheet, so all of
// them rendered at the body size while asking to be secondary.
//
// The borrow direction matters too: the detail tabs deliberately reuse
// app.css's `.tab-btn` (the Settings page's tab idiom) rather than
// restyling it, which makes an app.css-only class load-bearing for a
// file that does not mention it.

var (
	// class: "a b c" literals. Template literals are skipped on purpose:
	// their interpolated half can't be resolved statically, and the
	// static half is always a class this catches at another call site.
	//
	// Both quote styles, though the codebase uses double throughout: a
	// single-quoted literal this failed to see would be a class the test
	// silently stopped checking, which is the direction that matters. The
	// open and close quotes are not tied together — RE2 has no
	// backreferences — so a mismatched pair would match; harmless here,
	// since it is not valid JavaScript in the first place.
	classLiteralRe = regexp.MustCompile(`class:\s*["']([^"'$` + "`" + `]+)["']`)
	// classList.add / toggle / remove("x")
	classListRe = regexp.MustCompile(`classList\.(?:add|toggle|remove)\(["']([^"'$` + "`" + `]+)["']`)
	// Any .name appearing in a stylesheet — selectors, not properties.
	cssClassRe = regexp.MustCompile(`\.([A-Za-z][\w-]*)`)
	// Comments are stripped BEFORE the scan. This file's own commentary
	// names the classes it discusses, and a comment mentioning .tab-btn
	// read as a definition of it — which made the borrow this test exists
	// to notice look locally defined, i.e. a false pass in the one
	// direction that matters.
	cssCommentRe = regexp.MustCompile(`(?s)/\*.*?\*/`)
)

// layoutOnlyClasses are emitted as structural hooks with no rule of their
// own, and are expected to stay that way. Each is a child of a container
// whose own rule positions it (a grid or flex item), so a rule here would
// be an empty block. Add to this list only with a reason.
var layoutOnlyClasses = map[string]string{
	"detail-head":        "grid child of .detail — positioned by the parent's template",
	"detail-artist-head": "modifier on .detail for the round-portrait variant",
	"tabpanels":          "plain block wrapper; the panels inside carry the styling",
	"track-why": "grid cell for the unplayable reason — always emitted so every " +
		"row has the same cell count and the list can share one grid; the chip " +
		"inside carries the styling, and an empty one must collapse to nothing",
}

func TestPlayerEmittedClassesAreStyled(t *testing.T) {
	emitted := emittedPlayerClasses(t)
	if len(emitted) < 40 {
		t.Fatalf("only %d classes scraped from the player module — the extraction "+
			"regexes have probably stopped matching, which would make this test "+
			"pass while checking nothing", len(emitted))
	}

	player := cssClasses(t, "static/player.css")
	app := cssClasses(t, "static/app.css")

	var missing, borrowed []string
	for _, c := range emitted {
		if _, ok := layoutOnlyClasses[c]; ok {
			continue
		}
		switch {
		case player[c]:
			// Styled by the player's own sheet — nothing to say.
		case app[c]:
			borrowed = append(borrowed, c)
		default:
			missing = append(missing, c)
		}
	}
	sort.Strings(missing)
	sort.Strings(borrowed)

	if len(missing) > 0 {
		t.Errorf("player JS emits classes that no stylesheet defines: %v\n"+
			"Each renders unstyled — the markup is right and the page is wrong, "+
			"which is invisible in review. Either add a rule, or add the class to "+
			"layoutOnlyClasses WITH the reason it needs none.", missing)
	}

	// Not an assertion, a record: these are the classes the player takes
	// from app.css, so a rename or deletion there breaks a file that never
	// mentions it. Logged so the set is visible when this test runs.
	if len(borrowed) > 0 {
		t.Logf("borrowed from app.css (renaming any of these breaks the player): %v", borrowed)
	}
}

func emittedPlayerClasses(t *testing.T) []string {
	t.Helper()
	set := map[string]bool{}
	dir := filepath.Join("static", "player")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".js") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		src := string(b)
		for _, m := range classLiteralRe.FindAllStringSubmatch(src, -1) {
			for _, tok := range strings.Fields(m[1]) {
				set[tok] = true
			}
		}
		for _, m := range classListRe.FindAllStringSubmatch(src, -1) {
			set[m[1]] = true
		}
	}
	out := make([]string, 0, len(set))
	for c := range set {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

func cssClasses(t *testing.T, path string) map[string]bool {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	set := map[string]bool{}
	body := cssCommentRe.ReplaceAllString(string(b), " ")
	for _, m := range cssClassRe.FindAllStringSubmatch(body, -1) {
		set[m[1]] = true
	}
	return set
}
