package manifest

import (
	"context"
	"path/filepath"
	"testing"
)

// A root added and then removed before any rescan leaves the library in
// the form it started in. The saved dates must still apply, and the scan
// that restores that form must clear them.
func TestAFlipBackBeforeAnyRescanKeepsTheDate(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	seedTrackDirs(t, filepath.Join(root, "Artist", "Album"))
	store, sc := newScanFixture(t, root)
	clockAt(store, reproT1)
	scanOnce(t, sc, "first")
	if err := store.RecordFirstIndexedCarry(ctx, false, filepath.Base(root)); err != nil {
		t.Fatal(err)
	}
	if err := store.WipeFilesystemTracks(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordFirstIndexedCarry(ctx, true, filepath.Base(root)); err != nil {
		t.Fatal(err)
	}
	if err := store.WipeFilesystemTracks(ctx); err != nil {
		t.Fatal(err)
	}
	clockAt(store, reproT3)
	scanOnce(t, sc, "single-root rescan")
	got := firstIndexedTime(t, store, "Artist/Album/song.flac")
	if !got.Equal(reproT1) {
		t.Fatalf("flip back dated %s, want %s", got, reproT1)
	}
	var n int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM first_indexed_carry`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("the restored scan left %d saved dates", n)
	}
}

// Removing the root while the post-add rescan has rewritten only one
// album must keep the date of the album that rescan has not reached.
func TestACollapseDuringThePostAddRescanKeepsBothAlbums(t *testing.T) {
	ctx := context.Background()
	root, other := t.TempDir(), t.TempDir()
	base := filepath.Base(root)
	seedTrackDirs(t,
		filepath.Join(root, "Artist", "X"),
		filepath.Join(root, "Artist", "Y"),
	)
	store, sc := newScanFixture(t, root)
	clockAt(store, reproT1)
	scanOnce(t, sc, "first")
	if err := store.RecordFirstIndexedCarry(ctx, false, base); err != nil {
		t.Fatal(err)
	}
	if err := store.WipeFilesystemTracks(ctx); err != nil {
		t.Fatal(err)
	}
	sc.SetRoots([]string{root, other})
	clockAt(store, reproT2)
	if _, err := sc.ScanSubtree(ctx, filepath.Join(root, "Artist", "X")); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordFirstIndexedCarry(ctx, true, base); err != nil {
		t.Fatal(err)
	}
	if err := store.WipeFilesystemTracks(ctx); err != nil {
		t.Fatal(err)
	}
	sc.SetRoots([]string{root})
	clockAt(store, reproT3)
	scanOnce(t, sc, "collapse scan")
	for _, path := range []string{"Artist/X/song.flac", "Artist/Y/song.flac"} {
		if got := firstIndexedTime(t, store, path); !got.Equal(reproT1) {
			t.Errorf("%s dated %s, want %s", path, got, reproT1)
		}
	}
}

// A config save that fails after the wipe runs its compensating scan in
// the form the library still has. That scan must keep the saved date.
func TestACompensatingScanAfterAFailedSaveKeepsTheDate(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	seedTrackDirs(t, filepath.Join(root, "Artist", "Album"))
	store, sc := newScanFixture(t, root)
	clockAt(store, reproT1)
	scanOnce(t, sc, "first")
	if err := store.RecordFirstIndexedCarry(ctx, false, filepath.Base(root)); err != nil {
		t.Fatal(err)
	}
	if err := store.WipeFilesystemTracks(ctx); err != nil {
		t.Fatal(err)
	}
	clockAt(store, reproT2)
	scanOnce(t, sc, "compensating scan")
	got := firstIndexedTime(t, store, "Artist/Album/song.flac")
	if !got.Equal(reproT1) {
		t.Fatalf("compensating scan dated %s, want %s", got, reproT1)
	}
}
