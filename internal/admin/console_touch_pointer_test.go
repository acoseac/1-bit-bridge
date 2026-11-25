package admin

import (
	"strings"
	"testing"
)

// The console by touch, by the pointer and on a phone's and a tablet's pages
// (B1021's second PR): what player.css and app.css give a coarse pointer at
// every width, the bar's rows for a finger from 641px, the hover paints that
// wait for a pointer that hovers, a phone's text, the tablet band and the
// narrow grids, pinned at the source on every host. What the rules do in a
// real engine is the record's WebKit evidence, which no Go guard can see.

// mediaRulesOf is every rule of the stylesheet at path whose @media query is
// query exactly (comments stripped first), where cssMediaBlocks takes every
// query that begins with it.
func mediaRulesOf(t *testing.T, path, query string) []touchRule {
	t.Helper()
	css := cssCommentRe.ReplaceAllString(readFile(t, path), "")
	var rules []touchRule
	head := "@media " + query
	for at := strings.Index(css, head); at >= 0; {
		rest := css[at+len(head):]
		if strings.HasPrefix(strings.TrimLeft(rest, " \t\n"), "{") {
			rules = append(rules, touchRulesOf(cssMediaBlocks(css[at:], query)[0])...)
		}
		next := strings.Index(rest, head)
		if next < 0 {
			break
		}
		at += len(head) + next
	}
	if len(rules) == 0 {
		t.Fatalf("no rule found in %s's %s blocks: the reader has stopped finding them", path, query)
	}
	return rules
}

// TestEveryControlTakesAFingerWhereACoarsePointerIs pins B1021's touch sizes
// at the source: in an (any-pointer: coarse) block of each stylesheet, which
// holds at every width, the bar's buttons, the output menu's picks, the queue
// panel's entries and Remove, and the player's sections and crumbs are at
// least 44px, the transport's buttons and the queue's entries 8px apart; and
// app.css's operator controls (a button, an <a class="btn"> made an
// inline-block that a min-height grows, a select, a switch's row, the gear, a
// summary row) are 44px, a radio's glyph 24px in a 44px label row, and the
// Player page's radio, which no label holds, a 44px target itself.
func TestEveryControlTakesAFingerWhereACoarsePointerIs(t *testing.T) {
	for _, file := range []struct {
		path  string
		wants [][3]string
	}{
		{"static/player.css", [][3]string{
			{".np-btn", "min-width", "44px"}, {".np-btn", "min-height", "44px"},
			{".np-btn-sm", "min-width", "44px"}, {".np-btn-sm", "min-height", "44px"},
			{".np-transport", "gap", "var(--space-2)"},
			{".np-pick", "min-height", "44px"},
			{".np-queue-list", "gap", "var(--space-2)"},
			{".np-queue-title", "min-height", "44px"},
			{".np-queue-remove", "min-width", "44px"}, {".np-queue-remove", "min-height", "44px"},
			{".player-section", "min-height", "44px"},
			{".crumb-link", "min-height", "44px"},
		}},
		{"static/app.css", [][3]string{
			{".btn", "min-height", "44px"},
			{"a.btn", "display", "inline-block"},
			{"select", "min-height", "44px"},
			{"label.checkbox", "min-height", "44px"},
			{".tray-toggle", "width", "44px"}, {".tray-toggle", "height", "44px"},
			{"summary", "min-height", "44px"},
			{`input[type="radio"]`, "width", "24px"}, {`input[type="radio"]`, "height", "24px"},
			{`label:has(> input[type="radio"])`, "min-height", "44px"},
			{`td > input[type="radio"]`, "width", "44px"}, {`td > input[type="radio"]`, "height", "44px"},
		}},
	} {
		rules := mediaRulesOf(t, file.path, "(any-pointer: coarse)")
		for _, w := range file.wants {
			if !anyRule(rules, w[0], w[1], w[2]) {
				t.Errorf("%s's (any-pointer: coarse) block does not give %s %s: %s, so a finger on a tablet or a touch laptop meets a mouse's size",
					file.path, w[0], w[1], w[2])
			}
		}
	}
}

