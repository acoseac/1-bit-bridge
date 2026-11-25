package admin

import (
	"regexp"
	"strings"
	"testing"
)

// The console by touch (B1021): what a phone's bar and pages need of
// player.css, pinned at the source on every host. The node harness runs the
// modules (TestTheBarCarriesItsSeekAndBothTimesAndSaysHowTallItIs and the
// album cases beside it); what the rules do in a real engine, at each width,
// is the record's WebKit evidence, which no Go guard can see.

// touchRule is one rule of a stylesheet block: its selector list and its
// declarations.
type touchRule struct{ sel, decl string }

// touchRulesOf is every rule in block (comments stripped first), a
// nested block's own rules included as they read.
func touchRulesOf(block string) []touchRule {
	var out []touchRule
	for _, m := range regexp.MustCompile(`([^{}]+)\{([^{}]*)\}`).FindAllStringSubmatch(block, -1) {
		out = append(out, touchRule{strings.TrimSpace(m[1]), m[2]})
	}
	return out
}

// selects reports whether the rule's selector list holds sel itself, one
// selector of the list.
func (r touchRule) selects(sel string) bool {
	for _, s := range strings.Split(r.sel, ",") {
		if strings.TrimSpace(s) == sel {
			return true
		}
	}
	return false
}

// declares reports whether the rule sets prop to value, white space aside.
func (r touchRule) declares(prop, value string) bool {
	norm := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	for _, d := range strings.Split(r.decl, ";") {
		k, v, ok := strings.Cut(d, ":")
		if ok && strings.TrimSpace(k) == prop && norm(v) == norm(value) {
			return true
		}
	}
	return false
}

// playerCSSBlocks is player.css's @media blocks for query, comments
// stripped.
func playerCSSBlocks(t *testing.T, query string) []touchRule {
	t.Helper()
	css := cssCommentRe.ReplaceAllString(readFile(t, "static/player.css"), "")
	var rules []touchRule
	for _, b := range cssMediaBlocks(css, query) {
		rules = append(rules, touchRulesOf(b)...)
	}
	if len(rules) == 0 {
		t.Fatalf("no rule found in player.css's %s blocks: the reader has stopped finding them", query)
	}
	return rules
}

// anyRule reports whether a rule of rules selects sel and sets prop to value.
func anyRule(rules []touchRule, sel, prop, value string) bool {
	for _, r := range rules {
		if r.selects(sel) && r.declares(prop, value) {
			return true
		}
	}
	return false
}

// TestThePhoneBarShowsItsSeekItsTimesAndItsWords pins B1021's items 1 and 2
// at the source: up to 768px the scrub row (the seek and both times) is a
// row of the bar of its own, hidden by no rule there, its seek 44px tall;
// up to 640px the words take a row the transport does not share, and the
// bar's buttons there are at least 44px square.
func TestThePhoneBarShowsItsSeekItsTimesAndItsWords(t *testing.T) {
	tablet := playerCSSBlocks(t, "(max-width: 768px)")
	for _, r := range tablet {
		if r.selects(".np-scrub") && r.declares("display", "none") {
			t.Errorf("up to 768px a rule hides the scrub row (%s): a phone has no seek and no times", r.sel)
		}
	}
	if !anyRule(tablet, ".np-scrub", "grid-area", "scrub") || !anyRule(tablet, ".np-center", "display", "contents") {
		t.Error("up to 768px the scrub row is not a row of the bar of its own (.np-center's contents, .np-scrub's grid area)")
	}
	if !anyRule(tablet, ".np-seek", "height", "44px") {
		t.Error("up to 768px the seek is not 44px tall: a finger has nothing to land on")
	}
	phone := playerCSSBlocks(t, "(max-width: 640px)")
	areas := ""
	for _, r := range phone {
		if r.selects(".npbar") {
			for _, d := range strings.Split(r.decl, ";") {
				if k, v, ok := strings.Cut(d, ":"); ok && strings.TrimSpace(k) == "grid-template-areas" {
					areas = v
				}
			}
		}
	}
	rows := regexp.MustCompile(`"([^"]*)"`).FindAllStringSubmatch(areas, -1)
	words := false
	for _, row := range rows {
		cells := strings.Fields(row[1])
		if strings.Contains(row[1], "now") {
			words = !strings.Contains(row[1], "ctl") && cells[0] == "now"
		}
	}
	if len(rows) < 3 || !words {
		t.Errorf("up to 640px the words do not take a row the transport leaves them (grid-template-areas %q)", strings.TrimSpace(areas))
	}
	if !anyRule(phone, ".np-btn", "min-width", "44px") || !anyRule(phone, ".np-btn", "min-height", "44px") ||
		!anyRule(phone, ".np-btn-sm", "min-width", "44px") || !anyRule(phone, ".np-btn-sm", "min-height", "44px") {
		t.Error("up to 640px the bar's buttons, which its rows move, are not at least 44px square")
	}
}

