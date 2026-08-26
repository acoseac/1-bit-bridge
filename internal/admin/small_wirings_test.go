package admin

import (
	"strings"
	"testing"
)

// TestConsoleSurfacesDeviceNames pins that the console names devices
// rather than showing a redacted token prefix.
//
// /api/devices has always carried deviceName; the retired playlists table
// rendered deviceTokenPrefix beside it, so the console showed an opaque
// hex string (a3f91c2e…) for a device whose name the bridge already knew,
// and PROTOCOL.md:664 promises the named surface.
//
// That table is gone — playlists live in the player now, and their
// "backed up by" line is resolved SERVER-side (deviceNamesByToken in
// handlers_player_collections.go, pinned by its own test). What still
// resolves names in app.js is the history page's per-device filter, so
// that is what this now guards.
func TestConsoleSurfacesDeviceNames(t *testing.T) {
	b, err := staticFS.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(b)
	for _, want := range []string{"loadDeviceNames", "loadHistoryDeviceFilter"} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js no longer defines %s; the history device filter would list raw prefixes (or nothing)", want)
		}
	}
	// The lookup must be fed from /api/devices, not invented locally.
	if !strings.Contains(js, `API.get("/api/devices")`) {
		t.Error("nothing fetches /api/devices; the name map would always be empty and every row would show a prefix")
	}
}

// TestRollbackButtonIsGatedOnCanRollback is the assertion that matters
// for the rollback wiring.
//
// POST /api/updates/rollback shipped with no caller, so a bad update was
// undoable only by curl. But rollback is a binary swap: a button offered
// when nothing is staged turns an attempted recovery into an error, at
// the exact moment the operator is least able to absorb one. The button
// must be revealed from canRollback, which the adapter derives by
// stat'ing the .bak the installer leaves behind.
func TestRollbackButtonIsGatedOnCanRollback(t *testing.T) {
	js, err := staticFS.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(js), "canRollback") {
		t.Error("app.js never reads canRollback; the Roll back button would show with nothing to roll back to")
	}
	// The Updates panel moved from the dashboard to Settings when the
	// dashboard became Stats: it is an ACTION surface (Check / Install /
	// Roll back), not a metric, and its IsSupervised caveat only makes
	// sense next to the auto-install toggle it qualifies.
	html, err := templateFS.ReadFile("templates/settings.html")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(html), `id="update-rollback"`) {
		t.Error("settings.html has no rollback button for app.js to reveal")
	}
	// Hidden at first paint: the server-rendered template cannot know
	// whether a .bak exists, so the default must be invisible and the
	// status frame must be what reveals it.
	if !strings.Contains(string(html), `id="update-rollback" class="btn" hidden`) {
		t.Error("the rollback button is not hidden by default; it would flash on every Settings load")
	}
}
