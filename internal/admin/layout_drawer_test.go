package admin

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"golang.org/x/net/html"
)

// The phone top bar holds the brand and the hamburger, and nothing else.
//
// Below 1024px header.sidebar is a flex ROW, so every child of it that is
// not inside .sidebar-drawer is in the top bar. The space meter was added as
// a plain rail child, and on a public bridge with uploads enabled the bar at
// 375px then held "42 GB free", the live dot, the theme pill and Sign out
// beside the 44px hamburger: the head shrank to 57px, the mark sat under the
// button and the library name rendered at 0px of its 70 (field report,
// 2026-09-18, on the operator bridge; the demo tenant looked fine at the
// same width only because uploads are off there and the meter never
// renders).
//
// No Go test can see the layout — CLAUDE.md's rule for this class of change
// is a browser — but the DOM containment the layout depends on is
// checkable. This drives the real page handler in public mode (the posture
// that renders Sign out) and asserts the CLOSED SET: a child of the header
// is the head or the drawer, so the next rail element lands red here rather
// than in the bar. The CSS half, below, pins that the phone block hides the
// WRAPPER (hiding #primary-nav alone is the shape that left the meter and
// the foot in the bar) and that the desktop rail dissolves the wrapper with
// display: contents, which is what keeps that layout byte-identical.
func TestEverythingBelowTheBrandIsInTheDrawer(t *testing.T) {
	header := renderPublicHeader(t)
	drawer := railFind(header, byID("sidebar-drawer"))
	if drawer == nil {
		t.Fatal("no #sidebar-drawer inside the header")
	}

	railChildrenAreHeadAndDrawer(t, header)

	// What the drawer must carry: the two ids the phone breakpoint used to
	// leave in the bar, plus the three the nav toggle already controlled.
	railInsideDrawer(t, header, drawer, "primary-nav", "space-meter", "conn-status", "theme-toggle", "logout-btn")
	if n := railFind(header, byClass("build-line")); n == nil || !railWithin(n, drawer) {
		t.Error(".build-line is missing or outside #sidebar-drawer")
	}

	// And what it must not: the two things the bar IS.
	brand := railFind(header, byClass("brand"))
	if brand == nil || railWithin(brand, drawer) {
		t.Fatal(".brand is missing or inside the drawer — the bar would be empty until the menu opens")
	}
	toggle := railFind(header, byID("nav-toggle"))
	if toggle == nil || railWithin(toggle, drawer) {
		t.Fatal("#nav-toggle is missing or inside the drawer it opens")
	}
	if got := railAttr(toggle, "aria-controls"); got != "sidebar-drawer" {
		t.Errorf("#nav-toggle aria-controls = %q, want \"sidebar-drawer\" — the button toggles the whole drawer, not the nav alone", got)
	}

	railBarBadge(t, header, brand, drawer)
}

// renderPublicHeader drives the real page handler in public mode and returns
// the parsed <header class="sidebar">.
func renderPublicHeader(t *testing.T) *html.Node {
	t.Helper()
	srv, cfg, _ := newTestServer(t)
	cfg.Deployment.Mode = "public"
	cfg.Deployment.AdminTLSTerminatedByProxy = true
	cfg.Autocert.Domain = "bridge.example.com"
	srv.deps.CfgHolder.Store(cfg)

	rw := httptest.NewRecorder()
	srv.pageUPnP(rw, httptest.NewRequest(http.MethodGet, "/upnp", nil))
	if rw.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rw.Code)
	}
	doc, err := html.Parse(rw.Body)
	if err != nil {
		t.Fatalf("page does not parse as HTML: %v", err)
	}
	header := railFind(doc, func(n *html.Node) bool { return n.Data == "header" && railHasClass(n, "sidebar") })
	if header == nil {
		t.Fatal("no <header class=\"sidebar\"> in the rendered page")
	}
	return header
}

// railChildrenAreHeadAndDrawer is the closed set. display: contents hands the
// drawer's children to the header's flex column only when nothing sits
// between, and the phone bar is the header's children minus the drawer — so
// both halves depend on the header having exactly these two element
// children.
func railChildrenAreHeadAndDrawer(t *testing.T, header *html.Node) {
	t.Helper()
	heads, drawers := 0, 0
	for c := header.FirstChild; c != nil; c = c.NextSibling {
		switch {
		case c.Type != html.ElementNode:
		case railHasClass(c, "sidebar-head"):
			heads++
		case railAttr(c, "id") == "sidebar-drawer":
			drawers++
		default:
			t.Errorf("header.sidebar has a child <%s class=%q id=%q> that is neither .sidebar-head nor #sidebar-drawer: "+
				"below 1024px it renders in the TOP BAR beside the brand, which is how the space meter ended up there. "+
				"Put it inside the drawer, or make the case for the bar in a comment and here.",
				c.Data, railAttr(c, "class"), railAttr(c, "id"))
		}
	}
	// Exactly one of each: a second head is a second bar, a second drawer a
	// second dropdown, and skipping the permitted names would let either
	// duplicate through.
	if heads != 1 || drawers != 1 {
		t.Errorf("header.sidebar has %d direct .sidebar-head and %d direct #sidebar-drawer children; want one of each", heads, drawers)
	}
}

