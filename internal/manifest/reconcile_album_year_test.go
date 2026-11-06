package manifest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/dupes"
)

// Backlog B756: one physical album has one year. The owner's Albums view
// showed one album as several: in 18 of 1,991 album folders a remaster's
// tracks carried two recording years (The Beatles (Remastered), 1968 and
// 2011), and the app's album identity takes the year. The year pass now
// gives every track of a directory's album the album's dominant recording
// year.

// albumOneYearCase is one row of testdata/album-one-year-cases.json: a
// library's tracks (a path, an album title, and a year, null for absent)
// and the year each one has once the year pass has run, in the tracks'
// order. The iOS app's mirror of the rule reads a byte-identical copy of
// the file, so the two sides vote alike: change it on both sides together.
type albumOneYearCase struct {
	Name   string `json:"name"`
	Tracks []struct {
		Path  string `json:"path"`
		Album string `json:"album"`
		Year  *int   `json:"year"`
	} `json:"tracks"`
	Expected []*int `json:"expected"`
}

// TestAnAlbumTakesOneYear runs the shared table through reconcileYears. Each
// track must end at its expected year, and the pass must return exactly the
// tracks whose year that moves: a track already at its album's year gets no
// write, so no indexed_at bump, no revision and no delta entry.
func TestAnAlbumTakesOneYear(t *testing.T) {
	for _, c := range readAlbumOneYearCases(t) {
		t.Run(c.Name, func(t *testing.T) {
			if len(c.Expected) != len(c.Tracks) {
				t.Fatalf("%d expected years for %d tracks", len(c.Expected), len(c.Tracks))
			}
			in := make([]ReconcileTarget, len(c.Tracks))
			at := make(map[string]int, len(c.Tracks))
			got := make([]*int, len(c.Tracks))
			for i, tr := range c.Tracks {
				if _, twice := at[tr.Path]; twice {
					t.Fatalf("the case names %q twice", tr.Path)
				}
				at[tr.Path] = i
				in[i] = ReconcileTarget{Path: tr.Path, Album: tr.Album, Year: copyIntPtr(tr.Year)}
				got[i] = copyIntPtr(tr.Year)
			}
			returned := map[string]bool{}
			for _, fix := range reconcileYears(in) {
				i, ok := at[fix.Path]
				if !ok || returned[fix.Path] {
					t.Fatalf("the pass returned %q, which is not one of the case's tracks or came twice", fix.Path)
				}
				returned[fix.Path] = true
				got[i] = copyIntPtr(fix.Year)
			}
			for i, tr := range c.Tracks {
				if !sameIntPtr(got[i], c.Expected[i]) {
					t.Errorf("%s: year %s, want %s", tr.Path, intPtrString(got[i]), intPtrString(c.Expected[i]))
				}
				if moves := !sameIntPtr(tr.Year, c.Expected[i]); returned[tr.Path] != moves {
					t.Errorf("%s: returned %v, want %v: the pass returns exactly the tracks whose year moves",
						tr.Path, returned[tr.Path], moves)
				}
				if !sameIntPtr(in[i].Year, tr.Year) {
					t.Errorf("%s: the pass changed its input to %s", tr.Path, intPtrString(in[i].Year))
				}
			}
		})
	}
}

// readAlbumOneYearCases decodes testdata/album-one-year-cases.json, unknown
// fields refused, and fails the test on an empty table or two cases of one
// name.
func readAlbumOneYearCases(t *testing.T) []albumOneYearCase {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "album-one-year-cases.json"))
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var cases []albumOneYearCase
	if err := dec.Decode(&cases); err != nil {
		t.Fatalf("the table does not decode: %v", err)
	}
	if len(cases) == 0 {
		t.Fatal("the table holds no case")
	}
	names := map[string]bool{}
	for _, c := range cases {
		if names[c.Name] {
			t.Fatalf("two cases are named %q", c.Name)
		}
		names[c.Name] = true
		if len(c.Expected) != len(c.Tracks) {
			t.Fatalf("%s: %d expected years for %d tracks", c.Name, len(c.Expected), len(c.Tracks))
		}
	}
	return cases
}

