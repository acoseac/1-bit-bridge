package manifest

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The request side of the playlist-file index (backlog B431): how the feed
// resolves stored lines against the track rows, over rows written straight
// into the store.

// newFeedStore opens an empty store with a track row at each path.
func newFeedStore(t *testing.T, tracks ...string) *Store {
	t.Helper()
	s, err := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	for _, p := range tracks {
		if err := s.UpsertTrack(context.Background(), &Track{Path: p, Size: 1, ModTime: time.Unix(1, 0)}); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

// storePlaylist writes one playlist file's row, as a read stores it.
func storePlaylist(t *testing.T, s *Store, path string, lines ...string) {
	t.Helper()
	if _, err := s.writePlaylistFiles(context.Background(), []playlistFileRead{{
		path: path, size: 1, mtimeNs: time.Unix(2, 0).UnixNano(), lines: lines,
	}}); err != nil {
		t.Fatal(err)
	}
}

// stampDuplicates marks each path with a duplicate group and whether the
// stamping pass suppressed it.
func stampDuplicates(t *testing.T, s *Store, group string, suppressed bool, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if _, err := s.db.Exec(`UPDATE tracks SET dupe_group_id = ?, dupe_suppressed = ? WHERE path = ?`,
			group, suppressed, p); err != nil {
			t.Fatal(err)
		}
	}
}

func TestThePlaylistFeedOfAnEmptyIndexIsAnEmptyList(t *testing.T) {
	s := newFeedStore(t)
	files, err := s.PlaylistFiles(context.Background())
	if err != nil || files == nil || len(files) != 0 {
		t.Fatalf("PlaylistFiles = %v, %v; want an empty, non-nil list", files, err)
	}
	storePlaylist(t, s, "Playlists/none.m3u")
	pf := playlistFeed(t, s)["Playlists/none.m3u"]
	if pf.Members == nil || pf.Unresolved == nil {
		t.Fatalf("a playlist with no lines: members %v unresolved %v; want empty lists, not nil", pf.Members, pf.Unresolved)
	}
}

func TestAPlaylistEntryFoldsOnlyToTheOneRowThatAnswers(t *testing.T) {
	const nfd, nfc = "Café", "Café"
	s := newFeedStore(t, "A/"+nfd+"/01.flac", "B/x.flac", "B/X.flac")
	storePlaylist(t, s, "Playlists/mix.m3u",
		"../A/"+nfc+"/01.flac", // another composition: the one row folds alike
		"../B/x.FLAC",          // two rows fold alike: refuse to pick
		"../B/X.flac",          // the exact bytes win over an ambiguous fold
	)
	wantPlaylist(t, playlistFeed(t, s), "Playlists/mix.m3u",
		[]string{"A/" + nfd + "/01.flac", "B/X.flac"}, []string{"../B/x.FLAC"})
}

func TestASuppressedDuplicateMemberIsServedAsItsServedCopy(t *testing.T) {
	s := newFeedStore(t,
		"MP3/a.mp3", "FLAC/a.flac", // one served copy
		"DSD/b.dsf", "PCM/b.flac", "Copy/b.dsf", // two served copies
		"Gone/c.flac") // a group with no served copy
	stampDuplicates(t, s, "g1", true, "MP3/a.mp3")
	stampDuplicates(t, s, "g1", false, "FLAC/a.flac")
	stampDuplicates(t, s, "g2", false, "DSD/b.dsf", "PCM/b.flac")
	stampDuplicates(t, s, "g2", true, "Copy/b.dsf")
	stampDuplicates(t, s, "g3", true, "Gone/c.flac")
	storePlaylist(t, s, "Playlists/mix.m3u", "../MP3/a.mp3", "../Copy/b.dsf", "../Gone/c.flac", "../FLAC/a.flac")
	wantPlaylist(t, playlistFeed(t, s), "Playlists/mix.m3u",
		[]string{"FLAC/a.flac", "FLAC/a.flac"}, []string{"../Copy/b.dsf", "../Gone/c.flac"})
}

func TestAnAlbumFolderPlaylistIsJudgedBeforeTheDuplicateMapping(t *testing.T) {
	// The ripper's playlist names its own folder's files; one of them is a
	// suppressed copy of a track elsewhere. Still an album-folder playlist.
	s := newFeedStore(t, "Album/01.flac", "Album/02.flac", "Best Of/02.flac")
	stampDuplicates(t, s, "g", true, "Album/02.flac")
	stampDuplicates(t, s, "g", false, "Best Of/02.flac")
	storePlaylist(t, s, "Album/Album.m3u", "01.flac", "02.flac")
	if feed := playlistFeed(t, s); len(feed) != 0 {
		t.Fatalf("served %v; the album-folder rule reads the files the playlist names", feedPaths(feed))
	}
}

func TestThePlaylistFeedHoldsItsBudget(t *testing.T) {
	s := newFeedStore(t, "A/01.flac")
	storePlaylist(t, s, "a.m3u", "A/01.flac", "A/01.flac") // costs 26
	storePlaylist(t, s, "b.m3u", strings.Repeat("x", 100)) // costs 104
	storePlaylist(t, s, "c.m3u", "A/01.flac")              // costs 13
	files, err := s.playlistFiles(context.Background(), 40)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, f := range files {
		got = append(got, f.Path+":"+strings.Join(f.Members, ",")+":"+strings.Join(f.Unresolved, ","))
		if f.Path == "b.m3u" && (!f.TooLarge || len(f.Members)+len(f.Unresolved) != 0) {
			t.Errorf("b.m3u past the budget: %+v", f)
		}
	}
	want := "a.m3u:A/01.flac,A/01.flac:|b.m3u::|c.m3u:A/01.flac:"
	if strings.Join(got, "|") != want {
		t.Fatalf("feed %q, want %q", strings.Join(got, "|"), want)
	}
}

func TestPlaylistFileWritesNoteTheLibraryOnlyWhenTheFeedChanges(t *testing.T) {
	s := newFeedStore(t, "A/01.flac")
	notes := 0
	s.SetSyncHooks(SyncHooks{Library: func(int64) { notes++ }})
	ctx := context.Background()
	write := func(stampNs int64, lines ...string) {
		t.Helper()
		if _, err := s.writePlaylistFiles(ctx, []playlistFileRead{{path: "p.m3u", size: 1, mtimeNs: stampNs, lines: lines}}); err != nil {
			t.Fatal(err)
		}
	}
	write(1, "A/01.flac")
	write(2, "A/01.flac") // touched, same lines
	if notes != 1 {
		t.Fatalf("%d notes after a new row and an unchanged one, want 1", notes)
	}
	write(3, "A/02.flac")
	if notes != 2 {
		t.Fatalf("%d notes after a change, want 2", notes)
	}
	if _, err := s.settlePlaylistFiles(ctx, nil, []string{"p.m3u"}, nil, 1); err != nil {
		t.Fatal(err)
	}
	if notes != 3 {
		t.Fatalf("%d notes after a deletion, want 3", notes)
	}
}

func TestClearMissingCountsClearsMissingPlaylistFiles(t *testing.T) {
	s := newFeedStore(t)
	storePlaylist(t, s, "kept.m3u")
	storePlaylist(t, s, "gone.m3u")
	ctx := context.Background()
	if _, err := s.settlePlaylistFiles(ctx, nil, []string{"gone.m3u"}, nil, 5); err != nil {
		t.Fatal(err)
	}
	n, err := s.ClearMissingCounts(ctx)
	if err != nil || n != 1 {
		t.Fatalf("ClearMissingCounts = %d, %v; want the one missing playlist file", n, err)
	}
	feed := playlistFeed(t, s)
	if _, ok := feed["gone.m3u"]; ok || len(feed) != 1 {
		t.Fatalf("feed after clear-missing: %v", feedPaths(feed))
	}
}
