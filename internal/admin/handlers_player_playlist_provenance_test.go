package admin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

const provPlaylistID = "22222222-2222-4222-8222-222222222222"

// seedProvenancePlaylist lays down a registered device and a playlist it
// backed up, mixing a resolvable member, a foreign reference, and a local
// path with no track behind it.
func seedProvenancePlaylist(t *testing.T, srv *Server) {
	t.Helper()
	ctx := t.Context()
	seedCollectionLibrary(t, srv.deps.Manifest)
	if err := srv.deps.Manifest.UpsertDeviceRegistration(ctx, testDeviceToken, "tok-1", "Kitchen iPad"); err != nil {
		t.Fatalf("UpsertDeviceRegistration: %v", err)
	}
	err := srv.deps.Manifest.UpsertPlaylist(ctx, testDeviceToken, manifest.PlaylistRow{
		ID: provPlaylistID, Name: "Road Trip", LastModifiedAt: time.Now().UnixNano(),
	}, []manifest.PlaylistItemRow{
		{Position: 0, Path: "A/Alpha/01.flac", Title: "A1", Artist: "Alpha Artist"},
		{Position: 1, OriginFingerprint: "AA:BB", OriginPath: "Other/02.flac", Title: "Second", Artist: "B"},
		{Position: 2, Path: "Gone/Missing/03.flac", Title: "Third", Artist: "C"},
	})
	if err != nil {
		t.Fatalf("UpsertPlaylist: %v", err)
	}
}

// A playlist listing is a BACKUP listing, and the two facts that makes it
// one — which device wrote it and when the bridge received it — used to
// live only on the retired operator table. Moving playlists into the
// player without them would have been a loss of information dressed as a
// consolidation.
func TestPlayerPlaylistsCarryBackupProvenance(t *testing.T) {
	srv, _, _ := newTestServer(t)
	seedProvenancePlaylist(t, srv)

	w, body := playerGet(t, srv, "/api/player/playlists")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	rows, _ := body["collections"].([]any)
	if len(rows) != 1 {
		t.Fatalf("collections = %d, want 1", len(rows))
	}
	c, _ := rows[0].(map[string]any)
	if got, _ := c["deviceName"].(string); got != "Kitchen iPad" {
		t.Errorf("deviceName = %q, want %q — the roster knows this device's name", got, "Kitchen iPad")
	}
	// The prefix rides along even when the name resolves: two devices can
	// share a name, and the prefix is what the CLI and every other console
	// surface key on, so it has to stay recoverable.
	if got, _ := c["deviceTokenPrefix"].(string); !strings.HasPrefix(testDeviceToken, strings.TrimSuffix(got, "…")) || got == "" {
		t.Errorf("deviceTokenPrefix = %q, want a redacted prefix of the device token", got)
	}
	if got, _ := c["updatedAt"].(string); got == "" {
		t.Error("updatedAt is empty — the listing cannot say whether this device is still syncing")
	}

	// The DETAIL carries it too: the tile answers "is this current?" and
	// the page has to answer the same question without going back.
	w, body = playerGet(t, srv, "/api/player/playlists/"+provPlaylistID)
	if w.Code != http.StatusOK {
		t.Fatalf("detail status %d: %s", w.Code, w.Body.String())
	}
	coll, _ := body["collection"].(map[string]any)
	if got, _ := coll["deviceName"].(string); got != "Kitchen iPad" {
		t.Errorf("detail deviceName = %q, want %q", got, "Kitchen iPad")
	}
	if got, _ := coll["updatedAt"].(string); got == "" {
		t.Error("detail updatedAt is empty")
	}
}

