package manifest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/dupes"
)

// Backlog B756 round 2. A stored year could outvote the tags it replaced
// for good: the vote read the rows as stored, so deleting an album's
// majority left the rest at the majority's year, and a retag across two
// scans was written back. Now the year pass records what a row's tag said
// the first time it moves the row (Track.taggedYear), the vote reads that
// record, and a group's year is a function of its rows' tags alone.

// requireTaggedYear fails the test unless the row at rel carries the year
// pass's record, saying its tag gave want (nil: no year).
func requireTaggedYear(t *testing.T, store *Store, rel string, want *int) {
	t.Helper()
	rec := readTrack(t, store, rel).taggedYear
	if rec == nil {
		t.Errorf("%s carries no record of its tag, want one of %s", rel, intPtrString(want))
		return
	}
	if !sameIntPtr(rec.year, want) {
		t.Errorf("%s records its tag as %s, want %s", rel, intPtrString(rec.year), intPtrString(want))
	}
}

// requireNoTaggedYear fails the test when the row at rel carries a record.
func requireNoTaggedYear(t *testing.T, store *Store, rel string) {
	t.Helper()
	if rec := readTrack(t, store, rel).taggedYear; rec != nil {
		t.Errorf("%s records its tag as %s, want no record", rel, intPtrString(rec.year))
	}
}

// removeLibraryFile deletes the file at the library path rel.
func removeLibraryFile(t *testing.T, root, rel string) {
	t.Helper()
	if err := os.Remove(filepath.Join(root, filepath.FromSlash(rel))); err != nil {
		t.Fatal(err)
	}
}

// TestScanner_AnAlbumWhoseMajorityIsDeletedTakesWhatItsTagsSay is the
// review's first case. Seven files tagged 2011 and five tagged 1968 store
// twelve rows at 2011, the five with a record of their 1968. The user
// deletes the seven: their rows keep voting 2011 through the deletion pass's
// grace (three scans by default), then are reaped, and the next tail votes
// the five rows' records, 1968, as a fresh index of the five files does.
// Until round 2 the five rows agreed at 2011 and stayed so until an
// ExtractorVersion bump.
func TestScanner_AnAlbumWhoseMajorityIsDeletedTakesWhatItsTagsSay(t *testing.T) {
	root := t.TempDir()
	store, sc := newScanFixture(t, root)
	sc.SetDeleteThreshold(config.DefaultDeleteAfterMissingScans)
	rels := albumOfDates(t, root, "Band/Remastered", "Remastered", sevenAndFive, nil)
	scanOnce(t, sc, "index")
	requireAlbumYear(t, store, "the index", rels, 2011)
	for _, rel := range rels[7:] {
		requireTaggedYear(t, store, rel, intptr(1968))
	}

	for _, rel := range rels[:7] {
		removeLibraryFile(t, root, rel)
	}
	for scan := 1; scan < config.DefaultDeleteAfterMissingScans; scan++ {
		scanOnce(t, sc, fmt.Sprintf("scan %d after the deletion", scan))
		requireAlbumYear(t, store, fmt.Sprintf("scan %d after the deletion, inside the grace", scan), rels[7:], 2011)
	}
	scanOnce(t, sc, "the scan that reaps the seven")
	if got, _ := store.GetTrack(context.Background(), rels[0]); got != nil {
		t.Fatalf("%s is still indexed after the grace: the fixture does not reap", rels[0])
	}
	requireAlbumYear(t, store, "once the seven are reaped", rels[7:], 1968)
	for _, rel := range rels[7:] {
		requireNoTaggedYear(t, store, rel)
	}

	fresh, freshScanner := newScanFixture(t, root)
	scanOnce(t, freshScanner, "a fresh index")
	requireAlbumYear(t, fresh, "a fresh index of the five files", rels[7:], 1968)
}

