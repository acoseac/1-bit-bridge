package manifest

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// audioBytes is the placeholder payload for "this is an audio extension
// file the scanner will index". Extract fails on it (not a real FLAC),
// but fillFromPath kicks in and the row still lands. Same shape used by
// scanner_walk_error_test.go.
var audioBytes = []byte("not-a-real-flac")

// containsString returns true when sl contains s.
func containsString(sl []string, s string) bool {
	for _, x := range sl {
		if x == s {
			return true
		}
	}
	return false
}

// TestScannerDeletesOrphanedFolders pins Fix 1: when a directory that was
// previously walked no longer exists on disk, the folder row must be reaped
// on the next full Scan. Pre-fix, only DeleteTrack ran in the deletion pass,
// so the folders table grew unboundedly across folder renames / removes.
func TestScannerDeletesOrphanedFolders(t *testing.T) {
	root := t.TempDir()
	keep := filepath.Join(root, "keep")
	doomed := filepath.Join(root, "doomed")
	for _, sub := range []string{keep, doomed} {
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(sub, "song.flac"), audioBytes, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	s, err := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	sc := NewScanner([]string{root}, s, "")
	if _, err := sc.Scan(context.Background()); err != nil {
		t.Fatalf("first scan: %v", err)
	}
	folders, _ := s.FolderPaths()
	if !containsString(folders, "doomed") || !containsString(folders, "keep") {
		t.Fatalf("first scan didn't index expected folders: %v", folders)
	}

	// Wipe the doomed dir and rescan — the row must come out of the table.
	if err := os.RemoveAll(doomed); err != nil {
		t.Fatal(err)
	}
	if _, err := sc.Scan(context.Background()); err != nil {
		t.Fatalf("second scan: %v", err)
	}
	folders, _ = s.FolderPaths()
	sort.Strings(folders)
	if containsString(folders, "doomed") {
		t.Errorf("orphaned folder row not deleted: got %v", folders)
	}
	if !containsString(folders, "keep") {
		t.Errorf("healthy folder row was wiped alongside the orphan: got %v", folders)
	}
}

// TestScanSubtreeRemovesStaleTrackOnRename pins one half of Fix 2: when a
// file is renamed inside a watched directory, the old track row must be
// reaped by the next ScanSubtree on that directory rather than waiting
// up to 6h for the next full Scan.
func TestScanSubtreeRemovesStaleTrackOnRename(t *testing.T) {
	root := t.TempDir()
	album := filepath.Join(root, "album")
	if err := os.MkdirAll(album, 0o755); err != nil {
		t.Fatal(err)
	}
	original := filepath.Join(album, "01 song.flac")
	renamed := filepath.Join(album, "01 song-renamed.flac")
	if err := os.WriteFile(original, audioBytes, 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	sc := NewScanner([]string{root}, s, "")
	if _, err := sc.Scan(context.Background()); err != nil {
		t.Fatalf("initial scan: %v", err)
	}
	if got, _ := s.GetTrack("album/01 song.flac"); got == nil {
		t.Fatal("initial scan didn't index original track")
	}

	// Rename, then run ScanSubtree on the parent directory — what fsnotify's
	// watcher would do after a Create/Rename event coalesces.
	if err := os.Rename(original, renamed); err != nil {
		t.Fatal(err)
	}
	if _, err := sc.ScanSubtree(context.Background(), album); err != nil {
		t.Fatalf("ScanSubtree: %v", err)
	}
	if got, _ := s.GetTrack("album/01 song.flac"); got != nil {
		t.Errorf("stale track row left behind after rename: %+v", got)
	}
	if got, _ := s.GetTrack("album/01 song-renamed.flac"); got == nil {
		t.Error("renamed track row not added")
	}
}

// TestScanSubtreeRemovesStaleFolderOnRename pins the folder analogue of
// the rename case — symmetric with the track rename above. Without
// folder cleanup in ScanSubtree, the folders table accumulates orphans
// for every directory rename until the next periodic full scan.
func TestScanSubtreeRemovesStaleFolderOnRename(t *testing.T) {
	root := t.TempDir()
	original := filepath.Join(root, "OldAlbum")
	renamed := filepath.Join(root, "NewAlbum")
	if err := os.MkdirAll(original, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(original, "song.flac"), audioBytes, 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	sc := NewScanner([]string{root}, s, "")
	if _, err := sc.Scan(context.Background()); err != nil {
		t.Fatalf("initial scan: %v", err)
	}
	folders, _ := s.FolderPaths()
	if !containsString(folders, "OldAlbum") {
		t.Fatalf("initial scan didn't index OldAlbum: %v", folders)
	}

	// Rename the directory, then ScanSubtree on the root: with the
	// bounded deletion pass scoped to root, the OldAlbum row clears and
	// the NewAlbum row lands.
	if err := os.Rename(original, renamed); err != nil {
		t.Fatal(err)
	}
	if _, err := sc.ScanSubtree(context.Background(), root); err != nil {
		t.Fatalf("ScanSubtree: %v", err)
	}
	folders, _ = s.FolderPaths()
	if containsString(folders, "OldAlbum") {
		t.Errorf("stale folder row left behind after rename: %v", folders)
	}
	if !containsString(folders, "NewAlbum") {
		t.Errorf("renamed folder row not added: %v", folders)
	}
}

// TestScanSubtreeAcrossRootsNoDuplicate pins the original concern that
// kept the deletion pass disabled: a file moved across configured library
// roots fires fsnotify on both, and each event's ScanSubtree only looks
// at its own scope. Source-side scan deletes the source row; destination-
// side scan adds the destination row. End state: one row, no duplicate,
// no waiting for the periodic full scan to reconcile.
func TestScanSubtreeAcrossRootsNoDuplicate(t *testing.T) {
	rootA := t.TempDir()
	rootB := t.TempDir()
	dirA := filepath.Join(rootA, "album")
	dirB := filepath.Join(rootB, "album")
	for _, d := range []string{dirA, dirB} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	source := filepath.Join(dirA, "song.flac")
	destination := filepath.Join(dirB, "song.flac")
	if err := os.WriteFile(source, audioBytes, 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// Multi-root: paths land as "<rootBase>/album/song.flac".
	sc := NewScanner([]string{rootA, rootB}, s, "")
	if _, err := sc.Scan(context.Background()); err != nil {
		t.Fatalf("initial scan: %v", err)
	}
	rootABase := filepath.Base(rootA)
	rootBBase := filepath.Base(rootB)
	sourcePath := rootABase + "/album/song.flac"
	destPath := rootBBase + "/album/song.flac"
	if got, _ := s.GetTrack(sourcePath); got == nil {
		t.Fatalf("initial scan didn't index source path %q", sourcePath)
	}

	// Move the file across roots. fsnotify would deliver events on both
	// dirs; we drive ScanSubtree directly to test the deletion pass
	// without depending on the watcher's debounce timing.
	if err := os.Rename(source, destination); err != nil {
		t.Fatal(err)
	}
	if _, err := sc.ScanSubtree(context.Background(), dirA); err != nil {
		t.Fatalf("ScanSubtree(dirA): %v", err)
	}
	if _, err := sc.ScanSubtree(context.Background(), dirB); err != nil {
		t.Fatalf("ScanSubtree(dirB): %v", err)
	}

	if got, _ := s.GetTrack(sourcePath); got != nil {
		t.Errorf("source-side row not deleted after move: %+v", got)
	}
	if got, _ := s.GetTrack(destPath); got == nil {
		t.Error("destination-side row not added after move")
	}
	tracks, _ := s.TrackPaths()
	if len(tracks) != 1 {
		t.Errorf("post-move track count = %d (%v), want 1", len(tracks), tracks)
	}
}
