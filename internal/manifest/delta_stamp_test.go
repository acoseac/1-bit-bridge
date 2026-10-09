package manifest

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/dsn"
)

// TestNextDeltaStampIsMaxOfTheClockAndOnePastEachWatermarkArm pins the
// scalar: the later of the clock and one past each watermark arm. The
// deleted_at case is the one an indexed_at-only expression answers with
// the clock.
func TestNextDeltaStampIsMaxOfTheClockAndOnePastEachWatermarkArm(t *testing.T) {
	s := openTempStore(t)
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
// The track upserts bind a value readNextDeltaStamp computed from
// selectNextDeltaStampSQL; that bound parameter is not another const.
// Migration v34's post() stays out, and `deleted_at = excluded.deleted_at`
// copies the SELECT that already stamps with this expression.
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

// TestAnUpsertClearsAWatermarkAboveTheRow is the gap the bump writers
// closed and the track upserts left: a tombstone or a coverage start
// strictly above this row's indexed_at, with the clock frozen on that
// watermark. The conflict arm's ELSE and a fresh insert both store the
// clock, which is the cursor a client already holds, and indexed_at >
// since then skips the row. A batch shares one stamp: readers see the
// commit whole, so two rows at the same value arrive together or not
// at all. The tombstone is a different path, because the upsert clears
// a tombstone of a path it just wrote and that would drop the watermark.
func TestAnUpsertClearsAWatermarkAboveTheRow(t *testing.T) {
	ctx := context.Background()
	for _, tc := range watermarkUpsertCases {
		t.Run(tc.name, func(t *testing.T) {
			runWatermarkUpsertCase(t, ctx, tc)
		})
	}
}

type watermarkUpsertCase struct {
	name  string
	arm   string
	fresh bool
	batch bool
}

var watermarkUpsertCases = []watermarkUpsertCase{
	{"tombstone conflict", "tombstone", false, false},
	{"tombstone conflict batch", "tombstone", false, true},
	{"tombstone fresh", "tombstone", true, false},
	{"tombstone fresh batch", "tombstone", true, true},
	{"coverage conflict", "coverage", false, false},
	{"coverage conflict batch", "coverage", false, true},
	{"coverage fresh", "coverage", true, false},
	{"coverage fresh batch", "coverage", true, true},
}

func runWatermarkUpsertCase(t *testing.T, ctx context.Context, tc watermarkUpsertCase) {
	t.Helper()
	s := openTempStore(t)
	// An hour ahead of OpenStore, so the v41 coverage seed is
	// behind the rows this case plants.
	base := time.Now().Add(time.Hour)
	s.now = func() time.Time { return base }
	seedWatermarkTracks(t, ctx, s, base)
	watermark := indexedAtOf(t, s, "Music/A/a.flac") + 1_000_000
	plantWatermarkArm(t, s, tc.arm, watermark)
	s.now = func() time.Time { return time.Unix(0, watermark) }
	written := watermarkUpsertPaths(tc)
	writeWatermarkUpsert(t, ctx, s, tc.batch, watermarkUpsertRows(written, watermark))
	requireSharedStampPastWatermark(t, s, written, watermark)
}

func seedWatermarkTracks(t *testing.T, ctx context.Context, s *Store, base time.Time) {
	t.Helper()
	for _, p := range []string{"Music/A/a.flac", "Music/A/b.flac"} {
		if err := s.UpsertTrack(ctx, &Track{Path: p, Size: 10, ModTime: base}); err != nil {
			t.Fatal(err)
		}
	}
}

func plantWatermarkArm(t *testing.T, s *Store, arm string, watermark int64) {
	t.Helper()
	if arm == "tombstone" {
		plantTombstone(t, s, "Music/A/gone.flac", watermark)
		return
	}
	setCoverage(t, s, watermark)
}

func watermarkUpsertPaths(tc watermarkUpsertCase) []string {
	written := []string{"Music/A/a.flac"}
	if tc.fresh {
		written = []string{"Music/A/new.flac"}
	}
	if !tc.batch {
		return written
	}
	if tc.fresh {
		return append(written, "Music/A/new2.flac")
	}
	return append(written, "Music/A/b.flac")
}

func watermarkUpsertRows(paths []string, watermark int64) []*Track {
	rows := make([]*Track, len(paths))
	mod := time.Unix(0, watermark)
	for i, p := range paths {
		rows[i] = &Track{Path: p, Size: 20, ModTime: mod}
	}
	return rows
}

func writeWatermarkUpsert(t *testing.T, ctx context.Context, s *Store, batch bool, rows []*Track) {
	t.Helper()
	var err error
	if batch {
		err = s.UpsertTrackBatch(ctx, rows)
	} else {
		err = s.UpsertTrack(ctx, rows[0])
	}
	if err != nil {
		t.Fatal(err)
	}
}

func requireSharedStampPastWatermark(t *testing.T, s *Store, written []string, watermark int64) {
	t.Helper()
	var stamps []int64
	for _, p := range written {
		requireUpsertPastWatermark(t, s, p, watermark)
		stamps = append(stamps, indexedAtOf(t, s, p))
	}
	if len(stamps) == 2 && stamps[0] != stamps[1] {
		t.Errorf("batch stamps differ: %d and %d", stamps[0], stamps[1])
	}
}

// TestAnUpsertHoldsTheWriteLockBeforeItReadsTheStamp is the window a
// deferred transaction opens: the stamp read takes a snapshot, and a
// second connection that commits before the upsert's write makes that
// write fail SQLITE_BUSY_SNAPSHOT. busy_timeout does not retry it.
// The second connection tries its write after the stamp read returns
// and before the upsert writes a row, on the same database file. The
// upsert has to come back with the row written.
func TestAnUpsertHoldsTheWriteLockBeforeItReadsTheStamp(t *testing.T) {
	ctx := context.Background()
	for _, batch := range []bool{false, true} {
		name := "one row"
		if batch {
			name = "batch"
		}
		t.Run(name, func(t *testing.T) {
			s := openTempStore(t)
			base := time.Now().Add(time.Hour)
			s.now = func() time.Time { return base }
			if err := s.UpsertTrack(ctx, &Track{Path: "Music/A/a.flac", Size: 10, ModTime: base}); err != nil {
				t.Fatal(err)
			}
			other := openStampRacer(t, s.path)
			var otherErr error
			s.afterDeltaStampRead = func() {
				_, otherErr = other.Exec(
					`INSERT INTO manifest_deletions(path, deleted_at) VALUES(?, ?)`,
					"Music/A/racer.flac", time.Now().UnixNano())
			}
			row := &Track{Path: "Music/A/a.flac", Size: 11, ModTime: base}
			var err error
			if batch {
				err = s.UpsertTrackBatch(ctx, []*Track{row})
			} else {
				err = s.UpsertTrack(ctx, row)
			}
			if err != nil {
				t.Fatalf("upsert: %v (the other connection: %v)", err, otherErr)
			}
			if otherErr == nil {
				t.Fatal("the other connection committed between the stamp read and the write")
			}
		})
	}
}

// TestAFailedRollbackDiscardsTheConnection pins that a ROLLBACK the
// connection cannot finish does not return that connection to the pool.
// The next checkout must be in autocommit: BEGIN succeeds. A failed
// COMMIT whose follow-up rollback also fails takes the same path. A
// follow-up rollback that lands leaves the connection checked out until
// Rollback, after the prepared statement has closed.
func TestAFailedRollbackDiscardsTheConnection(t *testing.T) {
	errEndRefused := errors.New("end refused")
	for _, name := range []string{"rollback", "commit"} {
		t.Run(name, func(t *testing.T) {
			s := openTempStore(t)
			s.db.SetMaxOpenConns(1)
			s.immediateOnEnd = func(string) error { return errEndRefused }
			ctx := context.Background()
			tx, err := s.beginImmediate(ctx)
			if err != nil {
				t.Fatal(err)
			}
			stmt, err := tx.PrepareContext(ctx, "SELECT 1")
			if err != nil {
				t.Fatal(err)
			}
			if name == "commit" {
				if err := tx.Commit(); err == nil {
					t.Fatal("commit succeeded")
				}
				// The upsert defers close the statement, then roll back.
				if err := stmt.Close(); err != nil {
					t.Fatal(err)
				}
				if err := tx.Rollback(); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := stmt.Close(); err != nil {
					t.Fatal(err)
				}
				if err := tx.Rollback(); err == nil {
					t.Fatal("rollback succeeded")
				}
			}
			if err := beginOnNextConn(t, s.db); err != nil {
				t.Fatalf("next checkout is not in autocommit: %v", err)
			}
		})
	}

	// A failed COMMIT whose rollback lands stays checked out until
	// Rollback, so the prepared statement closes first. The connection
	// that comes back is in autocommit.
	t.Run("commit then rollback", func(t *testing.T) {
		s := openTempStore(t)
		s.db.SetMaxOpenConns(1)
		s.immediateOnEnd = func(stmt string) error {
			if stmt == "COMMIT" {
				return errEndRefused
			}
			return nil
		}
		ctx := context.Background()
		tx, err := s.beginImmediate(ctx)
		if err != nil {
			t.Fatal(err)
		}
		stmt, err := tx.PrepareContext(ctx, "SELECT 1")
		if err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err == nil {
			t.Fatal("commit succeeded")
		}
		if err := stmt.Close(); err != nil {
			t.Fatal(err)
		}
		if err := tx.Rollback(); err != nil {
			t.Fatal(err)
		}
		if err := beginOnNextConn(t, s.db); err != nil {
			t.Fatalf("next checkout is not in autocommit: %v", err)
		}
	})
}

// TestClosingAfterAFailedRollbackLeavesTheNextUserInsideTheTransaction is
// the negative control: Close returns the connection while BEGIN IMMEDIATE
// is still open, and the next checkout's BEGIN fails.
func TestClosingAfterAFailedRollbackLeavesTheNextUserInsideTheTransaction(t *testing.T) {
	s := openTempStore(t)
	s.db.SetMaxOpenConns(1)
	ctx := context.Background()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, "NOT SQL"); err == nil {
		t.Fatal("bad SQL succeeded")
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	err = beginOnNextConn(t, s.db)
	if err == nil || !strings.Contains(err.Error(), "transaction") {
		t.Fatalf("next BEGIN = %v, want a transaction error", err)
	}
}

func beginOnNextConn(t *testing.T, db *sql.DB) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	_, err = conn.ExecContext(ctx, "BEGIN")
	if err == nil {
		_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
	}
	return err
}