// railInsideDrawer asserts that each id renders and sits inside the drawer.
func railInsideDrawer(t *testing.T, header, drawer *html.Node, ids ...string) {
	t.Helper()
	for _, id := range ids {
		n := railFind(header, byID(id))
		switch {
		case n == nil:
			t.Errorf("#%s is not rendered in the public-mode header", id)
		case !railWithin(n, drawer):
			t.Errorf("#%s is outside #sidebar-drawer: below 1024px it sits in the top bar", id)
		}
	}
}

// railBarBadge pins the bar's own live badge. The foot's #conn-status is in
// the drawer, which is display: none on a phone until the menu opens, so its
// aria-live region announces nothing there; the bar carries a second
// .conn-badge inside .brand (dot-only below 1024px, hidden on the desktop
// rail), and applyConnState updates every .conn-badge. Exactly two, one in
// each place, both live regions — a viewport shows exactly one of them.
func railBarBadge(t *testing.T, header, brand, drawer *html.Node) {
	t.Helper()
	badges := railFindAll(header, byClass("conn-badge"))
	if len(badges) != 2 {
		t.Fatalf("%d .conn-badge elements in the header, want 2: the foot's #conn-status in the drawer and the bar's copy inside .brand", len(badges))
	}
	inBrand, inDrawer := 0, 0
	for _, b := range badges {
		if railWithin(b, brand) {
			inBrand++
		}
		if railWithin(b, drawer) {
			inDrawer++
		}
		if railAttr(b, "aria-live") == "" {
			t.Errorf("a .conn-badge (id=%q) has no aria-live: below 1024px the bar's copy is the only region a phone can hear", railAttr(b, "id"))
		}
	}
	if inBrand != 1 || inDrawer != 1 {
		t.Errorf("badges inside .brand = %d, inside the drawer = %d; want one each", inBrand, inDrawer)
	}
}

// TestPhoneShellHidesTheDrawerAndTheDesktopRailDissolvesIt is the CSS half.
func TestPhoneShellHidesTheDrawerAndTheDesktopRailDissolvesIt(t *testing.T) {
	css := readCSSNoComments(t, "static/app.css")
	top, err := cssRuleList(css)
	if err != nil {
		t.Fatalf("app.css does not brace-match once comments are stripped: %v", err)
	}

	var desktop, barBadge []string
	var phone string
	for _, r := range top {
		switch normSelector(r.sel) {
		case ".sidebar-drawer":
			desktop = append(desktop, r.body)
		case ".conn-badge-bar":
			barBadge = append(barBadge, r.body)
		case "@media(max-width:1023px)":
			// Concatenate rather than assign: a second phone block would
			// otherwise be scanned for nothing, and the absence checks below
			// (no .sidebar-foot override) would pass vacuously over it.
			phone += r.body
		}
	}
	if len(desktop) != 1 || !strings.Contains(compactCSS(desktop[0]), "display:contents") {
		t.Errorf("app.css needs exactly one top-level `.sidebar-drawer` rule declaring display: contents "+
			"(got %d rule(s): %q) — as a box, the wrapper would take the rail's column layout away from "+
			"#primary-nav and .sidebar-foot", len(desktop), desktop)
	}
	if len(barBadge) != 1 || !strings.Contains(compactCSS(barBadge[0]), "display:none") {
		t.Errorf("app.css needs exactly one top-level `.conn-badge-bar` rule declaring display: none "+
			"(got %q) — on the desktop rail the foot's badge is on screen and the bar's copy must not be", barBadge)
	}
	if phone == "" {
		t.Fatal("no @media (max-width: 1023px) block in app.css — the shell breakpoint moved; update this test")
	}
	checkPhoneBlockRules(t, phone)
}