// A smart mix is generated HERE, so it has no device and no receipt time.
// omitempty has to drop both, or the mix page would render a "Backed up
// by" line about a backup that does not exist.
func TestPlayerMixesCarryNoProvenance(t *testing.T) {
	srv, _, _ := newTestServer(t)
	seedCollectionLibrary(t, srv.deps.Manifest)
	if err := srv.deps.Manifest.ReplaceSmartPlaylists(t.Context(), []manifest.StoredSmartPlaylist{{
		Slug: "heavy-rotation", Title: "Heavy Rotation", Kind: "heavyRotation",
		Position: 0, ItemsJSON: []byte(`[{"path":"A/Alpha/01.flac"}]`),
	}}); err != nil {
		t.Fatalf("ReplaceSmartPlaylists: %v", err)
	}

	w, body := playerGet(t, srv, "/api/player/mixes")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	rows, _ := body["collections"].([]any)
	if len(rows) != 1 {
		t.Fatalf("collections = %d, want 1", len(rows))
	}
	c, _ := rows[0].(map[string]any)
	for _, key := range []string{"deviceName", "deviceTokenPrefix", "updatedAt"} {
		if _, present := c[key]; present {
			t.Errorf("mix carries %q — a generated mix has no backup provenance to report", key)
		}
	}
}

// The count alone was all the player ever had. Naming the members is the
// one thing the retired operator table could do that this could not, and
// it is what an operator repairing a backup actually needs.
func TestPlayerPlaylistNamesUnresolvedMembers(t *testing.T) {
	srv, _, _ := newTestServer(t)
	seedProvenancePlaylist(t, srv)

	w, body := playerGet(t, srv, "/api/player/playlists/"+provPlaylistID)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if n, _ := body["unresolved"].(float64); int(n) != 2 {
		t.Fatalf("unresolved = %v, want 2 (a foreign ref and a path with no track)", body["unresolved"])
	}
	items, _ := body["unresolvedItems"].([]any)
	if len(items) != 2 {
		t.Fatalf("unresolvedItems = %d, want 2: %+v", len(items), items)
	}

	byPos := map[int]map[string]any{}
	for _, raw := range items {
		it, _ := raw.(map[string]any)
		pos, _ := it["position"].(float64)
		byPos[int(pos)] = it
	}
	foreign := byPos[1]
	if foreign == nil {
		t.Fatalf("the foreign member (position 1) is missing: %+v", items)
	}
	if got, _ := foreign["foreign"].(bool); !got {
		t.Error("the foreign member is not flagged foreign — the reader cannot tell " +
			"'another bridge has this' from 'this is gone'")
	}
	if got, _ := foreign["origin"].(string); got != "Other/02.flac" {
		t.Errorf("foreign origin = %q, want the origin PATH (not the fingerprint, "+
			"which identifies a bridge the reader can do nothing with)", got)
	}
	if got, _ := foreign["title"].(string); got != "Second" {
		t.Errorf("foreign title = %q, want %q — the stored title is the only name this member has", got, "Second")
	}

	missing := byPos[2]
	if missing == nil {
		t.Fatalf("the removed-since member (position 2) is missing: %+v", items)
	}
	if got, _ := missing["foreign"].(bool); got {
		t.Error("a local path with no track behind it is flagged foreign — it is ours and gone")
	}
	if got, _ := missing["origin"].(string); got != "Gone/Missing/03.flac" {
		t.Errorf("missing origin = %q, want its local path", got)
	}

	// The RESOLVED member must not appear: it is playable and in the list
	// above, so naming it here would double-count it.
	if _, ok := byPos[0]; ok {
		t.Error("the resolvable member is listed as unresolved")
	}
}

// The count is exact; only the LIST is capped. A backup whose library
// lives on another bridge would otherwise name every one of its members
// in a response nobody reads past the first screen.
func TestUnresolvedPlaylistItemsCapsTheListNotTheCount(t *testing.T) {
	items := make([]manifest.PlaylistItemRow, 0, maxUnresolvedListed+50)
	for i := 0; i < maxUnresolvedListed+50; i++ {
		items = append(items, manifest.PlaylistItemRow{
			Position: i, OriginFingerprint: "AA:BB", OriginPath: "x.flac",
		})
	}
	got := unresolvedPlaylistItems(items, nil)
	if len(got) != maxUnresolvedListed {
		t.Errorf("listed %d items, want the cap of %d", len(got), maxUnresolvedListed)
	}
}