// sevenAndFive are the dates of the album the owner hit, in track order:
// seven tracks of 2011, then five of 1968.
var sevenAndFive = []string{"2011", "2011", "2011", "2011", "2011", "2011", "2011", "1968", "1968", "1968", "1968", "1968"}

// albumTrackRel is track n's library path in dir.
func albumTrackRel(dir string, n int) string { return fmt.Sprintf("%s/%02d Track.flac", dir, n) }

// writeAlbumTrack writes track n of the album in dir under root, titled
// title ("Track NN" when empty), by the album artist "Band", its DATE date
// (none when empty), with extra's tags besides, and returns its library
// path. A file written again gets an mtime an hour past its last one, so the
// next scan reads it as changed whatever the clock's grain.
func writeAlbumTrack(t *testing.T, root, dir, album string, n int, title, date string, extra map[string]string) string {
	t.Helper()
	rel := albumTrackRel(dir, n)
	if title == "" {
		title = fmt.Sprintf("Track %02d", n)
	}
	tags := bandTags(title, "Band", album, date)
	for k, v := range extra {
		tags[k] = v
	}
	abs := filepath.Join(root, filepath.FromSlash(rel))
	prev, statErr := os.Stat(abs)
	libraryFLAC(t, root, rel, tags)
	if statErr == nil {
		later := prev.ModTime().Add(time.Hour)
		if err := os.Chtimes(abs, later, later); err != nil {
			t.Fatal(err)
		}
	}
	return rel
}

// albumOfDates writes one track per date into dir (writeAlbumTrack's
// defaults) and returns their library paths, in that order.
func albumOfDates(t *testing.T, root, dir, album string, dates []string, extra map[string]string) []string {
	t.Helper()
	rels := make([]string, len(dates))
	for i, date := range dates {
		rels[i] = writeAlbumTrack(t, root, dir, album, i+1, "", date, extra)
	}
	return rels
}

// requireAlbumYear fails the test for each row of rels whose year is not want.
func requireAlbumYear(t *testing.T, store *Store, label string, rels []string, want int) {
	t.Helper()
	for _, rel := range rels {
		if y := readTrack(t, store, rel).Year; y == nil || *y != want {
			t.Errorf("%s: %s year %s, want %d", label, rel, intPtrString(y), want)
		}
	}
}

// indexedAtsOf is each row's indexed_at, for movedRows.
func indexedAtsOf(t *testing.T, store *Store, rels []string) map[string]int64 {
	t.Helper()
	out := make(map[string]int64, len(rels))
	for _, rel := range rels {
		out[rel] = trackIndexedAt(t, store, rel)
	}
	return out
}

// storeYear writes year into each row's tags_json as a bridge before B756
// stored it, with no record of a tag (Track.taggedYear), moving nothing
// else: the stored state the next full scan's tail reads.
func storeYear(t *testing.T, store *Store, rels []string, year int) {
	t.Helper()
	for _, rel := range rels {
		if _, err := store.db.Exec(`UPDATE tracks SET tags_json = json_remove(json_set(tags_json, '$.year', ?), '$.taggedYear') WHERE path = ?`, year, rel); err != nil {
			t.Fatal(err)
		}
	}
}

// subtreeScanOf runs a subtree scan of the library folder rel, as the
// watcher does after an event there.
func subtreeScanOf(t *testing.T, sc *Scanner, root, rel string) {
	t.Helper()
	if _, err := sc.ScanSubtree(context.Background(), filepath.Join(root, filepath.FromSlash(rel))); err != nil {
		t.Fatalf("subtree scan of %s: %v", rel, err)
	}
}

// TestScanner_AnAlbumOfTwoRecordingYearsTakesOneAndKeepsIt: the first scan
// gives the album 2011, and the next scan of the same files rewrites nothing
// and moves no row.
func TestScanner_AnAlbumOfTwoRecordingYearsTakesOneAndKeepsIt(t *testing.T) {
	root := t.TempDir()
	store, sc := newScanFixture(t, root)
	rels := albumOfDates(t, root, "Band/Remastered", "Remastered", sevenAndFive, nil)
	scanOnce(t, sc, "index")
	requireAlbumYear(t, store, "the first scan", rels, 2011)

	before := indexedAtsOf(t, store, rels)
	scanOnce(t, sc, "the rescan")
	requireAlbumYear(t, store, "the rescan", rels, 2011)
	if moved := movedRows(t, store, before); len(moved) != 0 {
		t.Errorf("rows moved by a rescan of the album: %v", moved)
	}
}

