package admin

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/dsn"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

func writeStubTrack(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "song.flac"), []byte("not-a-real-flac"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func stampFirstIndexed(t *testing.T, dataDir, path string, at time.Time) {
	t.Helper()
	db, err := sql.Open("sqlite", dsn.File(manifest.DefaultDBPath(dataDir), "_pragma=busy_timeout(5000)"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	res, err := db.Exec(`UPDATE tracks SET first_indexed_at = ? WHERE path = ?`, at.UnixNano(), path)
	if err != nil {
		t.Fatal(err)
	}
	n, err := res.RowsAffected()
	if err != nil || n != 1 {
		t.Fatalf("stamped %d rows (%v)", n, err)
	}
}

func carryCount(t *testing.T, dataDir string) int {
	t.Helper()
	db, err := sql.Open("sqlite", dsn.File(manifest.DefaultDBPath(dataDir), "_pragma=busy_timeout(5000)"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM first_indexed_carry`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

const singleRootSong = "Artist/Album/song.flac"

func seedSingleRootTrack(t *testing.T) (srv *Server, dataDir, cfgPath, album, rootBase string, kept time.Time) {
	t.Helper()
	opened, cfg, path := newTestServer(t)
	srv, cfgPath = opened, path
	dataDir = cfg.DataDir
	rootBase = filepath.Base(cfg.LibraryRoots[0])
	album = filepath.Join(cfg.LibraryRoots[0], "Artist", "Album")
	writeStubTrack(t, album)
	kept = time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := srv.deps.Manifest.UpsertTrack(t.Context(), &manifest.Track{
		Path: singleRootSong, Size: 14, ModTime: kept,
	}); err != nil {
		t.Fatal(err)
	}
	stampFirstIndexed(t, dataDir, singleRootSong, kept)
	return
}

func emptyExtraRoot(t *testing.T, dataDir string) string {
	t.Helper()
	extra := filepath.Join(filepath.Dir(dataDir), "Extra")
	if err := os.MkdirAll(extra, 0o755); err != nil {
		t.Fatal(err)
	}
	return extra
}

func addExtraRoot(t *testing.T, srv *Server, dataDir string) string {
	t.Helper()
	extra := filepath.Join(filepath.Dir(dataDir), "Extra")
	writeStubTrack(t, filepath.Join(extra, "Artist", "Album"))
	if code := doJSON(t, srv.Handler(), http.MethodPost, "/api/roots", map[string]string{"path": extra}, nil); code != http.StatusCreated {
		t.Fatalf("add status %d", code)
	}
	srvBgScansWait(srv)
	return extra
}

func requireKeptDate(t *testing.T, srv *Server, path string, kept time.Time, what string) {
	t.Helper()
	got, err := srv.deps.Manifest.GetTrack(t.Context(), path)
	if err != nil || got == nil || got.FirstIndexedAt == nil || !got.FirstIndexedAt.Equal(kept) {
		t.Fatalf("%s %v err %v", what, got, err)
	}
}

func requireNoCarry(t *testing.T, dataDir, what string) {
	t.Helper()
	if n := carryCount(t, dataDir); n != 0 {
		t.Errorf("%s left %d saved dates", what, n)
	}
}

func cancelRootFlipAt(t *testing.T, stage string) context.Context {
	t.Helper()
	reqCtx, cancel := context.WithCancel(context.Background())
	manifest.SetRootFlipStageHookForTest(func(got string) {
		if got == stage {
			cancel()
		}
	})
	t.Cleanup(func() { manifest.SetRootFlipStageHookForTest(nil) })
	return reqCtx
}

func TestAddingARootKeepsTheOldFilesDateAndDatesTheNewRootAtTheScan(t *testing.T) {
	srv, dataDir, _, _, rootBase, kept := seedSingleRootTrack(t)
	addExtraRoot(t, srv, dataDir)

	music := rootBase + "/" + singleRootSong
	requireKeptDate(t, srv, music, kept, "old root")
	added, err := srv.deps.Manifest.GetTrack(t.Context(), "Extra/Artist/Album/song.flac")
	if err != nil || added == nil || added.FirstIndexedAt == nil {
		t.Fatalf("added root %v err %v", added, err)
	}
	if added.FirstIndexedAt.Equal(kept) {
		t.Fatal("the added root inherited the old root's date")
	}
}

func TestCollapsingSeveralRootsKeepsTheSurvivorsOwnDate(t *testing.T) {
	srv, cfg, _ := newTestServer(t)
	ctx := t.Context()
	music := cfg.LibraryRoots[0]
	parent := filepath.Dir(cfg.DataDir)
	jazz := filepath.Join(parent, "Jazz")
	spoken := filepath.Join(parent, "Spoken")
	for _, root := range []string{music, jazz, spoken} {
		writeStubTrack(t, filepath.Join(root, "Artist", "Album"))
	}
	srv.deps.Scanner.SetRoots([]string{music, jazz, spoken})

	survivor := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	removed := time.Date(2015, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, row := range []struct {
		path string
		at   time.Time
	}{
		{"Music/Artist/Album/song.flac", survivor},
		{"Jazz/Artist/Album/song.flac", removed},
		{"Spoken/Artist/Album/song.flac", removed},
	} {
		if err := srv.deps.Manifest.UpsertTrack(ctx, &manifest.Track{
			Path: row.path, Size: 14, ModTime: row.at,
		}); err != nil {
			t.Fatal(err)
		}
		stampFirstIndexed(t, cfg.DataDir, row.path, row.at)
	}

	if code := doJSON(t, srv.Handler(), http.MethodDelete, "/api/roots", map[string]string{"path": spoken}, nil); code != http.StatusNoContent {
		t.Fatalf("prefix delete status %d", code)
	}
	srvBgScansWait(srv)
	if n := carryCount(t, cfg.DataDir); n != 0 {
		t.Fatalf("a prefix delete recorded %d dates", n)
	}

	if code := doJSON(t, srv.Handler(), http.MethodDelete, "/api/roots", map[string]string{"path": jazz}, nil); code != http.StatusNoContent {
		t.Fatalf("collapse status %d", code)
	}
	srvBgScansWait(srv)
	got, err := srv.deps.Manifest.GetTrack(ctx, "Artist/Album/song.flac")
	if err != nil || got == nil || got.FirstIndexedAt == nil {
		t.Fatalf("survivor %v err %v", got, err)
	}
	if !got.FirstIndexedAt.Equal(survivor) {
		t.Fatalf("survivor dated %s, want its own %s", got.FirstIndexedAt, survivor)
	}
	if got.FirstIndexedAt.Equal(removed) {
		t.Fatal("the survivor inherited a removed root's date")
	}
}

// A save that fails after the wipe leaves the library in its old form.
// The dates recorded for the flip have to be consumed by the compensating
// scan and then dropped, so a file removed and put back is dated at that
// later scan.
func TestASaveFailureAfterAddingARootDropsTheSavedDates(t *testing.T) {
	srv, dataDir, cfgPath, album, _, kept := seedSingleRootTrack(t)
	extra := emptyExtraRoot(t, dataDir)
	refuseConfigSave(t, cfgPath)
	if code := doJSON(t, srv.Handler(), http.MethodPost, "/api/roots", map[string]string{"path": extra}, nil); code != http.StatusInternalServerError {
		t.Fatalf("add status %d, want 500", code)
	}
	srvBgScansWait(srv)
	requireKeptDate(t, srv, singleRootSong, kept, "restored file")
	requireNoCarry(t, dataDir, "save failure")
	if readdedKeeps(t, srv, album, singleRootSong, kept) {
		t.Error("a file put back after the failed add kept the date recorded for the flip")
	}
}

func TestASaveFailureAfterCollapsingRootsDropsTheSavedDates(t *testing.T) {
	srv, dataDir, cfgPath, album, _, kept := seedSingleRootTrack(t)
	extra := addExtraRoot(t, srv, dataDir)

	refuseConfigSave(t, cfgPath)
	if code := doJSON(t, srv.Handler(), http.MethodDelete, "/api/roots", map[string]string{"path": extra}, nil); code != http.StatusInternalServerError {
		t.Fatalf("collapse status %d, want 500", code)
	}
	srvBgScansWait(srv)
	stored := "Music/Artist/Album/song.flac"
	requireKeptDate(t, srv, stored, kept, "restored file")
	requireNoCarry(t, dataDir, "save failure")
	if readdedKeeps(t, srv, album, stored, kept) {
		t.Error("a file put back after the failed collapse kept the date recorded for the flip")
	}
}

// A request cancelled after the dates are saved and before the wipe
// finishes leaves the rows in place. The generation just recorded has
// to go, or a later re-add copies the old date.
func TestACancelledWipeDropsTheSavedDates(t *testing.T) {
	srv, dataDir, _, album, _, kept := seedSingleRootTrack(t)
	extra := emptyExtraRoot(t, dataDir)
	reqCtx := cancelRootFlipAt(t, "record")
	if code := doJSONContext(t, srv.Handler(), reqCtx, http.MethodPost, "/api/roots", map[string]string{"path": extra}); code != http.StatusInternalServerError {
		t.Fatalf("add status %d, want 500", code)
	}
	requireNoCarry(t, dataDir, "cancelled wipe")
	requireKeptDate(t, srv, singleRootSong, kept, "row after the cancelled wipe")
	if readdedKeeps(t, srv, album, singleRootSong, kept) {
		t.Error("a file put back after the cancelled wipe kept the date recorded for the flip")
	}
}

// The save fails and the request context is already cancelled. Retargeting
// the saved dates has to use a context that is not that one, or the
// compensating scan never clears them.
func TestACancelledSaveAfterTheWipeDropsTheSavedDates(t *testing.T) {
	srv, dataDir, cfgPath, album, _, kept := seedSingleRootTrack(t)
	extra := emptyExtraRoot(t, dataDir)
	refuseConfigSave(t, cfgPath)
	reqCtx := cancelRootFlipAt(t, "wipe")
	if code := doJSONContext(t, srv.Handler(), reqCtx, http.MethodPost, "/api/roots", map[string]string{"path": extra}); code != http.StatusInternalServerError {
		t.Fatalf("add status %d, want 500", code)
	}
	srvBgScansWait(srv)
	requireKeptDate(t, srv, singleRootSong, kept, "restored file")
	requireNoCarry(t, dataDir, "cancelled save")
	if readdedKeeps(t, srv, album, singleRootSong, kept) {
		t.Error("a file put back after the cancelled save kept the date recorded for the flip")
	}
}

func doJSONContext(t *testing.T, h http.Handler, ctx context.Context, method, path string, body any) int {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(raw))
	req = req.WithContext(ctx)
	req.RemoteAddr = "127.0.0.1:54321"
	req.Host = testConsoleHost
	req.Header.Set("content-type", "application/json")
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)
	return rw.Code
}

func refuseConfigSave(t *testing.T, cfgPath string) {
	t.Helper()
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("a read-only config directory is not a save failure here")
	}
	dir := filepath.Dir(cfgPath)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
}

// readdedKeeps removes the file, lets a scan reap the row, puts the file
// back and reports whether that scan copied kept.
func readdedKeeps(t *testing.T, srv *Server, album, stored string, kept time.Time) bool {
	t.Helper()
	if err := os.Remove(filepath.Join(album, "song.flac")); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.deps.Scanner.Scan(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got, err := srv.deps.Manifest.GetTrack(t.Context(), stored); err != nil || got != nil {
		t.Fatalf("row after the file was removed: %v %v", got, err)
	}
	writeStubTrack(t, album)
	if _, err := srv.deps.Scanner.Scan(t.Context()); err != nil {
		t.Fatal(err)
	}
	got, err := srv.deps.Manifest.GetTrack(t.Context(), stored)
	if err != nil || got == nil || got.FirstIndexedAt == nil {
		t.Fatalf("re-added file %v err %v", got, err)
	}
	return got.FirstIndexedAt.Equal(kept)
}
