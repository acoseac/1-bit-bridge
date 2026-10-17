package manifest

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// The scan side of the playlist-file index (backlog B431): which files a
// walk reads, when it reads them again, and when a row leaves.

// roadTrip is a playlist that names tracks in another folder, with every
// shape of line the feed must keep apart.
const roadTrip = "#EXTM3U\n" +
	"#EXTINF:215,Artist - One\n" +
	"../Artist/Album/01 One.flac\n" +
	"..\\Artist\\Album\\02 Two.flac\n" + // Windows separators, folded
	"../Missing/03.flac\n" + // placed, and no track there
	"http://example.com/stream.mp3\n" + // a URL: not placed
	"../../../x.flac\n" + // above the library root: not placed
	"../artist/album/01 one.flac\n" + // another case: the one row that folds alike
	"../Artist/Album/01 One.flac\n" // a duplicate, kept

// newRoadTripLibrary is a library holding roadTrip and two of its tracks.
func newRoadTripLibrary(t *testing.T) (root string, s *Store, sc *Scanner) {
	t.Helper()
	root = t.TempDir()
	stubTracks(t, root, "Artist/Album/01 One.flac", "Artist/Album/02 Two.flac")
	writeLibraryFile(t, root, "Playlists/Road Trip.m3u8", []byte(roadTrip))
	s, sc = newScanFixture(t, root)
	return root, s, sc
}

// wantPlaylist fails the test unless the feed lists path with exactly these
// members and unresolved lines.
func wantPlaylist(t *testing.T, feed map[string]PlaylistFile, path string, members, unresolved []string) PlaylistFile {
	t.Helper()
	pf, ok := feed[path]
	if !ok {
		t.Fatalf("the feed has no %q; it has %v", path, feedPaths(feed))
	}
	if strings.Join(pf.Members, "|") != strings.Join(members, "|") {
		t.Errorf("%s members %q, want %q", path, pf.Members, members)
	}
	if strings.Join(pf.Unresolved, "|") != strings.Join(unresolved, "|") {
		t.Errorf("%s unresolved %q, want %q", path, pf.Unresolved, unresolved)
	}
	return pf
}

func feedPaths(feed map[string]PlaylistFile) []string {
	var out []string
	for p := range feed {
		out = append(out, p)
	}
	return out
}

func TestAFullScanServesAPlaylistFileWithItsMembers(t *testing.T) {
	root, s, sc := newRoadTripLibrary(t)
	scanOnce(t, sc, "scan")

	pf := wantPlaylist(t, playlistFeed(t, s), "Playlists/Road Trip.m3u8",
		[]string{"Artist/Album/01 One.flac", "Artist/Album/02 Two.flac", "Artist/Album/01 One.flac", "Artist/Album/01 One.flac"},
		[]string{"../Missing/03.flac", "http://example.com/stream.mp3", "../../../x.flac"})
	if pf.Name != "Road Trip" || pf.TooLarge {
		t.Errorf("name %q tooLarge %v", pf.Name, pf.TooLarge)
	}
	info, err := os.Stat(filepath.Join(root, "Playlists", "Road Trip.m3u8"))
	if err != nil {
		t.Fatal(err)
	}
	if !pf.ModTime.Equal(info.ModTime()) || pf.ModTime.Location() != time.UTC {
		t.Errorf("mtime %v, want the file's %v in UTC", pf.ModTime, info.ModTime())
	}
	// A playlist file is never a track.
	if tr, err := s.GetTrack(context.Background(), "Playlists/Road Trip.m3u8"); err != nil || tr != nil {
		t.Errorf("the playlist file has a track row: %v, %v", tr, err)
	}
	mustIndexed(t, s, "Artist/Album/01 One.flac", "Artist/Album/02 Two.flac")
}