// TestScanner_AnAlbumIndexedBeforeTheRuleTakesOneYearAtTheNextFullScan is
// the upgrade. A library indexed before B756 stores the five 1968 tracks as
// tagged; the first full scan's tail gives them 2011, with no
// ExtractorVersion bump and no re-read. A delta from before that scan
// carries exactly the five, at 2011: the seven already at 2011 get no write,
// and the pass leaves enriched_at as it was.
func TestScanner_AnAlbumIndexedBeforeTheRuleTakesOneYearAtTheNextFullScan(t *testing.T) {
	root := t.TempDir()
	store, sc := newScanFixture(t, root)
	ctx := context.Background()
	rels := albumOfDates(t, root, "Band/Remastered", "Remastered", sevenAndFive, nil)
	scanOnce(t, sc, "index")
	storeYear(t, store, rels[7:], 1968)
	if _, err := store.db.Exec(`UPDATE tracks SET enriched_at = 1`); err != nil {
		t.Fatal(err)
	}
	since := syncedThrough(t, store)

	scanOnce(t, sc, "the first scan after the upgrade")

	requireAlbumYear(t, store, "the first scan after the upgrade", rels, 2011)
	delta, err := BuildManifest(ctx, store, []string{root}, since)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for i := range delta.Tracks {
		got[delta.Tracks[i].Path] = intPtrString(delta.Tracks[i].Year)
	}
	want := map[string]string{}
	for _, rel := range rels[7:] {
		want[rel] = "2011"
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the delta holds %v, want exactly the five rewritten tracks at 2011 %v", got, want)
	}
	if len(delta.Deleted) != 0 || delta.DeltaIncomplete {
		t.Errorf("the delta deletes %v (incomplete %v), want nothing", delta.Deleted, delta.DeltaIncomplete)
	}
	for _, rel := range rels {
		if at := enrichedAtOf(t, store, rel); at != 1 {
			t.Errorf("%s: enriched_at = %d, want 1: the year pass re-queued its enrichment", rel, at)
		}
		if v := trackColumn(t, store, rel, "extractor_version"); v != int64(ExtractorVersion) {
			t.Errorf("%s: extractor_version = %d, want %d", rel, v, ExtractorVersion)
		}
	}
}

// TestScanner_AChangedFileTakesItsAlbumsYear: a file the scan reads again
// because it changed is upserted as its tags say (1968), and the tail gives
// it the album's year again.
func TestScanner_AChangedFileTakesItsAlbumsYear(t *testing.T) {
	root := t.TempDir()
	store, sc := newScanFixture(t, root)
	rels := albumOfDates(t, root, "Band/Remastered", "Remastered", sevenAndFive, nil)
	scanOnce(t, sc, "index")

	rel := writeAlbumTrack(t, root, "Band/Remastered", "Remastered", 10, "Track 10 (Remix)", "1968", nil)
	scanOnce(t, sc, "the rescan")

	if got := readTrack(t, store, rel).Title; got != "Track 10 (Remix)" {
		t.Fatalf("the rescan left title %q: the fixture's file was not read again", got)
	}
	requireAlbumYear(t, store, "the rescan", rels, 2011)
}

