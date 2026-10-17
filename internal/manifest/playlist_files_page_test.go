package manifest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"testing"
	"time"
)

// The paged playlist-file feed (backlog B442): pages that add up to the
// whole feed, a version that moves with every change a page can show, a
// cursor of another version refused, and a page's budget that never makes
// a file tooLarge.

// playlistWalk is every page of one read of the feed.
type playlistWalk struct {
	pages []PlaylistFilePage
	files []PlaylistFile
}

// walkPlaylistPages reads the feed from its first page to its last under
// budget, each page from the cursor the one before gave and at the first
// page's version, failing the test on an error or a page that does not end.
func walkPlaylistPages(t *testing.T, s *Store, budget, batchBytes int) playlistWalk {
	t.Helper()
	var w playlistWalk
	version, after := "", ""
	for {
		page, err := s.playlistFilesPage(context.Background(), version, after, budget, batchBytes)
		if err != nil {
			t.Fatalf("page after %q: %v", after, err)
		}
		w.pages = append(w.pages, page)
		w.files = append(w.files, page.Files...)
		if page.Next == "" {
			return w
		}
		if page.Next <= after || len(w.pages) > 10_000 {
			t.Fatalf("page after %q ends at %q: the walk does not advance", after, page.Next)
		}
		version, after = page.Version, page.Next
	}
}

// pageCostOf is what f costs a page, by the budget's documented model.
func pageCostOf(f PlaylistFile) int {
	cost := len(f.Path) + len(f.Name) + playlistFileOverhead
	for _, e := range f.Members {
		cost += len(e) + 4
	}
	for _, e := range f.Unresolved {
		cost += len(e) + 4
	}
	return cost
}