// TestScanner_AnAlbumRetaggedAcrossTwoScansFollowsItsFiles is the review's
// second case. From the seven-and-five album, stored at 2011: track 1 is
// retagged 1968, so the files say six of each, a tie the larger year wins,
// and the scan keeps 2011, as a fresh index does; then tracks 2 to 7, so
// every file says 1968, and the album is 1968. Until round 2 the second
// scan's stored rows voted six to six, the tie went to 2011, and the store
// said 2011 though every file said 1968.
func TestScanner_AnAlbumRetaggedAcrossTwoScansFollowsItsFiles(t *testing.T) {
	root := t.TempDir()
	store, sc := newScanFixture(t, root)
	const dir, album = "Band/Remastered", "Remastered"
	rels := albumOfDates(t, root, dir, album, sevenAndFive, nil)
	scanOnce(t, sc, "index")

	writeAlbumTrack(t, root, dir, album, 1, "", "1968", nil)
	scanOnce(t, sc, "the retag of track 1")
	requireAlbumYear(t, store, "after the retag of track 1", rels, 2011)
	first, firstScanner := newScanFixture(t, root)
	scanOnce(t, firstScanner, "a fresh index after track 1")
	requireAlbumYear(t, first, "a fresh index of six and six", rels, 2011)

	for n := 2; n <= 7; n++ {
		writeAlbumTrack(t, root, dir, album, n, "", "1968", nil)
	}
	scanOnce(t, sc, "the retag of tracks 2 to 7")
	requireAlbumYear(t, store, "after the retag of tracks 2 to 7", rels, 1968)
	for _, rel := range rels {
		requireNoTaggedYear(t, store, rel)
	}
	second, secondScanner := newScanFixture(t, root)
	scanOnce(t, secondScanner, "a fresh index after tracks 2 to 7")
	requireAlbumYear(t, second, "a fresh index of twelve 1968s", rels, 1968)
}

// tableTargets is a case's tracks as the passes read them, each with its
// tag's year and no record: what a fresh index stores before its tail.
func tableTargets(c albumOneYearCase) []ReconcileTarget {
	out := make([]ReconcileTarget, len(c.Tracks))
	for i, tr := range c.Tracks {
		out[i] = ReconcileTarget{Path: tr.Path, Album: tr.Album, Year: copyIntPtr(tr.Year)}
	}
	return out
}

// reconcileInto runs the year pass over state and writes its answer back,
// as a scan's tail does.
func reconcileInto(state []ReconcileTarget) {
	at := make(map[string]int, len(state))
	for i := range state {
		at[state[i].Path] = i
	}
	for _, fix := range reconcileYears(state) {
		takeAlbumYear(&state[at[fix.Path]], fix)
	}
}

// TestAnIncrementalHistoryAgreesWithAFreshIndexOnTheTablesShapes: for every
// shape of the shared table, two histories end where a fresh index of the
// same tags does. In the first every track was tagged 2099 and the scans
// gave the albums that year, then the files were retagged to the case's
// years one scan at a time; in the second each album held three more tracks
// tagged 2099 beside each of its own, which outvoted them, and which a scan
// then deleted. Each row then votes its own tag (votingYear), as the table
// says it should.
func TestAnIncrementalHistoryAgreesWithAFreshIndexOnTheTablesShapes(t *testing.T) {
	for _, c := range readAlbumOneYearCases(t) {
		t.Run(c.Name, func(t *testing.T) {
			fresh := tableTargets(c)
			reconcileInto(fresh)
			requireAgreement := func(label string, state []ReconcileTarget) {
				t.Helper()
				for i := range c.Tracks {
					if !sameIntPtr(state[i].Year, fresh[i].Year) {
						t.Errorf("%s: %s at %s, where a fresh index gives %s", label, c.Tracks[i].Path,
							intPtrString(state[i].Year), intPtrString(fresh[i].Year))
					}
					if !sameIntPtr(votingYear(state[i]), c.Tracks[i].Year) {
						t.Errorf("%s: %s votes %s, but its tag says %s", label, c.Tracks[i].Path,
							intPtrString(votingYear(state[i])), intPtrString(c.Tracks[i].Year))
					}
				}
			}

			retagged := tableTargets(c)
			for i := range retagged {
				retagged[i].Year = intptr(2099)
			}
			reconcileInto(retagged)
			for i, tr := range c.Tracks {
				// The re-read row is written afresh: its tag, and no record.
				retagged[i].Year, retagged[i].taggedYear = copyIntPtr(tr.Year), nil
				reconcileInto(retagged)
			}
			requireAgreement("retagged one file a scan", retagged)

			crowded := tableTargets(c)
			for _, tr := range c.Tracks {
				for k := 0; k < 3; k++ {
					crowded = append(crowded, ReconcileTarget{
						Path:  fmt.Sprintf("%s/extra-%s-%d.flac", trackDir(tr.Path), pathStem(tr.Path), k),
						Album: tr.Album,
						Year:  intptr(2099),
					})
				}
			}
			reconcileInto(crowded)
			deleted := crowded[:len(c.Tracks)]
			reconcileInto(deleted)
			requireAgreement("outvoted, then its outvoters deleted", deleted)
		})
	}
}