// TestScanner_ARetagOfSomeFilesIsJudgedOnTheTags: the pass votes on what
// each row's tag said, the year pass's record where it moved a row
// (Track.taggedYear), never on what an earlier scan wrote. With the album
// stored at 2011, three of the seven 2011 files are retagged 1968, so the
// files say 8 × 1968 and 4 × 2011: the scan gives the album 1968, as a fresh
// index of the same files does. Until round 2 the stored rows voted 9 × 2011
// against the three re-read and wrote the three back, for good. An
// ExtractorVersion bump then re-reads every tag and moves nothing.
func TestScanner_ARetagOfSomeFilesIsJudgedOnTheTags(t *testing.T) {
	root := t.TempDir()
	store, sc := newScanFixture(t, root)
	const dir, album = "Band/Remastered", "Remastered"
	rels := albumOfDates(t, root, dir, album, sevenAndFive, nil)
	scanOnce(t, sc, "index")

	for n := 1; n <= 3; n++ {
		writeAlbumTrack(t, root, dir, album, n, "", "1968", nil)
	}
	scanOnce(t, sc, "the retag")
	requireAlbumYear(t, store, "after the retag", rels, 1968)

	fresh, freshScanner := newScanFixture(t, root)
	scanOnce(t, freshScanner, "a fresh index")
	requireAlbumYear(t, fresh, "a fresh index of the same files", rels, 1968)

	before := rewindAll(t, store)
	scanOnce(t, sc, "the bump")
	requireAlbumYear(t, store, "after the bump", rels, 1968)
	if moved := movedRows(t, store, before); len(moved) != 0 {
		t.Errorf("rows the bump moved: %v, want none", moved)
	}
}

// TestScanner_ATagEditThatOutvotesTheStoredYearMovesTheAlbum: the seven 2011
// files retagged 1968 outvote the year all twelve rows are stored at, 2011
// (the five rows the pass moved vote their records of 1968, the seven their
// new tags), and the album moves to 1968; a whole-album retag moves it again.
func TestScanner_ATagEditThatOutvotesTheStoredYearMovesTheAlbum(t *testing.T) {
	root := t.TempDir()
	store, sc := newScanFixture(t, root)
	const dir, album = "Band/Remastered", "Remastered"
	rels := albumOfDates(t, root, dir, album, sevenAndFive, nil)
	scanOnce(t, sc, "index")

	for n := 1; n <= 7; n++ {
		writeAlbumTrack(t, root, dir, album, n, "", "1968", nil)
	}
	scanOnce(t, sc, "the retag of the seven")
	requireAlbumYear(t, store, "after the retag of the seven", rels, 1968)

	for n := 1; n <= len(rels); n++ {
		writeAlbumTrack(t, root, dir, album, n, "", "1999", nil)
	}
	scanOnce(t, sc, "the retag of the whole album")
	requireAlbumYear(t, store, "after the retag of the whole album", rels, 1999)
}

// TestScanner_AVersionStaleReReadOfAnAlbumGivenOneYearOnlyStamps: a bump over
// the rewritten album. An unchanged scan takes the skip gate and a changed
// file is not a version-stale re-read, so the one way in is a stale
// extractor_version with size and mtime kept: each file is read again, and
// the five tagged 1968 read as their records say (mergeYear), so each keeps
// the album's year and its record, as a re-read of a row the pass never
// moved keeps its stored values. Every row is only stamped: its year, its
// record, indexed_at and enriched_at stay, its extractor_version advances,
// and no pass writes a row, through a full scan and a subtree scan alike.
// (Until round 2 the five were held to the scan's tail and judged there,
// the B188 settlement, which is what a re-read whose reading changed still
// takes: TestScanner_ReReadsOfAnAlbumGivenOneYearPastTheHoldLimitSettleInOneScan.)
func TestScanner_AVersionStaleReReadOfAnAlbumGivenOneYearOnlyStamps(t *testing.T) {
	for _, scope := range []string{"a full scan", "a subtree scan"} {
		t.Run(scope, func(t *testing.T) {
			root := t.TempDir()
			store, sc := newScanFixture(t, root)
			rels := albumOfDates(t, root, "Band/Remastered", "Remastered", sevenAndFive, nil)
			scanOnce(t, sc, "index")
			before := rewindAll(t, store)

			var rewritten []string
			beforeApplyReconciledHookForTests = func(paths []string) { rewritten = append(rewritten, paths...) }
			t.Cleanup(func() { beforeApplyReconciledHookForTests = nil })
			if scope == "a full scan" {
				scanOnce(t, sc, "the bump")
			} else {
				subtreeScanOf(t, sc, root, "Band/Remastered")
			}
			beforeApplyReconciledHookForTests = nil

			requireAlbumYear(t, store, "after the bump", rels, 2011)
			if moved := movedRows(t, store, before); len(moved) != 0 {
				t.Errorf("rows the bump moved: %v, want none", moved)
			}
			if len(rewritten) != 0 {
				t.Errorf("a pass after the settlement wrote %v: each of those rows moved twice in one scan", rewritten)
			}
			for _, rel := range rels {
				if at := enrichedAtOf(t, store, rel); at != 1 {
					t.Errorf("%s: enriched_at = %d, want 1: the bump re-queued its enrichment", rel, at)
				}
				if v := trackColumn(t, store, rel, "extractor_version"); v != int64(ExtractorVersion) {
					t.Errorf("%s: extractor_version = %d, want %d", rel, v, ExtractorVersion)
				}
			}
			for _, rel := range rels[7:] {
				requireTaggedYear(t, store, rel, intptr(1968))
			}
		})
	}
}

