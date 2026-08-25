package admin

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// `.track` is a CSS grid and trackRow's child count is CONDITIONAL: it
// appends an extra "why this can't play" chip only for a track the
// browser cannot decode. Nothing connects the two numbers, and when they
// disagree the failure is silent in the worst way — the row still
// renders, it just quietly wraps its last cell onto a second line.
//
// That is not hypothetical. The template declared seven columns while an
// unplayable track rendered eight children, so every DSD track put its
// Download link on a line of its own, left-aligned under the track
// number, doubling the row from ~36px to 72px. A whole DSD album was
// twice the height it should be, and it shipped that way (field-reported
// on bridge.ars.md against "Bitches Brew", 6 of 6 rows).
//
// The fix was to stop hardcoding the count — `grid-auto-flow: column`
// makes the trailing columns implicit, so the row no longer cares how
// many trailing items there are. This test allows either shape: implicit
// columns, or an explicit template with at least as many columns as
// trackRow can ever append.

// trackAppendRe counts the appends in trackRow's body. Both the
// unconditional calls and the one inside `if (!playable)` match, which is
// what makes the count a MAXIMUM rather than a typical case.
var trackAppendRe = regexp.MustCompile(`(?m)^\s*row\.appendChild\(`)

// baseTrackRuleRe finds the `.track { … }` rule at column 0 — the base
// rule, deliberately NOT the indented one inside the max-width media
// query, which drives an intentional wrap with its own spans.
var baseTrackRuleRe = regexp.MustCompile(`(?m)^\.track \{[^}]*\}`)

func TestTrackRowGridHoldsEveryConditionalChild(t *testing.T) {
	// Normalized to LF before any scanning: a Windows checkout carries
	// CRLF and there is no .gitattributes pinning eol. The sibling
	// page-init parity test failed on windows-latest for exactly that
	// reason from the day it was added.
	views := readNormalized(t, "static/player/views.js")
	css := readNormalized(t, "static/player.css")

	start := strings.Index(views, "function trackRow(")
	if start < 0 {
		t.Fatal("trackRow not found in views.js — this test has stopped checking anything")
	}
	end := strings.Index(views[start:], "\n}\n")
	if end < 0 {
		t.Fatal("could not find the end of trackRow")
	}
	maxChildren := len(trackAppendRe.FindAllString(views[start:start+end], -1))
	if maxChildren < 6 {
		t.Fatalf("only %d appends scraped from trackRow — the regex has stopped matching, "+
			"which would make this test pass while checking nothing", maxChildren)
	}

	rule := baseTrackRuleRe.FindString(css)
	if rule == "" {
		t.Fatal("no base `.track { … }` rule found in player.css")
	}

	// Implicit trailing columns: the count cannot disagree because there
	// is no count. This is the shape the fix uses.
	if strings.Contains(rule, "grid-auto-flow: column") {
		return
	}

	cols := declaredColumnCount(rule)
	if cols == 0 {
		t.Fatalf("`.track` declares neither `grid-auto-flow: column` nor a readable "+
			"`grid-template-columns`; rule was:\n%s", rule)
	}
	if cols < maxChildren {
		t.Errorf("`.track` declares %d columns but trackRow appends up to %d children — "+
			"the extra child wraps to a second row and the last cell lands under the "+
			"track number. Either widen the template or use `grid-auto-flow: column`.",
			cols, maxChildren)
	}
}

// declaredColumnCount counts track-list entries in a
// `grid-template-columns` declaration. Returns 0 when the declaration is
// absent or uses a function form this simple count cannot read
// (repeat(), minmax() with an inner space), so the caller can fail loudly
// rather than silently comparing against a wrong number.
func declaredColumnCount(rule string) int {
	i := strings.Index(rule, "grid-template-columns:")
	if i < 0 {
		return 0
	}
	decl := rule[i+len("grid-template-columns:"):]
	if j := strings.IndexAny(decl, ";}"); j >= 0 {
		decl = decl[:j]
	}
	if strings.ContainsAny(decl, "(") {
		return 0 // repeat()/minmax() — not countable by whitespace
	}
	return len(strings.Fields(decl))
}

func readNormalized(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return strings.ReplaceAll(string(b), "\r\n", "\n")
}