// filesJSON is files as the test compares them.
func filesJSON(t *testing.T, files []PlaylistFile) string {
	t.Helper()
	raw, err := json.Marshal(files)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// newPagedFeedStore is an index whose path order interleaves what a page
// boundary can fall on: album-folder playlists at the start, in the middle
// and before the last file, an empty playlist, a file past the cap, and
// files from one entry to fifty.
func newPagedFeedStore(t *testing.T) *Store {
	t.Helper()
	s := newFeedStore(t, "A/01.flac", "A/02.flac", "B/x.flac", "B/X.flac", "D/one.flac", "D/one.dsf",
		"Albums/One/01.flac", "Albums/One/02.flac", "Albums/Two/01.flac", "Mid/Album/01.flac")
	stampDuplicates(t, s, "g1", true, "D/one.dsf")
	stampDuplicates(t, s, "g1", false, "D/one.flac")
	storePlaylist(t, s, "Albums/One/One.m3u", "01.flac", "02.flac")
	storePlaylist(t, s, "Albums/Two/Two.m3u", "01.flac")
	storePlaylist(t, s, "Lists/aa-empty.m3u")
	if _, err := s.writePlaylistFiles(context.Background(), []playlistFileRead{{
		path: "Lists/big.m3u", size: PlaylistFileMaxBytes, mtimeNs: 3, tooLarge: true,
	}}); err != nil {
		t.Fatal(err)
	}
	shared := []string{"../A/01.flac", "../A/02.FLAC", "../B/x.FLAC", "../D/one.dsf", "http://example.com/x.mp3",
		"../../../up.flac", "../Missing/m.flac"}
	for i := 0; i < 10; i++ {
		var lines []string
		for j := 0; j <= i; j++ {
			lines = append(lines, shared[(i+j)%len(shared)])
		}
		storePlaylist(t, s, fmt.Sprintf("Lists/p%d.m3u", i), lines...)
	}
	var fifty []string
	for j := 0; j < 50; j++ {
		fifty = append(fifty, shared[j%len(shared)])
	}
	storePlaylist(t, s, "Lists/q-fifty.m3u", fifty...)
	storePlaylist(t, s, "Mid/Album/Album.m3u", "01.flac")
	storePlaylist(t, s, "Mid/mix.m3u", "../A/01.flac")
	storePlaylist(t, s, "Z/Album.m3u", "../Mid/Album/01.flac")
	return s
}

func TestPlaylistFilePagesConcatenateToTheWholeFeed(t *testing.T) {
	s := newPagedFeedStore(t)
	whole := walkPlaylistPages(t, s, math.MaxInt, 1<<30)
	if len(whole.pages) != 1 {
		t.Fatalf("an unbounded read took %d pages", len(whole.pages))
	}
	wantJSON := filesJSON(t, whole.files)
	var paths []string
	for _, f := range whole.files {
		paths = append(paths, f.Path)
		if f.TooLarge != (f.Path == "Lists/big.m3u") {
			t.Errorf("%s: tooLarge %v; only the file past the cap is", f.Path, f.TooLarge)
		}
	}
	if !sort.StringsAreSorted(paths) || strings.Contains(strings.Join(paths, "|"), "Album.m3u|") ||
		len(paths) != 15 {
		t.Fatalf("the whole feed %v: want the 15 listed files in path order, no album-folder playlist", paths)
	}
	for _, budget := range []int{1, 150, 400, 1000, 5000} {
		for _, batchBytes := range []int{1, 64, 1 << 30} {
			w := walkPlaylistPages(t, s, budget, batchBytes)
			label := fmt.Sprintf("budget %d, batches of %d bytes", budget, batchBytes)
			if got := filesJSON(t, w.files); got != wantJSON {
				t.Errorf("%s: the pages add up to\n%s\nwant the whole feed\n%s", label, got, wantJSON)
			}
			for i, page := range w.pages {
				cost := 0
				for _, f := range page.Files {
					cost += pageCostOf(f)
				}
				if cost > budget && len(page.Files) != 1 {
					t.Errorf("%s: page %d holds %d files costing %d", label, i, len(page.Files), cost)
				}
				if page.Version != w.pages[0].Version {
					t.Errorf("%s: page %d at version %q, the first at %q", label, i, page.Version, w.pages[0].Version)
				}
			}
			if budget == 1 && len(w.pages) != len(whole.files) {
				t.Errorf("%s: %d pages, want one per listed file (%d)", label, len(w.pages), len(whole.files))
			}
		}
	}
}

func TestAFileLargerThanAPageHasAPageOfItsOwn(t *testing.T) {
	s := newFeedStore(t, "A/01.flac")
	storePlaylist(t, s, "a.m3u", "A/01.flac")
	var many []string
	for i := 0; i < 40; i++ {
		many = append(many, "A/01.flac")
	}
	storePlaylist(t, s, "b.m3u", many...)
	storePlaylist(t, s, "c.m3u", "A/01.flac")
	const budget = 200 // a.m3u and c.m3u cost 83 each, b.m3u 610
	w := walkPlaylistPages(t, s, budget, playlistFeedBatchBytes)
	var got []string
	for _, page := range w.pages {
		var names []string
		for _, f := range page.Files {
			names = append(names, fmt.Sprintf("%s:%d", f.Path, len(f.Members)))
			if f.TooLarge {
				t.Errorf("%s is tooLarge: a page's budget never makes a file so", f.Path)
			}
		}
		got = append(got, strings.Join(names, ","))
	}
	if want := "a.m3u:1|b.m3u:40|c.m3u:1"; strings.Join(got, "|") != want {
		t.Fatalf("pages %q, want %q: the file past the budget alone on its page, whole", got, want)
	}
}

func TestTheFeedVersionMovesWithEveryChangeAPageCanShow(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		moves bool
		do    func(t *testing.T, s *Store)
	}{
		{"a playlist read again with other lines", true, func(t *testing.T, s *Store) {
			storePlaylist(t, s, "Lists/a.m3u", "../A/02.flac")
		}},
		{"a playlist touched (its mtime)", true, func(t *testing.T, s *Store) {
			writePlaylistRow(t, s, playlistFileRead{path: "Lists/a.m3u", size: 1, mtimeNs: 99, lines: []string{"../A/01.flac"}, folderWhole: true})
		}},
		{"a playlist's folder evidence", true, func(t *testing.T, s *Store) {
			if _, err := s.markPlaylistFolders(ctx, nil, []string{"Lists/a.m3u"}); err != nil {
				t.Fatal(err)
			}
		}},
		{"a playlist removed", true, func(t *testing.T, s *Store) {
			if _, err := s.settlePlaylistFiles(ctx, nil, []string{"Lists/a.m3u"}, nil, 1); err != nil {
				t.Fatal(err)
			}
		}},
		{"a playlist added", true, func(t *testing.T, s *Store) { storePlaylist(t, s, "Lists/new.m3u", "../A/01.flac") }},
		{"a track added", true, func(t *testing.T, s *Store) {
			if err := s.UpsertTrack(ctx, &Track{Path: "A/03.flac", Size: 1, ModTime: time.Unix(1, 0)}); err != nil {
				t.Fatal(err)
			}
		}},
		{"a track removed", true, func(t *testing.T, s *Store) {
			if err := s.DeleteTracksBatch(ctx, []string{"A/02.flac"}); err != nil {
				t.Fatal(err)
			}
		}},
		{"a track's path changed", true, func(t *testing.T, s *Store) {
			execSQL(t, s, `UPDATE tracks SET path = 'A/9.flac' WHERE path = 'A/02.flac'`)
		}},
		{"a track suppressed as a duplicate", true, func(t *testing.T, s *Store) { stampDuplicates(t, s, "g", true, "A/01.flac") }},
		{"a duplicate group renamed", true, func(t *testing.T, s *Store) {
			execSQL(t, s, `UPDATE tracks SET dupe_group_id = 'g2' WHERE path = 'A/02.flac'`)
		}},
		{"the index marked complete", true, func(t *testing.T, s *Store) {
			if err := s.markPlaylistFilesComplete(ctx); err != nil {
				t.Fatal(err)
			}
		}},
		{"the backup epoch replaced (a restore)", true, func(t *testing.T, s *Store) {
			if _, err := ReplaceBackupEpoch(ctx, s.db); err != nil {
				t.Fatal(err)
			}
		}},
		{"a track's tags written again", false, func(t *testing.T, s *Store) {
			if err := s.UpsertTrack(ctx, &Track{Path: "A/01.flac", Size: 2, ModTime: time.Unix(2, 0), Title: "Enriched"}); err != nil {
				t.Fatal(err)
			}
			execSQL(t, s, `UPDATE tracks SET enriched_at = 1 WHERE path = 'A/01.flac'`)
		}},
		{"a playlist missed once, below the threshold", false, func(t *testing.T, s *Store) {
			if _, err := s.settlePlaylistFiles(ctx, nil, []string{"Lists/a.m3u"}, nil, 5); err != nil {
				t.Fatal(err)
			}
		}},
		{"a playlist written again as it was", false, func(t *testing.T, s *Store) {
			storePlaylist(t, s, "Lists/a.m3u", "../A/01.flac")
		}},
		{"a page read", false, func(t *testing.T, s *Store) { walkPlaylistPages(t, s, 1, playlistFeedBatchBytes) }},
	} {
		s := newFeedStore(t, "A/01.flac", "A/02.flac")
		stampDuplicates(t, s, "g1", false, "A/02.flac")
		storePlaylist(t, s, "Lists/a.m3u", "../A/01.flac")
		before := feedVersion(t, s)
		tc.do(t, s)
		if moved := feedVersion(t, s) != before; moved != tc.moves {
			t.Errorf("%s: version moved %v, want %v", tc.name, moved, tc.moves)
		}
	}
}