// TestScanner_ReReadsOfAnAlbumGivenOneYearPastTheHoldLimitSettleInOneScan: a
// bump whose extractor reads a moved row's tag otherwise than its record
// says (the record is set to 1950 here, as an older extractor might have
// read the DATE that now reads 1968). Such a re-read keeps its own reading
// and no record (mergeYear), differs from its row's year, and is held to
// the scan's tail, at most heldLimit of them (maxHeldReconciles, 10,000, in
// production; it counts only the re-reads that differ from their rows in a
// field a pass writes, not the library's rows). Held, it is judged with
// every held re-read in place and written once, whole, as its record moved
// (1950 to 1968; the served year stays 2011). Past the limit it is written
// as its file reads (1968, its enrichment re-queued) and the tail gives it
// the album's year again: it churns once, as every re-read past the limit
// has since B188. Every row ends at 2011 with the new reading recorded, and
// nothing is re-queued: the scan after moves nothing.
func TestScanner_ReReadsOfAnAlbumGivenOneYearPastTheHoldLimitSettleInOneScan(t *testing.T) {
	root := t.TempDir()
	store, sc := newScanFixture(t, root)
	rels := albumOfDates(t, root, "Band/Remastered", "Remastered", sevenAndFive, nil)
	scanOnce(t, sc, "index")
	sc.heldLimit = 2
	for _, rel := range rels[7:] {
		if _, err := store.db.Exec(`UPDATE tracks SET tags_json = json_set(tags_json, '$.taggedYear', json('{"year":1950}')) WHERE path = ?`, rel); err != nil {
			t.Fatal(err)
		}
	}
	before := rewindAll(t, store)

	scanOnce(t, sc, "the bump")

	requireAlbumYear(t, store, "after the bump", rels, 2011)
	minority := map[string]bool{}
	for _, rel := range rels[7:] {
		minority[rel] = true
		requireTaggedYear(t, store, rel, intptr(1968))
	}
	moved := movedRows(t, store, before)
	if len(moved) != len(rels[7:]) {
		t.Errorf("rows the bump moved: %v, want the five whose reading changed", moved)
	}
	for _, rel := range moved {
		if !minority[rel] {
			t.Errorf("%s moved: only a re-read whose reading changed differs from its row", rel)
		}
	}
	for _, rel := range rels {
		if v := trackColumn(t, store, rel, "extractor_version"); v != int64(ExtractorVersion) {
			t.Errorf("%s: extractor_version = %d, want %d: a re-read was dropped", rel, v, ExtractorVersion)
		}
	}
	settled := indexedAtsOf(t, store, rels)
	scanOnce(t, sc, "the scan after the bump")
	if again := movedRows(t, store, settled); len(again) != 0 {
		t.Errorf("rows moved again after the bump: %v", again)
	}
}

