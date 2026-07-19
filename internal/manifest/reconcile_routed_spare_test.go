package manifest

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// seedRoutedAlbumTrack seeds a UPnP-routed track with caller-controlled
// AlbumArtist/Album/Year (unlike seedRoutedTrack's fixed values) so the
// reconciliation passes have a real disagreement to act on if the routed-row
// exclusion ever regresses.
func seedRoutedAlbumTrack(t *testing.T, store *Store, tr *Track) {
	t.Helper()
	ctx := context.Background()
	tr.Size, tr.ModTime, tr.Title, tr.Artist = 999, time.Unix(100, 0), "x", "x"
	if err := store.UpsertTrack(ctx, tr); err != nil {
		t.Fatalf("UpsertTrack routed %q: %v", tr.Path, err)
	}
	if err := store.UpsertUPnPRouting(ctx, &UPnPRouting{
		SourcePath: tr.Path,
		ServerUDN:  "uuid:test",
		ResURL:     "http://h:8200/x.flac",
		LastSeenAt: time.Unix(100, 0),
	}); err != nil {
		t.Fatalf("UpsertUPnPRouting %q: %v", tr.Path, err)
	}
}

// TestReconciliationPasses_SpareUPnPRoutedRows is the regression guard for the
// wipe-loop class: the five metadata reconciliation passes (AlbumArtist / Year /
// AlbumTitle / YearByMBID / TrackNumber) must NOT rewrite UPnP-routed rows. Reconciling a
// routed row writes exactly the fields walkFieldsEqual diffs, so the next UPnP
// walk sees a mismatch and re-upserts it — resetting enriched_at and re-bumping
// indexed_at forever. A routed album carrying the same disagreement as a
// filesystem album must be left untouched while the FS album is still unified.
func TestReconciliationPasses_SpareUPnPRoutedRows(t *testing.T) {
	yp := func(n int) *int { return &n }
	ctx := context.Background()
	dir := t.TempDir()
	store, err := OpenStore(filepath.Join(dir, "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	albumArtistOf := func(path string) string {
		t.Helper()
		tr, err := store.GetTrack(ctx, path)
		if err != nil || tr == nil {
			t.Fatalf("GetTrack %q: %v (nil=%v)", path, err, tr == nil)
		}
		return tr.AlbumArtist
	}
	yearOf := func(path string) *int {
		t.Helper()
		tr, err := store.GetTrack(ctx, path)
		if err != nil || tr == nil {
			t.Fatalf("GetTrack %q: %v (nil=%v)", path, err, tr == nil)
		}
		return tr.Year
	}
	trackNumberOf := func(path string) *int {
		t.Helper()
		tr, err := store.GetTrack(ctx, path)
		if err != nil || tr == nil {
			t.Fatalf("GetTrack %q: %v (nil=%v)", path, err, tr == nil)
		}
		return tr.TrackNumber
	}

	// Routed album: AlbumArtist disagreement + a missing year on the minority
	// track — a candidate for both reconcileAlbumArtists and reconcileYears.
	seedRoutedAlbumTrack(t, store, &Track{Path: "2go/Aspiration/Live/01.flac", Album: "Live", AlbumArtist: "Aspiration", Year: yp(2005)})
	seedRoutedAlbumTrack(t, store, &Track{Path: "2go/Aspiration/Live/02.flac", Album: "Live", AlbumArtist: "Aspiration", Year: yp(2005)})
	seedRoutedAlbumTrack(t, store, &Track{Path: "2go/Aspiration/Live/03. Take.flac", Album: "Live", AlbumArtist: "Peter Asplund; Aspiration"})

	// Filesystem album with the SAME disagreement — positive control that the
	// passes still DO their job on non-routed rows (so the test can't pass by
	// the passes simply doing nothing).
	fs := []*Track{
		{Path: "Aspiration/Live/01.flac", Album: "Live", AlbumArtist: "Aspiration", Year: yp(2005)},
		{Path: "Aspiration/Live/02.flac", Album: "Live", AlbumArtist: "Aspiration", Year: yp(2005)},
		{Path: "Aspiration/Live/03. Take.flac", Album: "Live", AlbumArtist: "Peter Asplund; Aspiration"},
	}
	for _, tr := range fs {
		tr.Size, tr.ModTime, tr.Title, tr.Artist = 10, time.Unix(100, 0), "x", "x"
		if err := store.UpsertTrack(ctx, tr); err != nil {
			t.Fatalf("UpsertTrack fs %q: %v", tr.Path, err)
		}
	}
	s := NewScanner([]string{dir}, store, "")

	// Compute the shared routed-exclusion set once, exactly as Scan's tail does.
	routedSet, err := s.routedExclusionSet(ctx)
	if err != nil {
		t.Fatalf("routedExclusionSet: %v", err)
	}

	// AlbumArtist pass: FS minority unified to the dominant; routed untouched.
	if _, err := s.runAlbumArtistReconciliation(ctx, routedSet); err != nil {
		t.Fatalf("runAlbumArtistReconciliation: %v", err)
	}
	if got := albumArtistOf("Aspiration/Live/03. Take.flac"); got != "Aspiration" {
		t.Errorf("fs minority AlbumArtist = %q, want unified %q", got, "Aspiration")
	}
	if got := albumArtistOf("2go/Aspiration/Live/03. Take.flac"); got != "Peter Asplund; Aspiration" {
		t.Errorf("routed AlbumArtist = %q, want SPARED %q", got, "Peter Asplund; Aspiration")
	}

	// Year fill pass: FS missing year filled from siblings; routed stays missing.
	if _, err := s.runYearReconciliation(ctx, routedSet); err != nil {
		t.Fatalf("runYearReconciliation: %v", err)
	}
	if got := yearOf("Aspiration/Live/03. Take.flac"); got == nil || *got != 2005 {
		t.Errorf("fs missing year = %v, want filled 2005", got)
	}
	if got := yearOf("2go/Aspiration/Live/03. Take.flac"); got != nil {
		t.Errorf("routed year = %v, want SPARED (nil)", *got)
	}

	// AlbumTitle + YearByMBID passes use the same routedExclusionSet guard;
	// run them and re-confirm the routed row is still untouched end-to-end.
	if _, err := s.runAlbumTitleReconciliation(ctx, routedSet); err != nil {
		t.Fatalf("runAlbumTitleReconciliation: %v", err)
	}
	if _, err := s.runYearReconciliationByMBID(ctx, routedSet); err != nil {
		t.Fatalf("runYearReconciliationByMBID: %v", err)
	}

	// TrackNumber pass: FS missing track number filled from the filename's
	// leading "NN"; routed row spared (its numbers belong to the upstream DIDL).
	if _, err := s.runTrackNumberReconciliation(ctx, routedSet); err != nil {
		t.Fatalf("runTrackNumberReconciliation: %v", err)
	}
	if got := trackNumberOf("Aspiration/Live/03. Take.flac"); got == nil || *got != 3 {
		t.Errorf("fs missing track number = %v, want filled 3", got)
	}
	if got := trackNumberOf("2go/Aspiration/Live/03. Take.flac"); got != nil {
		t.Errorf("routed track number = %v, want SPARED (nil)", *got)
	}

	if got := albumArtistOf("2go/Aspiration/Live/03. Take.flac"); got != "Peter Asplund; Aspiration" {
		t.Errorf("after all passes, routed AlbumArtist = %q, want SPARED", got)
	}
	if got := yearOf("2go/Aspiration/Live/03. Take.flac"); got != nil {
		t.Errorf("after all passes, routed year = %v, want SPARED (nil)", *got)
	}
}
