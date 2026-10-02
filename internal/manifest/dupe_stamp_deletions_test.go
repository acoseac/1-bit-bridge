package manifest

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/dupes"
	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
)

// The rows seedDupePair writes: the larger copy is served, the smaller one
// suppressed, and the third track is no copy of anything.
const (
	pairLoser  = "CopyA/Album/01 Song.flac"
	pairWinner = "CopyB/Album/01 Song.flac"
	pairSolo   = "Other/Album/02 Other.flac"
)

// stampedPair seeds seedDupePair's rows into a fresh store and stamps them.
func stampedPair(t *testing.T) (*Store, *Scanner) {
	t.Helper()
	s := openTestStore(t)
	mode := dupes.FilterHighestQuality
	sc := dupeScanner(t, s, &mode)
	seedDupePair(t, s)
	if _, err := sc.RestampDuplicates(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !stampOf(t, s, pairLoser).Suppressed || stampOf(t, s, pairWinner).Suppressed {
		t.Fatal("precondition: the smaller copy suppressed, the larger served")
	}
	return s, sc
}

func stampedDeletionsOf(t *testing.T, s *Store) int64 {
	t.Helper()
	n, err := s.stampedDeletions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// countStampingPasses counts the stamping passes that reach their commit
// step, from now until the test ends.
func countStampingPasses(t *testing.T) *atomic.Int32 {
	t.Helper()
	var n atomic.Int32
	beforeApplyDupeStampsHookForTests = func() { n.Add(1) }
	t.Cleanup(func() { beforeApplyDupeStampsHookForTests = nil })
	return &n
}

// TestEveryDeletionOfAStampedRowIsCounted: every way a row leaves the tracks
// table counts the rows that carried a duplicate stamp (migration v51's
// trigger), and only those, so a stamping pass can tell whether its stamps
// still describe the table (backlog B218). A deletion of a row that is no copy
// of anything counts nothing, which is what keeps it costing no pass.
func TestEveryDeletionOfAStampedRowIsCounted(t *testing.T) {
	for _, tc := range trackDeleters(context.Background()) {
		t.Run(tc.name, func(t *testing.T) {
			s, sc := stampedPair(t)
			want := int64(2)
			if !tc.wholeTable {
				want = 1
				requireDeletionUncounted(t, s, sc, func() error { return tc.del(s, pairSolo) })
			}
			requireDeletionCounted(t, s, sc, func() error { return tc.del(s, pairWinner) }, want)
		})
	}
}

// trackDeleter is one of the Store functions that delete tracks rows: del
// deletes the row at a path, or every row where the function cannot single
// one out (wholeTable: both copies are deleted with it).
type trackDeleter struct {
	name       string
	del        func(s *Store, path string) error
	wholeTable bool
}

// trackDeleters lists every Store function that deletes tracks rows.
func trackDeleters(ctx context.Context) []trackDeleter {
	return []trackDeleter{
		{"DeleteTrack", func(s *Store, p string) error { return s.DeleteTrack(ctx, p) }, false},
		{"DeleteTracksBatch", func(s *Store, p string) error { return s.DeleteTracksBatch(ctx, []string{p}) }, false},
		{"DeleteTracksByPrefix", func(s *Store, p string) error {
			_, err := s.DeleteTracksByPrefix(ctx, strings.SplitN(p, "/", 2)[0])
			return err
		}, false},
		{"IncrementMissingTracksAndDeleteAtThreshold", func(s *Store, p string) error {
			_, err := s.IncrementMissingTracksAndDeleteAtThreshold(ctx, []string{p}, 1)
			return err
		}, false},
		{"ClearMissingCounts", func(s *Store, p string) error {
			// A scan that missed the file counted it first.
			if _, err := s.IncrementMissingTracksAndDeleteAtThreshold(ctx, []string{p}, 3); err != nil {
				return err
			}
			_, err := s.ClearMissingCounts(ctx)
			return err
		}, false},
		{"WipeFilesystemTracks", func(s *Store, _ string) error { return s.WipeFilesystemTracks(ctx) }, true},
		{"WipeAllTracks", func(s *Store, _ string) error { return s.WipeAllTracks(ctx) }, true},
	}
}

// requireDeletionUncounted deletes pairSolo, a row that is no copy of
// anything, and fails unless the deletion counted nothing.
func requireDeletionUncounted(t *testing.T, s *Store, sc *Scanner, del func() error) {
	t.Helper()
	ctx := context.Background()
	if err := del(); err != nil {
		t.Fatal(err)
	}
	if tr, err := s.GetTrack(ctx, pairSolo); err != nil || tr != nil {
		t.Fatalf("precondition: %s still has a row (%v)", pairSolo, err)
	}
	if got := stampedDeletionsOf(t, s); got != 0 {
		t.Errorf("deleting a row that is no copy of anything counted %d", got)
	}
	if sc.stampsBehindDeletions(ctx) {
		t.Error("the stamps read as behind after deleting a row that carried none")
	}
}

// requireDeletionCounted deletes the served copy and fails unless the
// deletion counted want stamped rows, and a stamping pass then covers it.
func requireDeletionCounted(t *testing.T, s *Store, sc *Scanner, del func() error, want int64) {
	t.Helper()
	ctx := context.Background()
	if err := del(); err != nil {
		t.Fatal(err)
	}
	if got := stampedDeletionsOf(t, s); got != want {
		t.Errorf("stamped rows counted deleted = %d, want %d", got, want)
	}
	if !sc.stampsBehindDeletions(ctx) {
		t.Error("the stamps do not read as behind after a stamped row was deleted")
	}
	if _, err := sc.RestampDuplicates(ctx); err != nil {
		t.Fatal(err)
	}
	if sc.stampsBehindDeletions(ctx) {
		t.Error("the stamps still read as behind after a pass that applied")
	}
}

// TestASubtreeScanRestampsAfterAStampedRowIsDeletedOutsideIt: a subtree scan
// that writes and reaps nothing still runs the stamping pass when a row that
// carried a stamp was deleted since the last one, as the console's delete
// deletes its rows before the scan it runs (backlog B218), and still runs none
// when the row deleted carried no stamp.
func TestASubtreeScanRestampsAfterAStampedRowIsDeletedOutsideIt(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name       string
		deleted    string
		wantPasses int32
	}{
		{"the served copy of a duplicate", pairWinner, 1},
		{"a track that is no copy of anything", pairSolo, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, sc := stampedPair(t)
			// An empty folder of the library, so the scan itself writes
			// and reaps nothing.
			empty := filepath.Join(sc.Roots()[0], "Empty")
			if err := os.MkdirAll(empty, 0o755); err != nil {
				t.Fatal(err)
			}
			if _, err := s.IncrementMissingTracksAndDeleteAtThreshold(ctx, []string{tc.deleted}, 1); err != nil {
				t.Fatal(err)
			}
			passes := countStampingPasses(t)
			if _, err := sc.ScanSubtree(ctx, empty); err != nil {
				t.Fatal(err)
			}
			if got := passes.Load(); got != tc.wantPasses {
				t.Errorf("stamping passes = %d, want %d", got, tc.wantPasses)
			}
			if tc.deleted == pairWinner && stampOf(t, s, pairLoser).Suppressed {
				t.Errorf("%s is still suppressed with its served twin deleted", pairLoser)
			}
		})
	}
}

