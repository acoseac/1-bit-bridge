package manifest

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func listedName(t *testing.T, dir, want string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() == want {
			return entry.Name()
		}
	}
	return ""
}

func firstIndexedTime(t *testing.T, s *Store, path string) time.Time {
	t.Helper()
	got, err := s.GetTrack(context.Background(), path)
	if err != nil {
		t.Fatalf("get %s: %v", path, err)
	}
	if got == nil || got.FirstIndexedAt == nil {
		t.Fatalf("%s has no first-indexed date", path)
	}
	return got.FirstIndexedAt.UTC()
}

func TestACaseOnlyRenameKeepsTheFirstIndexedDate(t *testing.T) {
	root := t.TempDir()
	album := filepath.Join(root, "Artist", "Album")
	seedTrackDirs(t, album)
	store, sc := newScanFixture(t, root)
	t1 := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	t2 := time.Date(2021, 6, 7, 8, 9, 10, 0, time.UTC)
	store.now = func() time.Time { return t1 }
	scanOnce(t, sc, "first")
	renamed := filepath.Join(root, "Artist", "album")
	if err := os.Rename(album, renamed); err != nil {
		t.Fatal(err)
	}
	if listedName(t, filepath.Join(root, "Artist"), "album") != "album" {
		t.Skip("this volume did not store the renamed case")
	}
	store.now = func() time.Time { return t2 }
	scanOnce(t, sc, "renamed")
	if got := firstIndexedTime(t, store, "Artist/album/song.flac"); !got.Equal(t1) {
		t.Fatalf("renamed path dated %s, want %s", got, t1)
	}
	if old, _ := store.GetTrack(context.Background(), "Artist/Album/song.flac"); old != nil {
		t.Fatal("the old-case row survived the rename")
	}
}

func TestASubtreeScanCopiesACaseOnlyRename(t *testing.T) {
	root := t.TempDir()
	album := filepath.Join(root, "Artist", "Album")
	seedTrackDirs(t, album)
	store, sc := newScanFixture(t, root)
	t1 := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	t2 := time.Date(2021, 6, 7, 8, 9, 10, 0, time.UTC)
	store.now = func() time.Time { return t1 }
	scanOnce(t, sc, "first")
	renamed := filepath.Join(root, "Artist", "album")
	if err := os.Rename(album, renamed); err != nil {
		t.Fatal(err)
	}
	if listedName(t, filepath.Join(root, "Artist"), "album") != "album" {
		t.Skip("this volume did not store the renamed case")
	}
	store.now = func() time.Time { return t2 }
	if _, err := sc.ScanSubtree(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	if got := firstIndexedTime(t, store, "Artist/album/song.flac"); !got.Equal(t1) {
		t.Fatalf("subtree renamed path dated %s, want %s", got, t1)
	}
}

func TestAnOutsideFolderMoveInsertsANewFirstIndexedDate(t *testing.T) {
	root := t.TempDir()
	album := filepath.Join(root, "Artist", "Album")
	seedTrackDirs(t, album)
	store, sc := newScanFixture(t, root)
	t1 := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	t2 := time.Date(2021, 6, 7, 8, 9, 10, 0, time.UTC)
	store.now = func() time.Time { return t1 }
	scanOnce(t, sc, "first")
	dest := filepath.Join(root, "Artist", "Other")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	err := os.Rename(filepath.Join(album, "song.flac"), filepath.Join(dest, "song.flac"))
	if err != nil {
		t.Fatal(err)
	}
	store.now = func() time.Time { return t2 }
	scanOnce(t, sc, "moved")
	got := firstIndexedTime(t, store, "Artist/Other/song.flac")
	if !got.Equal(t2) {
		t.Fatalf("moved path dated %s, want the scan clock %s", got, t2)
	}
	if got.Equal(t1) {
		t.Fatal("an outside folder move copied the old date")
	}
}

func TestARootFlipCarriesTheFirstIndexedDateAndAFullScanClearsIt(t *testing.T) {
	root := t.TempDir()
	other := t.TempDir()
	seedTrackDirs(t, filepath.Join(root, "Artist", "Album"))
	store, sc := newScanFixture(t, root)
	t1 := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	t2 := time.Date(2021, 6, 7, 8, 9, 10, 0, time.UTC)
	store.now = func() time.Time { return t1 }
	scanOnce(t, sc, "first")
	if err := store.RecordFirstIndexedCarry(context.Background(), false, filepath.Base(root)); err != nil {
		t.Fatal(err)
	}
	seedTrackDirs(t, filepath.Join(root, "Artist", "Late"))
	if err := store.WipeFilesystemTracks(context.Background()); err != nil {
		t.Fatal(err)
	}
	sc.SetRoots([]string{root, other})
	store.now = func() time.Time { return t2 }
	if _, err := sc.ScanSubtree(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	base := filepath.Base(root)
	kept := base + "/Artist/Album/song.flac"
	late := base + "/Artist/Late/song.flac"
	if got := firstIndexedTime(t, store, kept); !got.Equal(t1) {
		t.Fatalf("kept date %s, want %s", got, t1)
	}
	if got := firstIndexedTime(t, store, late); !got.Equal(t2) {
		t.Fatalf("a file added after the snapshot dated %s, want %s", got, t2)
	}
	var n int
	err := store.db.QueryRow(`SELECT COUNT(*) FROM first_indexed_carry`).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("a subtree scan cleared the carry table")
	}
	scanOnce(t, sc, "full")
	err = store.db.QueryRow(`SELECT COUNT(*) FROM first_indexed_carry`).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("full scan left %d carry rows", n)
	}
	if got := firstIndexedTime(t, store, kept); !got.Equal(t1) {
		t.Fatalf("full scan moved the kept date to %s", got)
	}
}
