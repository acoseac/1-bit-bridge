package manifest

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/dupes"
)

// seedFileBackedTrack writes `size` bytes at abs and upserts a row whose
// Size + ModTime MATCH the file, so the scanner's size+mtime fast-skip
// fires and the seeded tags survive the scan. The dupe-key fields are
// identical across calls on purpose — differing only in size makes the
// larger copy the deterministic winner.
func seedFileBackedTrack(t *testing.T, s *Store, rel, abs string, size int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		t.Fatal(err)
	}
	tr := &Track{
		Path: rel, Size: info.Size(), ModTime: info.ModTime().UTC(),
		Title: "Song", Artist: "Artist", AlbumArtist: "Artist", Album: "Album",
		TrackNumber: intptr(1), DiscNumber: intptr(1), Year: intptr(2020),
		Duration: f64ptr(200), SampleRate: f64ptr(44100),
		BitsPerSample: intptr(16), IsDSD: boolPtr(false), Codec: "FLAC",
	}
	if err := s.UpsertTrack(context.Background(), tr); err != nil {
		t.Fatalf("seed %q: %v", rel, err)
	}
}

// TestScanSubtree_RestampsAfterReapingTheServedWinner pins the duplicate
// stamping tail onto ScanSubtree.
//
// RestampDuplicates used to run only in the FULL Scan's success tail,
// while ScanSubtree — the watcher's incremental path — both re-indexes
// rows (its upserts deliberately leave the v31 dupe columns alone, on the
// premise that "the full-scan tail runs in the SAME scan as these
// upserts", true of Scan and never of this) and runs a bounded deletion
// pass. Reaping a group's served winner therefore left its twin stamped
// `dupe_suppressed = 1` with no served copy in the group: the album
// vanished from /v1/manifest, from the DLNA surface and from the smart-mix
// pools until the next full scan — up to ScanIntervalSec (6h) later.
//
// The file-backed rows are what make it a real subtree scan: size+mtime
// match, so the surviving copy takes the fast-skip and keeps its tags.
func TestScanSubtree_RestampsAfterReapingTheServedWinner(t *testing.T) {
	root := t.TempDir()
	store, sc := newScanFixture(t, root)
	mode := dupes.FilterHighestQuality
	sc.SetDupePolicy(func() dupes.Policy { return dupes.Policy{Mode: mode} })
	ctx := context.Background()

	dir := filepath.Join(root, "Dir")
	const loserRel, winnerRel = "Dir/CopyA/01 Song.flac", "Dir/CopyB/01 Song.flac"
	seedFileBackedTrack(t, store, loserRel, filepath.Join(dir, "CopyA", "01 Song.flac"), 900)
	seedFileBackedTrack(t, store, winnerRel, filepath.Join(dir, "CopyB", "01 Song.flac"), 1000)

	if _, err := sc.RestampDuplicates(ctx); err != nil {
		t.Fatalf("initial stamping: %v", err)
	}
	if st := stampOf(t, store, loserRel); !st.Suppressed {
		t.Fatalf("precondition: the smaller copy must be suppressed, got %+v", st)
	}

	// The winner leaves the disk; the watcher fires on its directory.
	if err := os.Remove(filepath.Join(dir, "CopyB", "01 Song.flac")); err != nil {
		t.Fatal(err)
	}
	if _, err := sc.ScanSubtree(ctx, dir); err != nil {
		t.Fatalf("ScanSubtree: %v", err)
	}
	if got, _ := store.GetTrack(ctx, winnerRel); got != nil {
		t.Fatal("precondition: the subtree deletion pass must have reaped the winner row")
	}

	if st := stampOf(t, store, loserRel); st.Suppressed {
		t.Errorf("surviving copy still suppressed after its winner was reaped: %+v — "+
			"nothing in the group is served, so the album is invisible to every client", st)
	}
	if n, _ := store.CountServedTracks(ctx); n != 1 {
		t.Errorf("served tracks = %d, want 1 (the surviving copy)", n)
	}
}

