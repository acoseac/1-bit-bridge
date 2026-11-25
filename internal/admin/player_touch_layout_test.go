package admin

import (
	"regexp"
	"strings"
	"testing"
)

// The console by touch (B1021): what a phone's bar and pages need of
// player.css, pinned at the source on every host. The bar's harness runs the
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
// up to 480px the words take a row the transport does not share, which no
// rule up to 640px makes a third row there (a phone held sideways keeps two,
// so the bar leaves the window room), and the bar's buttons up to 640px are
// at least 44px square.
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
	for _, r := range phone {
		if r.selects(".npbar") && strings.Contains(r.decl, "grid-template-areas") {
			t.Errorf("up to 640px a rule gives the bar its rows (%s): a phone held sideways takes the three rows a narrow one needs", strings.TrimSpace(r.decl))
		}
	}
	narrow := playerCSSBlocks(t, "(max-width: 480px)")
	areas := ""
	for _, r := range narrow {
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
		t.Errorf("up to 480px the words do not take a row the transport leaves them (grid-template-areas %q)", strings.TrimSpace(areas))
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
// rows, and that the A–Z rail fits between where it sticks, below the
// shell's top bar where that bar is, and that bar (or the window's foot), its
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
		if r.selects(".az-rail") && strings.Contains(r.decl, "max-height: calc(100dvh - var(--az-top) - var(--az-foot))") {
			rail = true
		}
	}
	if !rail || !anyRule(all, ".az-rail", "top", "var(--az-top)") || !anyRule(all, "body.has-player-bar", "--az-foot", "var(--player-bottom-inset)") ||
		!anyRule(all, ".az-letter", "min-height", "0") || !anyRule(all, ".az-letter", "overflow", "hidden") {
		t.Error("the A–Z rail has no height that stops above the bar, or its letters cannot shrink to fit it each in its own box")
	}
	if !anyRule(playerCSSBlocks(t, "(max-width: 1023px)"), ":root", "--az-top", "calc(65px + var(--space-2))") {
		t.Error("below 1024px the A–Z rail sticks under the shell's top bar, which covers its first letters")
	}
	np := stripJSComments(readFile(t, "static/player/nowplaying.js"))
	mount := extractJSFunction(t, np, "mount")
	if !strings.Contains(mount, "new ResizeObserver(") || !strings.Contains(mount, `setProperty("--np-bar-h", `) || !strings.Contains(mount, "bar.offsetHeight") {
		t.Error("mount does not hand the bar's rendered height (--np-bar-h) to the page as the bar changes size")
	}
}

// TestTheDrawerAndThePanelsStayClearOfTheBarAndInTheWindow pins at the
// source that what the taller bar sits beside stays reachable: the shell's
// drawer, open below 1024px with the bar shown, ends above the bar as it
// renders, and the output menu and the queue panel stay within the window
// above it (the queue within half of it where that fits), each scrolling
// inside where its content is longer.
func TestTheDrawerAndThePanelsStayClearOfTheBarAndInTheWindow(t *testing.T) {
	drawer := false
	for _, r := range playerCSSBlocks(t, "(max-width: 1023px)") {
		if r.selects(`body.has-player-bar header[data-nav-open="true"] .sidebar-drawer`) && strings.Contains(r.decl, "100dvh") &&
			strings.Contains(r.decl, "var(--player-bottom-inset)") {
			drawer = true
		}
	}
	if !drawer {
		t.Error("below 1024px the open drawer is not held above the bar as it renders: the bar covers its foot")
	}
	css := cssCommentRe.ReplaceAllString(readFile(t, "static/player.css"), "")
	panels, queue := false, false
	for _, r := range touchRulesOf(css) {
		if r.selects(".np-menu") && r.selects(".np-queue") && strings.Contains(r.decl, "max-height: calc(100dvh - var(--np-panel-bottom) - var(--space-2))") &&
			r.declares("overflow-y", "auto") && strings.Contains(r.decl, "bottom: var(--np-panel-bottom)") {
			panels = true
		}
		if r.selects(".np-queue") && strings.Contains(r.decl, "max-height: min(50vh, calc(100dvh - var(--np-panel-bottom) - var(--space-2)))") {
			queue = true
		}
	}
	if !panels || !queue {
		t.Error("the output menu and the queue panel are not held within the window above the bar, scrolling inside")
	}
}

// TestTheRailTakesColumnsWhereTheWindowIsShort pins at the source that where
// the window is short (a phone held sideways) the A–Z rail's letters take
// seven columns, since one column of 27 cannot stand between the top bar and
// the bar there.
func TestTheRailTakesColumnsWhereTheWindowIsShort(t *testing.T) {
	short := playerCSSBlocks(t, "(max-height: 500px)")
	if !anyRule(short, ".az-rail", "display", "grid") || !anyRule(short, ".az-rail", "grid-template-columns", "repeat(7, auto)") {
		t.Error("where the window is short the A–Z rail keeps one column, whose letters run under the bar")
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
	if !anyRule(phone, ".track-why", "grid-column", "2 / -1") {
		t.Error("up to 640px a row's reason is not on a line of its own under the title")
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

// TestAnAlbumsControlsTakeAFingerOnATouchScreen pins at the source that an
// album's Play, Shuffle and Add to queue are at least 44px square where the
// pointer is a finger (B1021's item 6).
func TestAnAlbumsControlsTakeAFingerOnATouchScreen(t *testing.T) {
	touch := playerCSSBlocks(t, "(pointer: coarse)")
	if !anyRule(touch, ".detail-actions .btn", "min-width", "44px") || !anyRule(touch, ".detail-actions .btn", "min-height", "44px") {
		t.Error("on a touch screen an album's action buttons are not at least 44px square")
	}
}

// TestAnOutputSwitchRedrawsTheRoute pins at the source what
// TestAnOutputSwitchRedrawsAnAlbumsRowsLineAndControls stands in for in the
// bar's harness: boot.js redraws the route whenever the output in effect
// changes (output.js's listen), so an open album's rows, count line and
// controls follow the output.
func TestAnOutputSwitchRedrawsTheRoute(t *testing.T) {
	wire := extractJSFunction(t, stripJSComments(readFile(t, "static/player/boot.js")), "wireOutput")
	if !strings.Contains(wire, "onOutput: () => route()") {
		t.Error("boot.js does not redraw the route when the output picked changes")
	}
}