func openStampRacer(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", dsn.File(path,
		"_pragma=journal_mode(WAL)&_pragma=busy_timeout(0)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func setCoverage(t *testing.T, s *Store, ns int64) {
	t.Helper()
	if _, err := s.db.Exec(
		`INSERT INTO scan_state(k, v) VALUES(?, ?) ON CONFLICT(k) DO UPDATE SET v = excluded.v`,
		deletionJournalCoverageKey, strconv.FormatInt(ns, 10)); err != nil {
		t.Fatal(err)
	}
}

func requireUpsertPastWatermark(t *testing.T, s *Store, path string, watermark int64) {
	t.Helper()
	ctx := context.Background()
	got := indexedAtOf(t, s, path)
	if got <= watermark {
		t.Errorf("%s indexed_at = %d, want past watermark %d", path, got, watermark)
	}
	since := time.Unix(0, watermark)
	listed, err := s.ListTracks(ctx, &since)
	if err != nil {
		t.Fatal(err)
	}
	if !trackListed(listed, path) {
		t.Errorf("ListTracks since the watermark omitted %s", path)
	}
	m, err := BuildManifest(ctx, s, []string{"/lib"}, since)
	if err != nil {
		t.Fatal(err)
	}
	if !trackListed(m.Tracks, path) {
		t.Errorf("BuildManifest since the watermark omitted %s", path)
	}
}

func trackListed(tracks []Track, path string) bool {
	for _, tr := range tracks {
		if tr.Path == path {
			return true
		}
	}
	return false
}
