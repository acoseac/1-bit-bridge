//go:build !windows

package manifest

import (
	"os"
	"path/filepath"
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

	// A folder.jpg that links to itself: its stat fails with ELOOP.
	loop := f.path("Artist/Album/folder.jpg")
	if err := os.Symlink("folder.jpg", loop); err != nil {
		t.Skipf("symlinks unsupported here: %v", err)
	}
	before := f.indexedAts(t, rels...)
	if n := f.scan(t, "a candidate that cannot be stat'ed"); n != 0 {
		t.Errorf("the scan re-read %d audio files, want 0 (a folder it could not see keeps its rows)", n)
	}
	f.requireArt(t, expectedLocalMBID(data), rels...)
	f.requireStill(t, before)

	// A cover.png that links to nothing is no cover: the key is unchanged.
	if err := os.Remove(loop); err != nil {
		t.Fatal(err)
	}
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