func TestAPlaylistFileIsReadOnlyWhenItChanges(t *testing.T) {
	root, s, sc := newRoadTripLibrary(t)
	reads := countPlaylistReads(sc, nil)
	const rel = "Playlists/Road Trip.m3u8"
	scanOnce(t, sc, "first scan")
	scanOnce(t, sc, "second scan")
	if reads[rel] != 1 {
		t.Fatalf("read %d times over two scans of an unchanged file, want 1", reads[rel])
	}
	abs := writeLibraryFile(t, root, rel, []byte("../Artist/Album/02 Two.flac\n"))
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(abs, later, later); err != nil {
		t.Fatal(err)
	}
	scanOnce(t, sc, "after the edit")
	if reads[rel] != 2 {
		t.Fatalf("read %d times, want the edit read once more", reads[rel])
	}
	wantPlaylist(t, playlistFeed(t, s), rel, []string{"Artist/Album/02 Two.flac"}, []string{})
}

func TestAPlaylistFileReadByAnOlderParserIsReadAgain(t *testing.T) {
	_, s, sc := newRoadTripLibrary(t)
	reads := countPlaylistReads(sc, nil)
	scanOnce(t, sc, "first scan")
	if _, err := s.db.Exec(`UPDATE playlist_files SET parser_version = ?`, playlistFileParserVersion-1); err != nil {
		t.Fatal(err)
	}
	scanOnce(t, sc, "second scan")
	if got := reads["Playlists/Road Trip.m3u8"]; got != 2 {
		t.Fatalf("read %d times, want a row an older parser read read again", got)
	}
}

func TestARemovedPlaylistFileLeavesTheFeedAtTheThreshold(t *testing.T) {
	root, s, sc := newRoadTripLibrary(t)
	sc.SetDeleteThreshold(3)
	const rel = "Playlists/Road Trip.m3u8"
	scanOnce(t, sc, "scan")
	if err := os.Remove(filepath.Join(root, filepath.FromSlash(rel))); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 2; i++ {
		scanOnce(t, sc, "missing scan")
		if _, ok := playlistFeed(t, s)[rel]; !ok {
			t.Fatalf("gone after %d missing scans; the threshold is 3", i)
		}
	}
	scanOnce(t, sc, "third missing scan")
	if _, ok := playlistFeed(t, s)[rel]; ok {
		t.Fatal("still served after the threshold's third missing scan")
	}
}

func TestAPlaylistFileSeenAgainBeforeTheThresholdStays(t *testing.T) {
	root, s, sc := newRoadTripLibrary(t)
	sc.SetDeleteThreshold(2)
	const rel = "Playlists/Road Trip.m3u8"
	abs := filepath.Join(root, filepath.FromSlash(rel))
	scanOnce(t, sc, "scan")
	if err := os.Rename(abs, abs+".away"); err != nil {
		t.Fatal(err)
	}
	scanOnce(t, sc, "missing once")
	if err := os.Rename(abs+".away", abs); err != nil {
		t.Fatal(err)
	}
	scanOnce(t, sc, "back")
	var missing int
	if err := s.db.QueryRow(`SELECT missing_count FROM playlist_files WHERE path = ?`, rel).Scan(&missing); err != nil {
		t.Fatal(err)
	}
	if missing != 0 {
		t.Fatalf("missing_count %d after the file came back, want 0", missing)
	}
}

func TestAPlaylistFileUnderAnEmptiedRootIsKept(t *testing.T) {
	// The clean-empty guard: a root whose walk finds nothing while the store
	// holds rows for it looks like a mount that dropped, and its rows are
	// spared. The playlist file's row is one of them.
	root, s, sc := newRoadTripLibrary(t)
	sc.SetDeleteThreshold(1)
	scanOnce(t, sc, "scan")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(root, e.Name())); err != nil {
			t.Fatal(err)
		}
	}
	scanOnce(t, sc, "scan of the emptied root")
	if _, ok := playlistFeed(t, s)["Playlists/Road Trip.m3u8"]; !ok {
		t.Fatal("the playlist file's row went with a root that looks unmounted")
	}
}