// TestScanner_ASubtreeScanOfAChangedFileSplitsItsAlbumUntilTheNextFullScan
// is a named residual, kept (round 2). A changed file takes the ordinary
// upsert, and a subtree scan runs no reconciliation pass, so the watcher's
// scan stores the file's own year until the next full scan
// (ScanIntervalSec, 6 h by default) gives it the album's again, as the
// album-title, album-artist and missing-year fills have always waited.
func TestScanner_ASubtreeScanOfAChangedFileSplitsItsAlbumUntilTheNextFullScan(t *testing.T) {
	root := t.TempDir()
	store, sc := newScanFixture(t, root)
	rels := albumOfDates(t, root, "Band/Remastered", "Remastered", sevenAndFive, nil)
	scanOnce(t, sc, "index")

	rel := writeAlbumTrack(t, root, "Band/Remastered", "Remastered", 10, "Track 10 (Remix)", "1968", nil)
	subtreeScanOf(t, sc, root, "Band/Remastered")
	tr := readTrack(t, store, rel)
	if tr.Title != "Track 10 (Remix)" {
		t.Fatalf("the subtree scan left title %q: the fixture's file was not read again", tr.Title)
	}
	if tr.Year == nil || *tr.Year != 1968 {
		t.Errorf("after the subtree scan %s holds year %s, want its file's 1968 until the next full scan", rel, intPtrString(tr.Year))
	}

	scanOnce(t, sc, "the next full scan")
	requireAlbumYear(t, store, "the next full scan", rels, 2011)
}

// TestScanner_AnAlbumRetaggedOneSubtreeScanAtATimeMovesAtTheNextFullScan is
// what the subtree scan's running no pass costs and spares. A tagger
// rewrites an album's date one file at a time and the watcher's subtree scan
// follows each file: each scan writes the edited file's row and no other, so
// the album is split while the retag runs and the next full scan finds it at
// its new year, with nothing left to move. A vote at each of those scans
// would follow the tags too (each row votes what its tag said), but would
// write each edit back to the old year until the retag passed half, and
// then move every row of the album: rows rewritten mid-retag that the full
// scan's one vote spares.
func TestScanner_AnAlbumRetaggedOneSubtreeScanAtATimeMovesAtTheNextFullScan(t *testing.T) {
	root := t.TempDir()
	store, sc := newScanFixture(t, root)
	const dir, album = "Band/Remastered", "Remastered"
	rels := albumOfDates(t, root, dir, album, sevenAndFive, nil)
	scanOnce(t, sc, "index")

	for n := 1; n <= len(rels); n++ {
		before := indexedAtsOf(t, store, rels)
		writeAlbumTrack(t, root, dir, album, n, "", "1999", nil)
		subtreeScanOf(t, sc, root, dir)
		if moved := movedRows(t, store, before); !reflect.DeepEqual(moved, []string{rels[n-1]}) {
			t.Errorf("the subtree scan after editing track %d moved %v, want its row alone", n, moved)
		}
	}
	requireAlbumYear(t, store, "after the retag", rels, 1999)
	settled := indexedAtsOf(t, store, rels)
	scanOnce(t, sc, "the next full scan")
	requireAlbumYear(t, store, "the next full scan", rels, 1999)
	if moved := movedRows(t, store, settled); len(moved) != 0 {
		t.Errorf("the next full scan moved %v, want nothing", moved)
	}
}

// TestScanner_StraysOfOneFolderThatBorrowDifferentYearsTakeOneInOneScan: two
// tracks of one folder's album whose date tags hold no year carry two
// release ids, whose albums elsewhere are 1990 and 2000. The MBID fill gives
// them 1990 and 2000, and the year pass's second run, after the MBID fill,
// gives the folder's album one year (the tie's larger) in the same scan,
// where until then only the next scan's year pass did. The held settlement
// replays the same table, so a version-stale re-read of the two settles at
// once too.
func TestScanner_StraysOfOneFolderThatBorrowDifferentYearsTakeOneInOneScan(t *testing.T) {
	const releaseA, releaseB = "aaaaaaaa-1111-4111-8111-aaaaaaaaaaaa", "bbbbbbbb-2222-4222-8222-bbbbbbbbbbbb"
	root := t.TempDir()
	store, sc := newScanFixture(t, root)
	albumOfDates(t, root, "Band/Release A", "Release A", []string{"1990", "1990"}, map[string]string{"MUSICBRAINZ_ALBUMID": releaseA})
	albumOfDates(t, root, "Band/Release B", "Release B", []string{"2000", "2000"}, map[string]string{"MUSICBRAINZ_ALBUMID": releaseB})
	strays := []string{
		writeAlbumTrack(t, root, "Band/Strays", "Strays", 1, "", "0000", map[string]string{"MUSICBRAINZ_ALBUMID": releaseA}),
		writeAlbumTrack(t, root, "Band/Strays", "Strays", 2, "", "0000", map[string]string{"MUSICBRAINZ_ALBUMID": releaseB}),
	}
	scanOnce(t, sc, "index")
	requireAlbumYear(t, store, "the first scan", strays, 2000)

	before := rewindAll(t, store)
	scanOnce(t, sc, "the bump")
	requireAlbumYear(t, store, "after the bump", strays, 2000)
	if moved := movedRows(t, store, before); len(moved) != 0 {
		t.Errorf("rows the bump moved: %v, want none", moved)
	}
}

