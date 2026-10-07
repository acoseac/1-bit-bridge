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
	root, store, sc := scanSeededAlbum(t, reproT1)
	base := filepath.Base(root)
	recordAndWipe(t, store, false, base)
	recordAndWipe(t, store, true, base)
	clockAt(store, reproT3)
	scanOnce(t, sc, "single-root rescan")
	got := firstIndexedTime(t, store, "Artist/Album/song.flac")
	if !got.Equal(reproT1) {
		t.Fatalf("flip back dated %s, want %s", got, reproT1)
	}
	if n := carryCount(t, store); n != 0 {
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
	recordAndWipe(t, store, false, base)
	sc.SetRoots([]string{root, other})
	clockAt(store, reproT2)
	if _, err := sc.ScanSubtree(ctx, filepath.Join(root, "Artist", "X")); err != nil {
		t.Fatal(err)
	}
	recordAndWipe(t, store, true, base)
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
	root, store, sc := scanSeededAlbum(t, reproT1)
	recordAndWipe(t, store, false, filepath.Base(root))
	clockAt(store, reproT2)
	scanOnce(t, sc, "compensating scan")
	got := firstIndexedTime(t, store, "Artist/Album/song.flac")
	if !got.Equal(reproT1) {
		t.Fatalf("compensating scan dated %s, want %s", got, reproT1)
	}
}