func TestASubtreeScanCoversOnlyItsOwnPlaylistFiles(t *testing.T) {
	root := t.TempDir()
	stubTracks(t, root, "A/01.flac")
	writeLibraryFile(t, root, "Playlists/x.m3u", []byte("../A/01.flac\n"))
	s, sc := newScanFixture(t, root)
	sc.SetDeleteThreshold(1)
	scanOnce(t, sc, "scan")

	stubTracks(t, root, "B/02.flac")
	writeLibraryFile(t, root, "B/b.m3u", []byte("02.flac\n../A/01.flac\n"))
	if err := os.Remove(filepath.Join(root, "Playlists", "x.m3u")); err != nil {
		t.Fatal(err)
	}
	if _, err := sc.ScanSubtree(context.Background(), filepath.Join(root, "B")); err != nil {
		t.Fatal(err)
	}
	feed := playlistFeed(t, s)
	wantPlaylist(t, feed, "B/b.m3u", []string{"B/02.flac", "A/01.flac"}, []string{})
	// x.m3u is outside the subtree: not counted missing by it.
	if _, ok := feed["Playlists/x.m3u"]; !ok {
		t.Fatal("a subtree scan of B removed a playlist file outside it")
	}
	if _, err := sc.ScanSubtree(context.Background(), filepath.Join(root, "Playlists")); err != nil {
		t.Fatal(err)
	}
	if _, ok := playlistFeed(t, s)["Playlists/x.m3u"]; ok {
		t.Fatal("a subtree scan of its own folder kept a removed playlist file")
	}
}

func TestAPlaylistMemberIndexedLaterResolvesWithoutARead(t *testing.T) {
	root := t.TempDir()
	stubTracks(t, root, "A/01.flac")
	writeLibraryFile(t, root, "Playlists/mix.m3u", []byte("../A/01.flac\n../Late/02.flac\n"))
	s, sc := newScanFixture(t, root)
	reads := countPlaylistReads(sc, nil)
	scanOnce(t, sc, "scan")
	wantPlaylist(t, playlistFeed(t, s), "Playlists/mix.m3u", []string{"A/01.flac"}, []string{"../Late/02.flac"})

	stubTracks(t, root, "Late/02.flac")
	if _, err := sc.ScanSubtree(context.Background(), filepath.Join(root, "Late")); err != nil {
		t.Fatal(err)
	}
	wantPlaylist(t, playlistFeed(t, s), "Playlists/mix.m3u", []string{"A/01.flac", "Late/02.flac"}, []string{})
	if got := reads["Playlists/mix.m3u"]; got != 1 {
		t.Fatalf("the playlist file was read %d times; its member resolved from the stored lines, so once", got)
	}
}

