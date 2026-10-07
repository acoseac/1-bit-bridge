package manifest

import (
	"context"
	"path/filepath"
	"testing"
)

// A full scan whose roots snapshot predates the flip finishes after the
// record and the wipe. It walks the previous form, so it leaves the
// saved dates for the scan that follows. The post-flip path keeps the
// date from before the flip.
func TestAnInFlightScanLeavesTheCarryForThePostFlipScan(t *testing.T) {
	root, store, sc := scanSeededAlbum(t, reproT1)
	other := t.TempDir()
	recordAndWipe(t, store, false, filepath.Base(root))
	clockAt(store, reproT2)
	scanOnce(t, sc, "in-flight scan over the old roots completes")
	sc.SetRoots([]string{root, other})
	clockAt(store, reproT3)
	scanOnce(t, sc, "post-add scan")
	got := firstIndexedTime(t, store, filepath.Base(root)+"/Artist/Album/song.flac")
	if !got.Equal(reproT1) {
		t.Fatalf("post-flip row dated %s, want carried %s", got, reproT1)
	}
}

// A file that arrives in the root a flip added is dated at that scan.
// It does not inherit the date of a file at the same relative path in
// the root that was already there.
func TestAnAddedRootDoesNotInheritTheOldRootsDate(t *testing.T) {
	root, store, sc := scanSeededAlbum(t, reproT1)
	other := t.TempDir()
	seedTrackDirs(t, filepath.Join(other, "Artist", "Album"))
	recordAndWipe(t, store, false, filepath.Base(root))
	sc.SetRoots([]string{root, other})
	clockAt(store, reproT2)
	scanOnce(t, sc, "post-add scan")
	got := firstIndexedTime(t, store, filepath.Base(other)+"/Artist/Album/song.flac")
	if !got.Equal(reproT2) {
		t.Fatalf("new root's file dated %s, want the scan clock %s", got, reproT2)
	}
}

// A collapse keeps the surviving root's own date. A file that lived
// only under the removed root does not hand its earlier date across.
func TestACollapseKeepsTheSurvivorsDateNotTheRemovedRoots(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	seedTrackDirs(t, filepath.Join(b, "Artist", "Album"))
	store, sc := newScanFixture(t, a)
	sc.SetRoots([]string{a, b})
	clockAt(store, reproT0)
	scanOnce(t, sc, "b only")
	seedTrackDirs(t, filepath.Join(a, "Artist", "Album"))
	clockAt(store, reproT1)
	scanOnce(t, sc, "a added")
	if got := firstIndexedTime(t, store, filepath.Base(a)+"/Artist/Album/song.flac"); !got.Equal(reproT1) {
		t.Fatalf("setup: a dated %s", got)
	}
	recordAndWipe(t, store, true, filepath.Base(a))
	sc.SetRoots([]string{a})
	clockAt(store, reproT2)
	scanOnce(t, sc, "collapse scan")
	got := firstIndexedTime(t, store, "Artist/Album/song.flac")
	if !got.Equal(reproT1) {
		t.Fatalf("survivor dated %s, want its own %s (removed root had %s)", got, reproT1, reproT0)
	}
}

// A row an older binary inserted with a null date is filled the next
// time this binary opens the database.
func TestARolledBackNullDateIsFilledOnTheNextOpen(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := OpenStore(filepath.Join(dir, "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertTrack(ctx, &Track{Path: "x.flac", Size: 1, ModTime: reproT1}); err != nil {
		t.Fatal(err)
	}
	// What a pre-v53 binary's INSERT leaves.
	if _, err := s.db.Exec(`UPDATE tracks SET first_indexed_at = NULL`); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	s2, err := OpenStore(filepath.Join(dir, "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	ok, err := s2.FirstIndexedAtReady(ctx)
	if err != nil || !ok {
		t.Fatalf("after reopening, ready=%v err=%v", ok, err)
	}
}
