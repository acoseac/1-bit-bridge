package manifest

// Backlog B157: an `.iso` that is not an SACD image (a DVD or CD image, a
// disk image named `.iso`) expands to nothing, so it has no row for
// processSACDISO's skip gate to key on, and every scan opened it and probed
// its master TOC again. A completed "nothing to expand" probe is now
// remembered for the image's size and mtime (Scanner.notSACD); a probe that
// failed is not.

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// countSACDOpens makes sc count every `.iso` it opens, reading the real file.
func countSACDOpens(sc *Scanner) *atomic.Int64 {
	var opens atomic.Int64
	installSACDOpener(sc, func(_ string, f *os.File) sacdContainer {
		opens.Add(1)
		return f
	})
	return &opens
}

// writeZeroISO writes a zero-filled `.iso` of size bytes: not an SACD, and
// large enough to hold every master-TOC sector the probe reads.
func writeZeroISO(t *testing.T, path string, size int64) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
}

// TestScanner_AnISOThatIsNotAnSACDIsProbedOnce is the defect, measured the
// way the backlog entry measured it: three full scans of an unchanged
// zero-filled image, counted through the openSACD seam. It was 3.
func TestScanner_AnISOThatIsNotAnSACDIsProbedOnce(t *testing.T) {
	root, store, sc := sacdScanFixture(t)
	writeZeroISO(t, filepath.Join(root, "Music", "dvd.iso"), 1_200_000)
	opens := countSACDOpens(sc)

	for _, label := range []string{"first", "second", "third"} {
		scanOnce(t, sc, label)
	}

	if n := opens.Load(); n != 1 {
		t.Fatalf("an unchanged image that is not an SACD was opened %d times in three scans, want 1", n)
	}
	if rows := sacdTrackPathsUnder(t, store, "Music/dvd.iso"); len(rows) != 0 {
		t.Fatalf("an image that is not an SACD has rows: %v", rows)
	}
}

// TestScanner_ANotAnSACDVerdictFollowsTheFile: the verdict is the probe's
// for the size and mtime it read. A new mtime, then a new size, each probe
// again, and an image replaced by a real SACD still expands.
func TestScanner_ANotAnSACDVerdictFollowsTheFile(t *testing.T) {
	root, store, sc := sacdScanFixture(t)
	iso := filepath.Join(root, "Music", "Album.iso")
	writeZeroISO(t, iso, 1_200_000)
	opens := countSACDOpens(sc)
	scanOnce(t, sc, "initial")

	touched := time.Now().Add(2 * time.Hour).Truncate(time.Second)
	if err := os.Chtimes(iso, touched, touched); err != nil {
		t.Fatal(err)
	}
	scanOnce(t, sc, "after a new mtime")
	if n := opens.Load(); n != 2 {
		t.Fatalf("after a new mtime: %d opens, want 2 (a changed image is probed again)", n)
	}

	writeZeroISO(t, iso, 1_300_000)
	if err := os.Chtimes(iso, touched, touched); err != nil {
		t.Fatal(err)
	}
	scanOnce(t, sc, "after a new size")
	if n := opens.Load(); n != 3 {
		t.Fatalf("after a new size at the same mtime: %d opens, want 3", n)
	}

	if err := os.Remove(iso); err != nil {
		t.Fatal(err)
	}
	writeSACDFixture(t, filepath.Join(root, "Music"), "Album.iso", twoFixtureTracks(), sacdFixtureOptions{})
	scanOnce(t, sc, "after the image became an SACD")
	if rows := sacdTrackPathsUnder(t, store, "Music/Album.iso"); len(rows) != 2 {
		t.Fatalf("an image replaced by an SACD did not expand: %v", rows)
	}
}

// TestScanner_AnISOWhoseProbeFailedIsProbedAgain holds B134's line: only a
// completed read can say "not an SACD". A probe whose master-TOC reads fail
// records nothing, so every scan probes the image again.
func TestScanner_AnISOWhoseProbeFailedIsProbedAgain(t *testing.T) {
	root, _, sc := sacdScanFixture(t)
	writeZeroISO(t, filepath.Join(root, "Music", "flaky.iso"), 1_200_000)
	var opens, failed atomic.Int64
	installSACDOpener(sc, func(_ string, f *os.File) sacdContainer {
		opens.Add(1)
		return failingISO{File: f, faults: sacdProbeFaults(0, syscall.EIO), failed: &failed}
	})

	for _, label := range []string{"first", "second", "third"} {
		scanOnce(t, sc, label)
	}

	if failed.Load() == 0 {
		t.Fatal("no read reached a fault, so the case tests nothing")
	}
	if n := opens.Load(); n != 3 {
		t.Fatalf("an image whose probe failed was opened %d times in three scans, want 3", n)
	}
}
