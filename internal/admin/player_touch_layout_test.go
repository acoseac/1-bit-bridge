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

// barAreas is the bar's grid-template-areas in rules, one string of cells a
// row, the last rule that sets them winning.
func barAreas(rules []touchRule) (areas string, rows []string) {
	for _, r := range rules {
		if r.selects(".npbar") {
			for _, d := range strings.Split(r.decl, ";") {
				if k, v, ok := strings.Cut(d, ":"); ok && strings.TrimSpace(k) == "grid-template-areas" {
					areas = strings.TrimSpace(v)
				}
			}
		}
	}
	for _, m := range regexp.MustCompile(`"([^"]*)"`).FindAllStringSubmatch(areas, -1) {
		rows = append(rows, m[1])
	}
	return areas, rows
}

// TestThePhoneBarShowsItsTimesAndItsWordsButNoSeek pins B1021's items 1 and
// 2 at the source: up to 768px the scrub row is a row of the bar of its own,
// the last, hidden by no rule there, its seek hidden (it lands only at its
// change, which WebKit fires after no touch) and the elapsed and total times
// at its two ends, read-only beside the 2px rail, which no rule hides up to
// 768px and one hides from 769px, where the seek shows; up to 480px the words
// take a row the transport does not share, the times' row last as from 481px
// to 768px, which no rule up to 640px makes a third row there (a phone held
// sideways keeps two, so the bar leaves the window room), and the bar's
// buttons up to 640px are at least 44px square.
func TestThePhoneBarShowsItsTimesAndItsWordsButNoSeek(t *testing.T) {
	tablet := playerCSSBlocks(t, "(max-width: 768px)")
	for _, r := range tablet {
		if r.selects(".np-scrub") && r.declares("display", "none") {
			t.Errorf("up to 768px a rule hides the scrub row (%s): a phone has no times", r.sel)
		}
	}
	if !anyRule(tablet, ".np-scrub", "grid-area", "scrub") || !anyRule(tablet, ".np-center", "display", "contents") {
		t.Error("up to 768px the scrub row is not a row of the bar of its own (.np-center's contents, .np-scrub's grid area)")
	}
	if !anyRule(tablet, ".np-seek", "display", "none") {
		t.Error("up to 768px the seek is shown: it lands only at its change, which WebKit fires after no touch, so a finger's seek would preview and never land")
	}
	if !anyRule(tablet, ".np-scrub", "justify-content", "space-between") {
		t.Error("up to 768px the times are not at the scrub row's two ends, where the hidden seek leaves them side by side")
	}
	phone := playerCSSBlocks(t, "(max-width: 640px)")
	narrow := playerCSSBlocks(t, "(max-width: 480px)")
	for _, block := range [][]touchRule{tablet, phone, narrow} {
		for _, r := range block {
			if r.selects(".np-rail") && r.declares("display", "none") {
				t.Errorf("a rule up to 768px hides the rail (%s): where the seek is hidden it is the bar's progress", r.sel)
			}
		}
	}
	if !anyRule(playerCSSBlocks(t, "(min-width: 769px)"), ".np-rail", "display", "none") {
		t.Error("from 769px, where the seek shows, the rail is not hidden")
	}
	tabletAreas, tabletRows := barAreas(tablet)
	if len(tabletRows) != 2 || tabletRows[len(tabletRows)-1] != "scrub scrub scrub" {
		t.Errorf("up to 768px the times' row is not the bar's last (grid-template-areas %q)", tabletAreas)
	}
	for _, r := range phone {
		if r.selects(".npbar") && strings.Contains(r.decl, "grid-template-areas") {
			t.Errorf("up to 640px a rule gives the bar its rows (%s): a phone held sideways takes the three rows a narrow one needs", strings.TrimSpace(r.decl))
		}
	}
	areas, rows := barAreas(narrow)
	words := false
	for _, row := range rows {
		cells := strings.Fields(row)
		if strings.Contains(row, "now") {
			words = !strings.Contains(row, "ctl") && cells[0] == "now"
		}
	}
	if len(rows) < 3 || !words {
		t.Errorf("up to 480px the words do not take a row the transport leaves them (grid-template-areas %q)", areas)
	}
	if len(rows) == 0 || rows[len(rows)-1] != "scrub scrub scrub" {
		t.Errorf("up to 480px the times' row is not the bar's last, as it is from 481px to 768px (grid-template-areas %q)", areas)
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
	if !anyRule(playerCSSBlocks(t, "(max-width: 1023px)"), ":root", "--az-top", "calc(65px + var(--list-head-pin) + var(--space-2))") {
		t.Error("below 1024px the A–Z rail sticks under the shell's top bar or the list's head, which cover its first letters")
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

// TestAListsHeadPinsBelowWhatIsPinnedAboveIt pins B1021 PR 3a's head at the
// source: where the window is taller than 500px (the default, which one
// override for a short window leaves) the head sticks under the shell's top
// bar (65px below 1024px, nothing from 1024px) at a fixed height token, the
// A–Z rail's top below it at every width, the rows' scroll margin all that is
// pinned over them, and the field a finger's 44px tall with text iOS does not
// zoom into wherever any pointer is coarse, a floor no later rule overrides.
func TestAListsHeadPinsBelowWhatIsPinnedAboveIt(t *testing.T) {
	css := cssCommentRe.ReplaceAllString(readFile(t, "static/player.css"), "")
	short := playerCSSBlocks(t, "(max-height: 500px)")
	rest := css
	for _, b := range cssMediaBlocks(css, "") {
		rest = strings.Replace(rest, b, "", 1)
	}
	all := touchRulesOf(rest)
	if !anyRule(all, ".list-head", "position", "sticky") || !anyRule(all, ".list-head", "top", "var(--list-head-top)") ||
		!anyRule(all, ":root", "--list-head-pin", "var(--list-head-h)") || !anyRule(all, ".list-head", "height", "var(--list-head-h)") {
		t.Error("the list's head does not stick at its fixed height under what is pinned above it")
	}
	if !anyRule(short, ".list-head", "position", "static") || !anyRule(short, ":root", "--list-head-pin", "0px") {
		t.Error("in a window 500px tall or less the head is pinned: a short window keeps its rail and pins no head")
	}
	if !anyRule(playerCSSBlocks(t, "(max-width: 1023px)"), ":root", "--list-head-top", "65px") {
		t.Error("below 1024px the head does not stick under the shell's 65px top bar")
	}
	if !anyRule(all, ":root", "--az-top", "calc(var(--list-head-pin) + var(--space-2))") {
		t.Error("from 1024px the A–Z rail does not stick below the list's pinned head")
	}
	for _, sel := range []string{".list-body > .grid", ".list-body > .rows"} {
		if !anyRule(all, sel, "scroll-margin-top", "calc(var(--list-head-top) + var(--list-head-pin))") {
			t.Errorf("%s carries no scroll margin of what is pinned over it: a jump lands its first row under the head", sel)
		}
	}
	if !anyRule(all, ".list-body > .rows", "content-visibility", "visible") {
		t.Error("a list's rows are content-visibility: auto, which WebKit gives no heights as they go in: a restored place clamps short")
	}
	coarse := playerCSSBlocks(t, "(any-pointer: coarse)")
	if !anyRule(coarse, ".list-filter", "min-height", "44px") || !anyRule(coarse, ".list-filter", "font-size", "16px") {
		t.Error("where any pointer is coarse the list's filter is not 44px tall with 16px text")
	}
	// And the floor wins the cascade there: the last rule that sets each of
	// the field's two is the coarse one, where the field's shared style (its
	// 0.9rem, 14.4px), later at the same specificity, overrode it.
	ordered := cssRulesInOrder(css)
	for _, floor := range [][2]string{{"min-height", "44px"}, {"font-size", "16px"}} {
		var last *cssRuleAt
		for i := range ordered {
			if ordered[i].selects(".list-filter") && ordered[i].sets(floor[0]) {
				last = &ordered[i]
			}
		}
		if last == nil || last.media != "(any-pointer: coarse)" || !last.declares(floor[0], floor[1]) {
			got := "none"
			if last != nil {
				got = last.sel + " {" + last.decl + "} in @media " + last.media
			}
			t.Errorf("the last rule setting the list filter's %s is not the coarse pointer's %s, which a later rule of the same specificity overrides: %s", floor[0], floor[1], got)
		}
	}
}

// cssRuleAt is a style rule of a stylesheet and the @media query of the
// innermost block around it ("" outside any).
type cssRuleAt struct {
	touchRule
	media string
}

// cssRulesInOrder is every style rule of css (comments stripped first), in
// source order, each with the @media query it is in.
func cssRulesInOrder(css string) []cssRuleAt {
	type open struct {
		prelude string
		body    int
	}
	var (
		out   []cssRuleAt
		stack []open
		from  int
	)
	for i := 0; i < len(css); i++ {
		switch css[i] {
		case '{':
			stack = append(stack, open{strings.TrimSpace(css[from:i]), i + 1})
			from = i + 1
		case '}':
			if len(stack) == 0 {
				from = i + 1
				continue
			}
			r := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if !strings.HasPrefix(r.prelude, "@") {
				media := ""
				for j := len(stack) - 1; j >= 0 && media == ""; j-- {
					if q, ok := strings.CutPrefix(stack[j].prelude, "@media"); ok {
						media = strings.TrimSpace(q)
					}
				}
				out = append(out, cssRuleAt{touchRule{r.prelude, css[r.body:i]}, media})
			}
			from = i + 1
		case ';':
			if len(stack) == 0 {
				from = i + 1
			}
		}
	}
	return out
}

// sets reports whether the rule declares prop, whatever its value.
func (r touchRule) sets(prop string) bool {
	for _, d := range strings.Split(r.decl, ";") {
		if k, _, ok := strings.Cut(d, ":"); ok && strings.TrimSpace(k) == prop {
			return true
		}
	}
	return false
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

// TestTheRailTakesAFingerWhereAnyPointerIsCoarse pins B1021 PR 3b's rail at
// the source: wherever any pointer is coarse (a touch laptop's main pointer is
// its touchpad), the rail is one column at least 44px wide, 8px from the list
// (12px left a 390px phone's albums one column), as tall as the room between
// where it sticks and the bar, its entries sharing that height and each as
// wide as the column with no gutter or gap around them, nothing selecting
// under a finger; the page pans under no touch or pen that goes down on the
// rail, whatever the window; a short window's block, after the coarse one,
// gives PR 1's grid back its letters' height (no 501px block: PR 2's
// TestTheBarTakesRowsForAFingerFrom641px refuses one); and the scrub's
// callout stands beside the rail, its words wrapping within the room left of
// it, touched by no pointer.
func TestTheRailTakesAFingerWhereAnyPointerIsCoarse(t *testing.T) {
	coarse := mediaRulesOf(t, "static/player.css", "(any-pointer: coarse)")
	for _, w := range [][3]string{
		{".az-rail", "min-width", "44px"}, {".az-rail", "margin-left", "var(--space-2)"},
		{".az-rail", "height", "calc(100vh - var(--az-top) - var(--az-foot))"},
		{".az-rail", "height", "calc(100dvh - var(--az-top) - var(--az-foot))"}, {".az-rail", "padding", "0"},
		{".az-rail", "gap", "0"}, {".az-rail", "align-items", "stretch"}, {".az-rail", "user-select", "none"},
		{".az-rail", "-webkit-user-select", "none"}, {".az-rail", "-webkit-touch-callout", "none"}, {".az-letter", "flex", "1 1 0"},
	} {
		if !anyRule(coarse, w[0], w[1], w[2]) {
			t.Errorf("where any pointer is coarse %s does not set %s: %s", w[0], w[1], w[2])
		}
	}
	css := cssCommentRe.ReplaceAllString(readFile(t, "static/player.css"), "")
	rest := css
	for _, b := range cssMediaBlocks(css, "") {
		rest = strings.Replace(rest, b, "", 1)
	}
	if !anyRule(touchRulesOf(rest), ".az-rail", "touch-action", "none") {
		t.Error("the rail lets the page pan under a finger in some window: its touch-action is not none outside every @media block")
	}
	coarseAt, shortAt := -1, -1
	for i := strings.Index(css, "@media "); i >= 0; {
		open := i + strings.Index(css[i:], "{")
		query := strings.TrimSpace(css[i+len("@media ") : open])
		block := cssMediaBlocks(css[i:], "")[0]
		for _, r := range touchRulesOf(block) {
			if r.selects(".az-rail") && query == "(any-pointer: coarse)" && r.declares("min-width", "44px") {
				coarseAt = i
			}
			if r.selects(".az-rail") && query == "(max-height: 500px)" && r.declares("height", "auto") {
				shortAt = i
			}
		}
		next := strings.Index(css[open+len(block):], "@media ")
		if next < 0 {
			break
		}
		i = open + len(block) + next
	}
	if coarseAt < 0 || shortAt < coarseAt {
		t.Errorf("the short window's grid does not take its letters' height back after the coarse column (coarse block at %d, short at %d)", coarseAt, shortAt)
	}
	all := touchRulesOf(css)
	if !anyRule(all, ".az-callout", "position", "absolute") || !anyRule(all, ".az-callout", "right", "calc(100% + var(--space-3))") ||
		!anyRule(all, ".az-callout", "pointer-events", "none") {
		t.Error("the scrub's callout does not stand beside the rail out of every pointer's way")
	}
	if !anyRule(all, ".az-callout", "width", "max-content") ||
		!anyRule(all, ".az-callout", "max-width", "calc(100vw - 100% - var(--shell-gutter) - var(--space-3) - var(--space-2))") {
		t.Error("the scrub's callout can run past the window's left edge beside a wide rail: its words do not wrap within the room left of the rail")
	}
	for _, r := range all {
		if r.selects(".az-callout") && r.declares("white-space", "nowrap") {
			t.Error("the scrub's callout keeps its words on one line, past the room left of the rail")
		}
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
