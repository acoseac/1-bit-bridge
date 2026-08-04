package manifest

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestScanSubtreeSparesCaseTwinDirectory is the end-to-end consequence of
// TrackPathsUnder's scope query folding ASCII case.
//
// ScanSubtree is the watcher's bounded re-scan. It snapshots "what did we
// know under this directory" via TrackPathsUnder, walks the directory, and
// reaps rows in the snapshot that the walk didn't see. With the snapshot
// built by `path LIKE 'Artist/Album/%'`, a case-sensitive filesystem's
// SEPARATE `Artist/album/` directory landed in it — and the walk, which
// only ever visits `Artist/Album/`, never puts those rows in `seen`.
//
// They are then not routed and not under an errored subtree, so they fall
// through to `caseOnlyRenames`, which fold-matches them against a path that
// WAS seen and classifies them as "the old case of a rename". The pass
// reaps those IMMEDIATELY — bypassing the `missing_count >= threshold`
// debounce entirely. Files still on disk, rows gone.
//
// That immediate reap is sound only because of the premise stated in
// caseOnlyRenames' own docblock: "a stored path that fold-matches a seen
// entry refers to a file the walker DID enumerate this pass." True for a
// full Scan, which walks everything. False the moment the SNAPSHOT is
// broader than the WALK — which is exactly what a case-folding scope query
// makes it. Keeping TrackPathsUnder case-exact is what keeps that premise
// true; the deletion pass cannot spare a row it was never told to consider.
//
// This is the CLAUDE.md rule "'we could not see this path' must dominate
// every '…but it looks like X' classification", reached through a vector
// the #568 ordering fix doesn't cover: there is no walk error here, so
// isUnderErroredSubtree never fires.
//
// Skips on a case-insensitive filesystem, where the two directories cannot
// be staged as distinct and the bug is unreachable — same guard as
// TestScannerSparesCaseTwinUnderWalkErrorSubtree. macOS APFS/HFS+ default
// to case-insensitive, so this runs on Linux CI and on a case-sensitive
// macOS volume.
func TestScanSubtreeSparesCaseTwinDirectory(t *testing.T) {
	root := t.TempDir()
	if !caseSensitiveFS(t, root) {
		t.Skip("case-insensitive filesystem: case-twin directories can't be staged (the bug is unreachable here)")
	}

	upper := filepath.Join(root, "Artist", "Album")
	lower := filepath.Join(root, "Artist", "album")
	seedTrackDirs(t, upper, lower)

	s, sc := newScanFixture(t, root)
	scanOnce(t, sc, "full scan")
	mustIndexed(t, s, "Artist/Album/song.flac", "Artist/album/song.flac")

	// The watcher's bounded re-scan of ONE of the twins. Nothing changed
	// on disk, so this must be a no-op for both.
	if _, err := sc.ScanSubtree(context.Background(), upper); err != nil {
		t.Fatalf("ScanSubtree(%q): %v", upper, err)
	}

	if got, _ := s.GetTrack(context.Background(), "Artist/album/song.flac"); got == nil {
		t.Error("Artist/album/song.flac was reaped by a subtree scan of its case-twin " +
			"Artist/Album — the scope snapshot must not reach a directory the walk never visits")
	}
	if got, _ := s.GetTrack(context.Background(), "Artist/Album/song.flac"); got == nil {
		t.Error("Artist/Album/song.flac was reaped by a scan of its own directory")
	}
}

// TestScanSubtreeStillReapsWithinScope is the negative half: the fix must
// not disarm the deletion pass for rows that genuinely ARE under the
// scanned directory and genuinely have gone away. Without this, narrowing
// the scope query to nothing at all would pass the test above.
//
// Runs everywhere — no case-twin fixture needed.
func TestScanSubtreeStillReapsWithinScope(t *testing.T) {
	root := t.TempDir()
	album := filepath.Join(root, "Artist", "Album")
	seedTrackDirs(t, album)
	extra := filepath.Join(album, "bonus.flac")
	if err := os.WriteFile(extra, []byte("not-a-real-flac"), 0o644); err != nil {
		t.Fatal(err)
	}

	s, sc := newScanFixture(t, root)
	scanOnce(t, sc, "full scan")
	mustIndexed(t, s, "Artist/Album/song.flac", "Artist/Album/bonus.flac")

	// Remove one file and re-scan just that subtree. The row is in scope
	// and absent from the walk, so it enters the missing-count pass; the
	// default threshold is >1 scan, so drive it until it reaps.
	if err := os.Remove(extra); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if _, err := sc.ScanSubtree(context.Background(), album); err != nil {
			t.Fatalf("ScanSubtree pass %d: %v", i, err)
		}
		if got, _ := s.GetTrack(context.Background(), "Artist/Album/bonus.flac"); got == nil {
			break
		}
	}
	if got, _ := s.GetTrack(context.Background(), "Artist/Album/bonus.flac"); got != nil {
		t.Error("a deleted file's row under the scanned directory was never reaped — " +
			"the scope query must still cover genuine descendants")
	}
	if got, _ := s.GetTrack(context.Background(), "Artist/Album/song.flac"); got == nil {
		t.Error("the surviving track in the same directory was reaped")
	}
}
