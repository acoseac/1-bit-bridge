package manifest

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// Scan-level coverage for the ExtractorVersion-2 heal + the version-stale
// diff-guard: a bump re-extracts every stale row once, multi-disc albums
// gain their album-root cover (indexed_at advances — iOS pulls them), and
// rows whose merged re-extract is byte-identical take the light
// extractor_version stamp with NO indexed_at / enriched_at / tags_json
// churn — the property that keeps a version bump from turning into a
// full-library delta on every paired client.

// newDiscArtScanFixture mirrors newScanFixture but wires a real artwork
// cache dir (the shared fixture passes "" and disables local art).
func newDiscArtScanFixture(t *testing.T, root string) (*Store, *Scanner) {
	t.Helper()
	s, err := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, NewScanner([]string{root}, s, filepath.Join(t.TempDir(), "artwork"))
}

func trackIndexedAt(t *testing.T, s *Store, rel string) int64 {
	t.Helper()
	var v int64
	if err := s.db.QueryRow("SELECT indexed_at FROM tracks WHERE path = ?", rel).Scan(&v); err != nil {
		t.Fatalf("indexed_at(%s): %v", rel, err)
	}
	return v
}

func trackColumn(t *testing.T, s *Store, rel, column string) int64 {
	t.Helper()
	var v int64
	if err := s.db.QueryRow("SELECT "+column+" FROM tracks WHERE path = ?", rel).Scan(&v); err != nil {
		t.Fatalf("%s(%s): %v", column, rel, err)
	}
	return v
}