func TestAlbumFolderPlaylistsAreNotServed(t *testing.T) {
	// The app's four terms (AlbumFolderPlaylist.isAlbumPlaylist, B421), end
	// to end through a scan; TestTheAlbumFolderRuleTermByTerm has each term.
	root := t.TempDir()
	stubTracks(t, root,
		"Ripped/Album/01.flac", "Ripped/Album/02.flac", "Ripped/Album/03.flac",
		"Part/Album/01.flac", "Part/Album/02.flac", "Part/Album/03.flac",
		"Out/Album/01.flac", "Out/Album/02.flac", "Out/Other/x.flac",
		"Stale/Album/01.flac", "Stale/Album/02.flac",
		"Dropped/Album/01.flac", "Dropped/Album/02.flac",
		"Case/Album/01.flac", "Case/Album/02.flac",
		"Disc/Album/01.flac", "Disc/Album/CD2/01.flac",
		"Box/00 Intro.flac", "Box/CD1/01.flac")
	// A ripper's playlist, in track order or not: every track of its folder.
	writeLibraryFile(t, root, "Ripped/Album/Album.m3u", []byte("03.flac\n01.flac\n02.flac\n02.flac\n"))
	// A line the path rule drops is no entry: a URL and a climb above the
	// root beside the folder's tracks.
	writeLibraryFile(t, root, "Dropped/Album/Album.m3u", []byte("http://example.com/x.mp3\n../../../up.flac\n01.flac\n02.flac\n"))
	// The folder's own tracks only: a subfolder's are not its own.
	writeLibraryFile(t, root, "Box/Box.m3u", []byte("00 Intro.flac\n"))
	// Part of its folder.
	writeLibraryFile(t, root, "Part/Album/Best.m3u", []byte("01.flac\n02.flac\n"))
	// Its folder and an entry outside it.
	writeLibraryFile(t, root, "Out/Album/Plus.m3u", []byte("01.flac\n02.flac\n../Other/x.flac\n"))
	// A ripper's file with one stale line: every entry must resolve.
	writeLibraryFile(t, root, "Stale/Album/Album.m3u", []byte("01.flac\n02.flac\n03 Renamed.flac\n"))
	// An entry found only by the case fold does not resolve at its exact path.
	writeLibraryFile(t, root, "Case/Album/Album.m3u", []byte("01.FLAC\n02.flac\n"))
	// Nothing resolves.
	writeLibraryFile(t, root, "Empty/Album/Album.m3u", []byte("gone.flac\n"))
	// A disc in a subfolder is not its own folder.
	writeLibraryFile(t, root, "Disc/Album/Album.m3u", []byte("01.flac\nCD2/01.flac\n"))
	s, sc := newScanFixture(t, root)
	scanOnce(t, sc, "scan")
	feed := playlistFeed(t, s)
	for _, skipped := range []string{"Ripped/Album/Album.m3u", "Dropped/Album/Album.m3u", "Box/Box.m3u"} {
		if _, ok := feed[skipped]; ok {
			t.Errorf("%s is an album-folder playlist and was served", skipped)
		}
	}
	wantPlaylist(t, feed, "Part/Album/Best.m3u", []string{"Part/Album/01.flac", "Part/Album/02.flac"}, []string{})
	wantPlaylist(t, feed, "Out/Album/Plus.m3u", []string{"Out/Album/01.flac", "Out/Album/02.flac", "Out/Other/x.flac"}, []string{})
	wantPlaylist(t, feed, "Stale/Album/Album.m3u", []string{"Stale/Album/01.flac", "Stale/Album/02.flac"}, []string{"03 Renamed.flac"})
	wantPlaylist(t, feed, "Case/Album/Album.m3u", []string{"Case/Album/01.flac", "Case/Album/02.flac"}, []string{})
	wantPlaylist(t, feed, "Empty/Album/Album.m3u", []string{}, []string{"gone.flac"})
	wantPlaylist(t, feed, "Disc/Album/Album.m3u", []string{"Disc/Album/01.flac", "Disc/Album/CD2/01.flac"}, []string{})
	if len(feed) != 6 {
		t.Errorf("the feed lists %v, want the six that are not album-folder playlists", feedPaths(feed))
	}
}

func TestAMultiRootLibraryResolvesFromTheManifestRoot(t *testing.T) {
	music := filepath.Join(t.TempDir(), "Music")
	other := filepath.Join(t.TempDir(), "Other")
	stubTracks(t, music, "A/02.flac")
	stubTracks(t, other, "Artist/01.flac")
	writeLibraryFile(t, music, "Playlists/mix.m3u", []byte(
		"../../Other/Artist/01.flac\n"+ // out of its root and into the other one
			"../A/02.flac\n"+ // its own root
			"/Other/Artist/01.flac\n"+ // from the manifest root, above the roots' names
			"../../../x.flac\n")) // above the manifest root: not placed
	writeLibraryFile(t, other, "Artist/o.m3u", []byte("01.flac\n../../Music/A/02.flac\n"))
	s, err := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	sc := NewScanner([]string{music, other}, s, "")
	scanOnce(t, sc, "scan")

	feed := playlistFeed(t, s)
	wantPlaylist(t, feed, "Music/Playlists/mix.m3u",
		[]string{"Other/Artist/01.flac", "Music/A/02.flac", "Other/Artist/01.flac"},
		[]string{"../../../x.flac"})
	wantPlaylist(t, feed, "Other/Artist/o.m3u", []string{"Other/Artist/01.flac", "Music/A/02.flac"}, []string{})

	// Removing a root removes its playlist files with its tracks.
	if _, err := s.DeleteTracksByPrefix(context.Background(), "Other/"); err != nil {
		t.Fatal(err)
	}
	feed = playlistFeed(t, s)
	if _, ok := feed["Other/Artist/o.m3u"]; ok {
		t.Error("a removed root's playlist file is still served")
	}
	wantPlaylist(t, feed, "Music/Playlists/mix.m3u", []string{"Music/A/02.flac"},
		[]string{"../../Other/Artist/01.flac", "/Other/Artist/01.flac", "../../../x.flac"})

	// The single↔multi flip wipes them all: their paths change form.
	if err := s.WipeFilesystemTracks(context.Background()); err != nil {
		t.Fatal(err)
	}
	if feed := playlistFeed(t, s); len(feed) != 0 {
		t.Errorf("a root flip's wipe left %v", feedPaths(feed))
	}
}

