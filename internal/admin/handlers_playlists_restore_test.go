package admin

import (
	"context"
	"fmt"
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
	if got.Burst != nil {
		t.Errorf("burst = %+v, want none on an empty list", got.Burst)
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

// The panel's sentence and the journal's "playlist mass delete" line have
// to describe one event, so the grouping is the server's — computed from
// the whole `deleted_by` token over a fixed window, not reconstructed in
// the browser from eight redacted characters and a chain of gaps
// (CodeRabbit on #942). This drives the real handler with the shape that
// reading got wrong: one delete from a second device inside the run.
func TestTheServedBurstSpansAnInterleavedDevice(t *testing.T) {
	srv, _, _ := newTestServer(t)
	ctx := context.Background()
	store := srv.deps.Manifest

	if err := store.UpsertDeviceRegistration(ctx, "aaaa1111bbbb", "tok-a", "Studio Mac"); err != nil {
		t.Fatalf("register A: %v", err)
	}
	if err := store.UpsertDeviceRegistration(ctx, "beefcafedead", "tok-b", "New iPhone"); err != nil {
		t.Fatalf("register B: %v", err)
	}

	// Real time, not an injected clock: manifest's is package-private, and
	// the property under test is the GROUPING, not the span. Seven
	// tombstones written back to back land well inside the window, which
	// is the state the incident produced. The span is pinned in
	// manifest's own unit tests, where the clock can be set.
	seed := func(id, deleter string) {
		t.Helper()
		p := manifest.PlaylistRow{ID: id, Name: id, LastModifiedAt: 1_700_000_000_000_000_000}
		if err := store.UpsertPlaylist(ctx, "aaaa1111bbbb", p,
			[]manifest.PlaylistItemRow{{Position: 0, Path: "A/B/c.flac"}}); err != nil {
			t.Fatalf("upsert %s: %v", id, err)
		}
		if ok, err := store.TombstonePlaylist(ctx, id, deleter); err != nil || !ok {
			t.Fatalf("tombstone %s: ok=%v err=%v", id, ok, err)
		}
	}
	// Six from A, with one from B landing in the middle of the run.
	for i := 0; i < 6; i++ {
		seed(fmt.Sprintf("pl-a%d", i), "aaaa1111bbbb")
		if i == 2 {
			seed("pl-b0", "beefcafedead")
		}
	}

	var got deletedPlaylistsResponse
	if code := doJSON(t, srv.Handler(), "GET", "/api/playlists/deleted", nil, &got); code != 200 {
		t.Fatalf("GET deleted: %d", code)
	}
	if len(got.Deleted) != 7 {
		t.Fatalf("deleted rows = %d, want 7", len(got.Deleted))
	}
	if got.Burst == nil {
		t.Fatal("no burst served — one delete from another device must not end the run")
	}
	if got.Burst.Count != 6 {
		t.Errorf("burst count = %d, want 6 (the chain-of-gaps reading would say 3)", got.Burst.Count)
	}
	if got.Burst.DeviceName != "Studio Mac" {
		t.Errorf("burst device = %q, want Studio Mac", got.Burst.DeviceName)
	}
	// The prefix is for display only; the grouping never used it.
	if got.Burst.DevicePrefix != "aaaa1111…" {
		t.Errorf("burst devicePrefix = %q, want the redacted aaaa1111…", got.Burst.DevicePrefix)
	}
}

// Three deletes is a person tidying up, and the panel must not dress it
// up as a mass delete.
func TestNoBurstIsServedBelowTheThreshold(t *testing.T) {
	srv, _, _ := newTestServer(t)
	seedDataFixture(t, srv)
	ctx := context.Background()
	if ok, err := srv.deps.Manifest.TombstonePlaylist(ctx, "pl-1", "beefcafedead"); err != nil || !ok {
		t.Fatalf("tombstone: ok=%v err=%v", ok, err)
	}
	var got deletedPlaylistsResponse
	if code := doJSON(t, srv.Handler(), "GET", "/api/playlists/deleted", nil, &got); code != 200 {
		t.Fatalf("GET deleted: %d", code)
	}
	if len(got.Deleted) != 1 {
		t.Fatalf("deleted rows = %d, want 1", len(got.Deleted))
	}
	if got.Burst != nil {
		t.Errorf("burst = %+v, want none for a single delete", got.Burst)
	}
}
