package manifest

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestNextDeltaStampIsMaxOfTheClockAndOnePastEachWatermarkArm pins the
// scalar: the later of the clock and one past each watermark arm. The
// deleted_at case is the one an indexed_at-only expression answers with
// the clock.
func TestNextDeltaStampIsMaxOfTheClockAndOnePastEachWatermarkArm(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { s.Close() })
	setWatermarkArms(t, s, 30, 50, 40)

	if got := stampAt(t, s, 45); got != 51 {
		t.Fatalf("stamp at 45 = %d, want 51 (one past deleted_at)", got)
	}
	if got := stampAt(t, s, 50); got != 51 {
		t.Fatalf("stamp at 50 = %d, want 51", got)
	}
	if got := stampAt(t, s, 60); got != 60 {
		t.Fatalf("stamp at 60 = %d, want the clock", got)
	}

	setWatermarkArms(t, s, 100, 10, 10)
	if got := stampAt(t, s, 30); got != 101 {
		t.Fatalf("stamp at 30 = %d, want 101 (one past indexed_at)", got)
	}

	setWatermarkArms(t, s, 10, 10, 80)
	if got := stampAt(t, s, 30); got != 81 {
		t.Fatalf("stamp at 30 = %d, want 81 (one past the coverage start)", got)
	}
}

func TestNextDeltaStampReadsTheCoverageKey(t *testing.T) {
	if indexedAtAdvanceSQL != nextDeltaStampSQL {
		t.Fatal("indexedAtAdvanceSQL and nextDeltaStampSQL diverged")
	}
	if selectNextDeltaStampSQL != "SELECT "+nextDeltaStampSQL {
		t.Fatal("selectNextDeltaStampSQL is not the stamp expression")
	}
	if !strings.Contains(nextDeltaStampSQL, deletionJournalCoverageKey) {
		t.Fatalf("stamp expression does not name %q", deletionJournalCoverageKey)
	}
	if !strings.Contains(journalInsertPrefixSQL, nextDeltaStampSQL) {
		t.Fatal("the journal insert does not stamp with nextDeltaStampSQL")
	}
}

// TestEveryDeltaStampConstEmbedsNextDeltaStamp names every SQL const that
// writes a delta-visible stamp and requires the expression verbatim.
// The copies stay literals: concatenating them trips SonarCloud go:S2077.
// Grep of indexed_at and deleted_at assignments: the upsert conflict
// arms, migration v34's post(), and `indexed_at = excluded.indexed_at`
// are the exclusions the advance docblock already names, and
// `deleted_at = excluded.deleted_at` copies the SELECT that already
// stamps with this expression.
func TestEveryDeltaStampConstEmbedsNextDeltaStamp(t *testing.T) {
	for name, stmt := range map[string]string{
		"indexedAtAdvanceSQL":          indexedAtAdvanceSQL,
		"selectNextDeltaStampSQL":      selectNextDeltaStampSQL,
		"journalInsertPrefixSQL":       journalInsertPrefixSQL,
		"journalThresholdReapBatchSQL": journalThresholdReapBatchSQL,
		"journalThresholdReapOneSQL":   journalThresholdReapOneSQL,
		"journalDeleteByPrefixSQL":     journalDeleteByPrefixSQL,
		"journalClearMissingSQL":       journalClearMissingSQL,
		"journalSinglePathSQL":         journalSinglePathSQL,
		"bumpIndexedAtByPathSQL":       bumpIndexedAtByPathSQL,
		"markEnrichedSQL":              markEnrichedSQL,
		"applyReconciledTrackSQL":      applyReconciledTrackSQL,
		"setArtworkVersionSQL":         setArtworkVersionSQL,
		"setBookletTagSQL":             setBookletTagSQL,
		"applyDupeStampBumpSQL":        applyDupeStampBumpSQL,
	} {
		if !strings.Contains(stmt, nextDeltaStampSQL) {
			t.Errorf("%s does not contain nextDeltaStampSQL", name)
		}
	}
}

