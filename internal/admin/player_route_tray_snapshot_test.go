package admin

import (
	"regexp"
	"strings"
	"testing"
)

// TestThePlayerRouteDropsTheTraySnapshot pins the two halves of the player's
// drop of the feature trays' shared settings snapshot: app.js publishes its
// invalidateTraySettings as window.BridgeFeatureTray.invalidate, and boot.js's
// route() calls it.
//
// An operator page drops the snapshot in its page init (dispatchPageInit),
// which the player never runs for its own navigation. Until 2026-09-28 a
// player tray therefore showed a switch as it was when the page loaded, and
// once the Smart mixes page read its switch from the server, a switch turned
// on in Settings read on in the page and off in its gear after the player's
// own navigation back to it (seen in a browser before the drop went in).
//
// Structural, because route() cannot run under node without booting the
// whole player; the drop's effect was measured in a browser. Comments are
// stripped first: the commentary beside both halves names the call.
func TestThePlayerRouteDropsTheTraySnapshot(t *testing.T) {
	app := stripJSComments(readConsoleJS(t, "static/app.js"))
	exported := regexp.MustCompile(`window\.BridgeFeatureTray\s*=\s*\{[^}]*\binvalidate\s*:\s*invalidateTraySettings\b`)
	if !exported.MatchString(app) {
		t.Error("app.js does not publish invalidateTraySettings as window.BridgeFeatureTray.invalidate, " +
			"so the player has nothing to call when it routes")
	}
	boot := stripJSComments(readConsoleJS(t, "static/player/boot.js"))
	body := jsFunctionBodies(boot)["route"]
	if body == "" {
		t.Fatal("no top-level function route in boot.js, so this test checks nothing")
	}
	if !regexp.MustCompile(`window\.BridgeFeatureTray\?\.invalidate\?\.\(\)`).MatchString(body) {
		t.Error("boot.js's route() does not call window.BridgeFeatureTray?.invalidate?.(): a player " +
			"tray then shows a switch as it was when the page loaded, beside a view that reads the " +
			"server fresh")
	}
	if strings.Count(body, "invalidate") > 1 {
		t.Error("boot.js's route() drops the tray snapshot more than once per route")
	}
}