// writePlaylistRow writes one read as it is.
func writePlaylistRow(t *testing.T, s *Store, r playlistFileRead) {
	t.Helper()
	if _, err := s.writePlaylistFiles(context.Background(), []playlistFileRead{r}); err != nil {
		t.Fatal(err)
	}
}

// execSQL runs one statement against the store's database.
func execSQL(t *testing.T, s *Store, query string) {
	t.Helper()
	if _, err := s.db.Exec(query); err != nil {
		t.Fatal(err)
	}
}

// feedVersion is the feed's version now.
func feedVersion(t *testing.T, s *Store) string {
	t.Helper()
	page, err := s.PlaylistFilesPage(context.Background(), "", "")
	if err != nil {
		t.Fatal(err)
	}
	return page.Version
}

func TestAPageAskedForAtAnotherVersionIsRefused(t *testing.T) {
	s := newFeedStore(t, "A/01.flac")
	for _, p := range []string{"a.m3u", "b.m3u", "c.m3u"} {
		storePlaylist(t, s, p, "A/01.flac")
	}
	ctx := context.Background()
	first, err := s.playlistFilesPage(ctx, "", "", 1, playlistFeedBatchBytes)
	if err != nil || first.Next != "a.m3u" {
		t.Fatalf("first page %+v, %v", first, err)
	}
	if err := s.UpsertTrack(ctx, &Track{Path: "B/01.flac", Size: 1, ModTime: time.Unix(1, 0)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.playlistFilesPage(ctx, first.Version, first.Next, 1, playlistFeedBatchBytes); !errors.Is(err, ErrPlaylistFilesChanged) {
		t.Fatalf("the second page at the first page's version after a change: %v, want ErrPlaylistFilesChanged", err)
	}
	again := walkPlaylistPages(t, s, 1, playlistFeedBatchBytes)
	if again.pages[0].Version == first.Version || len(again.files) != 3 {
		t.Fatalf("the walk again: version %q (was %q), %d files", again.pages[0].Version, first.Version, len(again.files))
	}
}

func TestTheFeedGenerationTriggersAreInPlace(t *testing.T) {
	// A table rebuild that drops tracks or playlist_files drops its
	// triggers, and the feed's version would stop moving with no error.
	s := newFeedStore(t)
	rows, err := s.db.Query(`SELECT name, tbl_name FROM sqlite_master WHERE type = 'trigger' AND name LIKE 'playlist_feed_%' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var name, table string
		if err := rows.Scan(&name, &table); err != nil {
			t.Fatal(err)
		}
		got = append(got, table+"."+name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := "playlist_files.playlist_feed_file_added playlist_files.playlist_feed_file_changed " +
		"playlist_files.playlist_feed_file_removed tracks.playlist_feed_track_added " +
		"tracks.playlist_feed_track_changed tracks.playlist_feed_track_removed"
	if strings.Join(got, " ") != want {
		t.Fatalf("triggers %v\nwant %s", got, want)
	}
}
