package manifest

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
)

// A SACD container's virtual rows appear in no disk walk: the deletion pass
// counts them seen whenever their container is (the container-seen
// branch), and never counts them missing then. It never reset their count
// either, and a container the walk saw unchanged took processSACDISO's
// early return, which wrote nothing: so "missing on `threshold`
// consecutive scans" became "missing on `threshold` scans ever" for every
// virtual row, while a plain file seen unchanged had its count reset by
// the skip gate (backlog B217). A container hidden for one scan on three
// separate occasions, at threshold 3: the plain file's count went back to
// 0 each time; the virtual rows' went 1, 1, 2, 2, and the third hide
// deleted them, with a tombstone to every paired device, while the .iso
// was back on disk the scan after.

// TestScanner_SACDRowsCountOnlyConsecutiveMisses — the review's shape
// through the real scanner, at threshold 3: the container hidden for one
// scan three times, each followed by a scan that sees it again. The
// virtual rows, like the plain file beside them, must come back to 0 each
// time and survive.
func TestScanner_SACDRowsCountOnlyConsecutiveMisses(t *testing.T) {
	root, store, sc := sacdScanFixture(t)
	sc.SetDeleteThreshold(3)
	music := filepath.Join(root, "Music")
	iso := writeSACDFixture(t, music, "Album.iso", twoFixtureTracks(), sacdFixtureOptions{})
	flac := filepath.Join(music, "Other", "01.flac")
	if err := os.MkdirAll(filepath.Dir(flac), 0o755); err != nil {
		t.Fatal(err)
	}
	writeMinimalFLAC(t, flac, 44100, 16, map[string]string{"TITLE": "Other"})
	scanOnce(t, sc, "initial")
	const virtual = "Music/Album.iso/st/01.dff"
	mustIndexed(t, store, virtual, "Music/Album.iso/st/02.dff", "Music/Other/01.flac")

	hidden := iso + ".away"
	for round := 1; round <= 3; round++ {
		if err := os.Rename(iso, hidden); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(flac, flac+".away"); err != nil {
			t.Fatal(err)
		}
		scanOnce(t, sc, "hidden")
		if got := trackMissingCount(t, store, virtual); got != 1 {
			t.Fatalf("round %d, the container hidden: the virtual row's missing count is %d, want 1 (one consecutive miss)", round, got)
		}
		if err := os.Rename(hidden, iso); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(flac+".away", flac); err != nil {
			t.Fatal(err)
		}
		scanOnce(t, sc, "restored")
		if got := trackMissingCount(t, store, "Music/Other/01.flac"); got != 0 {
			t.Fatalf("round %d, restored: the plain file's missing count is %d, want 0", round, got)
		}
		if got := trackMissingCount(t, store, virtual); got != 0 {
			t.Fatalf("round %d, restored: the virtual row's missing count is %d, want 0, as the plain file's is", round, got)
		}
	}
	mustIndexed(t, store, virtual, "Music/Album.iso/st/02.dff")
	if deleted := sacdTombstones(t, store); len(deleted) != 0 {
		t.Errorf("tombstones %v, want none: the container never missed %d scans in a row", deleted, 3)
	}
}

// sacdRowsMissedOnce sets both virtual rows' missing count to 1, the count a
// scan that did not see the container leaves.
func sacdRowsMissedOnce(t *testing.T, store *Store) {
	t.Helper()
	if _, err := store.db.Exec(`UPDATE tracks SET missing_count = 1 WHERE path IN (?, ?)`,
		"Music/Album.iso/st/01.dff", "Music/Album.iso/st/02.dff"); err != nil {
		t.Fatal(err)
	}
}

// requireSACDRowsSeen fails the test unless both virtual rows are back at a
// missing count of 0.
func requireSACDRowsSeen(t *testing.T, store *Store, what string) {
	t.Helper()
	for _, p := range []string{"Music/Album.iso/st/01.dff", "Music/Album.iso/st/02.dff"} {
		if got := trackMissingCount(t, store, p); got != 0 {
			t.Errorf("%s: %s's missing count is %d, want 0: the walk saw its container", what, p, got)
		}
	}
}