// checkPhoneBlockRules asserts what the <1024px block must and must not do.
func checkPhoneBlockRules(t *testing.T, phone string) {
	t.Helper()
	inner, err := cssRuleList(phone)
	if err != nil {
		t.Fatalf("the phone block does not brace-match: %v", err)
	}
	rules := map[string]string{}
	for _, r := range inner {
		// Keyed by every MEMBER of the selector group, not by the group
		// as one string.
		//
		// The "must not" assertions below look rules up by a single
		// class name, so `.sidebar-foot, .meta { flex-direction: row }`
		// produced the key ".sidebar-foot,.meta" and matched none of
		// them — the exact top-bar layout #934 removed could come back
		// silently. The POSITIVE assertions fail closed under grouping
		// (a missing key is a missing declaration); only the negative
		// half failed open, which is the direction that matters.
		//
		// The whole group is kept as a key too, so a lookup written
		// against the literal selector still resolves.
		rules[normSelector(r.sel)] += r.body
		for _, member := range splitSelectorGroup(r.sel) {
			if m := normSelector(member); m != "" {
				rules[m] += r.body
			}
		}
	}
	if !strings.Contains(compactCSS(rules[".sidebar-drawer"]), "display:none") {
		t.Errorf("the phone block must hide .sidebar-drawer (got %q)", rules[".sidebar-drawer"])
	}
	// The DESCENDANT selector, space included: normSelector keeps it, so the
	// compound `header[…].sidebar-drawer` (which matches nothing) cannot
	// satisfy this lookup.
	if open := compactCSS(rules[`header[data-nav-open="true"] .sidebar-drawer`]); !strings.Contains(open, "position:absolute") {
		t.Errorf("the phone block must reveal header[data-nav-open=\"true\"] .sidebar-drawer as the absolutely-positioned dropdown (got %q)", open)
	}
	if strings.Contains(compactCSS(rules["#primary-nav"]), "display:none") {
		t.Error("the phone block hides #primary-nav alone — that leaves the space meter and the foot block in the top bar; hide .sidebar-drawer")
	}
	if !strings.Contains(compactCSS(rules[".conn-badge-bar"]), "display:inline-flex") {
		t.Errorf("the phone block must show .conn-badge-bar (got %q) — it is the only live region a phone can hear", rules[".conn-badge-bar"])
	}
	for _, sel := range []string{".sidebar-foot", ".build-line", ".meta"} {
		if body, ok := rules[sel]; ok {
			t.Errorf("the phone block styles %s (%q): the foot block sits in the drawer now and keeps its desktop shape; "+
				"a row override or a hidden build line is the top-bar layout coming back", sel, body)
		}
	}
}

// TestCSSRuleListRefusesUnbalancedBraces pins the scanner's two halves: a
// balanced input yields the rules with at-rule bodies kept verbatim for the
// caller to recurse into, and an unbalanced one is refused rather than
// scanned best-effort.
func TestCSSRuleListRefusesUnbalancedBraces(t *testing.T) {
	rules, err := cssRuleList("a { x: 1 } @media (q) { b { y: 2 } c { z: 3 } }")
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 2 || normSelector(rules[0].sel) != "a" || compactCSS(rules[0].body) != "x:1" || normSelector(rules[1].sel) != "@media(q)" {
		t.Fatalf("rules = %+v", rules)
	}
	inner, err := cssRuleList(rules[1].body)
	if err != nil {
		t.Fatal(err)
	}
	if len(inner) != 2 || normSelector(inner[0].sel) != "b" || compactCSS(inner[1].body) != "z:3" {
		t.Fatalf("nested rules = %+v", inner)
	}
	for _, bad := range []string{"a { x: 1 } }", "} a { x: 1 }", "a { x: 1", "a { b { y: 2 }", `a { content: "}`} {
		if got, err := cssRuleList(bad); err == nil {
			t.Errorf("%q: accepted as %+v; want a refusal", bad, got)
		}
	}

	// Braces inside string literals are text: a quoted `}` must not close
	// the rule, a quoted `{` in an attribute selector must not open one, and
	// an escaped quote must not end the string early.
	quoted, err := cssRuleList(`a::after { content: "}" } [data-v='{'] { y: 2 } b { content: "\"{"; z: 3 } .c\{d { w: 4 }`)
	if err != nil {
		t.Fatal(err)
	}
	if len(quoted) != 4 || normSelector(quoted[1].sel) != `[data-v="{"]` || compactCSS(quoted[2].body) != `content:"\"{";z:3` || normSelector(quoted[3].sel) != `.c\{d` {
		t.Fatalf("quoted rules = %+v", quoted)
	}
}

