package manifest

import (
	"context"
	"testing"
)

// Backlog B756 round 3. A version-stale re-read that differs from its row
// only in the year pass's record (Track.taggedYear) changes nothing a client
// reads: the served year, every wire field and the duplicate stamps stay.
// Until round 3 markStampIfUnchanged compared the stored bytes, record
// included, so such a re-read was written whole: its indexed_at advanced (a
// delta entry to every paired device) and its enrichment was re-queued. Now
// it is stamped and its record written beside the stamp, with no revision.

// seedRecords sets the stored record of each row of rels to rec, as an older
// extractor's reading of the tag would have left it, moving nothing else.
func seedRecords(t *testing.T, store *Store, rels []string, rec string) {
	t.Helper()
	for _, rel := range rels {
		if _, err := store.db.Exec(`UPDATE tracks SET tags_json = json_set(tags_json, '$.taggedYear', json(?)) WHERE path = ?`, rec, rel); err != nil {
			t.Fatal(err)
		}
	}
}

// requireNoRevision fails the test for each row of rels that moved since
// before or whose enrichment was re-queued, and checks each was stamped.
func requireNoRevision(t *testing.T, store *Store, label string, rels []string, before map[string]int64) {
	t.Helper()
	if moved := movedRows(t, store, before); len(moved) != 0 {
		t.Errorf("%s: rows moved: %v, want none: nothing a client reads changed", label, moved)
	}
	for _, rel := range rels {
		if at := enrichedAtOf(t, store, rel); at != 1 {
			t.Errorf("%s: %s: enriched_at = %d, want 1: its enrichment was re-queued", label, rel, at)
		}
		if v := trackColumn(t, store, rel, "extractor_version"); v != int64(ExtractorVersion) {
			t.Errorf("%s: %s: extractor_version = %d, want %d", label, rel, v, ExtractorVersion)
		}
	}
}

// TestScanner_AChangeInTheRecordAloneIsWrittenWithoutARevision is the
// review's case: served year 2011, a recorded tag of 1950 (what an older
// extractor read), and a re-read giving 1968. The re-read is held, the passes
// give it 2011 again, and the record is the one change: written with the
// stamp, through a full scan and a subtree scan alike, with no indexed_at
// advance and no enrichment reset.
func TestScanner_AChangeInTheRecordAloneIsWrittenWithoutARevision(t *testing.T) {
	for _, scope := range []string{"a full scan", "a subtree scan"} {
		t.Run(scope, func(t *testing.T) {
			root := t.TempDir()
			store, sc := newScanFixture(t, root)
			rels := albumOfDates(t, root, "Band/Remastered", "Remastered", sevenAndFive, nil)
			scanOnce(t, sc, "index")
			seedRecords(t, store, rels[7:], `{"year":1950}`)
			before := rewindAll(t, store)

			if scope == "a full scan" {
				scanOnce(t, sc, "the bump")
			} else {
				subtreeScanOf(t, sc, root, "Band/Remastered")
			}

			requireAlbumYear(t, store, "after the bump", rels, 2011)
			requireNoRevision(t, store, "after the bump", rels, before)
			for _, rel := range rels[7:] {
				requireTaggedYear(t, store, rel, intptr(1968))
			}
			settled := indexedAtsOf(t, store, rels)
			scanOnce(t, sc, "the scan after the bump")
			if again := movedRows(t, store, settled); len(again) != 0 {
				t.Errorf("rows moved again after the bump: %v", again)
			}
		})
	}
}

// TestScanner_ABumpThatDropsARecordAloneWritesNoRevision: the same on the
// path a re-read takes when it is not held. A row of 2011 whose record says
// its tag read 1999 (a hand edit can leave one) is re-read as its file says,
// 2011: its year is its row's, so it is not held, and the record goes, which
// is the one change, written with the stamp and no revision.
func TestScanner_ABumpThatDropsARecordAloneWritesNoRevision(t *testing.T) {
	root := t.TempDir()
	store, sc := newScanFixture(t, root)
	rels := albumOfDates(t, root, "Band/Agreed", "Agreed", []string{"2011", "2011", "2011"}, nil)
	scanOnce(t, sc, "index")
	seedRecords(t, store, rels[:1], `{"year":1999}`)
	before := rewindAll(t, store)

	scanOnce(t, sc, "the bump")

	requireAlbumYear(t, store, "after the bump", rels, 2011)
	requireNoRevision(t, store, "after the bump", rels, before)
	requireNoTaggedYear(t, store, rels[0])
}

