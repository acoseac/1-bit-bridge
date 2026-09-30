//go:build !windows

package manifest

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestScanner_ACoverThisUserCannotReadIsReadOnceItCan pins the failed read
// with a real open and no seam: an album indexed while its cover is mode 0
// (as a `sudo cp` leaves one the service user may not read) gets no art, and
// the first scan after the cover is made readable gives it the cover. On
// main the skip gate kept the rows without it until their audio changed.
func TestScanner_ACoverThisUserCannotReadIsReadOnceItCan(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode-0 file")
	}
	f := newArtFixture(t)
	rels := []string{"Artist/Album/01.flac", "Artist/Album/02.flac"}
	for _, rel := range rels {
		f.flac(t, rel)
	}
	data := coverBytes("chmod")
	f.cover(t, "Artist/Album/cover.jpg", data, t0)
	p := f.path("Artist/Album/cover.jpg")
	if err := os.Chmod(p, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(p, 0o644) })

	f.scan(t, "index, the cover unreadable")
	f.requireArt(t, "", rels...)

	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatal(err)
	}
	f.scan(t, "the cover readable")
	f.requireArt(t, expectedLocalMBID(data), rels...)
	f.requireSettled(t, rels...)
}

// TestFolderArtKeyLeavesOutWhatIsNotAFile pins the key's list of kinds: a
// link to a directory called cover.png and a named pipe called folder.png
// are no covers (the lookup refuses to read them), so neither is in the key,
// and the album is not re-read when the linked directory changes.
func TestFolderArtKeyLeavesOutWhatIsNotAFile(t *testing.T) {
	f := newArtFixture(t)
	rels := []string{"Artist/Album/01.flac", "Artist/Album/02.flac"}
	for _, rel := range rels {
		f.flac(t, rel)
	}
	data := coverBytes("real")
	f.cover(t, "Artist/Album/cover.jpg", data, t0)
	elsewhere := t.TempDir()
	if err := os.Symlink(elsewhere, f.path("Artist/Album/cover.png")); err != nil {
		t.Skipf("symlinks unsupported here: %v", err)
	}
	if err := syscall.Mkfifo(f.path("Artist/Album/folder.png"), 0o644); err != nil {
		t.Skipf("named pipes unsupported here: %v", err)
	}
	key, seen := folderArtKey(f.path(rels[0]), &ExtractContext{})
	if want := statKey(t, f.path("Artist/Album/cover.jpg")); !seen || key != want {
		t.Fatalf("folderArtKey = %q (seen %v), want only the file's %q", key, seen, want)
	}
	f.scan(t, "index")
	f.requireArt(t, expectedLocalMBID(data), rels...)
	f.requireSettled(t, rels...)

	// The linked directory changes: nothing the album is given changes.
	if err := os.WriteFile(filepath.Join(elsewhere, "new"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(elsewhere, t0.Add(2*time.Hour), t0.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	f.requireSettled(t, rels...)
}

// TestScanner_AWipedCacheThatCannotBeRewrittenKeepsTheArt pins the recovery
// of a wiped artwork cache (needsLocalArtworkRecovery) against the merge
// rule that drops a removed cover's art: a cover read whole whose cache file
// cannot be written (the artwork directory made read-only) is no verdict, so
// the rows keep their `local-` value, and the first scan that can write the
// cache restores it. Dropped there, the rows lost the cover for good: nothing
// sends the gate back to a row whose art is "" and whose folder is unchanged.
func TestScanner_AWipedCacheThatCannotBeRewrittenKeepsTheArt(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes into a read-only directory")
	}
	f := newArtFixture(t)
	rels := []string{"Artist/Album/01.flac", "Artist/Album/02.flac"}
	for _, rel := range rels {
		f.flac(t, rel)
	}
	data := coverBytes("cached")
	f.cover(t, "Artist/Album/cover.jpg", data, t0)
	f.scan(t, "index")
	want := expectedLocalMBID(data)
	f.requireArt(t, want, rels...)

	cached := filepath.Join(f.sc.artDir, want+"-500.jpg")
	if err := os.Remove(cached); err != nil {
		t.Fatalf("wipe the cache: %v", err)
	}
	if err := os.Chmod(f.sc.artDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(f.sc.artDir, 0o755) })
	f.scan(t, "the cache wiped and unwritable")
	f.requireArt(t, want, rels...)

	if err := os.Chmod(f.sc.artDir, 0o755); err != nil {
		t.Fatal(err)
	}
	f.scan(t, "the cache writable again")
	f.requireArt(t, want, rels...)
	if _, err := os.Stat(cached); err != nil {
		t.Errorf("the cache file was not restored: %v", err)
	}
	f.requireSettled(t, rels...)
}

// TestScanner_AFolderWhoseCoverCannotBeSeenKeepsItsRows pins "we could not
// see it": a cover replaced by one whose stat fails (a link that loops) says
// nothing about the folder's art, so the rows keep the art they had and the
// scan re-reads nothing, while a link to nothing is no cover at all.
func TestScanner_AFolderWhoseCoverCannotBeSeenKeepsItsRows(t *testing.T) {
	f := newArtFixture(t)
	rels := []string{"Artist/Album/01.flac", "Artist/Album/02.flac"}
	for _, rel := range rels {
		f.flac(t, rel)
	}
	data := coverBytes("seen")
	f.cover(t, "Artist/Album/cover.jpg", data, t0)
	f.scan(t, "index")
	f.requireArt(t, expectedLocalMBID(data), rels...)

	// The cover.jpg the rows were given, replaced by a link to itself: its
	// stat fails with ELOOP, as an EIO would. Read as "no cover", the rows
	// would lose their art over a stat that did not complete.
	loop := f.path("Artist/Album/cover.jpg")
	saved, err := os.ReadFile(loop)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(loop); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("cover.jpg", loop); err != nil {
		t.Skipf("symlinks unsupported here: %v", err)
	}
	before := f.indexedAts(t, rels...)
	if n := f.scan(t, "the cover cannot be stat'ed"); n != 0 {
		t.Errorf("the scan re-read %d audio files, want 0 (a folder it could not see keeps its rows)", n)
	}
	f.requireArt(t, expectedLocalMBID(data), rels...)
	f.requireStill(t, before)

	// The cover back as it was, and a cover.png that links to nothing,
	// which is no cover: the key is unchanged.
	if err := os.Remove(loop); err != nil {
		t.Fatal(err)
	}
	f.cover(t, "Artist/Album/cover.jpg", saved, t0)
	if err := os.Symlink(filepath.Join(f.root, "nowhere.png"), f.path("Artist/Album/cover.png")); err != nil {
		t.Fatal(err)
	}
	if n := f.scan(t, "a candidate linking to nothing"); n != 0 {
		t.Errorf("the scan re-read %d audio files, want 0 (a link to nothing is no cover)", n)
	}
	f.requireStill(t, before)

	// And the unseen state lifts: a cover made visible again is read.
	replacement := coverBytes("again")
	f.cover(t, "Artist/Album/cover.jpg", replacement, t0.Add(time.Hour))
	f.scan(t, "the cover replaced")
	f.requireArt(t, expectedLocalMBID(replacement), rels...)
	f.requireSettled(t, rels...)
}