func TestAPlaylistFileReadFailureKeepsItsRow(t *testing.T) {
	root, s, sc := newRoadTripLibrary(t)
	const rel = "Playlists/Road Trip.m3u8"
	failing := false
	reads := countPlaylistReads(sc, func(r string) error {
		if failing && r == rel {
			return &os.PathError{Op: "read", Path: "/host-side/library/" + r, Err: errors.New("input/output error")}
		}
		return nil
	})
	scanOnce(t, sc, "scan")
	before := playlistFeed(t, s)[rel]

	abs := writeLibraryFile(t, root, rel, []byte("../Artist/Album/02 Two.flac\n"))
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(abs, later, later); err != nil {
		t.Fatal(err)
	}
	failing = true
	buf := captureScanLogs(t)
	sc.SetDeleteThreshold(1)
	scanOnce(t, sc, "scan whose read fails")
	if reads[rel] != 2 {
		t.Fatalf("read %d times, want the changed file tried again", reads[rel])
	}
	after, ok := playlistFeed(t, s)[rel]
	if !ok || strings.Join(after.Members, "|") != strings.Join(before.Members, "|") {
		t.Fatalf("a failed read changed the row: before %q, after %q (present %v)", before.Members, after.Members, ok)
	}
	logged := buf.String()
	if strings.Count(logged, msgPlaylistFilesUnread) != 1 || !strings.Contains(logged, "example="+`"`+rel+`"`) {
		t.Errorf("want one line naming the file library-relative:\n%s", logged)
	}
	if strings.Contains(logged, "host-side") {
		t.Errorf("the line names the host path:\n%s", logged)
	}

	failing = false
	scanOnce(t, sc, "scan whose read works")
	wantPlaylist(t, playlistFeed(t, s), rel, []string{"Artist/Album/02 Two.flac"}, []string{})
}

func TestAPlaylistFilePastTheSizeCapIsServedTooLarge(t *testing.T) {
	root := t.TempDir()
	stubTracks(t, root, "A/01.flac")
	// An #EXTINF line before each entry keeps the entries under
	// playlistFileMaxEntries, so the byte cap is the one measured.
	line := []byte("#EXTINF:-1,Some Artist - Some Title of a Song\n../A/01.flac\n")
	under := make([]byte, 0, PlaylistFileMaxBytes)
	for len(under)+len(line) <= PlaylistFileMaxBytes-1 {
		under = append(under, line...)
	}
	for len(under) < PlaylistFileMaxBytes-1 {
		under = append(under, '\n')
	}
	writeLibraryFile(t, root, "Playlists/under.m3u", under)
	writeLibraryFile(t, root, "Playlists/at.m3u", append(append([]byte{}, under...), '\n'))
	s, sc := newScanFixture(t, root)
	scanOnce(t, sc, "scan")
	feed := playlistFeed(t, s)
	if pf := feed["Playlists/at.m3u"]; !pf.TooLarge || len(pf.Members) != 0 || len(pf.Unresolved) != 0 {
		t.Errorf("a file of %d bytes: tooLarge %v, %d members, %d unresolved; want it not read",
			PlaylistFileMaxBytes, pf.TooLarge, len(pf.Members), len(pf.Unresolved))
	}
	if pf := feed["Playlists/under.m3u"]; pf.TooLarge || len(pf.Members) == 0 {
		t.Errorf("a file of %d bytes: tooLarge %v, %d members; want it read", PlaylistFileMaxBytes-1, pf.TooLarge, len(pf.Members))
	}
}