// TestScanner_DiscFoldersOfOneReleaseKeepAYearEach is a named residual. An
// album split into CD1 and CD2 folders takes a year per folder, and the MBID
// pass, which only fills, does not join two folders whose dominant years
// differ, though they share a release id: on the owner's library 1 of 103
// disc-folder albums is this shape.
func TestScanner_DiscFoldersOfOneReleaseKeepAYearEach(t *testing.T) {
	const release = "cccccccc-3333-4333-8333-cccccccccccc"
	root := t.TempDir()
	store, sc := newScanFixture(t, root)
	mbid := map[string]string{"MUSICBRAINZ_ALBUMID": release}
	cd1 := albumOfDates(t, root, "Singer/Remixed/CD1", "Remixed", []string{"2005", "2005", "2006"}, mbid)
	cd2 := albumOfDates(t, root, "Singer/Remixed/CD2", "Remixed", []string{"2006", "2006", "2005"}, mbid)
	scanOnce(t, sc, "index")
	requireAlbumYear(t, store, "CD1", cd1, 2005)
	requireAlbumYear(t, store, "CD2", cd2, 2006)
}

// TestScanner_EditionsInTwoFoldersKeepTheirOwnYears is the shape of the 32
// titles the owner's library holds in two or more folders with different
// years: real editions. Each folder's album takes its own year, and neither
// crosses into the other.
func TestScanner_EditionsInTwoFoldersKeepTheirOwnYears(t *testing.T) {
	root := t.TempDir()
	store, sc := newScanFixture(t, root)
	first := albumOfDates(t, root, "Band/Greatest Hits 1990", "Greatest Hits", []string{"1990", "1990", "1985"}, nil)
	second := albumOfDates(t, root, "Band/Greatest Hits 2004", "Greatest Hits", []string{"2004", "2004", "2001"}, nil)
	scanOnce(t, sc, "index")
	requireAlbumYear(t, store, "the 1990 edition", first, 1990)
	requireAlbumYear(t, store, "the 2004 edition", second, 2004)
}

