package manifest

// The clean-empty guard asks the store how many rows a root owns, and a
// UPnP-routed row is owned by no root: the deletion passes never reap one
// (routedPathSet), because it never appears in a walk of the disk. Counted
// anyway, the guard read an empty root beside a routed upstream as a
// dropped mount, logged `suspected clean-empty mount failure` on every scan,
// and the owning-root audit refused every subtree scan below it.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
)

// routedBesideTheRoot are routed rows whose paths share the filesystem's
// namespace, as an upstream configured with an empty path prefix writes them.
var routedBesideTheRoot = []string{
	"Artist/Album/01.flac",
	"Artist/Album/02.flac",
	"Other Artist/Album/01.flac",
}

// TestScanner_AnEmptyRootBesideRoutedRowsIsNotAMountDrop: a single-root
// bridge that relays an upstream and holds nothing on disk is not a dropped
// mount. Before the count left routed rows out, each scan here logged the
// guard line (measured: three lines in three scans, for a full scan and for
// a subtree scan of the root alike), and nothing it spared was ever at
// stake. The routed rows stay either way.
func TestScanner_AnEmptyRootBesideRoutedRowsIsNotAMountDrop(t *testing.T) {
	for _, subtree := range []bool{false, true} {
		t.Run(fmt.Sprintf("subtree=%v", subtree), func(t *testing.T) {
			root := t.TempDir()
			store, sc := newScanFixture(t, root)
			sc.SetDeleteThreshold(3)
			for _, p := range routedBesideTheRoot {
				seedRoutedTrack(t, store, p)
			}
			rec := loggingtest.Record(t)
			for i := 1; i <= 3; i++ {
				scanTheRoot(t, sc, root, subtree)
			}
			if lines := rec.Lines(msgCleanEmpty); len(lines) != 0 {
				t.Fatalf("an empty root beside routed rows was read as a dropped mount:\n%s", strings.Join(lines, "\n"))
			}
			mustIndexed(t, store, routedBesideTheRoot...)
		})
	}
}

// TestScanner_ASubtreeScanBelowAnEmptyRootBesideRoutedRowsProceeds: the
// owning-root audit a subtree scan runs when its subtree is missing asked the
// same count, so on the bridge above every subtree scan of a directory that
// was not there failed with "no library content on disk but 3 tracks in DB".
// It proceeds now, and its bounded pass leaves the routed rows under the
// same prefix alone, as it always did.
func TestScanner_ASubtreeScanBelowAnEmptyRootBesideRoutedRowsProceeds(t *testing.T) {
	root := t.TempDir()
	store, sc := newScanFixture(t, root)
	sc.SetDeleteThreshold(1)
	for _, p := range routedBesideTheRoot {
		seedRoutedTrack(t, store, p)
	}
	for i := 1; i <= 3; i++ {
		if _, err := sc.ScanSubtree(context.Background(), filepath.Join(root, "Artist")); err != nil {
			t.Fatalf("subtree scan %d below an empty root beside routed rows: %v", i, err)
		}
	}
	mustIndexed(t, store, routedBesideTheRoot...)
}

// TestScanner_TheCleanEmptyGuardCountsOnlyTheRootsOwnRows: where the guard
// does fire, the rows it reports are the ones the root's walk owns. Routed
// rows under the same prefix (an empty path prefix in single-root mode, a
// prefix equal to the root's basename in multi-root mode) were counted with
// them: the line said rows_in_db=5 about a root that held 2.
func TestScanner_TheCleanEmptyGuardCountsOnlyTheRootsOwnRows(t *testing.T) {
	for _, multi := range []bool{false, true} {
		t.Run(fmt.Sprintf("multi=%v", multi), func(t *testing.T) {
			f := newIndexedRootIn(t, multi)
			for _, p := range []string{"Routed/Album/01.flac", "Routed/Album/02.flac", "Routed/Album/03.flac"} {
				seedRoutedTrack(t, f.store, f.prefix+p)
			}
			if err := os.RemoveAll(filepath.Join(f.root, "Artist")); err != nil {
				t.Fatal(err)
			}
			rec := loggingtest.Record(t)
			scanOnce(t, f.sc, "the root emptied")
			lines := rec.Lines(msgCleanEmpty)
			if len(lines) != 1 {
				t.Fatalf("%d %q lines, want 1:\n%s", len(lines), msgCleanEmpty, strings.Join(lines, "\n"))
			}
			if want := fmt.Sprintf(" rows_in_db=%d ", len(linkedRootTracks)); !strings.Contains(lines[0]+" ", want) {
				t.Errorf("the line does not count the root's own %d rows alone: %s", len(linkedRootTracks), lines[0])
			}
			f.requireRowsKept(t, "the root emptied beside routed rows")
		})
	}
}