func TestAManifestPathBackslashIsPartOfAName(t *testing.T) {
	// Only a playlist's LINES fold "\" to "/". A manifest path is "/"-separated
	// on every platform, and a "\" in it is a character of a name (a Linux
	// folder may be called that).
	if dir := playlistFileDir(`Odd\Dir/mix.m3u`); dir != `Odd\Dir` {
		t.Errorf("dir %q", dir)
	}
	if dir := playlistFileDir(`Odd\mix.m3u`); dir != "" {
		t.Errorf("dir %q, want the root", dir)
	}
	if name := playlistFileDisplayName(`a/b\c.m3u`); name != `b\c` {
		t.Errorf("name %q", name)
	}
	if p, ok := playlistEntryPath(`Odd\Dir`, "x.flac"); !ok || p != `Odd\Dir/x.flac` {
		t.Errorf("entry %q, %v", p, ok)
	}
	if runtime.GOOS == "windows" {
		return // a "\" is a separator in a Windows file name
	}
	root := t.TempDir()
	stubTracks(t, root, `Odd\Dir/x.flac`, `Odd\Dir/y.flac`)
	writeLibraryFile(t, root, `Odd\Dir/mix.m3u`, []byte("x.flac\n"))
	s, sc := newScanFixture(t, root)
	scanOnce(t, sc, "scan")
	wantPlaylist(t, playlistFeed(t, s), `Odd\Dir/mix.m3u`, []string{`Odd\Dir/x.flac`}, []string{})
}

func TestThePlaylistIndexIsCompleteOnlyOnceAFullScanBacksEveryFile(t *testing.T) {
	root, s, sc := newRoadTripLibrary(t)
	const rel = "Playlists/Road Trip.m3u8"
	ctx := context.Background()
	complete := func(when string) bool {
		t.Helper()
		feed, err := s.PlaylistFiles(ctx)
		if err != nil {
			t.Fatalf("%s: %v", when, err)
		}
		return feed.Complete
	}
	if complete("fresh store") {
		t.Fatal("a store the migration just made reads complete")
	}
	if _, err := sc.ScanSubtree(ctx, filepath.Join(root, "Playlists")); err != nil {
		t.Fatal(err)
	}
	if _, ok := playlistFeed(t, s)[rel]; !ok || complete("after a subtree scan") {
		t.Fatalf("a subtree scan: listed %v, complete %v; want listed and not complete", ok, complete("again"))
	}
	scanOnce(t, sc, "full scan")
	if !complete("after a full scan") {
		t.Fatal("a full scan that read every file did not mark the index complete")
	}

	// The root flip's wipe empties the index, and a full scan whose read of
	// a file with no row fails leaves it incomplete.
	if err := s.WipeFilesystemTracks(ctx); err != nil {
		t.Fatal(err)
	}
	if complete("after the wipe") {
		t.Fatal("the wipe left the index complete")
	}
	failing := true
	countPlaylistReads(sc, func(r string) error {
		if failing && r == rel {
			return errors.New("input/output error")
		}
		return nil
	})
	scanOnce(t, sc, "full scan whose read fails")
	if complete("after a failed read") {
		t.Fatal("a full scan that could not read a file with no row marked the index complete")
	}
	failing = false
	scanOnce(t, sc, "full scan whose read works")
	if !complete("after the read works") {
		t.Fatal("not complete once every file was read")
	}

	// Past that point a failed read keeps the row, and the index stays
	// complete.
	abs := filepath.Join(root, filepath.FromSlash(rel))
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(abs, later, later); err != nil {
		t.Fatal(err)
	}
	failing = true
	scanOnce(t, sc, "full scan whose re-read fails")
	if !complete("after a failed re-read") {
		t.Fatal("a failed read of a file with a row cleared the index's completeness")
	}
}

