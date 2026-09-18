package admin

import (
	"net/http"
	"net/http/httptest"
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
	doc, err := html.Parse(strings.NewReader(rw.Body.String()))
	if err != nil {
		t.Fatalf("page does not parse as HTML: %v", err)
	}

	header := railFind(doc, func(n *html.Node) bool { return n.Data == "header" && railHasClass(n, "sidebar") })
	if header == nil {
		t.Fatal("no <header class=\"sidebar\"> in the rendered page")
	}
	drawer := railFind(header, func(n *html.Node) bool { return railAttr(n, "id") == "sidebar-drawer" })
	if drawer == nil {
		t.Fatal("no #sidebar-drawer inside the header")
	}

	// The closed set. display: contents hands the drawer's children to the
	// header's flex column only when nothing sits between, and the phone
	// bar is the header's children minus the drawer — so both halves depend
	// on the header having exactly these two element children.
	for c := header.FirstChild; c != nil; c = c.NextSibling {
		if c.Type != html.ElementNode {
			continue
		}
		if railHasClass(c, "sidebar-head") || railAttr(c, "id") == "sidebar-drawer" {
			continue
		}
		t.Errorf("header.sidebar has a child <%s class=%q id=%q> that is neither .sidebar-head nor #sidebar-drawer: "+
			"below 1024px it renders in the TOP BAR beside the brand, which is how the space meter ended up there. "+
			"Put it inside the drawer, or make the case for the bar in a comment and here.",
			c.Data, railAttr(c, "class"), railAttr(c, "id"))
	}

	// What the drawer must carry: the two IDs the phone breakpoint used to
	// leave in the bar, plus the three the nav toggle already controlled.
	for _, id := range []string{"primary-nav", "space-meter", "conn-status", "theme-toggle", "logout-btn"} {
		n := railFind(header, func(n *html.Node) bool { return railAttr(n, "id") == id })
		if n == nil {
			t.Errorf("#%s is not rendered in the public-mode header", id)
			continue
		}
		if !railWithin(n, drawer) {
			t.Errorf("#%s is outside #sidebar-drawer: below 1024px it sits in the top bar", id)
		}
	}
	if n := railFind(header, func(n *html.Node) bool { return railHasClass(n, "build-line") }); n == nil || !railWithin(n, drawer) {
		t.Error(".build-line is missing or outside #sidebar-drawer")
	}

	// And what it must not: the two things the bar IS.
	brand := railFind(header, func(n *html.Node) bool { return railHasClass(n, "brand") })
	if brand == nil || railWithin(brand, drawer) {
		t.Error(".brand is missing or inside the drawer — the bar would be empty until the menu opens")
	}
	toggle := railFind(header, func(n *html.Node) bool { return railAttr(n, "id") == "nav-toggle" })
	if toggle == nil || railWithin(toggle, drawer) {
		t.Fatal("#nav-toggle is missing or inside the drawer it opens")
	}
	if got := railAttr(toggle, "aria-controls"); got != "sidebar-drawer" {
		t.Errorf("#nav-toggle aria-controls = %q, want \"sidebar-drawer\" — the button toggles the whole drawer, not the nav alone", got)
	}
}

// TestPhoneShellHidesTheDrawerAndTheDesktopRailDissolvesIt is the CSS half.
func TestPhoneShellHidesTheDrawerAndTheDesktopRailDissolvesIt(t *testing.T) {
	css := readCSSNoComments(t, "static/app.css")
	top := cssRuleList(css)

	var desktop []string
	var phone string
	for _, r := range top {
		switch compactCSS(r.sel) {
		case ".sidebar-drawer":
			desktop = append(desktop, r.body)
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
	if phone == "" {
		t.Fatal("no @media (max-width: 1023px) block in app.css — the shell breakpoint moved; update this test")
	}

	phoneRules := map[string]string{}
	for _, r := range cssRuleList(phone) {
		phoneRules[compactCSS(r.sel)] += r.body
	}
	if !strings.Contains(compactCSS(phoneRules[".sidebar-drawer"]), "display:none") {
		t.Errorf("the phone block must hide .sidebar-drawer (got %q)", phoneRules[".sidebar-drawer"])
	}
	if open := compactCSS(phoneRules[`header[data-nav-open="true"].sidebar-drawer`]); !strings.Contains(open, "position:absolute") {
		t.Errorf("the phone block must reveal header[data-nav-open=\"true\"] .sidebar-drawer as the absolutely-positioned dropdown (got %q)", open)
	}
	if strings.Contains(compactCSS(phoneRules["#primary-nav"]), "display:none") {
		t.Error("the phone block hides #primary-nav alone — that leaves the space meter and the foot block in the top bar; hide .sidebar-drawer")
	}
	for _, sel := range []string{".sidebar-foot", ".build-line", ".meta"} {
		if body, ok := phoneRules[sel]; ok {
			t.Errorf("the phone block styles %s (%q): the foot block sits in the drawer now and keeps its desktop shape; "+
				"a row override or a hidden build line is the top-bar layout coming back", sel, body)
		}
	}
}

// --- helpers ------------------------------------------------------------

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
func cssRuleList(css string) []cssRule {
	var out []cssRule
	depth, start, selStart := 0, 0, 0
	for i, ch := range css {
		switch ch {
		case '{':
			if depth == 0 {
				start = i + 1
			}
			depth++
		case '}':
			depth--
			if depth == 0 {
				out = append(out, cssRule{sel: strings.TrimSpace(css[selStart : start-1]), body: css[start:i]})
				selStart = i + 1
			}
		}
	}
	return out
}

// compactCSS drops every whitespace character so a selector or declaration
// compares by content rather than by formatting.
func compactCSS(s string) string {
	return strings.Join(strings.Fields(s), "")
}
