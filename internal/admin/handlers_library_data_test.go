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

// TestM3U8ExportSanitizesNewlineInjection pins the line-injection fix:
// device-supplied playlist names / tags / paths containing CR/LF must
// not be able to start a new playlist line. A bare injected line is a
// media LOCATION in M3U — pre-fix, a hostile value like
// "Song\nhttp://evil/beacon" turned the export into a network beacon
// the moment the operator opened it in a player.
func TestM3U8ExportSanitizesNewlineInjection(t *testing.T) {
	srv, _, _ := newTestServer(t)
	ctx := context.Background()
	if err := srv.deps.Manifest.UpsertDeviceRegistration(ctx, testDeviceToken, "tok-1", "Test iPhone"); err != nil {
		t.Fatalf("UpsertDeviceRegistration: %v", err)
	}
	pl := manifest.PlaylistRow{ID: "pl-evil", Name: "Road Trip\nEVIL-NAME", LastModifiedAt: 1_700_000_000_000_000_000}
	items := []manifest.PlaylistItemRow{
		// Local item: injection via title + artist.
		{Position: 0, Path: "Artist/Album/01.flac", Title: "First\nEVIL-TITLE", Artist: "A\r\nEVIL-ARTIST"},
		// Foreign item: injection via both origin fields.
		{Position: 1, OriginFingerprint: "AA:BB\nEVIL-FP", OriginPath: "Other/02.flac\nEVIL-OP"},
		// Unresolvable local item: injection via the path echoed in
		// the "# unresolved:" comment.
		{Position: 2, Path: "../escape\nEVIL-PATH"},
	}
	if err := srv.deps.Manifest.UpsertPlaylist(ctx, testDeviceToken, pl, items); err != nil {
		t.Fatalf("UpsertPlaylist: %v", err)
	}

	rec := doExport(t, srv.Handler(), "/api/playlists/export?device=abcdef01&id=pl-evil&format=m3u8")
	if rec.Code != 200 {
		t.Fatalf("export: %d", rec.Code)
	}
	body := rec.Body.String()
	for i, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "EVIL") {
			t.Errorf("line %d starts with injected payload %q — newline sanitization regressed:\n%s", i, line, body)
		}
	}
	// The flattened values must still be present (replaced with a
	// space, not dropped).
	if !strings.Contains(body, "#PLAYLIST:Road Trip EVIL-NAME") {
		t.Errorf("playlist name not flattened-in-place:\n%s", body)
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

// TestHistoryExportPagesPastStoreLimit pins the cursor-paging fix: an
// export of more than the store's 1000-per-call cap must return every
// event, not silently truncate to the 200 default (Gemini on PR #341).
func TestHistoryExportPagesPastStoreLimit(t *testing.T) {
	srv, _, _ := newTestServer(t)
	h := srv.Handler()
	ctx := context.Background()

	const n = 1500
	batch := make([]manifest.PlaybackHistoryRow, 0, n)
	for i := 0; i < n; i++ {
		batch = append(batch, manifest.PlaybackHistoryRow{
			DeviceToken: testDeviceToken, Path: "Artist/Album/t.flac",
			StartedAt: int64(i + 1), DurationUsed: 1, Codec: "FLAC", IfaceType: "USB-DAC",
		})
	}
	if err := srv.deps.Manifest.InsertHistoryBatch(ctx, batch); err != nil {
		t.Fatalf("InsertHistoryBatch: %v", err)
	}

	rec := doExport(t, h, "/api/history/export?format=csv")
	if rec.Code != 200 {
		t.Fatalf("export: %d", rec.Code)
	}
	// CSV = 1 header line + n data lines.
	lines := strings.Split(strings.TrimRight(rec.Body.String(), "\n"), "\n")
	if got := len(lines) - 1; got != n { // minus header
		t.Errorf("export rows = %d, want %d (truncated at store cap?)", got, n)
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
