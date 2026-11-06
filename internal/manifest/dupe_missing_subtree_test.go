package manifest

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/dupes"
)

// Backlog B756 round 3. Since round 2 the duplicate election reads each
// row's missing count (dupes.Row.Missing), and a subtree scan restamped only
// when it wrote or deleted a row. A scan of a deleted copy's folder, inside
// the deletion pass's grace, counts the copy missing and writes nothing, so
// the gone copy stayed elected and the one playable copy stayed suppressed
// until a full scan (ScanIntervalSec, 6 h by default) or another restamp; and
// a copy whose file came back unchanged stayed suppressed as long.

// copyWinsFixture is two folders of one album, every track a duplicate of
// its twin, the copy's files the larger (a longer tag), so each copy wins its
// group while both are there. It returns the original's paths and the
// copy's, after a full scan.
func copyWinsFixture(t *testing.T, root string, store *Store, sc *Scanner) (rels, copies []string) {
	t.Helper()
	sc.SetDeleteThreshold(config.DefaultDeleteAfterMissingScans)
	sc.SetDupePolicy(func() dupes.Policy { return dupes.Policy{Mode: dupes.FilterHighestQuality} })
	twelve := make([]string, len(sevenAndFive))
	for i := range twelve {
		twelve[i] = "2011"
	}
	rels = albumOfDates(t, root, "Band/Remastered", "Remastered", twelve, nil)
	copies = albumOfDates(t, root, "Band/Remastered (Copy)", "Remastered", twelve,
		map[string]string{"COMMENT": "the same album, ripped again with a longer tag"})
	scanOnce(t, sc, "index")
	if !stampOf(t, store, rels[7]).Suppressed || stampOf(t, store, copies[7]).Suppressed {
		t.Fatalf("the fixture's copy does not win its group: %s %+v, %s %+v",
			rels[7], stampOf(t, store, rels[7]), copies[7], stampOf(t, store, copies[7]))
	}
	return rels, copies
}

// requireElected fails the test unless served is its group's served copy and
// suppressed is not.
func requireElected(t *testing.T, store *Store, label, served, suppressed string) {
	t.Helper()
	if stampOf(t, store, served).Suppressed {
		t.Errorf("%s: %s is suppressed, want it served", label, served)
	}
	if !stampOf(t, store, suppressed).Suppressed {
		t.Errorf("%s: %s is served, want it suppressed", label, suppressed)
	}
}

// TestIncrementMissingTracksCountsTheRowsItMakesMissing: the deletion pass
// says how many rows it counted missing for the first time (0 to 1), which
// is when their election input changes, and a row already missing is not
// one of them; a row the threshold reaps is counted in both.
func TestIncrementMissingTracksCountsTheRowsItMakesMissing(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	for _, p := range []string{"A/1.flac", "A/2.flac", "A/3.flac"} {
		if err := s.UpsertTrack(ctx, &Track{Path: p, Size: 1}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.Exec(`UPDATE tracks SET missing_count = 1 WHERE path = 'A/2.flac'`); err != nil {
		t.Fatal(err)
	}
	pass, err := s.IncrementMissingTracks(ctx, []string{"A/1.flac", "A/2.flac", "A/gone.flac"}, 5)
	if err != nil {
		t.Fatal(err)
	}
	if pass != (MissingTracksPass{NewlyMissing: 1}) {
		t.Errorf("the first pass = %+v, want one row newly missing (A/1.flac) and none deleted", pass)
	}
	pass, err = s.IncrementMissingTracks(ctx, []string{"A/1.flac", "A/2.flac"}, 5)
	if err != nil {
		t.Fatal(err)
	}
	if pass != (MissingTracksPass{}) {
		t.Errorf("the second pass = %+v, want nothing newly missing: both rows were missing already", pass)
	}
	pass, err = s.IncrementMissingTracks(ctx, []string{"A/3.flac"}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if pass != (MissingTracksPass{Deleted: 1, NewlyMissing: 1}) {
		t.Errorf("a pass at threshold 1 = %+v, want A/3.flac counted missing and deleted", pass)
	}
	if cleared, err := s.ResetTrackMissingCount(ctx, "A/1.flac"); err != nil || !cleared {
		t.Errorf("resetting a missing row says cleared %v (err %v), want true", cleared, err)
	}
	if cleared, err := s.ResetTrackMissingCount(ctx, "A/1.flac"); err != nil || cleared {
		t.Errorf("resetting a row at 0 says cleared %v (err %v), want false", cleared, err)
	}
}

// TestScanner_ASubtreeScanElectsAgainWhenACopyGoesOrComesBack is the
// review's case, through ScanSubtree: a subtree scan that counts the winning
// copy missing, writing and deleting nothing, elects the copy still there;
// and one whose skip gate finds the copy back, unchanged, elects it again.
func TestScanner_ASubtreeScanElectsAgainWhenACopyGoesOrComesBack(t *testing.T) {
	t.Run("a copy that goes", func(t *testing.T) {
		root := t.TempDir()
		store, sc := newScanFixture(t, root)
		rels, copies := copyWinsFixture(t, root, store, sc)

		removeLibraryFile(t, root, copies[7])
		subtreeScanOf(t, sc, root, "Band/Remastered (Copy)")

		if got := trackColumn(t, store, copies[7], "missing_count"); got != 1 {
			t.Fatalf("%s: missing_count = %d, want 1: the copy's row is not inside the grace", copies[7], got)
		}
		requireElected(t, store, "the subtree scan after the copy went", rels[7], copies[7])
	})
	t.Run("a copy that comes back", func(t *testing.T) {
		root := t.TempDir()
		store, sc := newScanFixture(t, root)
		rels, copies := copyWinsFixture(t, root, store, sc)

		abs := filepath.Join(root, filepath.FromSlash(copies[7]))
		body, err := os.ReadFile(abs)
		if err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(abs)
		if err != nil {
			t.Fatal(err)
		}
		removeLibraryFile(t, root, copies[7])
		scanOnce(t, sc, "the full scan after the copy went")
		requireElected(t, store, "the full scan after the copy went", rels[7], copies[7])

		// The file comes back as it was, as a volume mounted again brings
		// it: the skip gate takes it unchanged and clears its count.
		if err := os.WriteFile(abs, body, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(abs, info.ModTime(), info.ModTime()); err != nil {
			t.Fatal(err)
		}
		subtreeScanOf(t, sc, root, "Band/Remastered (Copy)")

		if got := trackColumn(t, store, copies[7], "missing_count"); got != 0 {
			t.Fatalf("%s: missing_count = %d, want 0: the skip gate did not take the file back", copies[7], got)
		}
		requireElected(t, store, "the subtree scan after the copy came back", copies[7], rels[7])
	})
}
