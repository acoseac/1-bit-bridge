package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

const testDeviceToken = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"

func seedDataFixture(t *testing.T, srv *Server) {
	t.Helper()
	ctx := context.Background()
	// One device registration so the per-device history filter can resolve.
	if err := srv.deps.Manifest.UpsertDeviceRegistration(ctx, testDeviceToken, "tok-1", "Test iPhone"); err != nil {
		t.Fatalf("UpsertDeviceRegistration: %v", err)
	}
	// A playlist with a local + a foreign item.
	pl := manifest.PlaylistRow{ID: "pl-1", Name: "Road Trip", LastModifiedAt: 1_700_000_000_000_000_000}
	items := []manifest.PlaylistItemRow{
		{Position: 0, Path: "Artist/Album/01.flac", Title: "First", Artist: "A"},
		{Position: 1, OriginFingerprint: "AA:BB", OriginPath: "Other/02.flac", Title: "Second", Artist: "B"},
	}
	if err := srv.deps.Manifest.UpsertPlaylist(ctx, testDeviceToken, pl, items); err != nil {
		t.Fatalf("UpsertPlaylist: %v", err)
	}
	// A couple of history events.
	if err := srv.deps.Manifest.InsertHistoryBatch(ctx, []manifest.PlaybackHistoryRow{
		{DeviceToken: testDeviceToken, Path: "Artist/Album/01.flac", StartedAt: 1_700_000_000_000_000_000, DurationUsed: 30.5, Codec: "FLAC", IfaceType: "CarPlay", OutputRate: 48000},
		{DeviceToken: testDeviceToken, Path: "Artist/Album/02.flac", StartedAt: 1_700_000_100_000_000_000, DurationUsed: 12, Codec: "DSF", IfaceType: "USB-DAC", OutputRate: 176400, IsDoP: true},
	}); err != nil {
		t.Fatalf("InsertHistoryBatch: %v", err)
	}
}

func TestDataPageRenders(t *testing.T) {
	srv, _, _ := newTestServer(t)
	req := httptest.NewRequest("GET", "/data", nil)
	req.RemoteAddr = "127.0.0.1:5000"
	rw := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rw, req)
	if rw.Code != 200 {
		t.Fatalf("GET /data: %d", rw.Code)
	}
	if !strings.Contains(rw.Body.String(), "Playlists") {
		t.Errorf("data page missing expected content")
	}
}

func TestPlaylistDetailAndExport(t *testing.T) {
	srv, _, _ := newTestServer(t)
	seedDataFixture(t, srv)
	h := srv.Handler()

	// Detail: full ordered contents, foreign flag set on item 2.
	var detail playlistDetailDTO
	code := doJSON(t, h, "GET", "/api/playlists/detail?device=abcdef01&id=pl-1", nil, &detail)
	if code != 200 {
		t.Fatalf("detail: %d", code)
	}
	if detail.Name != "Road Trip" || len(detail.Items) != 2 {
		t.Fatalf("detail wrong: %+v", detail)
	}
	if detail.Items[0].Foreign || !detail.Items[1].Foreign {
		t.Errorf("foreign flags wrong: %+v", detail.Items)
	}

	// Unknown playlist → 404.
	if code := doJSON(t, h, "GET", "/api/playlists/detail?device=abcdef01&id=nope", nil, nil); code != 404 {
		t.Errorf("unknown playlist: got %d, want 404", code)
	}

	// CSV export: content-type + disposition + header row.
	rec := doExport(t, h, "/api/playlists/export?device=abcdef01&id=pl-1&format=csv")
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/csv") {
		t.Errorf("csv content-type = %q", ct)
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "attachment") || !strings.Contains(cd, ".csv") {
		t.Errorf("csv disposition = %q", cd)
	}
	if !strings.Contains(rec.Body.String(), "position,title,artist,path") {
		t.Errorf("csv missing header: %q", rec.Body.String())
	}

	// M3U8 export: #EXTM3U + an #EXTINF for the local item + a foreign comment.
	rec = doExport(t, h, "/api/playlists/export?device=abcdef01&id=pl-1&format=m3u8")
	if ct := rec.Header().Get("Content-Type"); ct != "audio/x-mpegurl" {
		t.Errorf("m3u8 content-type = %q", ct)
	}
	body := rec.Body.String()
	if !strings.HasPrefix(body, "#EXTM3U") || !strings.Contains(body, "#EXTINF") || !strings.Contains(body, "# foreign") {
		t.Errorf("m3u8 body malformed:\n%s", body)
	}

	// Bad format → 400.
	rec = doExport(t, h, "/api/playlists/export?device=abcdef01&id=pl-1&format=xml")
	if rec.Code != 400 {
		t.Errorf("bad format: got %d, want 400", rec.Code)
	}
}

func TestHistoryEventsAndExport(t *testing.T) {
	srv, _, _ := newTestServer(t)
	seedDataFixture(t, srv)
	h := srv.Handler()

	// Global feed (no device param).
	var page struct {
		Events     []historyEventDTO `json:"events"`
		NextCursor int64             `json:"nextCursor"`
	}
	if code := doJSON(t, h, "GET", "/api/history/events?limit=50", nil, &page); code != 200 {
		t.Fatalf("events: %d", code)
	}
	if len(page.Events) != 2 || page.NextCursor == 0 {
		t.Fatalf("events page wrong: %+v", page)
	}
	if page.Events[0].Route == "" || page.Events[0].StartedAt == "" {
		t.Errorf("event DTO missing fields: %+v", page.Events[0])
	}

	// Per-device filter resolves the prefix.
	if code := doJSON(t, h, "GET", "/api/history/events?device=abcdef01&limit=50", nil, &page); code != 200 {
		t.Fatalf("device events: %d", code)
	}
	if len(page.Events) != 2 {
		t.Errorf("device-scoped events: got %d, want 2", len(page.Events))
	}

	// Unknown device → 404.
	if code := doJSON(t, h, "GET", "/api/history/events?device=ffffffff", nil, nil); code != 404 {
		t.Errorf("unknown device: got %d, want 404", code)
	}

	// CSV export.
	rec := doExport(t, h, "/api/history/export?format=csv")
	if !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/csv") {
		t.Errorf("history csv content-type = %q", rec.Header().Get("Content-Type"))
	}
	if !strings.Contains(rec.Body.String(), "started_at,path,codec") {
		t.Errorf("history csv missing header")
	}
}

// doExport fires a GET and returns the raw recorder (exports aren't JSON).
func doExport(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	req.RemoteAddr = "127.0.0.1:5000"
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)
	return rw
}
