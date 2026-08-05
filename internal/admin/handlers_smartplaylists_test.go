package admin

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// uuidV4Re pins the id shape "Save as playlist" mints. LOAD-BEARING: iOS's
// restore path parses playlist ids with UUID(uuidString:) and silently skips
// anything else, so a drifting id scheme would make saved mixes invisible to
// every device.
var uuidV4Re = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func seedSmartMixCache(t *testing.T, srv *Server, rows ...manifest.StoredSmartPlaylist) {
	t.Helper()
	if err := srv.deps.Manifest.ReplaceSmartPlaylists(context.Background(), rows); err != nil {
		t.Fatalf("seed smart playlists: %v", err)
	}
}

func TestSmartMixRegenerateOne(t *testing.T) {
	srv, cfg, _ := newTestServer(t)
	h := srv.Handler()

	// Feature off (default config) → 404 gate, mirroring the wholesale handler.
	if code := doJSON(t, h, "POST", "/api/smart-playlists/heavy-rotation/regenerate", nil, nil); code != 404 {
		t.Fatalf("disabled regenerate = %d, want 404", code)
	}

	cfg.SmartPlaylists.Enabled = true
	srv.deps.CfgHolder.Store(cfg)

	// Unknown slug: not cached, not in fresh output → 404.
	if code := doJSON(t, h, "POST", "/api/smart-playlists/no-such-mix/regenerate", nil, nil); code != 404 {
		t.Fatalf("unknown slug = %d, want 404", code)
	}

	// A cached family the fresh run no longer populates (empty store → the
	// engine emits nothing) is REMOVED, its sibling left untouched.
	seedSmartMixCache(t, srv,
		manifest.StoredSmartPlaylist{Slug: "stale-mix", Kind: "heavyRotation", Title: "Stale",
			Position: 0, RefreshedAt: 1000, ItemsJSON: []byte(`[{"position":0,"path":"a.flac"}]`)},
		manifest.StoredSmartPlaylist{Slug: "sibling-mix", Kind: "recentlyPlayed", Title: "Sibling",
			Position: 1, RefreshedAt: 1000, ItemsJSON: []byte(`[{"position":0,"path":"b.flac"}]`)},
	)
	var resp struct {
		Regenerated bool `json:"regenerated"`
		Removed     bool `json:"removed"`
		ItemCount   int  `json:"itemCount"`
	}
	if code := doJSON(t, h, "POST", "/api/smart-playlists/stale-mix/regenerate", nil, &resp); code != 200 {
		t.Fatalf("regenerate cached = %d, want 200", code)
	}
	if resp.Regenerated || !resp.Removed || resp.ItemCount != 0 {
		t.Fatalf("resp = %+v, want regenerated=false removed=true itemCount=0", resp)
	}
	rows, err := srv.deps.Manifest.LoadSmartPlaylists(context.Background())
	if err != nil {
		t.Fatalf("LoadSmartPlaylists: %v", err)
	}
	if len(rows) != 1 || rows[0].Slug != "sibling-mix" || rows[0].RefreshedAt != 1000 {
		t.Fatalf("cache after removal = %+v, want only the untouched sibling", rows)
	}
}

func TestSmartMixSaveAsPlaylist(t *testing.T) {
	srv, _, _ := newTestServer(t)
	h := srv.Handler()
	ctx := context.Background()

	// Positions 5/5/9 in the blob: the saved playlist MUST re-index 0..N-1
	// (the (playlist_id, position) PK requires uniqueness).
	seedSmartMixCache(t, srv, manifest.StoredSmartPlaylist{
		Slug: "drive-mix", Kind: "listening", Title: "Drive Mix", Position: 0, RefreshedAt: 1000,
		ItemsJSON: []byte(`[
			{"position":5,"path":"A/x.flac","title":"X","artist":"AX"},
			{"position":5,"path":"B/y.flac","title":"Y","artist":"AY"},
			{"position":9,"path":"C/z.flac","title":"Z","artist":"AZ"}]`),
	})

	var resp struct {
		ID         string `json:"id"`
		Name       string `json:"name"`
		TrackCount int    `json:"trackCount"`
	}
	if code := doJSON(t, h, "POST", "/api/smart-playlists/drive-mix/save-as-playlist",
		map[string]any{}, &resp); code != 200 {
		t.Fatalf("save = %d, want 200", code)
	}
	if !uuidV4Re.MatchString(resp.ID) {
		t.Fatalf("saved id %q is not a canonical lowercase UUID v4", resp.ID)
	}
	if resp.TrackCount != 3 {
		t.Fatalf("trackCount = %d, want 3", resp.TrackCount)
	}
	if want := "Drive Mix — " + time.Now().Format("Jan 2, 2006"); resp.Name != want {
		t.Fatalf("default name = %q, want %q", resp.Name, want)
	}

	row, items, err := srv.deps.Manifest.GetPlaylist(ctx, resp.ID)
	if err != nil || row == nil {
		t.Fatalf("GetPlaylist(%s): row=%v err=%v", resp.ID, row, err)
	}
	if row.DeviceToken != smartMixSavedByToken {
		t.Errorf("device token = %q, want the %q sentinel", row.DeviceToken, smartMixSavedByToken)
	}
	if len(items) != 3 {
		t.Fatalf("items = %d, want 3", len(items))
	}
	wantPaths := []string{"A/x.flac", "B/y.flac", "C/z.flac"}
	for i, it := range items {
		if it.Position != i || it.Path != wantPaths[i] {
			t.Errorf("item %d = {pos=%d path=%q}, want {pos=%d path=%q}", i, it.Position, it.Path, i, wantPaths[i])
		}
		if it.OriginFingerprint != "" || it.OriginPath != "" {
			t.Errorf("item %d carries foreign origin fields; saved mixes are local-only", i)
		}
	}

	// Operator-supplied name wins; a second save mints a DISTINCT playlist.
	var resp2 struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if code := doJSON(t, h, "POST", "/api/smart-playlists/drive-mix/save-as-playlist",
		map[string]any{"name": "  Friday Drive  "}, &resp2); code != 200 {
		t.Fatalf("save with name = %d, want 200", code)
	}
	if resp2.Name != "Friday Drive" {
		t.Fatalf("name = %q, want trimmed %q", resp2.Name, "Friday Drive")
	}
	if resp2.ID == resp.ID {
		t.Fatalf("second save reused id %s; every save must mint a fresh playlist", resp.ID)
	}
	summaries, err := srv.deps.Manifest.ListPlaylists(ctx)
	if err != nil {
		t.Fatalf("ListPlaylists: %v", err)
	}
	if len(summaries) != 2 {
		t.Fatalf("playlists = %d, want 2", len(summaries))
	}

	// Unknown slug → 404; empty mix → 409.
	if code := doJSON(t, h, "POST", "/api/smart-playlists/nope/save-as-playlist", nil, nil); code != 404 {
		t.Fatalf("unknown slug save = %d, want 404", code)
	}
	seedSmartMixCache(t, srv, manifest.StoredSmartPlaylist{
		Slug: "empty-mix", Kind: "listening", Title: "Empty", ItemsJSON: []byte(`[]`)})
	if code := doJSON(t, h, "POST", "/api/smart-playlists/empty-mix/save-as-playlist", nil, nil); code != 409 {
		t.Fatalf("empty mix save = %d, want 409", code)
	}
}

