package manifest

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestACaseOnlyRenameKeepsTheFirstIndexedDate(t *testing.T) {
	root, store, sc := scanSeededAlbum(t, reproT1)
	renameAlbumCase(t, root)
	clockAt(store, reproT2)
	scanOnce(t, sc, "renamed")
	if got := firstIndexedTime(t, store, "Artist/album/song.flac"); !got.Equal(reproT1) {
		t.Fatalf("renamed path dated %s, want %s", got, reproT1)
	}
	if old, _ := store.GetTrack(context.Background(), "Artist/Album/song.flac"); old != nil {
		t.Fatal("the old-case row survived the rename")
	}
}

func TestASubtreeScanCopiesACaseOnlyRename(t *testing.T) {
	root, store, sc := scanSeededAlbum(t, reproT1)
	renameAlbumCase(t, root)
	clockAt(store, reproT2)
	if _, err := sc.ScanSubtree(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	if got := firstIndexedTime(t, store, "Artist/album/song.flac"); !got.Equal(reproT1) {
		t.Fatalf("subtree renamed path dated %s, want %s", got, reproT1)
	}
}

func TestAnOutsideFolderMoveInsertsANewFirstIndexedDate(t *testing.T) {
	root, store, sc := scanSeededAlbum(t, reproT1)
	dest := filepath.Join(root, "Artist", "Other")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	err := os.Rename(filepath.Join(root, "Artist", "Album", "song.flac"), filepath.Join(dest, "song.flac"))
	if err != nil {
		t.Fatal(err)
	}
	clockAt(store, reproT2)
	scanOnce(t, sc, "moved")
	got := firstIndexedTime(t, store, "Artist/Other/song.flac")
	if !got.Equal(reproT2) {
		t.Fatalf("moved path dated %s, want the scan clock %s", got, reproT2)
	}
	if got.Equal(reproT1) {
		t.Fatal("an outside folder move copied the old date")
	}
}

func TestARootFlipCarriesTheFirstIndexedDateAndAFullScanClearsIt(t *testing.T) {
	root, store, sc := scanSeededAlbum(t, reproT1)
	other := t.TempDir()
	ctx := context.Background()
	if _, err := store.RecordFirstIndexedCarry(ctx, false, filepath.Base(root)); err != nil {
		t.Fatal(err)
	}
	seedTrackDirs(t, filepath.Join(root, "Artist", "Late"))
	if err := store.WipeFilesystemTracks(ctx); err != nil {
		t.Fatal(err)
	}
	sc.SetRoots([]string{root, other})
	clockAt(store, reproT2)
	if _, err := sc.ScanSubtree(ctx, root); err != nil {
		t.Fatal(err)
	}
	base := filepath.Base(root)
	kept := base + "/Artist/Album/song.flac"
	late := base + "/Artist/Late/song.flac"
	if got := firstIndexedTime(t, store, kept); !got.Equal(reproT1) {
		t.Fatalf("kept date %s, want %s", got, reproT1)
	}
	if got := firstIndexedTime(t, store, late); !got.Equal(reproT2) {
		t.Fatalf("a file added after the snapshot dated %s, want %s", got, reproT2)
	}
	if carryCount(t, store) == 0 {
		t.Fatal("a subtree scan cleared the carry table")
	}
	scanOnce(t, sc, "full")
	if n := carryCount(t, store); n != 0 {
		t.Fatalf("full scan left %d carry rows", n)
	}
	if got := firstIndexedTime(t, store, kept); !got.Equal(reproT1) {
		t.Fatalf("full scan moved the kept date to %s", got)
	}
}