func TestNextDeltaStampUsesTheWatermarkIndexes(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { s.Close() })
	setWatermarkArms(t, s, 30, 50, 40)
	if _, err := s.db.Exec(`ANALYZE`); err != nil {
		t.Fatal(err)
	}
	plan := explainPlan(t, s, `SELECT `+nextDeltaStampSQL, int64(1))
	t.Logf("next stamp: %s", plan)
	for _, want := range []string{
		"SEARCH tracks USING COVERING INDEX idx_tracks_indexed",
		"SEARCH manifest_deletions USING COVERING INDEX idx_manifest_deletions_deleted_at",
		"SEARCH scan_state",
	} {
		if !strings.Contains(plan, want) {
			t.Errorf("plan does not use %s:\n%s", want, plan)
		}
	}
	watermark := explainPlan(t, s, `
		SELECT MAX(v) FROM (
			SELECT MAX(indexed_at) AS v FROM tracks
			UNION ALL
			SELECT MAX(deleted_at) AS v FROM manifest_deletions
			UNION ALL
			SELECT CAST(v AS INTEGER) AS v FROM scan_state WHERE k = ?
		)`, deletionJournalCoverageKey)
	t.Logf("library watermark: %s", watermark)
	for _, want := range []string{
		"SEARCH tracks USING COVERING INDEX idx_tracks_indexed",
		"SEARCH manifest_deletions USING COVERING INDEX idx_manifest_deletions_deleted_at",
		"SEARCH scan_state",
	} {
		if !strings.Contains(watermark, want) {
			t.Errorf("watermark plan does not use %s:\n%s", want, watermark)
		}
	}
}

// TestADeleteInTheSameNanosecondReachesADelta plants a tombstone strictly
// above every indexed_at and deletes in that same nanosecond. The new
// tombstone is past that watermark, and a delta taken there returns the
// path. An indexed_at-only stamp lands on the planted tombstone and the
// strict `>` filter drops it.
func TestADeleteInTheSameNanosecondReachesADelta(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { s.Close() })
	ctx := context.Background()
	frozen := time.Now().Add(time.Hour)
	s.now = func() time.Time { return frozen }
	const target = "A/1.flac"
	for _, p := range []string{target, "A/2.flac", "A/3.flac", "A/4.flac"} {
		upsertParent(t, s, p)
	}
	watermark := indexedAtOf(t, s, target) + 1_000_000
	plantTombstone(t, s, "A/older.flac", watermark)
	s.now = func() time.Time { return time.Unix(0, watermark) }

	if err := s.DeleteTrack(ctx, target); err != nil {
		t.Fatal(err)
	}
	var deletedAt int64
	if err := s.db.QueryRow(`SELECT deleted_at FROM manifest_deletions WHERE path = ?`, target).Scan(&deletedAt); err != nil {
		t.Fatal(err)
	}
	if deletedAt <= watermark {
		t.Fatalf("deleted_at %d is not past the watermark %d", deletedAt, watermark)
	}
	m, err := BuildManifest(ctx, s, []string{"/lib"}, time.Unix(0, watermark).UTC())
	if err != nil {
		t.Fatal(err)
	}
	if m.DeltaIncomplete {
		t.Fatal("a cursor at the earlier tombstone is inside the journal's coverage")
	}
	if !containsPath(m.Deleted, target) {
		t.Fatalf("delta since %d deleted %v, want %s", watermark, m.Deleted, target)
	}
}

