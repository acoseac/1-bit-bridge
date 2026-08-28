package manifest

// Scanner-level SACD expansion tests: discovery, the deletion-pass
// container-seen membership (THE survival control — without the
// SACDVirtualContainer guard the threshold pass reaps every virtual row),
// container removal, re-rip supersession, and the unchanged-skip gate.

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func sacdScanFixture(t *testing.T) (string, *Store, *Scanner) {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "Music"), 0o755); err != nil {
		t.Fatal(err)
	}
	store, sc := newScanFixture(t, root)
	return root, store, sc
}

func sacdTrackPathsUnder(t *testing.T, s *Store, prefix string) []string {
	t.Helper()
	paths, err := s.TrackPathsUnder(context.Background(), prefix)
	if err != nil {
		t.Fatalf("TrackPathsUnder: %v", err)
	}
	return paths
}

func TestScanner_SACDISO_ExpandsToVirtualRows(t *testing.T) {
	root, store, sc := sacdScanFixture(t)
	writeSACDFixture(t, filepath.Join(root, "Music"), "Album.iso",
		twoFixtureTracks(), sacdFixtureOptions{year: 2004})

	scanOnce(t, sc, "initial")

	rows := sacdTrackPathsUnder(t, store, "Music/Album.iso")
	if len(rows) != 2 {
		t.Fatalf("virtual rows: %v", rows)
	}
	mustIndexed(t, store, "Music/Album.iso/st/01.dff", "Music/Album.iso/st/02.dff")
	// The container itself gets NO row.
	if _, err := store.GetTrackStat(context.Background(), "Music/Album.iso"); err == nil {
		st, _ := store.GetTrackStat(context.Background(), "Music/Album.iso")
		if st != nil {
			t.Fatalf("the container must not have a row")
		}
	}
	// Tags rode the expansion.
	tr, err := store.GetTrack(context.Background(), "Music/Album.iso/st/01.dff")
	if err != nil || tr == nil {
		t.Fatalf("GetTrack: %v", err)
	}
	if tr.Title != "Opening" || tr.Compression != "DST" || tr.Codec != "DFF" {
		t.Fatalf("tags: %q %q %q", tr.Title, tr.Compression, tr.Codec)
	}
}

// THE membership control (red-first verified by reverting the
// SACDVirtualContainer guard in the deletion pass): virtual paths never
// appear in any disk walk, so without the container-seen sparing each
// rescan increments missing_count and the threshold reaps every
// virtual row (the fixture's threshold is 1, so the FIRST rescan).
func TestScanner_SACDVirtualRows_SurviveRescansOfAnUnchangedLibrary(t *testing.T) {
	root, store, sc := sacdScanFixture(t)
	writeSACDFixture(t, filepath.Join(root, "Music"), "Album.iso",
		twoFixtureTracks(), sacdFixtureOptions{})

	scanOnce(t, sc, "initial")
	for pass := 1; pass <= 4; pass++ {
		scanOnce(t, sc, "rescan")
		rows := sacdTrackPathsUnder(t, store, "Music/Album.iso")
		if len(rows) != 2 {
			t.Fatalf("virtual rows reaped on rescan %d — deletion-pass membership broken: %v",
				pass, rows)
		}
	}
}

func TestScanner_SACDContainerRemoval_ReapsVirtualRowsAtThreshold(t *testing.T) {
	root, store, sc := sacdScanFixture(t)
	iso := filepath.Join(root, "Music", "Album.iso")
	writeSACDFixture(t, filepath.Join(root, "Music"), "Album.iso",
		twoFixtureTracks(), sacdFixtureOptions{})
	scanOnce(t, sc, "initial")
	if rows := sacdTrackPathsUnder(t, store, "Music/Album.iso"); len(rows) != 2 {
		t.Fatalf("precondition: %v", rows)
	}

	if err := os.Remove(iso); err != nil {
		t.Fatal(err)
	}
	// The fixture's delete threshold is 1; three passes also cover the
	// production default (3 consecutive missing scans).
	for i := 0; i < 3; i++ {
		scanOnce(t, sc, "post-removal")
	}
	if rows := sacdTrackPathsUnder(t, store, "Music/Album.iso"); len(rows) != 0 {
		t.Fatalf("container gone ⇒ virtual rows reap at the threshold: %v", rows)
	}
}

