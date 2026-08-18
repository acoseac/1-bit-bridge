package manifest

import (
	"context"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// TestVariantFailureThresholdMirrorsSQL pins the two spellings of the
// threshold together. The predicate has to be a true Go const (the shape that
// keeps go:S2077 quiet), so the number is inlined as a string literal — which
// means retuning the Go const alone would silently leave the SQL on the old
// value, and the debounce would suppress at a different count than every
// docblock claims.
func TestVariantFailureThresholdMirrorsSQL(t *testing.T) {
	if got := strconv.Itoa(variantFailureThreshold); got != variantFailureThresholdSQL {
		t.Fatalf("threshold drift: Go const = %s, SQL literal = %s", got, variantFailureThresholdSQL)
	}
}

func openVariantFailStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// seedFailTrack writes a track whose size + mtime the suppression predicate
// compares against.
func seedFailTrack(t *testing.T, s *Store, path string, size, mtimeNS int64) {
	t.Helper()
	rate, bits, dsd := 96000.0, 24, false
	if err := s.UpsertTrack(context.Background(), &Track{
		Path:          path,
		Size:          size,
		ModTime:       time.Unix(0, mtimeNS),
		SampleRate:    &rate,
		BitsPerSample: &bits,
		Codec:         "FLAC",
		IsDSD:         &dsd,
	}); err != nil {
		t.Fatalf("UpsertTrack: %v", err)
	}
}

func suppressed(t *testing.T, s *Store, path string) bool {
	t.Helper()
	var n int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM tracks WHERE path = ? AND `+variantFailureSuppressedSQL,
		path, s.VariantFailureCutoff()).Scan(&n)
	if err != nil {
		t.Fatalf("suppression query: %v", err)
	}
	return n > 0
}

// TestVariantFailureSuppressesOnlyAfterThreshold pins the debounce. One
// failure proves nothing — a full disk or a dropped mount produces a non-zero
// sox exit for a perfectly good file — so a source must fail
// variantFailureThreshold CONSECUTIVE times before it is sidelined.
func TestVariantFailureSuppressesOnlyAfterThreshold(t *testing.T) {
	s := openVariantFailStore(t)
	const path, size, mtime = "A/01.flac", int64(1000), int64(1700000000)
	seedFailTrack(t, s, path, size, mtime)

	for i := 1; i < variantFailureThreshold; i++ {
		if err := s.RecordVariantFailure(context.Background(), path, size, mtime); err != nil {
			t.Fatal(err)
		}
		if suppressed(t, s, path) {
			t.Fatalf("suppressed after %d failure(s), want only after %d", i, variantFailureThreshold)
		}
	}
	if err := s.RecordVariantFailure(context.Background(), path, size, mtime); err != nil {
		t.Fatal(err)
	}
	if !suppressed(t, s, path) {
		t.Fatalf("still not suppressed after %d consecutive failures", variantFailureThreshold)
	}
}

// TestVariantFailureSuccessResetsTheCount pins that the counter measures
// CONSECUTIVE failures. Without the reset, a file that fails occasionally over
// a long life — with successes in between — would eventually suppress itself.
func TestVariantFailureSuccessResetsTheCount(t *testing.T) {
	s := openVariantFailStore(t)
	const path, size, mtime = "A/01.flac", int64(1000), int64(1700000000)
	seedFailTrack(t, s, path, size, mtime)

	for i := 0; i < variantFailureThreshold-1; i++ {
		if err := s.RecordVariantFailure(context.Background(), path, size, mtime); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.ClearVariantFailure(context.Background(), path); err != nil {
		t.Fatal(err)
	}
	// Post-success strikes start from zero, so one more must NOT suppress.
	if err := s.RecordVariantFailure(context.Background(), path, size, mtime); err != nil {
		t.Fatal(err)
	}
	if suppressed(t, s, path) {
		t.Fatal("a success did not reset the strike count — occasional failures would accumulate into a permanent suppression")
	}
}

// TestVariantFailureVersionGateReopensChangedFile pins the self-healing half:
// the strikes describe ONE version of the file, so re-encoding or repairing it
// re-opens the candidate with no operator action.
func TestVariantFailureVersionGateReopensChangedFile(t *testing.T) {
	s := openVariantFailStore(t)
	const path = "A/01.flac"
	seedFailTrack(t, s, path, 1000, 1700000000)
	for i := 0; i < variantFailureThreshold; i++ {
		if err := s.RecordVariantFailure(context.Background(), path, 1000, 1700000000); err != nil {
			t.Fatal(err)
		}
	}
	if !suppressed(t, s, path) {
		t.Fatal("precondition: expected suppression")
	}

	// The operator repairs the file; the scanner rewrites size + mtime.
	seedFailTrack(t, s, path, 2000, 1800000000)
	if suppressed(t, s, path) {
		t.Fatal("still suppressed after the file changed — a repaired file must re-enter the candidate set on its own")
	}
}

// TestVariantFailureStrikesAgainstANewVersionRestartTheCount: strikes carried
// over from a previous version could otherwise combine with a single new one
// to cross the threshold.
func TestVariantFailureStrikesAgainstANewVersionRestartTheCount(t *testing.T) {
	s := openVariantFailStore(t)
	const path = "A/01.flac"
	seedFailTrack(t, s, path, 1000, 1700000000)
	for i := 0; i < variantFailureThreshold-1; i++ {
		if err := s.RecordVariantFailure(context.Background(), path, 1000, 1700000000); err != nil {
			t.Fatal(err)
		}
	}
	// File changes, then fails once against the NEW version.
	seedFailTrack(t, s, path, 2000, 1800000000)
	if err := s.RecordVariantFailure(context.Background(), path, 2000, 1800000000); err != nil {
		t.Fatal(err)
	}
	if suppressed(t, s, path) {
		t.Fatal("old strikes counted toward the new version's total")
	}
}

// TestVariantFailureTTLExpires pins that a suppression stops applying once it
// is older than the TTL — what changes over time is the toolchain, and a sox
// upgrade must be able to re-open the file without operator action.
func TestVariantFailureTTLExpires(t *testing.T) {
	s := openVariantFailStore(t)
	const path, size, mtime = "A/01.flac", int64(1000), int64(1700000000)
	seedFailTrack(t, s, path, size, mtime)

	// Record the strikes "in the past" via the injectable clock.
	past := time.Now().Add(-variantFailureTTL - time.Hour)
	s.now = func() time.Time { return past }
	for i := 0; i < variantFailureThreshold; i++ {
		if err := s.RecordVariantFailure(context.Background(), path, size, mtime); err != nil {
			t.Fatal(err)
		}
	}
	s.now = time.Now
	if suppressed(t, s, path) {
		t.Fatal("a suppression older than the TTL still applies — a sox upgrade could never re-open the file")
	}
}

// TestClearVariantFailuresUnderPrefixIsByteRanged pins the case-exact scope of
// the operator's retry. This is a path-keyed WRITE, and SQLite's LIKE folds
// ASCII case, so the LIKE form would also clear a case-twin sibling directory
// — a different directory on a case-sensitive filesystem.
func TestClearVariantFailuresUnderPrefixIsByteRanged(t *testing.T) {
	s := openVariantFailStore(t)
	for _, p := range []string{"Album/01.flac", "album/01.flac"} {
		seedFailTrack(t, s, p, 1000, 1700000000)
		for i := 0; i < variantFailureThreshold; i++ {
			if err := s.RecordVariantFailure(context.Background(), p, 1000, 1700000000); err != nil {
				t.Fatal(err)
			}
		}
	}
	n, err := s.ClearVariantFailuresUnderPrefix(context.Background(), "Album")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("cleared %d rows, want 1 — the case twin is a different directory", n)
	}
	if suppressed(t, s, "Album/01.flac") {
		t.Error("target still suppressed after the retry")
	}
	if !suppressed(t, s, "album/01.flac") {
		t.Error("case twin was cleared too — the predicate folded case")
	}
}

// TestListAutoOptimizeCandidatesSkipsSuppressed is the end-to-end point of the
// whole mechanism: the sweeper's candidate query must stop returning a source
// that keeps failing. Without it the sweeper re-selects the same doomed file
// on every tick, forever, because a failed job writes no variant row.
func TestListAutoOptimizeCandidatesSkipsSuppressed(t *testing.T) {
	s := openVariantFailStore(t)
	// Hi-res so both rows are genuine optimize candidates.
	seedFailTrack(t, s, "A/good.flac", 1000, 1700000000)
	seedFailTrack(t, s, "A/doomed.flac", 1000, 1700000000)

	before, err := s.ListAutoOptimizeCandidates(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != 2 {
		t.Fatalf("precondition: got %d candidates, want 2", len(before))
	}

	for i := 0; i < variantFailureThreshold; i++ {
		if err := s.RecordVariantFailure(context.Background(), "A/doomed.flac", 1000, 1700000000); err != nil {
			t.Fatal(err)
		}
	}
	after, err := s.ListAutoOptimizeCandidates(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 1 {
		t.Fatalf("got %d candidates after suppression, want 1 — the sweeper would loop on the doomed file", len(after))
	}
	if after[0].Path != "A/good.flac" {
		t.Errorf("surviving candidate = %q, want A/good.flac", after[0].Path)
	}
}