// onDiskPair writes seedDupePair's three files under root and their rows
// under the files' stat, so a scan's skip gate keeps the rows as they are,
// and stamps them with a full scan.
func onDiskPair(t *testing.T, s *Store, root string) *Scanner {
	t.Helper()
	ctx := context.Background()
	mode := dupes.FilterHighestQuality
	sc := NewScanner([]string{root}, s, "")
	sc.SetDupePolicy(func() dupes.Policy { return dupes.Policy{Mode: mode} })
	for _, f := range []struct {
		path, title string
		size, track int
	}{
		{pairLoser, "Song", 900, 1},
		{pairWinner, "Song", 1000, 1},
		{pairSolo, "Other", 500, 2},
	} {
		abs := filepath.Join(root, filepath.FromSlash(f.path))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(strings.Repeat("x", f.size)), 0o644); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(abs)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.UpsertTrack(ctx, &Track{
			Path: f.path, Size: info.Size(), ModTime: info.ModTime(),
			Title: f.title, Artist: "Artist", AlbumArtist: "Artist", Album: "Album",
			TrackNumber: intptr(f.track), DiscNumber: intptr(1), Year: intptr(2020),
			Duration: f64ptr(200), SampleRate: f64ptr(44100), BitsPerSample: intptr(16), Codec: "FLAC",
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := sc.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	if !stampOf(t, s, pairLoser).Suppressed || stampOf(t, s, pairWinner).Suppressed {
		t.Fatal("precondition: the scan stamped the smaller copy suppressed, the larger served")
	}
	return sc
}

// TestAFullScanRestampsBeforeItWalksWhenAStampedRowWasDeleted: a full scan
// that follows the deletion of a row that carried a stamp, where no stamping
// pass ran in between, restamps BEFORE its walk, so the copy of a duplicate
// whose served twin went is served again at the scan's start rather than at
// its end, minutes later on a large library (backlog B218). The deletions are
// the three that a full scan follows: the console's delete of more folders
// than it rescans one by one, a root removal, and `bridge manifest
// clear-missing` while the bridge was stopped, which deletes through another
// process's store; the next scan's process learns of it from the database.
func TestAFullScanRestampsBeforeItWalksWhenAStampedRowWasDeleted(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		delete func(t *testing.T, s *Store, dbPath string)
	}{
		{"the console's delete", func(t *testing.T, s *Store, _ string) {
			mustDelete(t, func() error {
				_, err := s.IncrementMissingTracksAndDeleteAtThreshold(ctx, []string{pairWinner}, 1)
				return err
			})
		}},
		{"a root removal", func(t *testing.T, s *Store, _ string) {
			mustDelete(t, func() error { _, err := s.DeleteTracksByPrefix(ctx, "CopyB"); return err })
		}},
		{"clear-missing while the bridge was stopped", clearMissingFromAnotherStore},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			dbPath := filepath.Join(t.TempDir(), "bridge.db")
			s, err := OpenStore(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close() })
			sc := onDiskPair(t, s, root)

			// The file goes with its row, as the console's delete moves it
			// to the trash, and a new file gives the walk one to extract.
			mustDelete(t, func() error { return os.Remove(filepath.Join(root, filepath.FromSlash(pairWinner))) })
			tc.delete(t, s, dbPath)
			probe := writeProbe(t, root)

			rec := loggingtest.Record(t)
			if scanWithTheLoserSuppressedMidWalk(t, s, sc, probe) {
				t.Errorf("%s was still suppressed while the scan walked: the restamp ran only after the walk", pairLoser)
			}
			if stampOf(t, s, pairLoser).Suppressed {
				t.Errorf("%s is still suppressed after the scan", pairLoser)
			}
			if got := rec.Lines(msgDupeStampingBeforeWalk); len(got) != 1 {
				t.Errorf("the pass before the walk was said %d times, want once: %q", len(got), got)
			}
		})
	}
}

