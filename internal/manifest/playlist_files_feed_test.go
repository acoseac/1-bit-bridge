package manifest

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
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

// storePlaylist writes one playlist file's row, as a read stores it in a
// scan that saw the file's folder whole.
func storePlaylist(t *testing.T, s *Store, path string, lines ...string) {
	t.Helper()
	if _, err := s.writePlaylistFiles(context.Background(), []playlistFileRead{{
		path: path, size: 1, mtimeNs: time.Unix(2, 0).UnixNano(), lines: lines, folderWhole: true,
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
	page, err := s.PlaylistFilesPage(context.Background(), "", "")
	if err != nil || page.Files == nil || len(page.Files) != 0 || page.Next != "" || page.Version == "" {
		t.Fatalf("PlaylistFilesPage = %+v, %v; want one page, an empty, non-nil list, a version", page, err)
	}
	storePlaylist(t, s, "Playlists/none.m3u")
	pf := playlistFeed(t, s)["Playlists/none.m3u"]
	if pf.Members == nil || pf.Unresolved == nil {
		t.Fatalf("a playlist with no lines: members %v unresolved %v; want empty lists, not nil", pf.Members, pf.Unresolved)
	}
}

func TestAPlaylistEntryFoldsOnlyToTheOneRowThatAnswers(t *testing.T) {
	const nfd, nfc = "Cafe\u0301", "Caf\u00e9"
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

func TestTheAlbumFolderRuleTermByTerm(t *testing.T) {
	// The app's truth table (AlbumFolderPlaylistTests.test_isAlbumPlaylist_truthTable,
	// B421), term by term, for a playlist at x/list.m3u. A subfolder's track
	// and another folder's are in the store, so each term is the only one
	// that can decide its case.
	s := newFeedStore(t, "x/1.flac", "x/2.flac", "x/sub/3.flac", "y/3.flac")
	for _, c := range []struct {
		label string
		lines []string
		album bool
	}{
		{"exactly the folder", []string{"1.flac", "2.flac"}, true},
		{"another order, a repeat", []string{"2.flac", "1.flac", "2.flac"}, true},
		{"a dropped line is no entry", []string{"http://h/a.mp3", "../../../z.flac", "1.flac", "2.flac"}, true},
		{"part of the folder: a folder track left unnamed", []string{"1.flac"}, false},
		{"an entry outside", []string{"1.flac", "2.flac", "../y/3.flac"}, false},
		{"a subfolder's entry", []string{"1.flac", "2.flac", "sub/3.flac"}, false},
		{"an entry that does not resolve", []string{"1.flac", "2.flac", "9.flac"}, false},
		{"an entry found only by the fold", []string{"1.FLAC", "2.flac"}, false},
		{"no entry", nil, false},
		{"only lines the rule drops", []string{"http://h/a.mp3"}, false},
	} {
		storePlaylist(t, s, "x/list.m3u", c.lines...)
		if _, listed := playlistFeed(t, s)["x/list.m3u"]; listed == c.album {
			t.Errorf("%s: listed %v, want listed %v (an album playlist is not listed)", c.label, listed, !c.album)
		}
	}
	// An empty file in a folder with no tracks is not an album playlist.
	storePlaylist(t, s, "empty/list.m3u")
	if _, listed := playlistFeed(t, s)["empty/list.m3u"]; !listed {
		t.Error("an empty file in a folder with no tracks was not listed")
	}
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

func TestAFileThatDoesNotFitStartsTheNextPage(t *testing.T) {
	s := newFeedStore(t, "A/01.flac")
	storePlaylist(t, s, "a.m3u", "A/01.flac", "A/01.flac") // costs 70 + 26
	storePlaylist(t, s, "b.m3u", strings.Repeat("x", 100)) // costs 70 + 104
	storePlaylist(t, s, "c.m3u", "A/01.flac")              // costs 70 + 13
	// One batch, and a batch per row: the budget is the page's, so it
	// carries from one batch to the next.
	for _, batchBytes := range []int{playlistFeedBatchBytes, 1} {
		var got []string
		for _, page := range walkPlaylistPages(t, s, 200, batchBytes).pages {
			var files []string
			for _, f := range page.Files {
				files = append(files, f.Path+":"+strings.Join(f.Members, ",")+":"+strings.Join(f.Unresolved, ","))
				if f.TooLarge {
					t.Errorf("batches of %d bytes: %s is tooLarge", batchBytes, f.Path)
				}
			}
			got = append(got, strings.Join(files, " "))
		}
		want := "a.m3u:A/01.flac,A/01.flac:|b.m3u::" + strings.Repeat("x", 100) + "|c.m3u:A/01.flac:"
		if strings.Join(got, "|") != want {
			t.Fatalf("batches of %d bytes: pages %q, want %q", batchBytes, strings.Join(got, "|"), want)
		}
	}
}

// TestPlaylistFeedBatchesHoldTheirBytes: a batch is the rows whose stored
// lines fit the bound together, a row larger than the bound is a batch of its
// own, and the batches cover every row in order. The bound is what keeps a
// request from holding the whole index.
func TestPlaylistFeedBatchesHoldTheirBytes(t *testing.T) {
	sizes := []int{10, 0, 30, 100, 5, 5, 0}
	rows := make([]playlistFileRow, len(sizes))
	for i, n := range sizes {
		rows[i] = playlistFileRow{path: fmt.Sprintf("%02d.m3u", i), lineBytes: n}
	}
	var got []string
	for start := 0; start < len(rows); {
		end := nextPlaylistFeedBatch(rows, start, 40)
		if end <= start {
			t.Fatalf("the batch at %d ends at %d", start, end)
		}
		got = append(got, fmt.Sprintf("%d-%d", start, end))
		start = end
	}
	if want := "0-3 3-4 4-7"; strings.Join(got, " ") != want {
		t.Fatalf("batches %v, want %s", got, want)
	}
}

// TestThePlaylistFeedAnswersTheSameInAnyBatchSize resolves one index in a
// single batch, a batch per row, and batches in between: the answer is the
// same, so batching changes what a request holds and nothing it serves. The
// index has a line of each kind (exact, fold-only, a fold two rows answer, a
// suppressed duplicate with one served copy and with none, a dropped line),
// the same lines in several files, an album-folder playlist and a file past
// the cap.
func TestThePlaylistFeedAnswersTheSameInAnyBatchSize(t *testing.T) {
	s := newFeedStore(t, "A/01.flac", "A/02.flac", "B/x.flac", "B/X.flac",
		"C/Caf\u00e9.flac", "D/one.flac", "D/one.dsf", "E/lone.flac", "Album/01.flac", "Album/02.flac")
	stampDuplicates(t, s, "g1", true, "D/one.dsf")
	stampDuplicates(t, s, "g1", false, "D/one.flac")
	stampDuplicates(t, s, "g2", true, "E/lone.flac")
	shared := []string{"../A/01.flac", "../A/02.FLAC", "../B/x.FLAC", "../C/Cafe\u0301.flac",
		"../D/one.dsf", "../E/lone.flac", "http://example.com/x.mp3", "../../../up.flac"}
	for i := 0; i < 6; i++ {
		storePlaylist(t, s, fmt.Sprintf("Lists/p%d.m3u", i), append(shared[i:], shared[:i]...)...)
	}
	storePlaylist(t, s, "Album/Album.m3u", "01.flac", "02.flac")
	if _, err := s.writePlaylistFiles(context.Background(), []playlistFileRead{{
		path: "Lists/big.m3u", size: PlaylistFileMaxBytes, mtimeNs: 3, tooLarge: true,
	}}); err != nil {
		t.Fatal(err)
	}
	answer := func(batchBytes int) string {
		t.Helper()
		page, err := s.playlistFilesPage(context.Background(), "", "", math.MaxInt, batchBytes)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(page)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	one := answer(1 << 30)
	if !strings.Contains(one, `"Path":"Lists/p0.m3u"`) || strings.Contains(one, "Album/Album.m3u") ||
		!strings.Contains(one, `"Path":"Lists/big.m3u"`) || !strings.Contains(one, `"TooLarge":true`) {
		t.Fatalf("the single-batch answer is not the fixture's: %s", one)
	}
	for _, batchBytes := range []int{1, 64, 200, 300} {
		if got := answer(batchBytes); got != one {
			t.Errorf("batches of %d bytes answer\n%s\nwant the single batch's\n%s", batchBytes, got, one)
		}
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

// TestASettleStepBindsEachOfItsPaths runs each of the settle's four steps
// over two paths at once. Every step prepares its statement once and binds
// one path per call, so a step that bound the first path for both would
// count, reset or delete the wrong rows; every other test settles one path
// per step.
func TestASettleStepBindsEachOfItsPaths(t *testing.T) {
	s := newFeedStore(t)
	for _, p := range []string{"a.m3u", "b.m3u", "c.m3u", "d.m3u"} {
		storePlaylist(t, s, p)
	}
	ctx := context.Background()
	settle := func(seen, missing, renamed []string, wantDeleted int64) {
		t.Helper()
		n, err := s.settlePlaylistFiles(ctx, seen, missing, renamed, 2)
		if err != nil || n != wantDeleted {
			t.Fatalf("settlePlaylistFiles(seen %v, missing %v, renamed %v) = %d, %v; want %d deleted",
				seen, missing, renamed, n, err, wantDeleted)
		}
	}
	missingCounts := func() string {
		t.Helper()
		rows, err := s.db.Query(`SELECT path, missing_count FROM playlist_files ORDER BY path`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var p string
			var n int
			if err := rows.Scan(&p, &n); err != nil {
				t.Fatal(err)
			}
			out = append(out, fmt.Sprintf("%s=%d", p, n))
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return strings.Join(out, " ")
	}
	want := func(step, counts string) {
		t.Helper()
		if got := missingCounts(); got != counts {
			t.Fatalf("after %s: %q, want %q", step, got, counts)
		}
	}

	settle(nil, []string{"a.m3u", "b.m3u"}, nil, 0)
	want("a miss each for a and b", "a.m3u=1 b.m3u=1 c.m3u=0 d.m3u=0")
	settle([]string{"a.m3u", "b.m3u"}, []string{"c.m3u", "d.m3u"}, nil, 0)
	want("a and b seen, c and d missed", "a.m3u=0 b.m3u=0 c.m3u=1 d.m3u=1")
	settle(nil, []string{"c.m3u", "d.m3u"}, nil, 2)
	want("c and d at the threshold", "a.m3u=0 b.m3u=0")
	settle(nil, nil, []string{"a.m3u", "b.m3u"}, 2)
	want("a and b renamed", "")
}