// TestTheBarTakesRowsForAFingerFrom641px pins B1021's bar under a coarse
// pointer from 641px (D5) at the source: its centre's transport and times are
// rows of the bar's own, three where the window is taller than 500px (the
// words beside the extras, the transport, the times) and two where it is
// 500px tall or less, a phone held sideways, its toggles and its chip hidden
// there as a phone's are: 44px buttons 8px apart do not fit the one row a
// mouse's bar keeps.
func TestTheBarTakesRowsForAFingerFrom641px(t *testing.T) {
	wide := mediaRulesOf(t, "static/player.css", "(any-pointer: coarse) and (min-width: 641px)")
	for _, w := range [][3]string{
		{".np-center", "display", "contents"}, {".np-now", "grid-area", "now"}, {".np-transport", "grid-area", "ctl"},
		{".np-scrub", "grid-area", "scrub"}, {".np-extra", "grid-area", "extra"}, {".npbar", "grid-template-columns", "minmax(0, 1fr) auto auto"},
	} {
		if !anyRule(wide, w[0], w[1], w[2]) {
			t.Errorf("under a coarse pointer from 641px %s does not set %s: %s", w[0], w[1], w[2])
		}
	}
	for _, c := range []struct {
		query string
		rows  []string
	}{
		{"(any-pointer: coarse) and (min-width: 641px) and (min-height: 501px)", []string{"now now extra", "ctl ctl ctl", "scrub scrub scrub"}},
		{"(any-pointer: coarse) and (min-width: 641px) and (max-height: 500px)", []string{"now ctl extra", "scrub scrub scrub"}},
	} {
		areas, rows := barAreas(mediaRulesOf(t, "static/player.css", c.query))
		if strings.Join(rows, "|") != strings.Join(c.rows, "|") {
			t.Errorf("%s: the bar's rows are %q, want %q", c.query, areas, c.rows)
		}
	}
	short := mediaRulesOf(t, "static/player.css", "(any-pointer: coarse) and (min-width: 641px) and (max-height: 500px)")
	if !anyRule(short, ".np-btn-toggle", "display", "none") || !anyRule(short, ".np-chip", "display", "none") {
		t.Error("in a short window under a coarse pointer the bar keeps its toggles or its chip, which a phone's two rows leave out")
	}
}

// TestTheBarAndTheSectionsHoverOnlyForAPointerThatHovers pins D12 at the
// source: every :hover paint of the bar, its menu and its queue panel, of the
// player's sections and of the crumbs sits in a (hover: hover) block, so a
// tap's paint does not stick on a touch screen, and none stands outside one.
func TestTheBarAndTheSectionsHoverOnlyForAPointerThatHovers(t *testing.T) {
	css := cssCommentRe.ReplaceAllString(readFile(t, "static/player.css"), "")
	all := touchRulesOf(css)
	hovering := touchRulesOf(strings.Join(cssMediaBlocks(css, "(hover: hover)"), "\n"))
	for _, sel := range []string{".player-section:hover", ".crumb-link:hover", ".np-title a:hover", ".np-btn:hover", ".np-btn-play:hover",
		`.np-btn-toggle[aria-pressed="true"]:hover`, `.np-btn-toggle[data-repeat="all"]:hover`, `.np-btn-toggle[data-repeat="one"]:hover`,
		".np-btn:disabled:hover", ".np-pick:hover", ".np-queue-title:hover", ".np-queue-remove:hover"} {
		inside, everywhere := 0, 0
		for _, r := range hovering {
			if r.selects(sel) {
				inside++
			}
		}
		for _, r := range all {
			if r.selects(sel) {
				everywhere++
			}
		}
		if inside == 0 || inside != everywhere {
			t.Errorf("%s: %d of its %d rules in a (hover: hover) block, want every one and at least one", sel, inside, everywhere)
		}
	}
}

// TestAPhonesTextIsReadable pins B1021's text on a phone at the source: up to
// 640px, whatever the pointer, the text tokens are 15, 14 and 12.5px, a
// hint's and a button's text --text-sm, a chip --text-xs, and no rule there
// shrinks a page's intro below them.
func TestAPhonesTextIsReadable(t *testing.T) {
	app := mediaRulesOf(t, "static/app.css", "(max-width: 640px)")
	for _, w := range [][3]string{
		{":root", "--text-base", "15px"}, {":root", "--text-sm", "14px"}, {":root", "--text-xs", "12.5px"},
		{".hint", "font-size", "var(--text-sm)"}, {".btn", "font-size", "var(--text-sm)"},
	} {
		if !anyRule(app, w[0], w[1], w[2]) {
			t.Errorf("up to 640px app.css does not give %s %s: %s", w[0], w[1], w[2])
		}
	}
	for _, r := range app {
		if (r.selects(".lede") || r.selects(".page > .muted")) && strings.Contains(r.decl, "font-size") {
			t.Errorf("up to 640px %s shrinks a page's intro (%s)", r.sel, strings.TrimSpace(r.decl))
		}
	}
	if !anyRule(mediaRulesOf(t, "static/player.css", "(max-width: 640px)"), ".chip", "font-size", "var(--text-xs)") {
		t.Error("up to 640px a chip keeps its 11.5px")
	}
}