// TestScanner_AnotherFoldersCopyOfARewrittenTrackIsSuppressedNotLost: the
// duplicate key leaves the directory out (dupes.KeyFor), so once the five
// 1968 tracks take 2011 they share their keys with another folder's copy of
// the album, as two same-year copies always have. Through the real stamping
// pass and the real delta: one copy of each track stays served, the copy the
// stamps now suppress reaches a client syncing from before the scan as a
// deletion, a rewritten track that stays served reaches it as a track, and
// no track leaves the served set.
func TestScanner_AnotherFoldersCopyOfARewrittenTrackIsSuppressedNotLost(t *testing.T) {
	for _, c := range []struct {
		name     string
		copyTags map[string]string // a longer tag makes the copy's files the larger, so they outrank
		copyWins bool
	}{
		{"the rewritten track is served", nil, false},
		{"the other folder's copy is served", map[string]string{"COMMENT": "the same album, ripped again with a longer tag"}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := t.TempDir()
			store, sc := newScanFixture(t, root)
			sc.SetDupePolicy(func() dupes.Policy { return dupes.Policy{Mode: dupes.FilterHighestQuality} })
			ctx := context.Background()
			rels := albumOfDates(t, root, "Band/Remastered", "Remastered", sevenAndFive, nil)
			twelve := make([]string, len(sevenAndFive))
			for i := range twelve {
				twelve[i] = "2011"
			}
			copies := albumOfDates(t, root, "Band/Remastered (Copy)", "Remastered", twelve, c.copyTags)
			scanOnce(t, sc, "index")

			// The library as a bridge before B756 left it: the five at 1968,
			// stamped as that bridge stamped them, each half served.
			storeYear(t, store, rels[7:], 1968)
			if _, err := sc.RestampDuplicates(ctx); err != nil {
				t.Fatal(err)
			}
			for i := 7; i < len(rels); i++ {
				if stampOf(t, store, rels[i]).Suppressed || stampOf(t, store, copies[i]).Suppressed {
					t.Fatalf("fixture: before the upgrade %s and its copy must both be served", rels[i])
				}
			}
			since := syncedThrough(t, store)

			scanOnce(t, sc, "the first scan after the upgrade")

			served, err := store.ListServedTracks(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			byTitle := map[string][]Track{}
			for _, tr := range served {
				byTitle[tr.Title] = append(byTitle[tr.Title], tr)
			}
			for n := 1; n <= len(rels); n++ {
				title := fmt.Sprintf("Track %02d", n)
				got := byTitle[title]
				if len(got) != 1 {
					t.Errorf("%s is served %d times, want once", title, len(got))
					continue
				}
				if y := got[0].Year; y == nil || *y != 2011 {
					t.Errorf("%s is served at year %s, want the album's 2011", title, intPtrString(y))
				}
			}
			paths, err := store.TrackPaths(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(paths) != len(rels)+len(copies) {
				t.Errorf("the store holds %d rows, want all %d: suppression deletes no row", len(paths), len(rels)+len(copies))
			}

			delta, err := BuildManifest(ctx, store, []string{root}, since)
			if err != nil {
				t.Fatal(err)
			}
			var gotTracks []string
			for i := range delta.Tracks {
				gotTracks = append(gotTracks, delta.Tracks[i].Path)
			}
			sort.Strings(gotTracks)
			wantTracks, wantDeleted := rels[7:], copies[7:]
			if c.copyWins {
				wantTracks, wantDeleted = nil, rels[7:]
			}
			if !reflect.DeepEqual(gotTracks, wantTracks) {
				t.Errorf("the delta's tracks are %v, want %v", gotTracks, wantTracks)
			}
			gotDeleted := append([]string(nil), delta.Deleted...)
			sort.Strings(gotDeleted)
			if !reflect.DeepEqual(gotDeleted, wantDeleted) || delta.DeltaIncomplete {
				t.Errorf("the delta deletes %v (incomplete %v), want %v", gotDeleted, delta.DeltaIncomplete, wantDeleted)
			}
		})
	}
}

// TestTheYearPassLeavesTheFavoriteAlbumsAsTheyAre: the store's favorites
// are the app's document, keyed by (album artist, album, year), and the year
// pass writes nothing there. With both halves of the split album hearted, the
// document keeps both tuples, its revision and its epoch: the app re-keys its
// heart to the album's one year and writes the new tuple back at its next
// favorites sync, and until then the 1968 tuple names no album here.
func TestTheYearPassLeavesTheFavoriteAlbumsAsTheyAre(t *testing.T) {
	root := t.TempDir()
	store, sc := newScanFixture(t, root)
	ctx := context.Background()
	rels := albumOfDates(t, root, "Band/Remastered", "Remastered", sevenAndFive, nil)
	scanOnce(t, sc, "index")
	storeYear(t, store, rels[7:], 1968)
	zero := int64(0)
	if _, err := store.SaveFavorites(ctx, "device-token", FavoritesSave{BaseRevision: &zero, Albums: []FavoriteAlbumRow{
		{AlbumArtist: "Band", Album: "Remastered", Year: 1968, FavoritedAt: 1},
		{AlbumArtist: "Band", Album: "Remastered", Year: 2011, FavoritedAt: 2},
	}}); err != nil {
		t.Fatal(err)
	}
	before, err := store.ReadFavorites(ctx)
	if err != nil {
		t.Fatal(err)
	}

	scanOnce(t, sc, "the first scan after the upgrade")

	requireAlbumYear(t, store, "the first scan after the upgrade", rels, 2011)
	after, err := store.ReadFavorites(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Errorf("the year pass changed the favorites: before %+v, after %+v", before, after)
	}
	if len(after.Albums) != 2 {
		t.Errorf("the favorite albums are %+v, want both tuples until the app writes its re-key back", after.Albums)
	}
}