// TestWhatSitsAboveTheBarClearsItAsItRenders pins at the source that main's
// bottom padding, the output menu and the queue panel clear the bar by the
// height it renders at (--np-bar-h, which nowplaying.js sets from the bar's
// own box as it changes) where --bar-h alone stayed 60px under a bar of three
// rows, and that the A–Z rail fits between its sticky top and that bar, its
// letters shrinking to fit (B1021's item 3).
func TestWhatSitsAboveTheBarClearsItAsItRenders(t *testing.T) {
	css := cssCommentRe.ReplaceAllString(readFile(t, "static/player.css"), "")
	all := touchRulesOf(css)
	inset := regexp.MustCompile(`--player-bottom-inset:\s*calc\(var\(--np-bar-h,\s*var\(--bar-h\)\)`)
	if !inset.MatchString(css) {
		t.Error("--player-bottom-inset does not read the bar's rendered height (--np-bar-h)")
	}
	panels := false
	for _, r := range all {
		if r.selects(".np-menu") && r.selects(".np-queue") && strings.Contains(r.decl, "var(--np-bar-h, var(--bar-h))") {
			panels = true
		}
	}
	if !panels {
		t.Error("the output menu and the queue panel do not sit above the bar's rendered height")
	}
	rail := false
	for _, r := range all {
		if r.selects("body.has-player-bar .az-rail") && strings.Contains(r.decl, "max-height") && strings.Contains(r.decl, "var(--player-bottom-inset)") {
			rail = true
		}
	}
	if !rail || !anyRule(all, ".az-letter", "min-height", "0") {
		t.Error("the A–Z rail has no height that stops above the bar, or its letters cannot shrink to fit it")
	}
	np := stripJSComments(readFile(t, "static/player/nowplaying.js"))
	mount := extractJSFunction(t, np, "mount")
	if !strings.Contains(mount, "new ResizeObserver(") || !strings.Contains(mount, `setProperty("--np-bar-h", `) || !strings.Contains(mount, "bar.offsetHeight") {
		t.Error("mount does not hand the bar's rendered height (--np-bar-h) to the page as the bar changes size")
	}
}

// TestTheDSDChipWrapsUnderItsTitleOnAPhone pins B1021's item 5 and the
// count line's Download at the source: up to 640px a row's reason takes a
// line of its own under the title and its chip wraps, where its nowrap ran
// the page sideways; at every width the chip may wrap within a bounded
// width, so the longer words of this browser's DSD rows run no page
// sideways where the rows share their columns; and the count line's
// Download words hide where the Download link does.
func TestTheDSDChipWrapsUnderItsTitleOnAPhone(t *testing.T) {
	phone := playerCSSBlocks(t, "(max-width: 640px)")
	if !anyRule(phone, ".track-why", "grid-column", "2 / -1") || !anyRule(phone, ".track-why .chip", "white-space", "normal") {
		t.Error("up to 640px a row's reason is not on a line of its own under the title, or its chip does not wrap")
	}
	css := cssCommentRe.ReplaceAllString(readFile(t, "static/player.css"), "")
	wraps := false
	for _, r := range touchRulesOf(css) {
		if r.selects(".track-why .chip") && r.declares("white-space", "normal") && strings.Contains(r.decl, "max-width") {
			wraps = true
		}
	}
	if !wraps {
		t.Error("a row's reason chip does not wrap within a bounded width where the rows share their columns")
	}
	hidden := false
	for _, r := range phone {
		if r.selects(".track-dl") && r.selects(".track-dl-note") && r.declares("display", "none") {
			hidden = true
		}
	}
	if !hidden {
		t.Error("up to 640px the count line offers Download where the Download link is hidden")
	}
}

// TestAnOutputSwitchRedrawsTheRoute pins at the source what
// TestAnOutputSwitchRedrawsAnAlbumsRowsLineAndControls stands in for under
// node: boot.js redraws the route whenever the output picked changes, so an
// open album's rows, count line and controls follow the output.
func TestAnOutputSwitchRedrawsTheRoute(t *testing.T) {
	wire := extractJSFunction(t, stripJSComments(readFile(t, "static/player/boot.js")), "wireOutput")
	if !strings.Contains(wire, "onOutput: () => route()") {
		t.Error("boot.js does not redraw the route when the output picked changes")
	}
}