func TestSmartMixSaveAsPlaylistFlattensTimeOfDay(t *testing.T) {
	srv, _, _ := newTestServer(t)
	h := srv.Handler()

	// The same path in two hour pools must save ONCE; positions re-index.
	hourly := []byte(`{"hourly":{
		"8":[{"position":0,"path":"a/morning.flac","title":"M"}],
		"18":[{"position":0,"path":"a/morning.flac","title":"M"},{"position":1,"path":"b/evening.flac","title":"E"}]}}`)
	seedSmartMixCache(t, srv, manifest.StoredSmartPlaylist{
		Slug: "time-of-day", Kind: "timeOfDay", Title: "Time of Day", ItemsJSON: hourly})

	var resp struct {
		ID         string `json:"id"`
		TrackCount int    `json:"trackCount"`
	}
	if code := doJSON(t, h, "POST", "/api/smart-playlists/time-of-day/save-as-playlist",
		map[string]any{}, &resp); code != 200 {
		t.Fatalf("save = %d, want 200", code)
	}
	if resp.TrackCount != 2 {
		t.Fatalf("trackCount = %d, want 2 distinct tracks", resp.TrackCount)
	}
	_, items, err := srv.deps.Manifest.GetPlaylist(context.Background(), resp.ID)
	if err != nil {
		t.Fatalf("GetPlaylist: %v", err)
	}
	for i, it := range items {
		if it.Position != i {
			t.Errorf("item %d position = %d, want re-indexed %d", i, it.Position, i)
		}
	}
}

func TestSmartMixSaveAsPlaylistCopiesCover(t *testing.T) {
	srv, cfg, _ := newTestServer(t)
	h := srv.Handler()
	ctx := context.Background()

	seedSmartMixCache(t, srv, manifest.StoredSmartPlaylist{
		Slug: "drive-mix", Kind: "listening", Title: "Drive Mix",
		ItemsJSON: []byte(`[{"position":0,"path":"A/x.flac","title":"X"}]`)})

	// Stage an operator cover for the mix (row + on-disk JPEG).
	coverBytes := []byte("jpeg-bytes-stand-in")
	coverDir := manifest.PlaylistCoverDir(cfg.DataDir)
	if err := os.MkdirAll(coverDir, 0o700); err != nil {
		t.Fatal(err)
	}
	src := manifest.PlaylistCoverPath(cfg.DataDir, manifest.CoverScopeSmartMix, "drive-mix", "jpg")
	if err := os.WriteFile(src, coverBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := srv.deps.Manifest.SetPlaylistCover(ctx, manifest.PlaylistCover{
		Scope: manifest.CoverScopeSmartMix, Key: "drive-mix",
		ImageHash: "abc123", Ext: "jpg", UpdatedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}

	var resp struct {
		ID string `json:"id"`
	}
	if code := doJSON(t, h, "POST", "/api/smart-playlists/drive-mix/save-as-playlist",
		map[string]any{}, &resp); code != 200 {
		t.Fatalf("save = %d, want 200", code)
	}

	c, ok, err := srv.deps.Manifest.GetPlaylistCover(ctx, manifest.CoverScopePlaylist, resp.ID)
	if err != nil || !ok {
		t.Fatalf("playlist cover mapping missing: ok=%v err=%v", ok, err)
	}
	if c.ImageHash != "abc123" || c.Ext != "jpg" {
		t.Fatalf("cover mapping = %+v, want the mix's hash/ext", c)
	}
	dst := manifest.PlaylistCoverPath(cfg.DataDir, manifest.CoverScopePlaylist, resp.ID, "jpg")
	got, err := os.ReadFile(dst)
	if err != nil || !bytes.Equal(got, coverBytes) {
		t.Fatalf("copied cover file mismatch: err=%v", err)
	}
	if filepath.Base(dst) == filepath.Base(src) {
		t.Fatalf("cover copy reused the source filename; scopes must not collide")
	}
}