// TestNormSelectorKeepsTheDescendantCombinator pins the one whitespace that
// carries meaning in a selector.
func TestNormSelectorKeepsTheDescendantCombinator(t *testing.T) {
	cases := map[string]string{
		`header[data-nav-open="true"]  .sidebar-drawer`: `header[data-nav-open="true"] .sidebar-drawer`,
		"header[data-nav-open=\"true\"].sidebar-drawer": `header[data-nav-open="true"].sidebar-drawer`,
		`header[data-nav-open='true'] .sidebar-drawer`:  `header[data-nav-open="true"] .sidebar-drawer`,
		"@media ( max-width : 1023px )":                 "@media(max-width:1023px)",
		"  .a ,\n .b  ":                                 ".a,.b",
	}
	for in, want := range cases {
		if got := normSelector(in); got != want {
			t.Errorf("normSelector(%q) = %q, want %q", in, got, want)
		}
	}
}

// --- helpers ------------------------------------------------------------

func byID(id string) func(*html.Node) bool {
	return func(n *html.Node) bool { return railAttr(n, "id") == id }
}

func byClass(class string) func(*html.Node) bool {
	return func(n *html.Node) bool { return railHasClass(n, class) }
}

func railFind(n *html.Node, pred func(*html.Node) bool) *html.Node {
	if n.Type == html.ElementNode && pred(n) {
		return n
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if m := railFind(c, pred); m != nil {
			return m
		}
	}
	return nil
}

func railFindAll(n *html.Node, pred func(*html.Node) bool) []*html.Node {
	var out []*html.Node
	if n.Type == html.ElementNode && pred(n) {
		out = append(out, n)
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		out = append(out, railFindAll(c, pred)...)
	}
	return out
}

func railAttr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

func railHasClass(n *html.Node, class string) bool {
	for _, c := range strings.Fields(railAttr(n, "class")) {
		if c == class {
			return true
		}
	}
	return false
}

// railWithin reports whether n is a strict descendant of ancestor.
func railWithin(n, ancestor *html.Node) bool {
	for p := n.Parent; p != nil; p = p.Parent {
		if p == ancestor {
			return true
		}
	}
	return false
}

type cssRule struct{ sel, body string }

// cssRuleList splits one level of comment-free CSS into (selector, body)
// pairs by brace matching. An at-rule's body holds nested rules verbatim,
// so the caller recurses into the block it wants. Brace matching is safe
// here only because the comments are already gone — a `{` in prose is the
// trap CLAUDE.md records for the deletion diffs.
//
// Unbalanced input is an ERROR, never a best-effort scan. A stray `}` would
// leave every later rule unmatched, and the absence checks the callers make
// (no `.sidebar-foot` override in the phone block) would then pass over a
// block the scan never saw. Tolerating the brace was proposed on #934; for
// a guard, refusing is the direction that cannot go quiet.
//
// A brace inside a string literal (`content: "}"`, `[data-v="{"]`) is text,
// not structure, so quoted runs are skipped, backslash escapes included —
// and a backslash outside a string escapes the next byte too (`.a\{b` is
// one identifier), so that byte is never read as a brace or a quote.
func cssRuleList(css string) ([]cssRule, error) {
	var out []cssRule
	depth, start, selStart := 0, 0, 0
	var quote byte
	// A byte loop, not `range`: the scanner only looks for ASCII braces and
	// quotes, so there is nothing to decode. (`range` would be correct too —
	// its index is the byte offset of each rune, and every bound taken here
	// is the offset of a brace, which is a rune boundary by construction.)
	for i := 0; i < len(css); i++ {
		if quote != 0 {
			switch css[i] {
			case '\\':
				i++ // the escaped byte, whatever it is, is not the closing quote
			case quote:
				quote = 0
			}
			continue
		}
		switch css[i] {
		case '\\':
			i++ // an escaped byte is part of an identifier, never structure
		case '"', '\'':
			quote = css[i]
		case '{':
			if depth == 0 {
				start = i + 1
			}
			depth++
		case '}':
			if depth == 0 {
				return nil, fmt.Errorf("stray '}' at byte %d", i)
			}
			depth--
			if depth == 0 {
				out = append(out, cssRule{sel: strings.TrimSpace(css[selStart : start-1]), body: css[start:i]})
				selStart = i + 1
			}
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("unterminated %c string", quote)
	}
	if depth != 0 {
		return nil, fmt.Errorf("%d block(s) still open at the end of the input", depth)
	}
	return out, nil
}

// cssNoSpaceAroundRe matches the whitespace a selector can lose without
// changing meaning: around parentheses, colons and commas.
var cssNoSpaceAroundRe = regexp.MustCompile(`\s*([(),:])\s*`)

// normSelector collapses whitespace runs to one space and drops the spaces
// that carry no meaning, so a selector compares by content while a
// DESCENDANT combinator — the space in `header[data-nav-open="true"]
// .sidebar-drawer` — survives. compactCSS would fold that onto the compound
// `header[…].sidebar-drawer`, which matches nothing in the DOM, and a lookup
// keyed on it would accept a stylesheet whose drawer never opens. Attribute
// quotes are folded to double, so a cosmetic `'true'` cannot fail a lookup.
func normSelector(s string) string {
	s = cssNoSpaceAroundRe.ReplaceAllString(strings.Join(strings.Fields(s), " "), "$1")
	return strings.ReplaceAll(s, "'", `"`)
}

// compactCSS drops every whitespace character, for DECLARATIONS — where no
// space carries meaning — so a body compares by content rather than by
// formatting. Selectors go through normSelector.
func compactCSS(s string) string {
	return strings.Join(strings.Fields(s), "")
}

// splitSelectorGroup splits a CSS selector group on its TOP-LEVEL commas.
//
// Quotes and brackets are tracked because a comma inside `[title="a,b"]`
// or `:is(a, b)` does not separate group members, and a naive Split would
// produce two keys that match nothing — a guard that silently stops
// guarding, which is the failure this helper exists to remove rather than
// to reintroduce one level down.
func splitSelectorGroup(sel string) []string {
	var (
		out     []string
		cur     strings.Builder
		quote   rune
		depth   int
		escaped bool
	)
	flush := func() {
		if t := strings.TrimSpace(cur.String()); t != "" {
			out = append(out, t)
		}
		cur.Reset()
	}
	for _, r := range sel {
		// A backslash escapes the next rune, so `[title="a\"b"]` does not
		// close its quote. Without this the state went out of sync and the
		// rest of the group was swallowed (Gemini on #964).
		if escaped {
			escaped = false
			cur.WriteRune(r)
			continue
		}
		if r == '\\' {
			escaped = true
			cur.WriteRune(r)
			continue
		}
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			}
		case r == '\'' || r == '"':
			quote = r
		case r == '[' || r == '(':
			depth++
		case r == ']' || r == ')':
			if depth > 0 {
				depth--
			}
		case r == ',' && depth == 0:
			flush()
			continue
		}
		cur.WriteRune(r)
	}
	flush()
	return out
}