// mustDelete fails the test on del's error.
func mustDelete(t *testing.T, del func() error) {
	t.Helper()
	if err := del(); err != nil {
		t.Fatal(err)
	}
}

// clearMissingFromAnotherStore deletes the served copy as `bridge manifest
// clear-missing` does, through a second store on the same database: after a
// scan that missed its file counted it.
func clearMissingFromAnotherStore(t *testing.T, s *Store, dbPath string) {
	t.Helper()
	ctx := context.Background()
	mustDelete(t, func() error {
		_, err := s.IncrementMissingTracksAndDeleteAtThreshold(ctx, []string{pairWinner}, 3)
		return err
	})
	cli, err := OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	if n, err := cli.ClearMissingCounts(ctx); err != nil || n != 1 {
		t.Fatalf("clear-missing = %d, %v; want the one row", n, err)
	}
}

// writeProbe writes a new file under root for a walk to extract.
func writeProbe(t *testing.T, root string) string {
	t.Helper()
	probe := filepath.Join(root, "Probe", "Album", "03 Probe.flac")
	if err := os.MkdirAll(filepath.Dir(probe), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(probe, []byte("probe"), 0o644); err != nil {
		t.Fatal(err)
	}
	return probe
}

// scanWithTheLoserSuppressedMidWalk runs a full scan and reports whether
// pairLoser was still suppressed when the walk extracted probe.
func scanWithTheLoserSuppressedMidWalk(t *testing.T, s *Store, sc *Scanner, probe string) bool {
	t.Helper()
	var walked, suppressed atomic.Bool
	afterExtractHookForTests = func(abs string) {
		if abs != probe {
			return
		}
		walked.Store(true)
		var sup int
		err := s.db.QueryRow(`SELECT dupe_suppressed FROM tracks WHERE path = ?`, pairLoser).Scan(&sup)
		suppressed.Store(err != nil || sup != 0)
	}
	t.Cleanup(func() { afterExtractHookForTests = nil })
	if _, err := sc.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !walked.Load() {
		t.Fatal("the walk never extracted the probe file")
	}
	return suppressed.Load()
}

// TestAFullScanWithNoStampedRowDeletedStampsOnce: the pass before the walk
// runs only for a deletion the stamps have not seen; a scan of a library that
// lost no stamped row stamps once, in its tail, as it always did.
func TestAFullScanWithNoStampedRowDeletedStampsOnce(t *testing.T) {
	s := openTestStore(t)
	sc := onDiskPair(t, s, t.TempDir())
	passes := countStampingPasses(t)
	rec := loggingtest.Record(t)
	if _, err := sc.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := passes.Load(); got != 1 {
		t.Errorf("stamping passes = %d, want the tail's one", got)
	}
	if got := rec.Lines(msgDupeStampingBeforeWalk); len(got) != 0 {
		t.Errorf("a scan with no stamped row deleted said it stamps before the walk: %q", got)
	}
}

// TestAStampingPassThatDoesNotApplyLeavesTheDeletionUncovered: a pass that
// stands down for a scan started under it (the restamp still stands down
// while a scan is in flight), or whose stamps fail to commit, records nothing,
// so the deletion it saw is still the next pass's to restamp for. Recorded
// whether or not it applied, the copy would stay suppressed with nothing left
// to restamp it until a full scan.
func TestAStampingPassThatDoesNotApplyLeavesTheDeletionUncovered(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		// arrange sets the pass up not to apply, and returns what undoes it.
		arrange func(t *testing.T, s *Store, sc *Scanner) func()
	}{
		{"it stands down for a scan started under it", func(t *testing.T, s *Store, sc *Scanner) func() {
			beforeApplyDupeStampsHookForTests = func() { sc.activeScans.Add(1) }
			return func() {
				beforeApplyDupeStampsHookForTests = nil
				sc.activeScans.Add(-1)
			}
		}},
		{"its stamps fail to commit", func(t *testing.T, s *Store, sc *Scanner) func() {
			abortOn(t, s, `BEFORE UPDATE OF dupe_group_id ON tracks`)
			return func() {}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, sc := stampedPair(t)
			if _, err := s.IncrementMissingTracksAndDeleteAtThreshold(ctx, []string{pairWinner}, 1); err != nil {
				t.Fatal(err)
			}
			undo := tc.arrange(t, s, sc)
			t.Cleanup(func() { beforeApplyDupeStampsHookForTests = nil })
			n, err := sc.RestampDuplicates(ctx)
			undo()
			if n != 0 {
				t.Fatalf("the pass changed %d rows (%v); want it not to apply", n, err)
			}
			if !stampOf(t, s, pairLoser).Suppressed {
				t.Fatal("precondition: the pass wrote nothing")
			}
			if !sc.stampsBehindDeletions(ctx) {
				t.Error("a pass that did not apply recorded the deletion it saw as covered")
			}
		})
	}
}

