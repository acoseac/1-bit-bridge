package admin

import (
	"net/http"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// TestThePlayerMixesAnswerTheSmartMixSwitchLive pins that GET
// /api/player/mixes says whether smart mixes are switched on, read per
// request, and whether the control plane owns that switch.
//
// The player's Smart mixes page drew "Smart mixes are off" from the page
// seed, read once per page load, so after its own gear saved the switch on
// (the PATCH answers `live`) the page still said off beside "Saved.", and
// went on saying it through the player's own navigation until a reload
// (backlog B35, seen in a browser). The page reads this answer instead, and
// redraws from it after a save. `managed` is what keeps its "turn them on
// with the gear above" off a bridge whose gear cannot.
func TestThePlayerMixesAnswerTheSmartMixSwitchLive(t *testing.T) {
	srv, _, _ := newTestServer(t)
	h := srv.Handler()
	seedCollectionLibrary(t, srv.deps.Manifest)
	if err := srv.deps.Manifest.ReplaceSmartPlaylists(t.Context(), []manifest.StoredSmartPlaylist{{
		Slug: "heavy-rotation", Title: "Heavy Rotation", Kind: "heavyRotation",
		Position: 0, ItemsJSON: []byte(`[{"path":"A/Alpha/01.flac"}]`),
	}}); err != nil {
		t.Fatalf("ReplaceSmartPlaylists: %v", err)
	}

	for step, on := range []bool{false, true, false} {
		var resp settingsPatchResponse
		if code := doJSON(t, h, "PATCH", "/api/settings",
			map[string]any{"smartPlaylistsEnabled": on}, &resp); code != http.StatusOK {
			t.Fatalf("step %d: PATCH smartPlaylistsEnabled=%v answered %d", step, on, code)
		}
		w, body := playerGet(t, srv, "/api/player/mixes")
		if w.Code != http.StatusOK {
			t.Fatalf("step %d: /api/player/mixes answered %d: %s", step, w.Code, w.Body.String())
		}
		enabled, present := body["enabled"].(bool)
		if !present || enabled != on {
			t.Errorf("step %d: the switch is %v and the mixes answer enabled=%v (present %v): the "+
				"page cannot tell off from on without a reload", step, on, body["enabled"], present)
		}
		rows, _ := body["collections"].([]any)
		if want := map[bool]int{false: 0, true: 1}[on]; len(rows) != want {
			t.Errorf("step %d: switch %v, %d collections, want %d", step, on, len(rows), want)
		}
		if _, present := body["managed"]; present {
			t.Errorf("step %d: managed is present on a bridge that manages nothing", step)
		}
	}

	if err := srv.deps.CfgHolder.Update(srv.deps.CfgPath, func(c *config.Config) error {
		c.Deployment.ManagedSettings = []string{"smartPlaylistsEnabled"}
		return nil
	}); err != nil {
		t.Fatalf("arrange: %v", err)
	}
	_, body := playerGet(t, srv, "/api/player/mixes")
	if managed, _ := body["managed"].(bool); !managed {
		t.Errorf("with smartPlaylistsEnabled managed, the mixes answer managed=%v, want true", body["managed"])
	}
}