// TestSplitSelectorGroupKeepsStructuralCommas pins the helper directly,
// for the reason TestRejectedCertGradingShapesSeeEverySpacing exists: the
// guard that uses it can only fail on CSS that is ALSO wrong, so a split
// it gets wrong is indistinguishable, from the guard's side, from CSS
// that does not use grouping.
func TestSplitSelectorGroupKeepsStructuralCommas(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want []string
	}{
		{".a", []string{".a"}},
		{".sidebar-foot, .meta", []string{".sidebar-foot", ".meta"}},
		{".a,.b ,  .c", []string{".a", ".b", ".c"}},
		{`a[title="x,y"], .b`, []string{`a[title="x,y"]`, ".b"}},
		{`a[title="x\",y"], .b`, []string{`a[title="x\",y"]`, ".b"}},
		{":is(.a, .b) .c", []string{":is(.a, .b) .c"}},
		{`header[data-nav-open="true"] .sidebar-drawer`, []string{`header[data-nav-open="true"] .sidebar-drawer`}},
	} {
		got := splitSelectorGroup(tc.in)
		if len(got) != len(tc.want) {
			t.Errorf("splitSelectorGroup(%q) = %q, want %q", tc.in, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("splitSelectorGroup(%q) = %q, want %q", tc.in, got, tc.want)
				break
			}
		}
	}
}

// TestPhoneBlockNegativeAssertionsSeeAGroupedSelector is the negative
// control for the keying above: the "must not" lookups have to fire on a
// grouped rule, which is the form that slipped past them.
func TestPhoneBlockNegativeAssertionsSeeAGroupedSelector(t *testing.T) {
	rules := map[string]string{}
	for _, r := range []struct{ sel, body string }{
		{".sidebar-foot, .meta", "flex-direction: row;"},
	} {
		rules[normSelector(r.sel)] += r.body
		for _, member := range splitSelectorGroup(r.sel) {
			if m := normSelector(member); m != "" {
				rules[m] += r.body
			}
		}
	}
	for _, sel := range []string{".sidebar-foot", ".meta"} {
		if _, ok := rules[sel]; !ok {
			t.Errorf("a grouped rule is invisible to a lookup on %s — the phone-block "+
				"negative assertions would pass while the top-bar layout came back", sel)
		}
	}
}
