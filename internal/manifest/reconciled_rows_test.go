package manifest

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"testing"
	"time"
)

// libraryFLAC writes a tagged FLAC at rel under root, making its folders.
func libraryFLAC(t *testing.T, root, rel string, tags map[string]string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	writeMinimalFLAC(t, p, 44100, 16, tags)
}

// bandTags are the tags of one of the fixture band's tracks. An empty album
// or date leaves that tag out.
func bandTags(title, albumArtist, album, date string) map[string]string {
	m := map[string]string{"TITLE": title, "ARTIST": "Band", "ALBUMARTIST": albumArtist}
	if album != "" {
		m["ALBUM"] = album
	}
	if date != "" {
		m["DATE"] = date
	}
	return m
}

// intPtrString renders an optional integer for a failure message.
func intPtrString(p *int) string {
	if p == nil {
		return "nil"
	}
	return strconv.Itoa(*p)
}

// rewindAll puts every row one extractor version behind, as an
// ExtractorVersion bump finds them, marked enriched, and returns each row's
// indexed_at: what a scan that only stamps them leaves as it is.
func rewindAll(t *testing.T, store *Store) map[string]int64 {
	t.Helper()
	if _, err := store.db.Exec(`UPDATE tracks SET extractor_version = ?, enriched_at = 1`, ExtractorVersion-1); err != nil {
		t.Fatal(err)
	}
	paths, err := store.TrackPaths(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	out := make(map[string]int64, len(paths))
	for _, p := range paths {
		out[p] = trackIndexedAt(t, store, p)
	}
	return out
}

// movedRows are the rows whose indexed_at moved since before, sorted: the
// rows a scan put in every paired device's delta.
func movedRows(t *testing.T, store *Store, before map[string]int64) []string {
	t.Helper()
	var moved []string
	for p, at := range before {
		if trackIndexedAt(t, store, p) != at {
			moved = append(moved, p)
		}
	}
	sort.Strings(moved)
	return moved
}

// TestScanner_ABumpOverReconciledRowsOnlyStampsThem is the measured case of
// backlog B188. A reconciliation pass rewrote a value the file sets: an album
// that is its folder's name, a minority album artist, a date tag that holds
// no year. After an ExtractorVersion bump the scan re-read each such file,
// the diff guard found the file's own value where the row served the
// reconciled one, and took the full upsert: the row served the file's value
// again, its enriched_at was reset (a re-enrichment), and the scan's tail
// reconciled it back (a second indexed_at bump). Every reconciled row went
// into every paired device's delta twice, on every bump.
//
// Now the scan judges such a re-read as its tail's passes would leave it,
// and a row whose file did not change is only stamped.
func TestScanner_ABumpOverReconciledRowsOnlyStampsThem(t *testing.T) {
	root := t.TempDir()
	store, sc := newScanFixture(t, root)
	// The album-title pass: the third track has no album tag, so its album
	// is its folder's name.
	libraryFLAC(t, root, "Band/Some Folder/01 One.flac", bandTags("One", "Band", "Real Album", "2001"))
	libraryFLAC(t, root, "Band/Some Folder/02 Two.flac", bandTags("Two", "Band", "Real Album", "2001"))
	libraryFLAC(t, root, "Band/Some Folder/03 Three.flac", bandTags("Three", "Band", "", "2001"))
	// The album-artist pass: one track credits another album artist.
	libraryFLAC(t, root, "Band/Second/01 A.flac", bandTags("A", "Band", "Second", "2002"))
	libraryFLAC(t, root, "Band/Second/02 B.flac", bandTags("B", "Band", "Second", "2002"))
	libraryFLAC(t, root, "Band/Second/03 C.flac", bandTags("C", "Band feat. Guest", "Second", "2002"))
	// The year pass: one track's date tag is "0000", which extracts as
	// year 0, the "no year" the pass fills.
	libraryFLAC(t, root, "Band/Third/01 X.flac", bandTags("X", "Band", "Third", "1999"))
	libraryFLAC(t, root, "Band/Third/02 Y.flac", bandTags("Y", "Band", "Third", "1999"))
	libraryFLAC(t, root, "Band/Third/03 Z.flac", bandTags("Z", "Band", "Third", "0000"))
	// Nothing to reconcile.
	libraryFLAC(t, root, "Band/Fourth/01 P.flac", bandTags("P", "Band", "Fourth", "2004"))
	libraryFLAC(t, root, "Band/Fourth/02 Q.flac", bandTags("Q", "Band", "Fourth", "2004"))
	scanOnce(t, sc, "index")

	requireReconciled := func(label string) {
		t.Helper()
		if got := readTrack(t, store, "Band/Some Folder/03 Three.flac").Album; got != "Real Album" {
			t.Errorf("%s: album %q, want the album-title pass's %q", label, got, "Real Album")
		}
		if got := readTrack(t, store, "Band/Second/03 C.flac").AlbumArtist; got != "Band" {
			t.Errorf("%s: album artist %q, want the album-artist pass's %q", label, got, "Band")
		}
		if y := readTrack(t, store, "Band/Third/03 Z.flac").Year; y == nil || *y != 1999 {
			t.Errorf("%s: year %s, want the year pass's 1999", label, intPtrString(y))
		}
	}
	requireReconciled("the first scan")

	before := rewindAll(t, store)
	scanOnce(t, sc, "the bump")

	requireReconciled("after the bump")
	if moved := movedRows(t, store, before); len(moved) != 0 {
		t.Errorf("rows moved by a bump over files that did not change: %v (each is a delta row to every paired device)", moved)
	}
	for p := range before {
		if at := enrichedAtOf(t, store, p); at != 1 {
			t.Errorf("%s: enriched_at = %d, want 1: the bump re-queued its enrichment", p, at)
		}
		if v := trackColumn(t, store, p, "extractor_version"); v != int64(ExtractorVersion) {
			t.Errorf("%s: extractor_version = %d, want %d", p, v, ExtractorVersion)
		}
	}
}

// TestScanner_ASubtreeScanAfterACoverTouchKeepsAReconciledAlbumTitle is the
// review's second measured case. Touching a folder's cover changes its
// folder-art key, so the next scan re-reads every track in the folder (B141)
// on the version-stale leg; a subtree scan runs no reconciliation, so the
// track whose album the album-title pass had rewritten served its folder's
// name ("Some Folder") until the next full scan put "Real Album" back, with
// a second bump.
func TestScanner_ASubtreeScanAfterACoverTouchKeepsAReconciledAlbumTitle(t *testing.T) {
	f := newArtFixture(t)
	rels := []string{"Band/Some Folder/01 One.flac", "Band/Some Folder/02 Two.flac", "Band/Some Folder/03 Three.flac"}
	libraryFLAC(t, f.root, rels[0], bandTags("One", "Band", "Real Album", "2001"))
	libraryFLAC(t, f.root, rels[1], bandTags("Two", "Band", "Real Album", "2001"))
	libraryFLAC(t, f.root, rels[2], bandTags("Three", "Band", "", "2001"))
	f.cover(t, "Band/Some Folder/cover.jpg", coverBytes("one"), t0)
	f.scan(t, "index")
	if got := readTrack(t, f.store, rels[2]).Album; got != "Real Album" {
		t.Fatalf("the first scan left album %q, want the album-title pass's %q", got, "Real Album")
	}
	before := f.indexedAts(t, rels...)

	// The same cover, touched: a new mtime, so a new folder-art key.
	if err := os.Chtimes(f.path("Band/Some Folder/cover.jpg"), t0.Add(time.Hour), t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if opened := f.subtreeScan(t, "Band/Some Folder"); opened != len(rels) {
		t.Fatalf("the subtree scan read %d files, want %d: the fixture no longer re-reads the folder", opened, len(rels))
	}
	if got := readTrack(t, f.store, rels[2]).Album; got != "Real Album" {
		t.Errorf("after the subtree scan the album is %q, want %q", got, "Real Album")
	}
	f.requireStill(t, before)

	f.scan(t, "the next full scan")
	if got := readTrack(t, f.store, rels[2]).Album; got != "Real Album" {
		t.Errorf("after the full scan the album is %q, want %q", got, "Real Album")
	}
	f.requireStill(t, before)
}

// TestScanner_ABumpStillAppliesAnExtractorChangeAcrossAWholeAlbum is the
// control the judging must pass: a bump exists to apply an extractor change,
// and a change that reads every track of an album differently leaves no
// outlier for a pass to vote down. Judged against the rows as stored (the old
// reading, two to one), each re-read would be voted back to the old value and
// the change never applied.
func TestScanner_ABumpStillAppliesAnExtractorChangeAcrossAWholeAlbum(t *testing.T) {
	root := t.TempDir()
	store, sc := newScanFixture(t, root)
	rels := []string{"Band/Album/01 One.flac", "Band/Album/02 Two.flac", "Band/Album/03 Three.flac"}
	for i, rel := range rels {
		libraryFLAC(t, root, rel, bandTags([]string{"One", "Two", "Three"}[i], "New Reading", "Album", "2001"))
	}
	scanOnce(t, sc, "index")
	// What an older extractor stored: another reading of every track's tag.
	if _, err := store.db.Exec(`UPDATE tracks SET tags_json = json_set(tags_json, '$.albumArtist', 'Old Reading')`); err != nil {
		t.Fatal(err)
	}
	before := rewindAll(t, store)

	scanOnce(t, sc, "the bump")

	for _, rel := range rels {
		if got := readTrack(t, store, rel).AlbumArtist; got != "New Reading" {
			t.Errorf("%s: album artist %q, want the extractor's new reading %q", rel, got, "New Reading")
		}
	}
	if moved := movedRows(t, store, before); len(moved) != len(rels) {
		t.Errorf("rows moved: %v, want all %d: every device must pull the new reading", moved, len(rels))
	}
}

// TestScanner_ABumpOverAnIDTheEnricherReplacedOnlyStampsIt is the review's
// third case: a post-scan writer that replaces an id the file carries. The
// enricher drops a tag's release id that is not a UUID (the scrub) and
// stores what its search finds; the acoustic fallback stores the
// fingerprint's recording id over one that is not a UUID. The diff guard kept
// the file's value wherever it was not empty, so each such row took the full
// upsert on every bump: served the tag's value again, and was re-enriched.
func TestScanner_ABumpOverAnIDTheEnricherReplacedOnlyStampsIt(t *testing.T) {
	const release, recording = "b2c7e9a1-8f3d-4c55-9a7e-2d1f0c3b4a56", "c3d8f0b2-9a4e-4d66-8b8f-3e2a1d4c5b67"
	for _, c := range []struct {
		name, tag string
		stamp     func(*Track)
		got       func(*Track) string
		want      string
	}{
		{"the release id", "MUSICBRAINZ_ALBUMID",
			func(t *Track) { t.MusicBrainzAlbumID = release },
			func(t *Track) string { return t.MusicBrainzAlbumID }, release},
		{"the recording id", "MUSICBRAINZ_TRACKID",
			func(t *Track) { t.MusicBrainzTrackID = recording },
			func(t *Track) string { return t.MusicBrainzTrackID }, recording},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := t.TempDir()
			store, sc := newScanFixture(t, root)
			tags := bandTags("One", "Band", "Album", "2001")
			tags[c.tag] = "not-an-mbid"
			const rel = "Band/Album/01 One.flac"
			libraryFLAC(t, root, rel, tags)
			scanOnce(t, sc, "index")

			// The enricher's pass, as it writes the row.
			tr := readTrack(t, store, rel)
			c.stamp(tr)
			if err := store.MarkEnriched(context.Background(), tr); err != nil {
				t.Fatal(err)
			}
			before := rewindAll(t, store)

			scanOnce(t, sc, "the bump")

			if got := c.got(readTrack(t, store, rel)); got != c.want {
				t.Errorf("%s %q after the bump, want the enricher's %q", c.name, got, c.want)
			}
			if moved := movedRows(t, store, before); len(moved) != 0 {
				t.Errorf("rows moved: %v, want none", moved)
			}
			if at := enrichedAtOf(t, store, rel); at != 1 {
				t.Errorf("enriched_at = %d, want 1: the bump re-queued the enrichment", at)
			}
		})
	}
}

