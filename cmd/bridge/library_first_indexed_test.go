package main

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/dsn"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

func writeOfflineLibrary(t *testing.T, roots ...string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	data := filepath.Join(dir, "data")
	if err := os.MkdirAll(data, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		LibraryRoots:    roots,
		ListenAddress:   "127.0.0.1:7788",
		AdminAddress:    "127.0.0.1:1",
		DataDir:         data,
		ScanIntervalSec: 3600,
		LibraryName:     "Test Library",
	}
	path := filepath.Join(dir, "bridge.yaml")
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	return path, data
}

func readCarry(t *testing.T, dataDir string) (key string, ns int64, generation, targetMulti int) {
	t.Helper()
	db, err := sql.Open("sqlite", dsn.File(manifest.DefaultDBPath(dataDir), "_pragma=busy_timeout(5000)"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	err = db.QueryRow(`SELECT path_key, first_indexed_at, generation, target_multi FROM first_indexed_carry`).Scan(&key, &ns, &generation, &targetMulti)
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM first_indexed_carry`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("carry rows %d (%v)", n, err)
	}
	return key, ns, generation, targetMulti
}

func TestLibraryAddRecordsTheExistingRootsFolderName(t *testing.T) {
	music := filepath.Join(t.TempDir(), "Music")
	extra := filepath.Join(t.TempDir(), "Extra")
	for _, dir := range []string{music, extra} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cfgPath, data := writeOfflineLibrary(t, music)
	kept := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	store, err := manifest.OpenStore(manifest.DefaultDBPath(data))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertTrack(context.Background(), &manifest.Track{
		Path: "Artist/Album/song.flac", Size: 1, ModTime: kept,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", dsn.File(manifest.DefaultDBPath(data), "_pragma=busy_timeout(5000)"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`UPDATE tracks SET first_indexed_at = ? WHERE path = ?`, kept.UnixNano(), "Artist/Album/song.flac"); err != nil {
		t.Fatal(err)
	}
	raw.Close()

	var out, errOut bytes.Buffer
	if code := libraryCmd(context.Background(), []string{"add", "--config", cfgPath, extra}, &out, &errOut); code != 0 {
		t.Fatalf("add %d\n%s%s", code, out.String(), errOut.String())
	}
	key, ns, generation, target := readCarry(t, data)
	if key != "Music/Artist/Album/song.flac" || ns != kept.UnixNano() || generation != 1 || target != 1 {
		t.Fatalf("carry key %q date %d generation %d target %d", key, ns, generation, target)
	}
}

func TestLibraryRemoveCollapseKeepsTheSurvivorsDate(t *testing.T) {
	base := t.TempDir()
	music := filepath.Join(base, "Music")
	jazz := filepath.Join(base, "Jazz")
	for _, dir := range []string{music, jazz} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cfgPath, data := writeOfflineLibrary(t, music, jazz)
	survivor := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	removed := time.Date(2015, 1, 1, 0, 0, 0, 0, time.UTC)
	store, err := manifest.OpenStore(manifest.DefaultDBPath(data))
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct {
		path string
		at   time.Time
	}{
		{"Music/Artist/Album/song.flac", survivor},
		{"Jazz/Artist/Album/song.flac", removed},
	} {
		if err := store.UpsertTrack(context.Background(), &manifest.Track{
			Path: row.path, Size: 1, ModTime: row.at,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", dsn.File(manifest.DefaultDBPath(data), "_pragma=busy_timeout(5000)"))
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct {
		path string
		at   time.Time
	}{
		{"Music/Artist/Album/song.flac", survivor},
		{"Jazz/Artist/Album/song.flac", removed},
	} {
		if _, err := raw.Exec(`UPDATE tracks SET first_indexed_at = ? WHERE path = ?`, row.at.UnixNano(), row.path); err != nil {
			t.Fatal(err)
		}
	}
	raw.Close()

	var out, errOut bytes.Buffer
	if code := libraryCmd(context.Background(), []string{"remove", "--config", cfgPath, jazz}, &out, &errOut); code != 0 {
		t.Fatalf("remove %d\n%s%s", code, out.String(), errOut.String())
	}
	key, ns, generation, target := readCarry(t, data)
	if key != "Artist/Album/song.flac" || ns != survivor.UnixNano() || generation != 1 || target != 0 {
		t.Fatalf("carry key %q date %d generation %d target %d", key, ns, generation, target)
	}
}
