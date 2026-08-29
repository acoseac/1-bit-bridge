package admin

import (
	"regexp"
	"strings"
	"testing"
)

// TestEveryPushStateCarriesTheRouteTrail pins that the player pushes history
// entries through pushRoute, which is the only thing that attaches the crumb
// trail.
//
// The trail is what lets a breadcrumb say where the reader came FROM — an
// album opened from a composer belongs, for them, to that composer's list, not
// to the artist it structurally hangs off. It rides history.state, so it is
// attached at push time and nowhere else.
//
// A new navigation site that calls history.pushState directly would still
// navigate correctly and still render a crumb: the destination falls back to
// its structural chain, silently, on that path only. That is the drift this
// catches — a bug with no error, visible only to someone who happened to
// arrive by the one route that lost its trail.
//
// replaceState is deliberately NOT covered. Those sites (a sort change, a
// search refinement, the scroll-position write) stay on the same logical page
// and preserve the existing state object rather than building a new one.
func TestEveryPushStateCarriesTheRouteTrail(t *testing.T) {
	src, err := staticFS.ReadFile("static/player/boot.js")
	if err != nil {
		t.Fatal(err)
	}
	body := stripJSComments(string(src))

	// Captures to the end of the LINE rather than to the first ")". The
	// arguments contain a nested call — pushState({… trail: trailFor(href) }…)
	// — so a paren-terminated match stops inside it, and an earlier
	// semicolon-excluding form could be truncated by an inline comment that
	// happened to contain one, failing a correct call. A multi-line
	// pushState captures nothing and fails loudly, which is the right
	// answer anyway: use pushRoute.
	pushes := regexp.MustCompile(`history\.pushState\(([^\n]*)`).FindAllStringSubmatch(body, -1)
	if len(pushes) == 0 {
		t.Fatal("no history.pushState call found in boot.js — the player navigates " +
			"somehow, so this test has stopped finding what it checks")
	}
	for _, m := range pushes {
		if !strings.Contains(m[1], "trail") {
			t.Errorf("history.pushState without a trail: %s\n"+
				"Push through pushRoute() instead. A raw pushState navigates fine and "+
				"renders a crumb, but the destination silently falls back to its "+
				"structural ancestors — so the breadcrumb is wrong only for readers "+
				"who arrived by this route, with nothing failing anywhere.",
				strings.Join(strings.Fields(m[1]), " "))
		}
	}

	// pushRoute is the funnel; if it stops existing this test still passes
	// above while checking nothing meaningful.
	if !strings.Contains(body, "function pushRoute(") {
		t.Error("pushRoute() is gone from boot.js — the trail has no single " +
			"attachment point any more, and the check above degrades to " +
			"whatever the remaining raw pushState calls happen to say")
	}
}

// stripJSComments removes block comments and // comments, so prose that
// mentions a symbol cannot be read as code. boot.js's commentary names
// pushState and trail repeatedly; without the strip, a comment would satisfy
// the check it exists to make — and that is not hypothetical. A negative
// control that removed the trail and wrote "// keep the trail? no" beside it
// PASSED against a line-comment-only stripper, because the scan reads to the
// end of the line and the comment supplied the word.
//
// The // strip is quote-aware rather than a plain cut at the first slashes:
// boot.js contains both e.href.startsWith("//") and a quoted "//evil.com", and
// a naive rule would cut a line of real code in half. Regex literals would
// need the same care; boot.js has none (splitPath is deliberately string ops),
// and one appearing later would at worst truncate a line this test does not
// read.
func stripJSComments(src string) string {
	src = regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(src, "")
	lines := strings.Split(src, "\n")
	for i, line := range lines {
		lines[i] = stripLineComment(line)
	}
	return strings.Join(lines, "\n")
}

// stripLineComment cuts a line at the first // that is outside a string
// literal, tracking single, double and template quotes and backslash escapes.
func stripLineComment(line string) string {
	var quote byte
	escaped := false
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case escaped:
			escaped = false
		case quote != 0 && c == '\\':
			escaped = true
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'' || c == '`':
			quote = c
		case c == '/' && i+1 < len(line) && line[i+1] == '/':
			return line[:i]
		}
	}
	return line
}
