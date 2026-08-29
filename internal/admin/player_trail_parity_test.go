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

	pushes := regexp.MustCompile(`history\.pushState\(([^;]*?)\)`).FindAllStringSubmatch(body, -1)
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

// stripJSComments removes // and /* */ comments so prose that mentions a
// symbol cannot be read as code. This file's own commentary names pushState
// and trail repeatedly; without the strip, a comment would satisfy the check
// it exists to make.
func stripJSComments(src string) string {
	src = regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(src, "")
	return regexp.MustCompile(`(?m)^\s*//.*$`).ReplaceAllString(src, "")
}