// TestThePlaylistPassWritesEachWindowBeforeReadingTheNext: a pass reads and
// writes its files a window at a time (nextPlaylistReadWindow), so it holds
// one window of parsed files, not the library's. 300 files are two windows
// of the count bound: the second window's reads find the first window's 256
// rows written, and every file is served at the end.
func TestThePlaylistPassWritesEachWindowBeforeReadingTheNext(t *testing.T) {
	root := t.TempDir()
	stubTracks(t, root, "A/01.flac")
	const files = 300
	for i := 0; i < files; i++ {
		writeLibraryFile(t, root, fmt.Sprintf("Lists/%03d.m3u", i), []byte("../A/01.flac\n"))
	}
	s, sc := newScanFixture(t, root)
	var mu sync.Mutex
	rowsSeen := map[int]int{} // rows already written -> reads that saw that many
	sc.readPlaylist = func(abs, rel string) (playlistFileRead, error) {
		var n int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM playlist_files`).Scan(&n); err != nil {
			return playlistFileRead{}, err
		}
		mu.Lock()
		rowsSeen[n]++
		mu.Unlock()
		return readPlaylistFile(abs, rel)
	}
	scanOnce(t, sc, "scan")
	if rowsSeen[0] != playlistWriteBatch || rowsSeen[playlistWriteBatch] != files-playlistWriteBatch || len(rowsSeen) != 2 {
		t.Fatalf("reads by the rows they found written: %v; want %d reads before any write and %d after the first window's",
			rowsSeen, playlistWriteBatch, files-playlistWriteBatch)
	}
	if feed := playlistFeed(t, s); len(feed) != files {
		t.Fatalf("%d files served, want %d", len(feed), files)
	}
}

// sizeInfo is a stat reporting a size, for the read windows' bounds.
type sizeInfo int64

func (n sizeInfo) Name() string       { return "entry.m3u" }
func (n sizeInfo) Size() int64        { return int64(n) }
func (n sizeInfo) Mode() fs.FileMode  { return 0o644 }
func (n sizeInfo) ModTime() time.Time { return time.Time{} }
func (n sizeInfo) IsDir() bool        { return false }
func (n sizeInfo) Sys() any           { return nil }

// TestPlaylistReadWindowsHoldTheirBytes: a read window is at most
// playlistWriteBatch files and playlistWriteBatchBytes of them by the walk's
// sizes, a file past the cap costs nothing (it is not read), and the windows
// cover every file in order.
func TestPlaylistReadWindowsHoldTheirBytes(t *testing.T) {
	windows := func(sizes ...int64) string {
		toRead := make([]playlistSighting, len(sizes))
		for i, n := range sizes {
			toRead[i] = playlistSighting{pathInfo: pathInfo{rel: fmt.Sprintf("%03d.m3u", i), info: sizeInfo(n)}}
		}
		var got []string
		for start := 0; start < len(toRead); {
			end := nextPlaylistReadWindow(toRead, start)
			if end <= start {
				t.Fatalf("the window at %d ends at %d", start, end)
			}
			got = append(got, fmt.Sprintf("%d-%d", start, end))
			start = end
		}
		return strings.Join(got, " ")
	}
	var bySize []int64
	for i := 0; i < 9; i++ {
		bySize = append(bySize, 1_900_000) // eight fit in 16 MiB, nine do not
	}
	bySize = append(bySize, PlaylistFileMaxBytes, 5, 5)
	if got, want := windows(bySize...), "0-8 8-12"; got != want {
		t.Errorf("windows by size %s, want %s", got, want)
	}
	byCount := make([]int64, playlistWriteBatch+1)
	for i := range byCount {
		byCount[i] = 1
	}
	if got, want := windows(byCount...), fmt.Sprintf("0-%d %d-%d", playlistWriteBatch, playlistWriteBatch, playlistWriteBatch+1); got != want {
		t.Errorf("windows by count %s, want %s", got, want)
	}
}
