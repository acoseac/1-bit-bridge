package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// The console's undo, end to end through the real route table and the
// real middleware — `doJSON` sets a loopback RemoteAddr and a JSON
// content-type, which is what csrfGuard and loopbackOnly both want.

func TestDeletedPlaylistsListIsEmptyUntilSomethingIsDeleted(t *testing.T) {
	srv, _, _ := newTestServer(t)
	seedDataFixture(t, srv)
	h := srv.Handler()

	var got deletedPlaylistsResponse
	if code := doJSON(t, h, "GET", "/api/playlists/deleted", nil, &got); code != 200 {
		t.Fatalf("GET /api/playlists/deleted: %d", code)
	}
	if len(got.Deleted) != 0 {
		t.Errorf("deleted = %+v, want none", got.Deleted)
	}
	// The two numbers the panel groups on come from the server, so the
	// sentence it renders and the bridge's own WARN describe one event.
	if got.BurstThreshold != manifest.PlaylistDeleteBurstThreshold {
		t.Errorf("burstThreshold = %d, want %d", got.BurstThreshold, manifest.PlaylistDeleteBurstThreshold)
	}
	if want := int(manifest.PlaylistDeleteBurstWindow.Seconds()); got.BurstWindowSec != want {
		t.Errorf("burstWindowSec = %d, want %d", got.BurstWindowSec, want)
	}
}

func TestDeletedPlaylistsListAndRestoreRoundTrip(t *testing.T) {
	srv, _, _ := newTestServer(t)
	seedDataFixture(t, srv)
	ctx := context.Background()
	h := srv.Handler()

	// A second device, which is the one that deletes — the shape the
	// 2026-09-20 incident had, and the one a single-device fixture cannot
	// tell apart from the writer.
	if err := srv.deps.Manifest.UpsertDeviceRegistration(ctx, "beefcafedeadbeef1234", "tok-2", "New iPhone"); err != nil {
		t.Fatalf("register deleter: %v", err)
	}
	if ok, err := srv.deps.Manifest.TombstonePlaylist(ctx, "pl-1", "beefcafedeadbeef1234"); err != nil || !ok {
		t.Fatalf("tombstone: ok=%v err=%v", ok, err)
	}

	// It leaves the live admin list...
	var live struct {
		Playlists []playlistBackupRow `json:"playlists"`
	}
	if code := doJSON(t, h, "GET", "/api/playlists", nil, &live); code != 200 {
		t.Fatalf("GET /api/playlists: %d", code)
	}
	if len(live.Playlists) != 0 {
		t.Errorf("tombstoned playlist still in the live list: %+v", live.Playlists)
	}

	// ...and appears in the deleted one, naming BOTH devices.
	var got deletedPlaylistsResponse
	if code := doJSON(t, h, "GET", "/api/playlists/deleted", nil, &got); code != 200 {
		t.Fatalf("GET /api/playlists/deleted: %d", code)
	}
	if len(got.Deleted) != 1 {
		t.Fatalf("deleted = %+v, want exactly one row", got.Deleted)
	}
	row := got.Deleted[0]
	if row.ID != "pl-1" || row.Name != "Road Trip" || row.TrackCount != 2 {
		t.Errorf("row = %+v, want pl-1/Road Trip/2 tracks", row)
	}
	if row.DeletedByName != "New iPhone" || row.DeletedByTokenID != "tok-2" {
		t.Errorf("deleter = %q/%q, want New iPhone/tok-2", row.DeletedByName, row.DeletedByTokenID)
	}
	if row.DeviceName != "Test iPhone" {
		t.Errorf("writer name = %q, want Test iPhone", row.DeviceName)
	}
	if row.DeletedAt == "" {
		t.Error("deletedAt is empty — the panel has nothing to date the row with")
	}
	// Device tokens are the client's recovery secrets: the panel gets a
	// prefix, never the whole thing.
	if row.DeletedByPrefix != "beefcafe…" {
		t.Errorf("deletedByPrefix = %q, want the redacted prefix beefcafe…", row.DeletedByPrefix)
	}

	// Restore puts it back, with its items.
	var restored playlistRestoredResponse
	if code := doJSON(t, h, "POST", "/api/playlists/pl-1/restore", nil, &restored); code != 200 {
		t.Fatalf("POST restore: %d", code)
	}
	if !restored.Restored || restored.ID != "pl-1" {
		t.Errorf("restore response = %+v", restored)
	}
	var detail playlistDetailDTO
	if code := doJSON(t, h, "GET", "/api/playlists/detail?id=pl-1", nil, &detail); code != 200 {
		t.Fatalf("detail after restore: %d", code)
	}
	if detail.Name != "Road Trip" || len(detail.Items) != 2 {
		t.Errorf("restored playlist lost content: %+v", detail)
	}

	// And the panel is empty again.
	got = deletedPlaylistsResponse{}
	if code := doJSON(t, h, "GET", "/api/playlists/deleted", nil, &got); code != 200 {
		t.Fatalf("GET deleted after restore: %d", code)
	}
	if len(got.Deleted) != 0 {
		t.Errorf("restored playlist still listed as deleted: %+v", got.Deleted)
	}
}