// TestScan_RestampsWhenTheReconciliationTailBailsEarly pins the duplicate
// stamping onto EVERY successful exit of Scan, not just the bottom one.
//
// Scan's tail has three `return count, nil` sites and the deletion pass
// commits before all of them, so an inline stamping call at the bottom is
// skipped exactly when a reaped winner may have orphaned its twin. This
// covers the routed-exclusion-set failure leg (the ctx-done leg cannot
// restamp — a cancelled context cannot write — and is documented as
// healing on the next scan).
//
// The failure is induced the way it actually presents: the store hands
// back a row the scanner can't consume. A NULL source_path breaks
// UPnPRoutedSourcePaths' scan into a string while leaving the stamping
// pass's own NOT EXISTS anti-join working (a NULL matches no track path).
func TestScan_RestampsWhenTheReconciliationTailBailsEarly(t *testing.T) {
	root := t.TempDir()
	store, sc := newScanFixture(t, root)
	// The seeded rows have no files under `root`; a grace period keeps the
	// deletion pass from reaping them and lets the tail be what's tested.
	sc.SetDeleteThreshold(5)
	mode := dupes.FilterHighestQuality
	sc.SetDupePolicy(func() dupes.Policy { return dupes.Policy{Mode: mode} })
	ctx := context.Background()

	seedDupePair(t, store)
	if _, err := sc.RestampDuplicates(ctx); err != nil {
		t.Fatalf("initial stamping: %v", err)
	}
	const loser = "CopyA/Album/01 Song.flac"
	if st := stampOf(t, store, loser); !st.Suppressed {
		t.Fatalf("precondition: %+v", st)
	}

	if _, err := store.db.Exec(`INSERT INTO upnp_track_routing
		(source_path, server_udn, object_id, res_url, last_seen_at)
		VALUES (NULL, 'udn', 'obj', 'http://x/1', 1)`); err != nil {
		t.Fatalf("stage the unreadable routing row: %v", err)
	}
	if _, err := store.UPnPRoutedSourcePaths(ctx); err == nil {
		t.Fatal("fixture: the routed-path read must fail for this test to exercise the bail-out leg")
	}

	// The operator flips the policy off; the scan's tail is what applies it.
	mode = dupes.FilterOff
	if _, err := sc.Scan(ctx); err != nil {
		t.Fatalf("Scan: %v", err)
	}

	if st := stampOf(t, store, loser); st.Suppressed {
		t.Errorf("row still suppressed: %+v — the tail bailed on the routed-set failure "+
			"and skipped stamping, though its deletion pass had already committed", st)
	}
}

// TestRestampDuplicates_AbandonsWhenAScanStartsMidPass pins the
// commit-time guard.
//
// The duplicates sweeper checks IsScanning() and THEN calls
// RestampDuplicates without s.mu — two full-library streams plus the
// election later, a scan that started in the gap is running its own
// stamping tail from fresher state. Committing the pre-scan snapshot over
// it un-suppresses rows the scan just suppressed (and vice versa) with
// nothing to heal it until the next scan.
//
// The hook makes the scan appear mid-pass, which is what distinguishes a
// commit-time re-check from a check at the top of the pass (the latter
// catches nothing the caller's own pre-check missed).
func TestRestampDuplicates_AbandonsWhenAScanStartsMidPass(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()
	ctx := context.Background()
	mode := dupes.FilterHighestQuality
	sc := dupeScanner(t, s, &mode)
	seedDupePair(t, s)

	beforeApplyDupeStampsHookForTests = func() { sc.activeScans.Add(1) }
	t.Cleanup(func() { beforeApplyDupeStampsHookForTests = nil })

	n, err := sc.RestampDuplicates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("stale pass committed %d rows; a pass whose snapshot a scan overtook must abandon", n)
	}
	if st := stampOf(t, s, "CopyA/Album/01 Song.flac"); st.GroupID != "" || st.Suppressed {
		t.Errorf("abandoned pass wrote stamps anyway: %+v", st)
	}

	// The in-scan caller is exempt — it IS what makes activeScans non-zero,
	// and its commit is the authoritative one. activeScans is still 1 here.
	beforeApplyDupeStampsHookForTests = nil
	n, err = sc.restampDuplicates(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("in-scan pass changed %d rows, want 2 — a scan's own tail must never abandon", n)
	}
	if st := stampOf(t, s, "CopyA/Album/01 Song.flac"); !st.Suppressed {
		t.Errorf("in-scan pass must commit: %+v", st)
	}
}

// TestScanSubtree_MarksItselfScanInFlight pins the other half of the
// guard: ScanSubtree writes duplicate stamps but deliberately does NOT
// set `scanning` (that one is full-Scan-only and drives the admin badge,
// the SSE fast tick and the booklet GC skip), so the sweeper's predicate
// has to be one that covers it — otherwise a sweeper pass can commit a
// stale snapshot over a subtree scan's tail with nothing detecting it.
//
// Sampled from INSIDE the scan (the extract hook fires mid-walk), since
// the counter is zero again by the time ScanSubtree returns.
func TestScanSubtree_MarksItselfScanInFlight(t *testing.T) {
	root := t.TempDir()
	_, sc := newScanFixture(t, root)
	ctx := context.Background()
	seedTrackDirs(t, filepath.Join(root, "Album"))

	// Non-blocking send: the fixture stages one file, but a hook that can
	// block a scan worker forever is not a shape to leave lying around.
	observed := make(chan int64, 1)
	afterExtractHookForTests = func(string) {
		select {
		case observed <- sc.activeScans.Load():
		default:
		}
	}
	t.Cleanup(func() { afterExtractHookForTests = nil })

	if _, err := sc.ScanSubtree(ctx, filepath.Join(root, "Album")); err != nil {
		t.Fatalf("ScanSubtree: %v", err)
	}
	select {
	case got := <-observed:
		if got == 0 {
			t.Error("activeScans was 0 during ScanSubtree — the sweeper cannot see a subtree scan, " +
				"so its stamping pass can commit a stale snapshot over the subtree scan's tail")
		}
	case <-time.After(time.Second):
		t.Fatal("the scan never reached the extract hook")
	}
	if got := sc.activeScans.Load(); got != 0 {
		t.Errorf("activeScans = %d after ScanSubtree returned, want 0", got)
	}
}