// TestScanner_ReReadsPastTheHoldLimitAreWrittenAsBefore: a scan holds at most
// heldLimit re-reads for its tail (maxHeldReconciles in production). Past it
// a re-read is decided as it was before backlog B188: it churns, but it is
// written, and the tail's passes reconcile it back. Nothing is lost.
func TestScanner_ReReadsPastTheHoldLimitAreWrittenAsBefore(t *testing.T) {
	root := t.TempDir()
	store, sc := newScanFixture(t, root)
	sc.heldLimit = 1
	albums := []string{"First", "Second"}
	for _, album := range albums {
		libraryFLAC(t, root, "Band/"+album+"/01 A.flac", bandTags("A", "Band", album, "2001"))
		libraryFLAC(t, root, "Band/"+album+"/02 B.flac", bandTags("B", "Band", album, "2001"))
		libraryFLAC(t, root, "Band/"+album+"/03 C.flac", bandTags("C", "Band feat. Guest", album, "2001"))
	}
	scanOnce(t, sc, "index")
	before := rewindAll(t, store)

	scanOnce(t, sc, "the bump")

	if moved := movedRows(t, store, before); len(moved) != 1 {
		t.Errorf("rows moved: %v, want exactly the one past the hold limit", moved)
	}
	for p := range before {
		if v := trackColumn(t, store, p, "extractor_version"); v != int64(ExtractorVersion) {
			t.Errorf("%s: extractor_version = %d, want %d: a re-read was dropped", p, v, ExtractorVersion)
		}
	}
	for _, album := range albums {
		if got := readTrack(t, store, "Band/"+album+"/03 C.flac").AlbumArtist; got != "Band" {
			t.Errorf("%s: album artist %q, want the reconciled %q", album, got, "Band")
		}
	}
}