// TestANilTagIsRecordedAndRestored: a track whose tag gives no year takes
// its album's, and its record says the tag gave none (a record of nil, told
// apart from no record); once the album's other tracks are gone it goes back
// to no year and the record goes, in memory and through a real scan.
func TestANilTagIsRecordedAndRestored(t *testing.T) {
	t.Run("in memory", func(t *testing.T) {
		state := []ReconcileTarget{
			{Path: "Band/Album/01.flac", Album: "Album", Year: intptr(2011)},
			{Path: "Band/Album/02.flac", Album: "Album"},
		}
		reconcileInto(state)
		if !sameIntPtr(state[1].Year, intptr(2011)) || state[1].taggedYear == nil || state[1].taggedYear.year != nil {
			t.Fatalf("the untagged track holds %s and record %+v, want 2011 and a record of no year",
				intPtrString(state[1].Year), state[1].taggedYear)
		}
		lone := state[1:]
		reconcileInto(lone)
		if lone[0].Year != nil || lone[0].taggedYear != nil {
			t.Errorf("alone, the untagged track holds %s and record %+v, want no year and no record",
				intPtrString(lone[0].Year), lone[0].taggedYear)
		}
	})
	t.Run("through a scan", func(t *testing.T) {
		root := t.TempDir()
		store, sc := newScanFixture(t, root)
		sc.SetDeleteThreshold(1)
		rels := albumOfDates(t, root, "Band/Album", "Album", []string{"2011", ""}, nil)
		scanOnce(t, sc, "index")
		requireAlbumYear(t, store, "the index", rels, 2011)
		requireTaggedYear(t, store, rels[1], nil)
		requireNoTaggedYear(t, store, rels[0])

		removeLibraryFile(t, root, rels[0])
		scanOnce(t, sc, "the scan that reaps the dated track")
		if y := readTrack(t, store, rels[1]).Year; y != nil {
			t.Errorf("the untagged track holds year %s once alone, want none, as its tag says", intPtrString(y))
		}
		requireNoTaggedYear(t, store, rels[1])
	})
}

// TestScanner_AReExtractClearsTheRecord: a changed file is read again and
// written afresh, with its tag and no record. Track 10 retagged 2011, the
// album's year, holds 2011 with no record after the scan; track 9, whose
// file did not change, keeps its record of 1968.
func TestScanner_AReExtractClearsTheRecord(t *testing.T) {
	root := t.TempDir()
	store, sc := newScanFixture(t, root)
	const dir, album = "Band/Remastered", "Remastered"
	rels := albumOfDates(t, root, dir, album, sevenAndFive, nil)
	scanOnce(t, sc, "index")
	requireTaggedYear(t, store, rels[9], intptr(1968))

	writeAlbumTrack(t, root, dir, album, 10, "", "2011", nil)
	scanOnce(t, sc, "the rescan")
	requireAlbumYear(t, store, "the rescan", rels, 2011)
	requireNoTaggedYear(t, store, rels[9])
	requireTaggedYear(t, store, rels[8], intptr(1968))
}

// TestTheTaggedYearIsStoredAndNeverServed: the record is in the row's
// tags_json, under its own key, and nowhere in the manifest a client reads,
// full or delta; json.Marshal of a Track does not see it.
func TestTheTaggedYearIsStoredAndNeverServed(t *testing.T) {
	root := t.TempDir()
	store, sc := newScanFixture(t, root)
	ctx := context.Background()
	rels := albumOfDates(t, root, "Band/Remastered", "Remastered", sevenAndFive, nil)
	scanOnce(t, sc, "index")

	var raw string
	if err := store.db.QueryRow(`SELECT tags_json FROM tracks WHERE path = ?`, rels[7]).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains([]byte(raw), []byte(`"taggedYear":{"year":1968}`)) {
		t.Errorf("the rewritten row's tags_json is %s, want its record of 1968", raw)
	}
	for _, since := range []time.Time{{}, time.Unix(0, 1)} {
		var body bytes.Buffer
		if err := WriteManifest(ctx, &body, store, []string{root}, since); err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(body.Bytes(), []byte(rels[7])) {
			t.Fatalf("the manifest since %v does not list %s: the fixture reads nothing", since, rels[7])
		}
		if bytes.Contains(body.Bytes(), []byte(taggedYearKey)) {
			t.Errorf("the manifest since %v carries the record: %s", since, body.String())
		}
	}
	wire, err := json.Marshal(readTrack(t, store, rels[7]))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(wire, []byte(taggedYearKey)) {
		t.Errorf("a Track marshals its record: %s", wire)
	}
}