// TestTheTabletBandEndsWhereTheContentBegins pins B1021's tablet band at the
// source: below 1024px the shell's gutter is 20px (16px up to 520px) and
// main's side padding reads it, so the band the player draws by the gutter
// (gutter, rail and 12px) ends before main's content begins (gutter, rail and
// the player's 24px gap), where a 36px gutter beside 20px of padding covered
// the content's first 4px.
func TestTheTabletBandEndsWhereTheContentBegins(t *testing.T) {
	for _, c := range []struct{ query, gutter, padding string }{
		{"(max-width: 1023px)", "20px", "22px var(--shell-gutter) 96px"},
		{"(max-width: 520px)", "16px", "18px var(--shell-gutter) 96px"},
	} {
		rules := mediaRulesOf(t, "static/app.css", c.query)
		if !anyRule(rules, ":root", "--shell-gutter", c.gutter) || !anyRule(rules, "main", "padding", c.padding) {
			t.Errorf("%s: the gutter is not %s with main's side padding reading it (%s)", c.query, c.gutter, c.padding)
		}
	}
	if !strings.Contains(cssCommentRe.ReplaceAllString(readFile(t, "static/player.css"), ""), "width: calc(var(--shell-gutter) + var(--nav-w) + 12px)") {
		t.Error("the player's band is no longer drawn by the shell's gutter")
	}
}

// TestNoNarrowGridKeepsAMinContentTrack pins B1021's narrow pages at the
// source: Settings' one column up to 768px and Jobs' cards take minmax(0,
// 1fr), where a 1fr track kept its widest item's minimum and ran the page
// sideways, and Settings' selects no wider than that column; up to 640px the
// stat cards take one full-width column, a switch's badges lines of their own
// under its words, and the duplicates' tier table scrolls inside its panel.
func TestNoNarrowGridKeepsAMinContentTrack(t *testing.T) {
	tablet := mediaRulesOf(t, "static/app.css", "(max-width: 768px)")
	if !anyRule(tablet, ".settings .settings-layout", "grid-template-columns", "minmax(0, 1fr)") {
		t.Error("up to 768px Settings' one column keeps a 1fr track's minimum")
	}
	if !anyRule(tablet, ".settings select", "max-width", "100%") {
		t.Error("up to 768px a Settings select is as wide as its longest option")
	}
	phone := mediaRulesOf(t, "static/app.css", "(max-width: 640px)")
	if !anyRule(phone, ".settings label.checkbox:has(> .prereq)", "grid-template-columns", "auto minmax(0, 1fr)") ||
		!anyRule(phone, ".settings label.checkbox > .prereq", "grid-column", "2") || !anyRule(phone, ".settings label.checkbox > .recommended", "grid-column", "2") {
		t.Error("up to 640px a switch's badges share its words' row and run the page sideways")
	}
	if !anyRule(phone, ".dupes-tier-table", "overflow-x", "auto") || !anyRule(phone, ".dupes-tier-table", "display", "block") {
		t.Error("up to 640px the duplicates' tier table runs the page sideways")
	}
	if !anyRule(touchRulesOf(cssCommentRe.ReplaceAllString(readFile(t, "static/app.css"), "")), ".jobs-cards", "grid-template-columns", "minmax(0, 1fr)") {
		t.Error("Jobs' cards keep a 1fr track's minimum")
	}
	if !anyRule(mediaRulesOf(t, "static/app.css", "(max-width: 640px)"), ".cards", "grid-template-columns", "minmax(0, 1fr)") {
		t.Error("up to 640px the stat cards stop short of the page's width")
	}
}

// TestTheBarDrawsNothingToMuteAndTheSectionsSayTheyScroll pins B1021's
// cosmetics and the no-cover mark at the source: the empty strip in this
// browser draws neither Mute nor the volume; the cover's square holds the mark
// inside itself at a size for its box; and up to 768px the section strip fades
// at its trailing edge over a spacer as wide as the fade, as .tab-nav does, so
// its last link scrolls clear of it.
func TestTheBarDrawsNothingToMuteAndTheSectionsSayTheyScroll(t *testing.T) {
	all := touchRulesOf(cssCommentRe.ReplaceAllString(readFile(t, "static/player.css"), ""))
	for _, sel := range []string{`.npbar-empty[data-output="browser"] .np-mute`, `.npbar-empty[data-output="browser"] .np-vol`} {
		if !anyRule(all, sel, "display", "none") {
			t.Errorf("the empty strip draws %s, with nothing to mute", sel)
		}
	}
	if !anyRule(all, ".np-art", "position", "relative") || !anyRule(all, ".np-art.cover-empty::after", "font-size", "1.25rem") {
		t.Error("the bar's square does not hold the no-cover mark inside itself at a size for its box")
	}
	tablet := playerCSSBlocks(t, "(max-width: 768px)")
	fade := "linear-gradient(to right, #000 calc(100% - 48px), transparent)"
	if !anyRule(tablet, ".player-nav nav", "mask-image", fade) || !anyRule(tablet, ".player-nav nav", "-webkit-mask-image", fade) ||
		!anyRule(tablet, ".player-nav nav::after", "width", "48px") {
		t.Error("up to 768px the section strip gives no sign that it scrolls, or its last link stays under the fade")
	}
}