// TestADeletionDuringAStampingPassIsLeftForTheNext: a stamped row deleted
// after a pass took its snapshot is not one the pass's stamps saw, so the pass
// does not record it as covered and the next one restamps for it. The pass
// reads the count BEFORE its snapshot; read at its commit, it would record
// this deletion and leave the copy suppressed with its twin gone.
func TestADeletionDuringAStampingPassIsLeftForTheNext(t *testing.T) {
	ctx := context.Background()
	s, sc := stampedPair(t)
	beforeApplyDupeStampsHookForTests = func() {
		beforeApplyDupeStampsHookForTests = nil
		if err := s.DeleteTrack(ctx, pairWinner); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { beforeApplyDupeStampsHookForTests = nil })
	if _, err := sc.RestampDuplicates(ctx); err != nil {
		t.Fatal(err)
	}
	if !stampOf(t, s, pairLoser).Suppressed {
		t.Fatal("precondition: the pass stamped from a snapshot that still held the winner")
	}
	if !sc.stampsBehindDeletions(ctx) {
		t.Fatal("the pass recorded a deletion it never saw as covered")
	}
	if _, err := sc.RestampDuplicates(ctx); err != nil {
		t.Fatal(err)
	}
	if stampOf(t, s, pairLoser).Suppressed || sc.stampsBehindDeletions(ctx) {
		t.Error("the next pass did not serve the copy whose twin went, or did not cover the deletion")
	}
}

// TestMigration51CountsFromZeroOnAnUpgradedLibrary: an upgraded library's
// stamps read as current (the upgrade costs no pass), and the first deletion
// of a stamped row after it counts.
func TestMigration51CountsFromZeroOnAnUpgradedLibrary(t *testing.T) {
	ctx := context.Background()
	s, sc := stampedPair(t)
	for _, stmt := range []string{
		`DROP TRIGGER tracks_stamped_row_deleted`,
		`DROP TABLE dupe_stamp_deletions`,
		`PRAGMA user_version = 50`,
	} {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if err := s.migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if v := readUserVersion(t, s.db); v < 51 {
		t.Fatalf("user_version = %d after the ladder, want v51 applied", v)
	}
	if sc.stampsBehindDeletions(ctx) {
		t.Error("an upgraded library's stamps read as behind")
	}
	if err := s.DeleteTrack(ctx, pairWinner); err != nil {
		t.Fatal(err)
	}
	if got := stampedDeletionsOf(t, s); got != 1 {
		t.Errorf("stamped rows counted deleted after the upgrade = %d, want 1", got)
	}
	// The ladder's sql runs again on a store at v51 unharmed (idempotent).
	var v51 string
	for _, m := range migrations {
		if m.version == 51 {
			v51 = m.sql
		}
	}
	if v51 == "" {
		t.Fatal("no migration v51")
	}
	if _, err := s.db.ExecContext(ctx, v51); err != nil {
		t.Fatalf("re-running v51: %v", err)
	}
	if got := stampedDeletionsOf(t, s); got != 1 {
		t.Errorf("re-running v51 reset the count to %d", got)
	}
}