func TestScanner_SACDReRipShrink_RetiresStaleTrailingRowsImmediately(t *testing.T) {
	root, store, sc := sacdScanFixture(t)
	writeSACDFixture(t, filepath.Join(root, "Music"), "Album.iso",
		twoFixtureTracks(), sacdFixtureOptions{})
	scanOnce(t, sc, "initial")
	if rows := sacdTrackPathsUnder(t, store, "Music/Album.iso"); len(rows) != 2 {
		t.Fatalf("precondition: %v", rows)
	}

	// Re-rip with ONE track and a NEWER mtime — the representative-row
	// gate sees the change, the expansion mints st/01 only, and the
	// stale st/02 retires IMMEDIATELY (journaled threshold-1 delete;
	// the container-seen sparing would otherwise keep it forever).
	iso := filepath.Join(root, "Music", "Album.iso")
	if err := os.WriteFile(iso, buildSACDImage(t,
		[]sacdFixtureTrack{{startFrame: 0, duration: 150, title: "Only One"}},
		sacdFixtureOptions{}), 0o644); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(2 * time.Hour)
	if err := os.Chtimes(iso, future, future); err != nil {
		t.Fatal(err)
	}
	scanOnce(t, sc, "re-rip")

	rows := sacdTrackPathsUnder(t, store, "Music/Album.iso")
	if len(rows) != 1 || rows[0] != "Music/Album.iso/st/01.dff" {
		t.Fatalf("shrink must retire the trailing row immediately: %v", rows)
	}
	tr, err := store.GetTrack(context.Background(), "Music/Album.iso/st/01.dff")
	if err != nil || tr == nil || tr.Title != "Only One" {
		t.Fatalf("surviving row refreshes in place: %v %v", tr, err)
	}
}

func TestScanner_NonSACDISO_ContributesNoRows(t *testing.T) {
	root, store, sc := sacdScanFixture(t)
	junk := filepath.Join(root, "Music", "Backup.iso")
	if err := os.WriteFile(junk, make([]byte, 2_000_000), 0o644); err != nil {
		t.Fatal(err)
	}
	seedTrackDirs(t, filepath.Join(root, "Music", "Real"))
	scanOnce(t, sc, "initial")

	if rows := sacdTrackPathsUnder(t, store, "Music/Backup.iso"); len(rows) != 0 {
		t.Fatalf("a data ISO must contribute nothing: %v", rows)
	}
	// The sibling real audio still indexed — the ISO branch never
	// aborts the walk.
	mustIndexed(t, store, "Music/Real/song.flac")
}

func TestScanner_SACDUnchangedRescan_TakesTheSkipGate(t *testing.T) {
	root, store, sc := sacdScanFixture(t)
	writeSACDFixture(t, filepath.Join(root, "Music"), "Album.iso",
		twoFixtureTracks(), sacdFixtureOptions{})
	scanOnce(t, sc, "initial")

	before, err := store.GetTrack(context.Background(), "Music/Album.iso/st/01.dff")
	if err != nil || before == nil {
		t.Fatalf("precondition: %v", err)
	}
	// Simulate the enricher having settled the row, then rescan the
	// UNCHANGED container. The skip-gate must leave the row alone: a
	// re-upsert would wholesale-replace tags_json with the TOC
	// extraction (reverting the title) and zero enriched_at — exactly
	// the delta-sync wave the gate exists to prevent.
	before.Title = "Enriched Title"
	if err := store.MarkEnriched(context.Background(), before); err != nil {
		t.Fatalf("MarkEnriched: %v", err)
	}
	scanOnce(t, sc, "unchanged rescan")
	after, err := store.GetTrack(context.Background(), "Music/Album.iso/st/01.dff")
	if err != nil || after == nil {
		t.Fatalf("post: %v", err)
	}
	if after.Title != "Enriched Title" {
		t.Fatalf("unchanged rescan re-upserted the row (title reverted to %q)", after.Title)
	}
}