func TestRestoreRefusesWhatItCannotRestore(t *testing.T) {
	srv, _, _ := newTestServer(t)
	seedDataFixture(t, srv)
	h := srv.Handler()

	// pl-1 is live: the console only ever offers the button for a row it
	// just listed as deleted, so a hit here means a stale view — worth
	// saying, not worth a cheerful 200.
	if code := doJSON(t, h, "POST", "/api/playlists/pl-1/restore", nil, nil); code != http.StatusNotFound {
		t.Errorf("restore of a live playlist = %d, want 404", code)
	}
	if code := doJSON(t, h, "POST", "/api/playlists/no-such-id/restore", nil, nil); code != http.StatusNotFound {
		t.Errorf("restore of an unknown id = %d, want 404", code)
	}
}

// A tombstone written before migration v45 carries no deleter. The panel
// must render it rather than hide the very rows an upgrade inherits.
func TestADeletedRowWithNoRecordedDeleterStillLists(t *testing.T) {
	srv, _, _ := newTestServer(t)
	seedDataFixture(t, srv)
	ctx := context.Background()

	if ok, err := srv.deps.Manifest.TombstonePlaylist(ctx, "pl-1", ""); err != nil || !ok {
		t.Fatalf("tombstone: ok=%v err=%v", ok, err)
	}
	var got deletedPlaylistsResponse
	if code := doJSON(t, srv.Handler(), "GET", "/api/playlists/deleted", nil, &got); code != 200 {
		t.Fatalf("GET deleted: %d", code)
	}
	if len(got.Deleted) != 1 {
		t.Fatalf("deleted = %+v, want one row", got.Deleted)
	}
	if got.Deleted[0].DeletedByPrefix != "" || got.Deleted[0].DeletedByName != "" {
		t.Errorf("invented an attribution: %+v", got.Deleted[0])
	}
	if got.Deleted[0].ID != "pl-1" {
		t.Errorf("row = %+v, want pl-1", got.Deleted[0])
	}
}

// The restore endpoints answer on the LAN guard exactly like every other
// route on this listener. A write that skipped it would be the one route
// a page on another origin could reach.
func TestRestoreRoutesAreBehindTheConsoleGuards(t *testing.T) {
	srv, _, _ := newTestServer(t)
	h := srv.Handler()
	for _, tc := range []struct{ method, path string }{
		{"GET", "/api/playlists/deleted"},
		{"POST", "/api/playlists/pl-1/restore"},
	} {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		req.RemoteAddr = "192.168.1.5:54321"
		rw := httptest.NewRecorder()
		h.ServeHTTP(rw, req)
		if rw.Code != http.StatusForbidden {
			t.Errorf("%s %s from a LAN address = %d, want 403", tc.method, tc.path, rw.Code)
		}
	}
}
