//go:build unix

package manifest

import (
	"os"
	"path/filepath"
	"testing"
)

// The album-folder rule's gate on a real scan (backlog B431, the app's B421):
// a playlist is judged an album playlist only on whole evidence about its
// folder. These lock a file out with chmod 0 (lockOut) or plant a link that
// cannot be followed, which only a POSIX host makes.

// TestAnAlbumFolderPlaylistIsServedWhileItsFolderHoldsAFileTheIndexLacks: the
// folder holds a file the scan could not read, so the index has no row for
// it, and the playlist names exactly the tracks the index holds there. Terms
// 1 to 4 hold against the index, but the index under-counts the folder (the
// file may be the eleventh of eleven), so the playlist is listed. Once the
// file is gone, the walk sees the folder whole and the same unchanged file
// is an album playlist.
func TestAnAlbumFolderPlaylistIsServedWhileItsFolderHoldsAFileTheIndexLacks(t *testing.T) {
	root := t.TempDir()
	stubTracks(t, root, "Album/01.flac", "Album/02.flac", "Album/03.flac")
	writeLibraryFile(t, root, "Album/Album.m3u", []byte("01.flac\n02.flac\n"))
	locked := filepath.Join(root, "Album", "03.flac")
	lockOut(t, locked)
	s, sc := newScanFixture(t, root)
	scanOnce(t, sc, "scan with 03.flac unreadable")
	mustIndexed(t, s, "Album/01.flac", "Album/02.flac")
	if tr, err := s.GetTrack(t.Context(), "Album/03.flac"); err != nil || tr != nil {
		t.Fatalf("the unreadable file has a row (%v, %v); the fixture needs a file the index lacks", tr, err)
	}
	if _, ok := playlistFeed(t, s)["Album/Album.m3u"]; !ok {
		t.Fatal("a playlist whose folder holds a file the index lacks was judged an album playlist")
	}

	if err := os.Remove(locked); err != nil {
		t.Fatal(err)
	}
	scanOnce(t, sc, "scan without 03.flac")
	if _, ok := playlistFeed(t, s)["Album/Album.m3u"]; ok {
		t.Fatal("an album-folder playlist whose folder the scan saw whole is listed")
	}
}

// TestAnAlbumFolderPlaylistBesideALinkTheWalkCannotFollowIsServed: an
// audio-named link whose target is gone is an entry of the folder the walk
// could not see (its row, had it one, would be spared), so the folder is not
// seen whole and its playlist is listed until the link goes.
func TestAnAlbumFolderPlaylistBesideALinkTheWalkCannotFollowIsServed(t *testing.T) {
	root := t.TempDir()
	stubTracks(t, root, "Album/01.flac", "Album/02.flac")
	writeLibraryFile(t, root, "Album/Album.m3u", []byte("01.flac\n02.flac\n"))
	link := filepath.Join(root, "Album", "03.flac")
	if err := os.Symlink(filepath.Join(root, "gone.flac"), link); err != nil {
		t.Fatal(err)
	}
	s, sc := newScanFixture(t, root)
	scanOnce(t, sc, "scan with a link to nothing")
	if _, ok := playlistFeed(t, s)["Album/Album.m3u"]; !ok {
		t.Fatal("a playlist beside a link the walk could not follow was judged an album playlist")
	}

	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	scanOnce(t, sc, "scan without the link")
	if _, ok := playlistFeed(t, s)["Album/Album.m3u"]; ok {
		t.Fatal("an album-folder playlist whose folder the scan saw whole is listed")
	}
}
