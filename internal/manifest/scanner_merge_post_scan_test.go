package manifest

import (
	"context"
	"path/filepath"
	"testing"
)

// enrichedAtOf reads the enriched_at column directly — the version-stale
// tests care about the COLUMN, not the JSON-spliced Enriched flag, because
// the whole point is which write leg the row took.
func enrichedAtOf(t *testing.T, s *Store, path string) int64 {
	t.Helper()
	var v int64
	if err := s.db.QueryRow(`SELECT enriched_at FROM tracks WHERE path = ?`, path).Scan(&v); err != nil {
		t.Fatalf("read enriched_at for %q: %v", path, err)
	}
	return v
}

// TestReExtractUnchanged_PreservesFingerprintRecordingMBID pins
// MusicBrainzTrackID into mergePostScanFields' set.
//
// The merge set is derived from the writers that mutate tags_json AFTER
// the scanner, and MusicBrainzTrackID has one: the enricher's acoustic
// fallback (internal/enrich/acoustic.go applyAcousticFallback writes
// t.MusicBrainzTrackID = m.RecordingMBID and commits via MarkEnriched).
// It was omitted on the belief that the field is extractor-owned, so on a
// size+mtime-UNCHANGED, version-stale, fingerprint-matched row the merged
// re-extract carried "" where the stored row carried the recording MBID,
// the marshalForStorage compare said "changed", and the row took the
// FULL-UPSERT leg: recording MBID erased, enriched_at zeroed (a fresh
// MB/CAA/Deezer crawl), indexed_at bumped (every paired client re-pulls
// it). The ExtractorVersion 2→3 bump makes that fire library-wide.
//
// The fixture's file carries no MUSICBRAINZ_TRACKID tag, so the only
// possible source for the value after the re-scan is the merge.
func TestReExtractUnchanged_PreservesFingerprintRecordingMBID(t *testing.T) {
	root := t.TempDir()
	store, sc := newScanFixture(t, root)
	ctx := context.Background()

	writeMinimalMP3(t, filepath.Join(root, "song.mp3"), map[string]string{
		"title": "Song", "artist": "Artist", "album": "Album", "year": "1991",
	})
	scanOnce(t, sc, "initial")

	const rel = "song.mp3"
	tr, err := store.GetTrack(ctx, rel)
	if err != nil || tr == nil {
		t.Fatalf("GetTrack after initial scan: err=%v nil=%v", err, tr == nil)
	}
	if tr.MusicBrainzTrackID != "" {
		t.Fatalf("fixture must carry no tag-derived recording MBID, got %q", tr.MusicBrainzTrackID)
	}

	// The acoustic fallback's write, verbatim in shape: a recording MBID
	// recovered from the AUDIO, committed through MarkEnriched.
	const recordingMBID = "11111111-2222-3333-4444-555555555555"
	tr.MusicBrainzTrackID = recordingMBID
	if err := store.MarkEnriched(ctx, tr); err != nil {
		t.Fatalf("MarkEnriched: %v", err)
	}
	indexedBefore := indexedAtOf(t, store, rel)
	if enrichedAtOf(t, store, rel) == 0 {
		t.Fatal("fixture: MarkEnriched must stamp enriched_at")
	}

	// A row written by an older binary: stale stamp, file untouched. The
	// next scan re-extracts it through reExtractUnchanged's diff-guard.
	if _, err := store.db.Exec(`UPDATE tracks SET extractor_version = 0 WHERE path = ?`, rel); err != nil {
		t.Fatalf("munge stale stamp: %v", err)
	}

	scanOnce(t, sc, "reheal")

	got, err := store.GetTrack(ctx, rel)
	if err != nil || got == nil {
		t.Fatalf("GetTrack after reheal scan: err=%v nil=%v", err, got == nil)
	}
	if got.MusicBrainzTrackID != recordingMBID {
		t.Errorf("recording MBID = %q, want %q — the version-stale re-extract erased a post-scan-owned field",
			got.MusicBrainzTrackID, recordingMBID)
	}
	if enrichedAtOf(t, store, rel) == 0 {
		t.Error("enriched_at zeroed — the row took the full-upsert leg instead of the version stamp")
	}
	if after := indexedAtOf(t, store, rel); after != indexedBefore {
		t.Errorf("indexed_at bumped (%d → %d) — an unchanged row must not enter every client's delta",
			indexedBefore, after)
	}
	if st, _ := store.GetTrackStat(ctx, rel); st == nil || st.ExtractorVersion != ExtractorVersion {
		t.Errorf("row must still be re-stamped to the current extractor version, got %+v", st)
	}
}