// A playlist whose members all resolve reports nothing, so the client
// renders no note at all rather than an empty disclosure.
func TestUnresolvedPlaylistItemsSilentWhenEverythingResolves(t *testing.T) {
	items := []manifest.PlaylistItemRow{{Position: 0, Path: "a.flac"}, {Position: 1, Path: "b.flac"}}
	tracks := []playerTrackDTO{{Path: "a.flac"}, {Path: "b.flac"}}
	if got := unresolvedPlaylistItems(items, tracks); got != nil {
		t.Errorf("got %+v, want nil", got)
	}
}

// The player's playlist page has no device prefix to send — playlists
// stopped being device-scoped in v1.7 — so export has to work from the id
// alone. A prefix that IS supplied still has to be checked, or the
// consistency guard would be silently gone for the callers that use it.
func TestPlaylistExportWithoutDeviceParam(t *testing.T) {
	srv, _, _ := newTestServer(t)
	seedProvenancePlaylist(t, srv)
	h := srv.Handler()

	get := func(target string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		req.RemoteAddr = "127.0.0.1:5000"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}

	for _, format := range []string{"json", "csv", "m3u8"} {
		w := get("/api/playlists/export?id=" + provPlaylistID + "&format=" + format)
		if w.Code != http.StatusOK {
			t.Errorf("export %s with no device: status %d, body %s", format, w.Code, w.Body.String())
			continue
		}
		if w.Body.Len() == 0 {
			t.Errorf("export %s with no device: empty body", format)
		}
	}

	// A supplied-but-wrong prefix is still a 404.
	if w := get("/api/playlists/export?device=deadbeef&id=" + provPlaylistID + "&format=json"); w.Code != http.StatusNotFound {
		t.Errorf("export with a mismatched device prefix: status %d, want 404 — "+
			"making the parameter optional must not make it unchecked", w.Code)
	}
	// And an unknown id is a 404 with or without one.
	if w := get("/api/playlists/export?id=no-such-playlist&format=json"); w.Code != http.StatusNotFound {
		t.Errorf("export of an unknown id: status %d, want 404", w.Code)
	}
}

// Favorites moved into the player alongside playlists and carry the same
// provenance, for the same reason: "hearts from a device that stopped
// syncing three months ago" is only visible if the date is.
func TestPlayerFavoritesCarryBackupProvenance(t *testing.T) {
	srv, _, _ := newTestServer(t)
	seedCollectionLibrary(t, srv.deps.Manifest)
	ctx := t.Context()
	if err := srv.deps.Manifest.UpsertDeviceRegistration(ctx, testDeviceToken, "tok-1", "Kitchen iPad"); err != nil {
		t.Fatalf("UpsertDeviceRegistration: %v", err)
	}
	if err := srv.deps.Manifest.UpsertFavorites(ctx, testDeviceToken, time.Now().UnixNano(),
		[]manifest.FavoriteTrackRow{{Path: "A/Alpha/01.flac"}}, nil); err != nil {
		t.Fatalf("UpsertFavorites: %v", err)
	}

	w, body := playerGet(t, srv, "/api/player/favorites")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if got, _ := body["deviceName"].(string); got != "Kitchen iPad" {
		t.Errorf("deviceName = %q, want %q", got, "Kitchen iPad")
	}
	if got, _ := body["updatedAt"].(string); got == "" {
		t.Error("updatedAt is empty")
	}

	// Never-stored carries none of it: there is no backup to describe.
	srv2, _, _ := newTestServer(t)
	_, body2 := playerGet(t, srv2, "/api/player/favorites")
	for _, key := range []string{"deviceName", "deviceTokenPrefix", "updatedAt"} {
		if _, present := body2[key]; present {
			t.Errorf("a never-stored favorites document carries %q", key)
		}
	}
}