// TestAMassOpCoverageStartInTheSameNanosecondIsNotCovered is the coverage
// arm. The stamp is taken while the tombstone above indexed_at still
// exists; a cursor equal to that tombstone is not covered, so the delta
// says it is incomplete instead of listing nothing. A stamp read after
// the wipe, or a plain clock, reports the cursor covered.
func TestAMassOpCoverageStartInTheSameNanosecondIsNotCovered(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { s.Close() })
	ctx := context.Background()
	frozen := time.Now().Add(time.Hour)
	s.now = func() time.Time { return frozen }
	for _, p := range []string{"A/1.flac", "A/2.flac", "A/3.flac", "A/4.flac"} {
		upsertParent(t, s, p)
	}
	watermark := indexedAtOf(t, s, "A/1.flac") + 1_000_000
	plantTombstone(t, s, "A/older.flac", watermark)
	s.now = func() time.Time { return time.Unix(0, watermark) }

	if err := s.DeleteTracksBatch(ctx, []string{"A/1.flac", "A/2.flac"}); err != nil {
		t.Fatal(err)
	}
	raw, err := s.GetScanState(ctx, deletionJournalCoverageKey)
	if err != nil {
		t.Fatal(err)
	}
	start, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	if start <= watermark {
		t.Fatalf("coverage start %d is not past the watermark %d", start, watermark)
	}
	covered, err := s.DeltaSinceCovered(ctx, time.Unix(0, watermark).UTC())
	if err != nil {
		t.Fatal(err)
	}
	if covered {
		t.Fatal("a cursor equal to the tombstone the wipe erased reads as covered")
	}
	m, err := BuildManifest(ctx, s, []string{"/lib"}, time.Unix(0, watermark).UTC())
	if err != nil {
		t.Fatal(err)
	}
	if !m.DeltaIncomplete {
		t.Fatal("the delta answered a cursor the journal no longer covers")
	}
	if len(m.Deleted) != 0 {
		t.Fatalf("an incomplete delta listed %v", m.Deleted)
	}
	// A cursor at the new start is covered. The filter stays `>=`.
	covered, err = s.DeltaSinceCovered(ctx, time.Unix(0, start).UTC())
	if err != nil {
		t.Fatal(err)
	}
	if !covered {
		t.Fatalf("a cursor at the coverage start %d is not covered", start)
	}
}

// TestAnIndexedAtBumpInTheSameNanosecondClearsATombstoneWatermark is the
// reverse. The clock equals a deleted_at that is the watermark, and
// MAX(indexed_at) is behind it. The bump has to pass that tombstone or
// `indexed_at > since` drops the row.
func TestAnIndexedAtBumpInTheSameNanosecondClearsATombstoneWatermark(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { s.Close() })
	frozen := time.Now().Add(time.Hour)
	s.now = func() time.Time { return frozen }
	const target = "A/target.flac"
	upsertParent(t, s, target)
	watermark := indexedAtOf(t, s, target) + 1_000_000
	plantTombstone(t, s, "A/older.flac", watermark)
	s.now = func() time.Time { return time.Unix(0, watermark) }

	if err := s.UpsertVariant(context.Background(), VariantRow{
		SourcePath: target, VariantID: "upscaled-v1-176400-24",
		SidecarPath: "/tmp/x.flac", Format: "flac",
		SampleRate: 176400, BitsPerSample: 24,
	}); err != nil {
		t.Fatal(err)
	}
	requireBumpClearedMax(t, s, "UpsertVariant", target, watermark)
}

func setWatermarkArms(t *testing.T, s *Store, indexed, deleted, coverage int64) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.db.ExecContext(ctx, `DELETE FROM tracks`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM manifest_deletions`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO tracks(path, size, mtime_ns, tags_json, indexed_at)
		VALUES('A/1.flac', 1, 1, x'7b7d', ?)`, indexed); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO manifest_deletions(path, deleted_at) VALUES('A/gone.flac', ?)`, deleted); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO scan_state(k, v) VALUES(?, ?)
		ON CONFLICT(k) DO UPDATE SET v = excluded.v`,
		deletionJournalCoverageKey, strconv.FormatInt(coverage, 10)); err != nil {
		t.Fatal(err)
	}
}

func stampAt(t *testing.T, s *Store, now int64) int64 {
	t.Helper()
	var stamp int64
	if err := s.db.QueryRow(selectNextDeltaStampSQL, now).Scan(&stamp); err != nil {
		t.Fatal(err)
	}
	return stamp
}

func plantTombstone(t *testing.T, s *Store, path string, deletedAt int64) {
	t.Helper()
	if _, err := s.db.Exec(`INSERT INTO manifest_deletions(path, deleted_at) VALUES(?, ?)`, path, deletedAt); err != nil {
		t.Fatal(err)
	}
}

func containsPath(paths []string, want string) bool {
	for _, p := range paths {
		if p == want {
			return true
		}
	}
	return false
}