// TestScanner_SACDRowsAreSeenWhereTheScanWritesNone — every exit of
// processSACDISO that writes no row for a container the walk saw resets its
// rows' count (keepSACDRowsSeen), and a subtree scan does as a full one: a
// read that did not complete (which retires nothing) and a container that
// changed during the scan (which is left for the next one) keep the rows as
// they were and still count them seen.
func TestScanner_SACDRowsAreSeenWhereTheScanWritesNone(t *testing.T) {
	t.Run("a read that did not complete", func(t *testing.T) {
		store, sc, _, stamped, _ := sacdStampedThenTouched(t)
		sacdRowsMissedOnce(t, store)
		var failed atomic.Int64
		installSACDOpener(sc, func(_ string, f *os.File) sacdContainer {
			return failingISO{File: f, faults: sacdProbeFaults(0, syscall.EIO), failed: &failed}
		})
		scanOnce(t, sc, "rescan with failing reads")
		if failed.Load() == 0 {
			t.Fatal("no read reached a fault, so the case tests nothing")
		}
		requireSACDRowsUntouched(t, store, stamped)
		requireSACDRowsSeen(t, store, "after a read that did not complete")
	})
	t.Run("a container that changed during the scan", func(t *testing.T) {
		store, sc, _, stamped, touched := sacdStampedThenTouched(t)
		sacdRowsMissedOnce(t, store)
		stageDir := t.TempDir()
		var moved atomic.Int64
		installSACDOpener(sc, func(abs string, f *os.File) sacdContainer {
			if err := sacdTouchLater(abs, stageDir, touched); err != nil {
				t.Error(err)
			} else {
				moved.Add(1)
			}
			return f
		})
		scanOnce(t, sc, "rescan of a container written after the walk")
		if moved.Load() == 0 {
			t.Fatal("the container was never written, so the case tests nothing")
		}
		requireSACDRowsUntouched(t, store, stamped)
		requireSACDRowsSeen(t, store, "after a container that changed during the scan")
	})
	t.Run("a subtree scan through the skip gate", func(t *testing.T) {
		root, store, sc := sacdScanFixture(t)
		writeSACDFixture(t, filepath.Join(root, "Music"), "Album.iso", twoFixtureTracks(), sacdFixtureOptions{})
		scanOnce(t, sc, "initial")
		sacdRowsMissedOnce(t, store)
		if _, err := sc.ScanSubtree(context.Background(), filepath.Join(root, "Music")); err != nil {
			t.Fatal(err)
		}
		requireSACDRowsSeen(t, store, "after a subtree scan")
	})
}

// TestResetTrackMissingCountsUnderIsAByteRange — the reset reaches the rows
// under one container and nothing beside it: never a case-twin (a LIKE
// would), never a sibling whose name only begins like it, with the trailing
// slash trimmed, and an empty prefix is an error, not the whole table.
func TestResetTrackMissingCountsUnderIsAByteRange(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	paths := []string{
		"Music/Album.iso/st/01.dff",
		"Music/Album.iso/mc/01.dff",
		"music/album.iso/st/01.dff",
		"Music/Album.iso2/st/01.dff",
		"Music/Album.iso-x/st/01.dff",
		"Music/Album.iso",
	}
	for _, p := range paths {
		if err := store.UpsertTrack(ctx, &Track{Path: p, Size: 1}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.db.Exec(`UPDATE tracks SET missing_count = 2`); err != nil {
		t.Fatal(err)
	}
	if err := store.ResetTrackMissingCountsUnder(ctx, "Music/Album.iso/"); err != nil {
		t.Fatal(err)
	}
	for _, p := range paths {
		want := 2
		if p == "Music/Album.iso/st/01.dff" || p == "Music/Album.iso/mc/01.dff" {
			want = 0
		}
		if got := trackMissingCount(t, store, p); got != want {
			t.Errorf("%s: missing count %d, want %d", p, got, want)
		}
	}
	if err := store.ResetTrackMissingCountsUnder(ctx, "/"); err == nil {
		t.Error("an empty prefix reset rows, want an error")
	}
}