// The production heal simulation: rows indexed art-less by an older binary
// (stale stamp, no artworkMBID) gain the album-root cover on the next scan
// — and their indexed_at ADVANCES, because a row that gained art is
// exactly what iOS must pull. A sibling album with a non-disc layout stays
// art-less (the grandparent negative at scan level).
func TestScanner_VersionBumpHealsDiscSubfolderArt(t *testing.T) {
	root := t.TempDir()
	albumDir := filepath.Join(root, "Puccini", "Turandot")
	for _, d := range []string{"Disc 1", "Disc 2"} {
		if err := os.MkdirAll(filepath.Join(albumDir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cover := append(append([]byte{}, minimalJPEG...), 'H', 'E', 'A', 'L')
	if err := os.WriteFile(filepath.Join(albumDir, "cover.jpg"), cover, 0o644); err != nil {
		t.Fatal(err)
	}
	writeMinimalDSF(t, filepath.Join(albumDir, "Disc 1", "a.dsf"), 2822400,
		map[string]string{"title": "A", "artist": "Zubin Mehta", "album": "Puccini: Turandot", "year": "2021"})
	writeMinimalDSF(t, filepath.Join(albumDir, "Disc 2", "b.dsf"), 2822400,
		map[string]string{"title": "B", "artist": "Zubin Mehta", "album": "Puccini: Turandot", "year": "2021"})
	// Grandparent negative: Artist2/cover.jpg + Artist2/Album2/track — a
	// non-disc-named album dir must NOT inherit the artist-level cover.
	album2 := filepath.Join(root, "Artist2", "Album2")
	if err := os.MkdirAll(album2, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "Artist2", "cover.jpg"), minimalJPEG, 0o644); err != nil {
		t.Fatal(err)
	}
	writeMinimalMP3(t, filepath.Join(album2, "c.mp3"),
		map[string]string{"title": "C", "artist": "Someone", "album": "Album2", "year": "1999"})

	store, sc := newDiscArtScanFixture(t, root)
	ctx := context.Background()
	scanOnce(t, sc, "initial")

	const relA = "Puccini/Turandot/Disc 1/a.dsf"
	const relB = "Puccini/Turandot/Disc 2/b.dsf"
	const relC = "Artist2/Album2/c.mp3"
	mustIndexed(t, store, relA, relB, relC)

	// Simulate rows written by an older binary: strip the art, stale stamp.
	if _, err := store.db.Exec(
		`UPDATE tracks SET tags_json = json_remove(tags_json, '$.artworkMBID'), extractor_version = 0`); err != nil {
		t.Fatalf("munge: %v", err)
	}
	beforeA := trackIndexedAt(t, store, relA)

	scanOnce(t, sc, "heal")

	want := expectedLocalMBID(cover)
	for _, rel := range []string{relA, relB} {
		tr, err := store.GetTrack(ctx, rel)
		if err != nil || tr == nil {
			t.Fatalf("GetTrack(%s): err=%v nil=%v", rel, err, tr == nil)
		}
		if tr.ArtworkMBID != want {
			t.Errorf("%s ArtworkMBID = %q, want %q (album-root cover via disc fallback)", rel, tr.ArtworkMBID, want)
		}
	}
	if after := trackIndexedAt(t, store, relA); after <= beforeA {
		t.Errorf("indexed_at did not advance for a row that GAINED art (%d -> %d) — iOS would never pull the heal", beforeA, after)
	}
	c, err := store.GetTrack(ctx, relC)
	if err != nil || c == nil {
		t.Fatalf("GetTrack(%s): err=%v nil=%v", relC, err, c == nil)
	}
	if c.ArtworkMBID != "" {
		t.Errorf("%s ArtworkMBID = %q, want \"\" (non-disc album dir must not inherit the artist-level cover)", relC, c.ArtworkMBID)
	}
}

// The no-full-delta proof, half one: a version-stale row whose content is
// UNCHANGED (no art to gain, tags identical) gets its extractor_version
// stamped WITHOUT indexed_at or enriched_at moving.
func TestScanner_VersionStaleUnchangedRow_StampsWithoutDelta(t *testing.T) {
	root := t.TempDir()
	// Complete tags (title/artist/album/year) so every reconciliation pass
	// stays inert and the diff genuinely measures the stamp leg.
	writeMinimalMP3(t, filepath.Join(root, "song.mp3"),
		map[string]string{"title": "Song", "artist": "A", "album": "B", "year": "1991"})

	store, sc := newDiscArtScanFixture(t, root)
	scanOnce(t, sc, "initial")

	const rel = "song.mp3"
	// Simulate a MarkEnriched-era stamp + an older extractor.
	if _, err := store.db.Exec(
		"UPDATE tracks SET extractor_version = 0, enriched_at = 42 WHERE path = ?", rel); err != nil {
		t.Fatalf("munge: %v", err)
	}
	before := trackIndexedAt(t, store, rel)

	scanOnce(t, sc, "stamp")

	if got := trackColumn(t, store, rel, "extractor_version"); got != int64(ExtractorVersion) {
		t.Errorf("extractor_version = %d, want %d (the stamp leg must advance it)", got, ExtractorVersion)
	}
	if after := trackIndexedAt(t, store, rel); after != before {
		t.Errorf("indexed_at moved (%d -> %d) for an UNCHANGED row — every client would re-pull it", before, after)
	}
	if got := trackColumn(t, store, rel, "enriched_at"); got != 42 {
		t.Errorf("enriched_at = %d, want 42 (the stamp leg must not re-queue enrichment)", got)
	}
	if got := trackColumn(t, store, rel, "missing_count"); got != 0 {
		t.Errorf("missing_count = %d, want 0 (seen-this-scan resilience contract)", got)
	}
}

// The no-full-delta proof, half two: enricher-owned fields survive the
// version-stale re-extract via the merge — no transient grey-tile window,
// no indexed_at bump.
func TestScanner_VersionStaleMergePreservesEnrichment(t *testing.T) {
	root := t.TempDir()
	writeMinimalMP3(t, filepath.Join(root, "song.mp3"),
		map[string]string{"title": "Song", "artist": "A", "album": "B", "year": "1991"})

	store, sc := newDiscArtScanFixture(t, root)
	ctx := context.Background()
	scanOnce(t, sc, "initial")

	const rel = "song.mp3"
	tr, err := store.GetTrack(ctx, rel)
	if err != nil || tr == nil {
		t.Fatalf("GetTrack: err=%v nil=%v", err, tr == nil)
	}
	// Simulate enrichment exactly as the enricher does.
	tr.MusicBrainzAlbumID = "6d541211-c604-4344-a799-11adfea40c9d"
	tr.ArtworkMBID = "6d541211-c604-4344-a799-11adfea40c9d"
	tr.ArtistMBID = "cc2c9c3c-b7bc-4b8b-84d8-4fbd8779e493"
	if err := store.MarkEnriched(ctx, tr); err != nil {
		t.Fatalf("MarkEnriched: %v", err)
	}
	if _, err := store.db.Exec(
		"UPDATE tracks SET extractor_version = 0 WHERE path = ?", rel); err != nil {
		t.Fatalf("munge: %v", err)
	}
	before := trackIndexedAt(t, store, rel)

	scanOnce(t, sc, "stamp")

	got, err := store.GetTrack(ctx, rel)
	if err != nil || got == nil {
		t.Fatalf("GetTrack after stamp: err=%v nil=%v", err, got == nil)
	}
	if got.MusicBrainzAlbumID != tr.MusicBrainzAlbumID ||
		got.ArtworkMBID != tr.ArtworkMBID ||
		got.ArtistMBID != tr.ArtistMBID {
		t.Errorf("enrichment fields lost across the version-stale re-extract: got %+q/%+q/%+q",
			got.MusicBrainzAlbumID, got.ArtworkMBID, got.ArtistMBID)
	}
	if after := trackIndexedAt(t, store, rel); after != before {
		t.Errorf("indexed_at moved (%d -> %d) — the merged-equal row must take the stamp leg", before, after)
	}
	if got := trackColumn(t, store, rel, "extractor_version"); got != int64(ExtractorVersion) {
		t.Errorf("extractor_version = %d, want %d", got, ExtractorVersion)
	}
}
