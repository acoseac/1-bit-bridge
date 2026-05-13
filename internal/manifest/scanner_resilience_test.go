package manifest

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestScannerSurvivesSingleMissingScanWithThreshold3 verifies the core
// promise of PR E: a track that disappears for one scan does NOT get
// reaped if the configured threshold is 3 — it survives the missing
// scan and the next confirm resets its counter.
//
// **Restoration uses identical mtime + size** (os.Chtimes) so the
// scanner sees a row that's "the same file" rather than a fresh upsert.
// That's the mode silent-empty-enumeration produces in production: the
// file never changed on disk, the NAS just briefly stopped listing it.
// The unconditional `missing_count = 0` reset in UpsertTrack (even on
// mtime-equal no-op writes) is what makes this case work, and this
// test is its regression guard. Gemini bot review on PR #193 caught
// that the original test wrote a new-mtime file and so didn't exercise
// the actual contract.
func TestScannerSurvivesSingleMissingScanWithThreshold3(t *testing.T) {
	dir := t.TempDir()
	keep := filepath.Join(dir, "keep.flac")
	flap := filepath.Join(dir, "flap.flac")
	writeMinimalAudio(t, keep)
	writeMinimalAudio(t, flap)

	// Capture flap's original mtime so the restoration below produces
	// a byte-and-mtime-identical file. Without this the second
	// writeMinimalAudio gives a new mtime and exercises a different
	// codepath (full re-upsert) than the production failure mode.
	flapInfo, err := os.Stat(flap)
	if err != nil {
		t.Fatal(err)
	}
	flapMTime := flapInfo.ModTime()

	store, err := OpenStore(filepath.Join(dir, "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	s := NewScanner([]string{dir}, store, "")
	s.SetDeleteThreshold(3)

	// Scan 1: both tracks indexed.
	if _, err := s.Scan(context.Background()); err != nil {
		t.Fatalf("scan 1: %v", err)
	}
	if got := countTracksHelper(t, store); got != 2 {
		t.Fatalf("scan 1: tracks = %d, want 2", got)
	}

	// Remove flap.flac, scan again. Track should SURVIVE with
	// missing_count = 1 (still under the threshold of 3).
	if err := os.Remove(flap); err != nil {
		t.Fatalf("rm flap: %v", err)
	}
	if _, err := s.Scan(context.Background()); err != nil {
		t.Fatalf("scan 2: %v", err)
	}
	if got := countTracksHelper(t, store); got != 2 {
		t.Errorf("scan 2: tracks = %d, want 2 (flap should survive with missing_count=1)", got)
	}
	if got := missingCountHelper(t, store, "flap.flac"); got != 1 {
		t.Errorf("scan 2: flap missing_count = %d, want 1", got)
	}

	// Restore flap.flac with the EXACT original mtime — simulates the
	// "silent partial enumeration came back" production case. Counter
	// must reset to 0 even though the file is byte-and-mtime-identical
	// to what's already indexed.
	writeMinimalAudio(t, flap)
	if err := os.Chtimes(flap, flapMTime, flapMTime); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	if _, err := s.Scan(context.Background()); err != nil {
		t.Fatalf("scan 3: %v", err)
	}
	if got := missingCountHelper(t, store, "flap.flac"); got != 0 {
		t.Errorf("after mtime-equal restoration: flap missing_count = %d, want 0", got)
	}
}

// TestScannerReapsAfterThresholdMissedScans verifies a track that misses
// the threshold number of consecutive scans IS reaped — the resilience
// shouldn't be a permanent stay of execution.
func TestScannerReapsAfterThresholdMissedScans(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "doomed.flac")
	keep := filepath.Join(dir, "keep.flac")
	writeMinimalAudio(t, target)
	writeMinimalAudio(t, keep)

	store, err := OpenStore(filepath.Join(dir, "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	s := NewScanner([]string{dir}, store, "")
	s.SetDeleteThreshold(3)

	// Initial scan: both indexed.
	if _, err := s.Scan(context.Background()); err != nil {
		t.Fatalf("scan 1: %v", err)
	}
	if got := countTracksHelper(t, store); got != 2 {
		t.Fatalf("scan 1: tracks = %d, want 2", got)
	}

	// Remove target; scan three more times. Track should be gone after
	// the third missing scan (missing_count reaches 3).
	if err := os.Remove(target); err != nil {
		t.Fatalf("rm: %v", err)
	}
	for i := 2; i <= 4; i++ {
		if _, err := s.Scan(context.Background()); err != nil {
			t.Fatalf("scan %d: %v", i, err)
		}
	}
	if got := countTracksHelper(t, store); got != 1 {
		t.Errorf("after 3 missing scans: tracks = %d, want 1 (doomed should be reaped)", got)
	}
}

// TestScannerThreshold1PreservesImmediateDelete verifies the operator
// opt-out path: setting threshold=1 reverts to the pre-resilience
// behaviour (delete on the very first missing scan).
func TestScannerThreshold1PreservesImmediateDelete(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "doomed.flac")
	writeMinimalAudio(t, target)

	store, err := OpenStore(filepath.Join(dir, "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	s := NewScanner([]string{dir}, store, "")
	s.SetDeleteThreshold(1)

	if _, err := s.Scan(context.Background()); err != nil {
		t.Fatalf("scan 1: %v", err)
	}
	if err := os.Remove(target); err != nil {
		t.Fatalf("rm: %v", err)
	}
	if _, err := s.Scan(context.Background()); err != nil {
		t.Fatalf("scan 2: %v", err)
	}
	if got := countTracksHelper(t, store); got != 0 {
		t.Errorf("threshold=1 should reap on first miss: got tracks = %d", got)
	}
}

// TestClearMissingCounts verifies the operator escape hatch wipes only
// rows whose missing_count > 0, leaving healthy rows untouched.
func TestClearMissingCounts(t *testing.T) {
	dir := t.TempDir()
	doomed := filepath.Join(dir, "doomed.flac")
	healthy := filepath.Join(dir, "healthy.flac")
	writeMinimalAudio(t, doomed)
	writeMinimalAudio(t, healthy)

	store, err := OpenStore(filepath.Join(dir, "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	s := NewScanner([]string{dir}, store, "")
	s.SetDeleteThreshold(5) // generous threshold so doomed accumulates count without being deleted
	if _, err := s.Scan(context.Background()); err != nil {
		t.Fatalf("scan 1: %v", err)
	}
	if err := os.Remove(doomed); err != nil {
		t.Fatalf("rm: %v", err)
	}
	if _, err := s.Scan(context.Background()); err != nil {
		t.Fatalf("scan 2: %v", err)
	}
	// doomed has missing_count=1 now; healthy is at 0.
	n, err := store.ClearMissingCounts(context.Background())
	if err != nil {
		t.Fatalf("ClearMissingCounts: %v", err)
	}
	if n != 1 {
		t.Errorf("ClearMissingCounts returned %d, want 1", n)
	}
	if got := countTracksHelper(t, store); got != 1 {
		t.Errorf("after clear: tracks = %d, want 1 (healthy must survive)", got)
	}
}

// TestPendingDeletionsCount verifies the count surfaced for ScanState.
func TestPendingDeletionsCount(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.flac")
	b := filepath.Join(dir, "b.flac")
	writeMinimalAudio(t, a)
	writeMinimalAudio(t, b)
	store, err := OpenStore(filepath.Join(dir, "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	s := NewScanner([]string{dir}, store, "")
	s.SetDeleteThreshold(5)
	if _, err := s.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}

	if got, _ := store.PendingDeletions(context.Background()); got != 0 {
		t.Errorf("clean state: PendingDeletions = %d, want 0", got)
	}

	if err := os.Remove(a); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, _ := store.PendingDeletions(context.Background()); got != 1 {
		t.Errorf("after first miss: PendingDeletions = %d, want 1", got)
	}
}

// --- helpers ---

func countTracksHelper(t *testing.T, store *Store) int {
	t.Helper()
	n, err := store.CountTracks(context.Background())
	if err != nil {
		t.Fatalf("CountTracks: %v", err)
	}
	return n
}

func missingCountHelper(t *testing.T, store *Store, path string) int {
	t.Helper()
	var n int
	if err := store.db.QueryRow(`SELECT missing_count FROM tracks WHERE path = ?`, path).Scan(&n); err != nil {
		t.Fatalf("missing_count query for %q: %v", path, err)
	}
	return n
}

// writeMinimalAudio creates a tiny valid-enough .flac file the scanner
// will index. The Scanner only checks extension; for tests we don't
// need the file to actually parse — `marshalForStorage` runs on a
// Track built from the FS info, and the tag-extraction failure is
// tolerated (track gets minimal tags).
func writeMinimalAudio(t *testing.T, path string) {
	t.Helper()
	// FLAC magic is enough to skirt obvious early rejects; the scanner
	// tolerates a tag-extraction panic / failure via the per-iteration
	// recover and stores the row regardless.
	if err := os.WriteFile(path, []byte("fLaC"), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