// TestTheRecordSurvivesTheEnrichersStamp: the enricher writes back a row it
// read (UnenrichedTracks, MarkEnriched), whole, and the row keeps its record.
func TestTheRecordSurvivesTheEnrichersStamp(t *testing.T) {
	root := t.TempDir()
	store, sc := newScanFixture(t, root)
	ctx := context.Background()
	rels := albumOfDates(t, root, "Band/Remastered", "Remastered", sevenAndFive, nil)
	scanOnce(t, sc, "index")

	batch, err := store.UnenrichedTracks(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	stamped := 0
	for i := range batch {
		if batch[i].Path != rels[7] {
			continue
		}
		batch[i].MusicBrainzAlbumID = "b2c7e9a1-8f3d-4c55-9a7e-2d1f0c3b4a56"
		if err := store.MarkEnriched(ctx, &batch[i]); err != nil {
			t.Fatal(err)
		}
		stamped++
	}
	if stamped != 1 {
		t.Fatalf("the enricher's batch held %s %d times, want once", rels[7], stamped)
	}
	requireTaggedYear(t, store, rels[7], intptr(1968))
	if got := readTrack(t, store, rels[7]).MusicBrainzAlbumID; got != "b2c7e9a1-8f3d-4c55-9a7e-2d1f0c3b4a56" {
		t.Errorf("the enricher's stamp left release id %q", got)
	}
}

// TestTheYearPassNeverWritesTheRecordAlone: a row already at its album's
// year is never returned, so the record never changes by itself, and no
// revision moves for it. A row whose record says what its year already is
// (Year 2011, recorded 2011: nothing the pass writes, but a hand edit can
// leave one) is left as it is.
func TestTheYearPassNeverWritesTheRecordAlone(t *testing.T) {
	root := t.TempDir()
	store, sc := newScanFixture(t, root)
	rels := albumOfDates(t, root, "Band/Agreed", "Agreed", []string{"2011", "2011", "2011"}, nil)
	scanOnce(t, sc, "index")
	if _, err := store.db.Exec(`UPDATE tracks SET tags_json = json_set(tags_json, '$.taggedYear', json('{"year":2011}')) WHERE path = ?`, rels[0]); err != nil {
		t.Fatal(err)
	}
	before := indexedAtsOf(t, store, rels)
	scanOnce(t, sc, "the rescan")
	if moved := movedRows(t, store, before); len(moved) != 0 {
		t.Errorf("rows moved: %v, want none: the record alone changed nothing served", moved)
	}
	requireTaggedYear(t, store, rels[0], intptr(2011))
}

// TestScanner_ACopyKnownMissingNeverWinsItsGroup is the review's third case:
// the duplicates test's copy-wins shape, with copies[7]'s file removed just
// before the first scan after the upgrade. Its row survives the deletion
// pass's grace (missing_count 1 of 3). Rewriting the reachable original from
// 1968 to 2011 gives the two one duplicate key, and the stamping pass must
// not elect the copy whose file is gone, though it is the larger: the
// original stays served, and the copy is suppressed and reaches a delta
// client as a deletion. Until round 2 the missing copy won on its stored
// size and the original was tombstoned, leaving clients a file that is not
// there until the row is reaped, two more full scans later.
func TestScanner_ACopyKnownMissingNeverWinsItsGroup(t *testing.T) {
	root := t.TempDir()
	store, sc := newScanFixture(t, root)
	sc.SetDeleteThreshold(config.DefaultDeleteAfterMissingScans)
	sc.SetDupePolicy(func() dupes.Policy { return dupes.Policy{Mode: dupes.FilterHighestQuality} })
	ctx := context.Background()
	rels := albumOfDates(t, root, "Band/Remastered", "Remastered", sevenAndFive, nil)
	twelve := make([]string, len(sevenAndFive))
	for i := range twelve {
		twelve[i] = "2011"
	}
	copies := albumOfDates(t, root, "Band/Remastered (Copy)", "Remastered", twelve,
		map[string]string{"COMMENT": "the same album, ripped again with a longer tag"})
	scanOnce(t, sc, "index")
	storeYear(t, store, rels[7:], 1968)
	if _, err := sc.RestampDuplicates(ctx); err != nil {
		t.Fatal(err)
	}
	since := syncedThrough(t, store)
	removeLibraryFile(t, root, copies[7])

	scanOnce(t, sc, "the first scan after the upgrade")

	if got := trackColumn(t, store, copies[7], "missing_count"); got != 1 {
		t.Fatalf("%s: missing_count = %d, want 1: the fixture's copy is not inside the grace", copies[7], got)
	}
	if st := stampOf(t, store, rels[7]); st.Suppressed {
		t.Errorf("%s, the reachable original, is suppressed in favour of a copy whose file is gone", rels[7])
	}
	if st := stampOf(t, store, copies[7]); !st.Suppressed {
		t.Errorf("%s, whose file is gone, is served", copies[7])
	}
	served, err := store.ListServedTracks(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tr := range served {
		if tr.Path == copies[7] {
			t.Errorf("the served set holds %s, whose file is gone", tr.Path)
		}
	}
	delta, err := BuildManifest(ctx, store, []string{root}, since)
	if err != nil {
		t.Fatal(err)
	}
	var gotTracks []string
	for i := range delta.Tracks {
		gotTracks = append(gotTracks, delta.Tracks[i].Path)
	}
	if !reflect.DeepEqual(gotTracks, []string{rels[7]}) {
		t.Errorf("the delta's tracks are %v, want the rewritten original alone, %s", gotTracks, rels[7])
	}
	gotDeleted := append([]string(nil), delta.Deleted...)
	sort.Strings(gotDeleted)
	wantDeleted := append([]string{copies[7]}, rels[8:]...)
	sort.Strings(wantDeleted)
	if !reflect.DeepEqual(gotDeleted, wantDeleted) {
		t.Errorf("the delta deletes %v, want %v", gotDeleted, wantDeleted)
	}
}

// TestScanner_ABackslashInANameIsNoFolder is the review's fourth case. On
// Linux and macOS a backslash is a character of a name: "Band/Live\01
// Track.flac" is a file in "Band", and the year pass must not put it in
// "Band/Live"'s album, two physical directories apart. Until round 2 the
// passes split on both separators everywhere, and the file took 1968 from
// the two tracks of "Band/Live". Windows has no such file (no name holds a
// backslash there), so the scan half runs elsewhere.
func TestScanner_ABackslashInANameIsNoFolder(t *testing.T) {
	inMemory := []ReconcileTarget{
		{Path: `Band/Live\01 Track.flac`, Album: "Live", Year: intptr(2011)},
		{Path: "Band/Live/02 Track.flac", Album: "Live", Year: intptr(1968)},
		{Path: "Band/Live/03 Track.flac", Album: "Live", Year: intptr(1968)},
	}
	for _, goos := range []string{"linux", "darwin", "windows"} {
		t.Run(goos, func(t *testing.T) {
			underSeparatorsOf(t, goos)
			moved := map[string]string{}
			for _, fix := range reconcileYears(inMemory) {
				moved[fix.Path] = intPtrString(fix.Year)
			}
			want := map[string]string{}
			if goos == "windows" {
				want[`Band/Live\01 Track.flac`] = "1968" // one folder there
			}
			if !reflect.DeepEqual(moved, want) {
				t.Errorf("the pass moved %v, want %v", moved, want)
			}
		})
	}
	if runtime.GOOS == "windows" {
		return
	}
	root := t.TempDir()
	store, sc := newScanFixture(t, root)
	libraryFLAC(t, root, `Band/Live\01 Track.flac`, bandTags("Track 01", "Band", "Live", "2011"))
	libraryFLAC(t, root, "Band/Live/02 Track.flac", bandTags("Track 02", "Band", "Live", "1968"))
	libraryFLAC(t, root, "Band/Live/03 Track.flac", bandTags("Track 03", "Band", "Live", "1968"))
	scanOnce(t, sc, "index")
	requireAlbumYear(t, store, "the file named with a backslash", []string{`Band/Live\01 Track.flac`}, 2011)
	requireAlbumYear(t, store, "the folder's two tracks", []string{"Band/Live/02 Track.flac", "Band/Live/03 Track.flac"}, 1968)
}

// albumOneYearTableSHA256 is the shared table's digest. The iOS app's copy of
// testdata/album-one-year-cases.json pins the same one, so a change to either
// copy fails here or there until the other follows.
const albumOneYearTableSHA256 = "c1f74e97537468f7d7a0f0b33aa9071797f9b85ae72069469cac433bcef681af"

// TestTheSharedYearTableIsTheMirroredBytes pins the shared table's bytes.
func TestTheSharedYearTableIsTheMirroredBytes(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "album-one-year-cases.json"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.IndexByte(raw, '\r') >= 0 {
		t.Fatal("the table holds a CR byte: it was checked out with CRLF line ends, which .gitattributes forbids")
	}
	sum := sha256.Sum256(raw)
	if got := hex.EncodeToString(sum[:]); got != albumOneYearTableSHA256 {
		t.Errorf("the table's SHA-256 is %s, want %s: change the iOS app's copy and both pins together", got, albumOneYearTableSHA256)
	}
}
