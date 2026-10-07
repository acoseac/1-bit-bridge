package admin

import (
	"database/sql"
	"net/http"
	"os"
	"path/filepath"
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

func TestAddingARootKeepsTheOldFilesDateAndDatesTheNewRootAtTheScan(t *testing.T) {
	srv, cfg, _ := newTestServer(t)
	ctx := t.Context()
	album := filepath.Join(cfg.LibraryRoots[0], "Artist", "Album")
	writeStubTrack(t, album)
	kept := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := srv.deps.Manifest.UpsertTrack(ctx, &manifest.Track{
		Path: "Artist/Album/song.flac", Size: 14, ModTime: kept,
	}); err != nil {
		t.Fatal(err)
	}
	stampFirstIndexed(t, cfg.DataDir, "Artist/Album/song.flac", kept)

	extra := filepath.Join(filepath.Dir(cfg.DataDir), "Extra")
	writeStubTrack(t, filepath.Join(extra, "Artist", "Album"))
	if code := doJSON(t, srv.Handler(), http.MethodPost, "/api/roots", map[string]string{"path": extra}, nil); code != http.StatusCreated {
		t.Fatalf("add status %d", code)
	}
	srvBgScansWait(srv)

	music := filepath.Base(cfg.LibraryRoots[0]) + "/Artist/Album/song.flac"
	got, err := srv.deps.Manifest.GetTrack(ctx, music)
	if err != nil || got == nil || got.FirstIndexedAt == nil || !got.FirstIndexedAt.Equal(kept) {
		t.Fatalf("old root %v err %v", got, err)
	}
	added, err := srv.deps.Manifest.GetTrack(ctx, "Extra/Artist/Album/song.flac")
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