// TestARecordOnlyStampWritesOnlyTheRowItWasMergedWith: markStampIfUnchanged
// marks a re-read that differs from its row in the record alone, at the
// row's version, and a change anywhere else as no stamp. The stamp leg then
// writes the record and the stamp with no indexed_at advance; over a row
// another writer changed since (the enricher's stamp here), it writes
// neither, and the row keeps what that writer wrote, its stale version
// sending the next scan back to it.
func TestARecordOnlyStampWritesOnlyTheRowItWasMergedWith(t *testing.T) {
	ctx := context.Background()
	seed := func(t *testing.T) (*Store, *Track, *Track) {
		t.Helper()
		s := openTestStore(t)
		row := &Track{Path: "Band/Album/08 Track.flac", Title: "Track 08", Year: intptr(2011),
			taggedYear: &yearRecord{year: intptr(1950)}}
		if err := s.UpsertTrack(ctx, row); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(`UPDATE tracks SET extractor_version = ?`, ExtractorVersion-1); err != nil {
			t.Fatal(err)
		}
		old, err := s.GetTrack(ctx, row.Path)
		if err != nil || old == nil {
			t.Fatalf("GetTrack: %v, %v", old, err)
		}
		fresh := *old
		fresh.taggedYear = &yearRecord{year: intptr(1968)}
		markStampIfUnchanged(&fresh, old)
		if !fresh.versionStampOnly || !fresh.yearRecordOnly || fresh.rowVersion != old.rowVersion {
			t.Fatalf("a re-read that differs in its record alone is marked stamp %v, record-only %v, at version %d (row %d)",
				fresh.versionStampOnly, fresh.yearRecordOnly, fresh.rowVersion, old.rowVersion)
		}
		other := *old
		other.Title = "Track 08 (Remix)"
		other.taggedYear = fresh.taggedYear
		markStampIfUnchanged(&other, old)
		if other.versionStampOnly || other.yearRecordOnly {
			t.Fatalf("a re-read whose title changed too is marked stamp %v, record-only %v, want the upsert",
				other.versionStampOnly, other.yearRecordOnly)
		}
		return s, old, &fresh
	}
	t.Run("the row as it was merged", func(t *testing.T) {
		s, old, fresh := seed(t)
		before := trackIndexedAt(t, s, old.Path)
		if err := s.StampExtractorVersionBatch(ctx, []*Track{fresh}); err != nil {
			t.Fatal(err)
		}
		requireTaggedYear(t, s, old.Path, intptr(1968))
		if after := trackIndexedAt(t, s, old.Path); after != before {
			t.Errorf("indexed_at moved from %d to %d for a record alone", before, after)
		}
		if v := trackColumn(t, s, old.Path, "extractor_version"); v != int64(ExtractorVersion) {
			t.Errorf("extractor_version = %d, want %d: the row was not stamped", v, ExtractorVersion)
		}
	})
	t.Run("a row changed since", func(t *testing.T) {
		s, old, fresh := seed(t)
		enriched := *old
		enriched.MusicBrainzAlbumID = "b2c7e9a1-8f3d-4c55-9a7e-2d1f0c3b4a56"
		if err := s.MarkEnriched(ctx, &enriched); err != nil {
			t.Fatal(err)
		}
		if err := s.StampExtractorVersionBatch(ctx, []*Track{fresh}); err != nil {
			t.Fatal(err)
		}
		requireTaggedYear(t, s, old.Path, intptr(1950))
		if got := readTrack(t, s, old.Path).MusicBrainzAlbumID; got != enriched.MusicBrainzAlbumID {
			t.Errorf("the record's write over a row changed since left release id %q", got)
		}
		if v := trackColumn(t, s, old.Path, "extractor_version"); v != int64(ExtractorVersion-1) {
			t.Errorf("extractor_version = %d, want %d: a row changed since was stamped", v, ExtractorVersion-1)
		}
	})
}

// TestARecordWrittenAloneSurvivesAWriteBackOfAnEarlierRead: the record is
// written without moving indexed_at, so a writer that read the row before
// and writes it back after passes its compare-and-set (B187). The enricher's
// stamp (MarkEnriched) carries the record the row holds now, never the one it
// read, so the bump's record stays and the enrichment lands.
func TestARecordWrittenAloneSurvivesAWriteBackOfAnEarlierRead(t *testing.T) {
	root := t.TempDir()
	store, sc := newScanFixture(t, root)
	ctx := context.Background()
	rels := albumOfDates(t, root, "Band/Remastered", "Remastered", sevenAndFive, nil)
	scanOnce(t, sc, "index")
	seedRecords(t, store, rels[7:8], `{"year":1950}`)
	rewindAll(t, store)
	if _, err := store.db.Exec(`UPDATE tracks SET enriched_at = 0 WHERE path = ?`, rels[7]); err != nil {
		t.Fatal(err)
	}
	batch, err := store.UnenrichedTracks(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	var read *Track
	for i := range batch {
		if batch[i].Path == rels[7] {
			read = &batch[i]
		}
	}
	if read == nil || read.taggedYear == nil || !sameIntPtr(read.taggedYear.year, intptr(1950)) {
		t.Fatalf("the enricher's batch holds %s as %+v, want it with its record of 1950", rels[7], read)
	}

	scanOnce(t, sc, "the bump")
	requireTaggedYear(t, store, rels[7], intptr(1968))

	read.MusicBrainzAlbumID = "b2c7e9a1-8f3d-4c55-9a7e-2d1f0c3b4a56"
	if err := store.MarkEnriched(ctx, read); err != nil {
		t.Fatalf("the enricher's stamp of the row it read before the bump: %v", err)
	}
	requireTaggedYear(t, store, rels[7], intptr(1968))
	if got := readTrack(t, store, rels[7]).MusicBrainzAlbumID; got != "b2c7e9a1-8f3d-4c55-9a7e-2d1f0c3b4a56" {
		t.Errorf("the enricher's stamp left release id %q", got)
	}
}
