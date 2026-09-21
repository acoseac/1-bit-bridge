package manifest

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestAnalysisFailureThresholdMirrorsSQL is the variant-debounce guard's twin:
// the predicate must be a true Go const, so the number lives inlined as a
// string literal beside it, and retuning one alone would suppress at a count
// no docblock claims.
func TestAnalysisFailureThresholdMirrorsSQL(t *testing.T) {
	if got := strconv.Itoa(analysisFailureThreshold); got != analysisFailureThresholdSQL {
		t.Fatalf("threshold drift: Go const = %s, SQL literal = %s", got, analysisFailureThresholdSQL)
	}
}

// TestAnalysisFailurePredicatesTakeOneBind pins the assumption
// analysisFailureSuppressedSQL2's derivation rests on.
//
// That variant is built with a single strings.Replace of the FIRST `?`, which
// is correct only while the predicate has exactly one placeholder. A second
// bind added to the base would leave the derived form mixing `?2` with a bare
// `?` — and SQLite numbers a bare `?` by position, so AnalysisCoverage would
// bind the schema version where a cutoff belongs and silently answer with a
// nonsense suppression count rather than failing.
func TestAnalysisFailurePredicatesTakeOneBind(t *testing.T) {
	for _, tc := range []struct{ name, sql string }{
		{"recorded", analysisFailureRecordedSQL},
		{"suppressed", analysisFailureSuppressedSQL},
	} {
		if n := strings.Count(tc.sql, "?"); n != 1 {
			t.Errorf("%s predicate has %d placeholders, want 1 — callers bind exactly one cutoff, "+
				"and analysisFailureSuppressedSQL2 renumbers by replacing the first", tc.name, n)
		}
	}
	if strings.Count(analysisFailureSuppressedSQL2, "?") != 1 ||
		!strings.Contains(analysisFailureSuppressedSQL2, "?2") {
		t.Errorf("derived predicate = %q, want exactly one placeholder, numbered ?2",
			analysisFailureSuppressedSQL2)
	}
}

func openAnalysisFailStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func seedAnalysisTrack(t *testing.T, s *Store, path string, size, mtimeNS int64) {
	t.Helper()
	if err := s.UpsertTrack(context.Background(), &Track{
		Path:    path,
		Size:    size,
		ModTime: time.Unix(0, mtimeNS),
		Codec:   "FLAC",
	}); err != nil {
		t.Fatalf("UpsertTrack %q: %v", path, err)
	}
}

func analysisSuppressed(t *testing.T, s *Store, path string) bool {
	t.Helper()
	var n int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM tracks WHERE path = ? AND `+analysisFailureSuppressedSQL,
		path, s.AnalysisFailureCutoff()).Scan(&n); err != nil {
		t.Fatalf("suppression query: %v", err)
	}
	return n > 0
}

// TestRecordAnalysisFailureCountsConsecutiveVerdicts walks the whole debounce:
// the count the caller uses as its log gate, and the strike at which the
// candidate stops being offered.
func TestRecordAnalysisFailureCountsConsecutiveVerdicts(t *testing.T) {
	s := openAnalysisFailStore(t)
	ctx := context.Background()
	seedAnalysisTrack(t, s, "a/broken.flac", 4096, 1234)

	for want := 1; want <= analysisFailureThreshold; want++ {
		got, err := s.RecordAnalysisFailure(ctx, "a/broken.flac", "sox: truncated")
		if err != nil {
			t.Fatalf("strike %d: %v", want, err)
		}
		if got != want {
			t.Fatalf("strike %d returned %d, want %d — the pool uses this as its "+
				"once-per-file-version log gate", want, got, want)
		}
		wantSuppressed := want >= analysisFailureThreshold
		if analysisSuppressed(t, s, "a/broken.flac") != wantSuppressed {
			t.Errorf("after %d strike(s): suppressed = %v, want %v", want, !wantSuppressed, wantSuppressed)
		}
	}
}

// TestRecordAnalysisFailureStampsFromTheRowNotTheCaller is the reason
// RecordAnalysisFailure takes no size/mtime argument.
//
// The strike must record the version the PREDICATES compare against. Binding
// the live stat the job decoded would stamp one world and compare in another,
// so a strike written while the scanner was behind could never match and the
// debounce would never suppress anything — the auto-optimize lesson
// ("staleness compares against the TRACK ROW, and the sweeper stamps from
// that same row") in a different subsystem.
func TestRecordAnalysisFailureStampsFromTheRowNotTheCaller(t *testing.T) {
	s := openAnalysisFailStore(t)
	ctx := context.Background()
	seedAnalysisTrack(t, s, "a/broken.flac", 4096, 1234)
	if _, err := s.RecordAnalysisFailure(ctx, "a/broken.flac", "sox: truncated"); err != nil {
		t.Fatal(err)
	}
	var size, mtime int64
	if err := s.db.QueryRow(
		`SELECT analysis_fail_size, analysis_fail_mtime_ns FROM tracks WHERE path = ?`,
		"a/broken.flac").Scan(&size, &mtime); err != nil {
		t.Fatal(err)
	}
	if size != 4096 || mtime != 1234 {
		t.Fatalf("stamped (size=%d, mtime=%d), want the row's (4096, 1234)", size, mtime)
	}
}

// TestANewFileVersionRestartsTheCount pins the operator's natural remedy:
// replacing the file re-opens it, with no flag and no button.
//
// The old strikes described a file that no longer exists at this path.
// Carrying them over would let two old failures plus one new one cross the
// threshold and suppress a freshly-repaired file on its FIRST refusal.
func TestANewFileVersionRestartsTheCount(t *testing.T) {
	s := openAnalysisFailStore(t)
	ctx := context.Background()
	seedAnalysisTrack(t, s, "a/broken.flac", 4096, 1234)
	for i := 0; i < analysisFailureThreshold; i++ {
		if _, err := s.RecordAnalysisFailure(ctx, "a/broken.flac", "sox: truncated"); err != nil {
			t.Fatal(err)
		}
	}
	if !analysisSuppressed(t, s, "a/broken.flac") {
		t.Fatal("not suppressed after the threshold — the rest of this test proves nothing")
	}

	// The operator replaces the file; the next scan records its new geometry.
	seedAnalysisTrack(t, s, "a/broken.flac", 9001, 5678)
	if analysisSuppressed(t, s, "a/broken.flac") {
		t.Fatal("still suppressed after the file was replaced — the version gate did not re-open it")
	}
	n, err := s.RecordAnalysisFailure(ctx, "a/broken.flac", "sox: truncated again")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("first verdict against the new version returned %d, want 1 "+
			"(stale strikes must not carry over)", n)
	}
	if analysisSuppressed(t, s, "a/broken.flac") {
		t.Error("one verdict against a fresh file version suppressed it")
	}
}

// TestRecordAnalysisFailureDoesNotBumpIndexedAt — a refused decode changes
// nothing a client can see, so it must not enter any paired device's delta.
// The field report was 30 files; a bump per strike per sweep is 30 no-op
// delta rows to every phone, forever.
func TestRecordAnalysisFailureDoesNotBumpIndexedAt(t *testing.T) {
	s := openAnalysisFailStore(t)
	ctx := context.Background()
	seedAnalysisTrack(t, s, "a/broken.flac", 4096, 1234)
	before := indexedAtOf(t, s, "a/broken.flac")
	for i := 0; i < analysisFailureThreshold; i++ {
		if _, err := s.RecordAnalysisFailure(ctx, "a/broken.flac", "sox: truncated"); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.ClearAnalysisFailure(ctx, "a/broken.flac"); err != nil {
		t.Fatal(err)
	}
	if after := indexedAtOf(t, s, "a/broken.flac"); after != before {
		t.Errorf("indexed_at moved %d → %d across the debounce", before, after)
	}
}

// TestASuccessClearsTheCount — the counter measures CONSECUTIVE failures.
// Without the clear it measures lifetime ones, and a file that fails twice a
// year with successes in between eventually suppresses itself.
func TestASuccessClearsTheCount(t *testing.T) {
	s := openAnalysisFailStore(t)
	ctx := context.Background()
	seedAnalysisTrack(t, s, "a/flaky.flac", 4096, 1234)
	for i := 0; i < analysisFailureThreshold-1; i++ {
		if _, err := s.RecordAnalysisFailure(ctx, "a/flaky.flac", "sox: pipe died"); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.ClearAnalysisFailure(ctx, "a/flaky.flac"); err != nil {
		t.Fatal(err)
	}
	n, err := s.RecordAnalysisFailure(ctx, "a/flaky.flac", "sox: pipe died")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("after a success the next verdict returned %d, want 1", n)
	}
}

// TestAnalysisSuppressionExpiresWithTheTTL — the toolchain is what can change
// the answer for a file nobody touched, and it needs no operator action to
// take effect.
func TestAnalysisSuppressionExpiresWithTheTTL(t *testing.T) {
	s := openAnalysisFailStore(t)
	ctx := context.Background()
	base := time.Unix(1_700_000_000, 0)
	s.now = func() time.Time { return base }
	seedAnalysisTrack(t, s, "a/broken.flac", 4096, 1234)
	for i := 0; i < analysisFailureThreshold; i++ {
		if _, err := s.RecordAnalysisFailure(ctx, "a/broken.flac", "sox: truncated"); err != nil {
			t.Fatal(err)
		}
	}
	if !analysisSuppressed(t, s, "a/broken.flac") {
		t.Fatal("not suppressed while fresh")
	}
	s.now = func() time.Time { return base.Add(analysisFailureTTL - time.Hour) }
	if !analysisSuppressed(t, s, "a/broken.flac") {
		t.Error("expired an hour BEFORE the TTL")
	}
	s.now = func() time.Time { return base.Add(analysisFailureTTL + time.Hour) }
	if analysisSuppressed(t, s, "a/broken.flac") {
		t.Error("still suppressed an hour past the TTL — a toolchain upgrade would never get a retry")
	}
}

// TestSuppressedAnalysisPathsCarryTheVersionTheyMatched — the walk holds a
// live stat and must be able to overrule a stale claim. Without the version
// travelling back it could only take the set on trust, and a file replaced
// between the last scan and this sweep would stay suppressed.
func TestSuppressedAnalysisPathsCarryTheVersionTheyMatched(t *testing.T) {
	s := openAnalysisFailStore(t)
	ctx := context.Background()
	seedAnalysisTrack(t, s, "a/broken.flac", 4096, 1234)
	seedAnalysisTrack(t, s, "a/fine.flac", 512, 99)
	for i := 0; i < analysisFailureThreshold; i++ {
		if _, err := s.RecordAnalysisFailure(ctx, "a/broken.flac", "sox: truncated"); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.SuppressedAnalysisPaths(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("suppressed set = %v, want exactly a/broken.flac", got)
	}
	v, ok := got["a/broken.flac"]
	if !ok {
		t.Fatalf("suppressed set = %v, missing a/broken.flac", got)
	}
	if v.SizeBytes != 4096 || v.MTimeNS != 1234 {
		t.Errorf("version = (%d, %d), want (4096, 1234)", v.SizeBytes, v.MTimeNS)
	}
}

// TestClearAnalysisFailuresByPathsRefusesAnEmptyList — an empty path list
// means "nothing was selected", never "so everything qualifies".
//
// This is the `nil` vs `[]string{}` trap the retention reap records, where
// one spelling deleted zero rows and the other deleted every one. Clearing
// the library is a DIFFERENT function here, so the dangerous reading has no
// spelling at all — this test pins that the safe one stays safe.
func TestClearAnalysisFailuresByPathsRefusesAnEmptyList(t *testing.T) {
	s := openAnalysisFailStore(t)
	ctx := context.Background()
	seedAnalysisTrack(t, s, "a/broken.flac", 4096, 1234)
	for i := 0; i < analysisFailureThreshold; i++ {
		if _, err := s.RecordAnalysisFailure(ctx, "a/broken.flac", "sox: truncated"); err != nil {
			t.Fatal(err)
		}
	}
	for _, empty := range [][]string{nil, {}} {
		n, err := s.ClearAnalysisFailuresByPaths(ctx, empty)
		if err != nil {
			t.Fatalf("empty clear returned an error: %v", err)
		}
		if n != 0 {
			t.Fatalf("empty list cleared %d row(s), want 0", n)
		}
		if !analysisSuppressed(t, s, "a/broken.flac") {
			t.Fatal("an empty path list cleared the suppression")
		}
	}
	// The named form still works, so the refusal above is not vacuous.
	n, err := s.ClearAnalysisFailuresByPaths(ctx, []string{"a/broken.flac"})
	if err != nil || n != 1 {
		t.Fatalf("named clear = (%d, %v), want (1, nil)", n, err)
	}
	if analysisSuppressed(t, s, "a/broken.flac") {
		t.Error("still suppressed after an explicit clear")
	}
}

// TestClearAllAnalysisFailuresIsWholeLibrary backs the console's "Retry all"
// and `bridge analyze --retry-failed` with no filter.
func TestClearAllAnalysisFailuresIsWholeLibrary(t *testing.T) {
	s := openAnalysisFailStore(t)
	ctx := context.Background()
	for _, p := range []string{"a/one.flac", "b/two.flac"} {
		seedAnalysisTrack(t, s, p, 4096, 1234)
		if _, err := s.RecordAnalysisFailure(ctx, p, "sox: truncated"); err != nil {
			t.Fatal(err)
		}
	}
	n, err := s.ClearAllAnalysisFailures(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("cleared %d, want 2", n)
	}
	counts, err := s.CountUnreadableTracks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if counts.Recorded != 0 {
		t.Errorf("recorded = %d after clearing the library, want 0", counts.Recorded)
	}
}

// TestUnreadableCountAndListDescribeTheSameSet — the summary heads the list,
// so the two disagreeing is the console telling two stories about one
// library. Both come from the same pair of predicates; this pins that they
// stay that way, including the recorded-vs-suppressed split.
func TestUnreadableCountAndListDescribeTheSameSet(t *testing.T) {
	s := openAnalysisFailStore(t)
	ctx := context.Background()
	seedAnalysisTrack(t, s, "a/gone.flac", 4096, 1234)   // past the threshold
	seedAnalysisTrack(t, s, "b/trying.flac", 2048, 5678) // one verdict only
	seedAnalysisTrack(t, s, "c/fine.flac", 512, 99)      // never refused
	for i := 0; i < analysisFailureThreshold; i++ {
		if _, err := s.RecordAnalysisFailure(ctx, "a/gone.flac", "sox: truncated"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.RecordAnalysisFailure(ctx, "b/trying.flac", "sox: bad header"); err != nil {
		t.Fatal(err)
	}

	counts, err := s.CountUnreadableTracks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if counts.Recorded != 2 || counts.Suppressed != 1 {
		t.Fatalf("counts = %+v, want {Recorded:2 Suppressed:1}", counts)
	}
	rows, err := s.ListUnreadableTracksForAdmin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != counts.Recorded {
		t.Fatalf("list has %d rows, count says %d", len(rows), counts.Recorded)
	}
	var listSuppressed int
	byPath := map[string]AdminUnreadableTrack{}
	for _, r := range rows {
		byPath[r.Path] = r
		if r.Suppressed {
			listSuppressed++
		}
	}
	if listSuppressed != counts.Suppressed {
		t.Errorf("list says %d suppressed, count says %d", listSuppressed, counts.Suppressed)
	}
	gone := byPath["a/gone.flac"]
	if gone.Strikes != analysisFailureThreshold || !gone.Suppressed {
		t.Errorf("a/gone.flac = %+v, want %d strikes and suppressed", gone, analysisFailureThreshold)
	}
	if gone.Reason != "sox: truncated" {
		t.Errorf("reason = %q, want the decoder's own message", gone.Reason)
	}
	if gone.SizeBytes != 4096 {
		t.Errorf("sizeBytes = %d, want 4096", gone.SizeBytes)
	}
	trying := byPath["b/trying.flac"]
	if trying.Strikes != 1 || trying.Suppressed {
		t.Errorf("b/trying.flac = %+v, want 1 strike and NOT suppressed — a file still "+
			"being retried must be visible without being reported as given up on", trying)
	}
	if _, listed := byPath["c/fine.flac"]; listed {
		t.Error("a track that was never refused is in the unreadable list")
	}
}

// TestFirstSeenSurvivesLaterVerdicts — the console renders first-seen so the
// operator can tell a file that broke this morning from one that has been
// broken for a month. A later strike must not restamp it.
func TestFirstSeenSurvivesLaterVerdicts(t *testing.T) {
	s := openAnalysisFailStore(t)
	ctx := context.Background()
	base := time.Unix(1_700_000_000, 0)
	s.now = func() time.Time { return base }
	seedAnalysisTrack(t, s, "a/broken.flac", 4096, 1234)
	if _, err := s.RecordAnalysisFailure(ctx, "a/broken.flac", "sox: truncated"); err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return base.Add(6 * time.Hour) }
	if _, err := s.RecordAnalysisFailure(ctx, "a/broken.flac", "sox: truncated"); err != nil {
		t.Fatal(err)
	}
	rows, err := s.ListUnreadableTracksForAdmin(ctx)
	if err != nil || len(rows) != 1 {
		t.Fatalf("list = (%d rows, %v), want 1", len(rows), err)
	}
	if rows[0].FirstSeenAt != base.UnixNano() {
		t.Errorf("firstSeenAt = %d, want the first verdict's %d", rows[0].FirstSeenAt, base.UnixNano())
	}
	if rows[0].LastSeenAt != base.Add(6*time.Hour).UnixNano() {
		t.Errorf("lastSeenAt = %d, want the latest verdict's %d",
			rows[0].LastSeenAt, base.Add(6*time.Hour).UnixNano())
	}
}

// TestRecordAnalysisFailureBoundsTheStoredReason — the reason is a decoder's
// stderr, i.e. a program's output about an untrusted file, and it is read
// back into a console page. Cap it at the source rather than at every render.
func TestRecordAnalysisFailureBoundsTheStoredReason(t *testing.T) {
	s := openAnalysisFailStore(t)
	ctx := context.Background()
	seedAnalysisTrack(t, s, "a/broken.flac", 4096, 1234)
	if _, err := s.RecordAnalysisFailure(ctx, "a/broken.flac",
		strings.Repeat("é", analysisFailureReasonMax)); err != nil {
		t.Fatal(err)
	}
	rows, err := s.ListUnreadableTracksForAdmin(ctx)
	if err != nil || len(rows) != 1 {
		t.Fatalf("list = (%d rows, %v), want 1", len(rows), err)
	}
	if len(rows[0].Reason) > analysisFailureReasonMax {
		t.Errorf("stored reason is %d bytes, cap is %d", len(rows[0].Reason), analysisFailureReasonMax)
	}
	// A multi-byte rune straddling the cut must not be stored half-written:
	// the row goes out as JSON, and an invalid UTF-8 byte there is a
	// replacement character in the operator's list at best.
	if !utf8ValidString(rows[0].Reason) {
		t.Errorf("stored reason is not valid UTF-8: %q", rows[0].Reason)
	}
}

// TestRecordAnalysisFailureOnAVanishedTrackIsNotAFirstStrike — the pool logs
// on count == 1, and a track deleted between the job starting and the verdict
// landing has no row to strike. Reporting 1 would put a WARN in the journal
// naming a track the manifest no longer has.
func TestRecordAnalysisFailureOnAVanishedTrackIsNotAFirstStrike(t *testing.T) {
	s := openAnalysisFailStore(t)
	n, err := s.RecordAnalysisFailure(context.Background(), "gone/entirely.flac", "sox: truncated")
	if err != nil {
		t.Fatalf("unexpected error for a missing row: %v", err)
	}
	if n != 0 {
		t.Errorf("returned %d for a track with no row, want 0", n)
	}
}

func utf8ValidString(s string) bool {
	for _, r := range s {
		if r == '�' {
			return false
		}
	}
	return true
}

// TestMigrationV46AddsColumnsAndIndexIdempotently — the six columns and the
// partial index exist after a fresh open, and a RE-RUN (version rewound, DDL
// already applied) neither fails nor duplicates. The ladder is append-only and
// every shipped migration has already run on both live bridges, so a re-run is
// the only thing a later edit could ever exercise.
//
// The index is part of the claim, not a detail: without it the unreadable
// count on /api/stats is a full scan of `tracks` on a 5-second SSE tick, which
// is the shape /api/diagnostics was pulled behind a TTL for.
func TestMigrationV46AddsColumnsAndIndexIdempotently(t *testing.T) {
	s := openAnalysisFailStore(t)
	ctx := context.Background()

	cols := []string{
		"analysis_fail_count", "analysis_fail_at", "analysis_fail_first_at",
		"analysis_fail_size", "analysis_fail_mtime_ns", "analysis_fail_reason",
	}
	for _, c := range cols {
		exists, err := atlasColumnExists(s.db, "tracks", c)
		if err != nil {
			t.Fatalf("inspect tracks.%s: %v", c, err)
		}
		if !exists {
			t.Errorf("v46 column tracks.%s missing after a fresh open", c)
		}
	}
	indexExists := func() bool {
		var n int
		if err := s.db.QueryRow(
			`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_tracks_analysis_fail'`).
			Scan(&n); err != nil {
			t.Fatalf("inspect index: %v", err)
		}
		return n == 1
	}
	if !indexExists() {
		t.Error("v46 partial index idx_tracks_analysis_fail missing after a fresh open")
	}

	// A recorded verdict survives the re-run: the migration must not touch
	// data, only shape.
	seedAnalysisTrack(t, s, "a/broken.flac", 4096, 1234)
	if _, err := s.RecordAnalysisFailure(ctx, "a/broken.flac", "sox: source appears truncated"); err != nil {
		t.Fatal(err)
	}

	for run := 1; run <= 2; run++ {
		if _, err := s.db.ExecContext(ctx, `PRAGMA user_version = 45`); err != nil {
			t.Fatalf("rewind user_version: %v", err)
		}
		if err := s.migrate(); err != nil {
			t.Fatalf("re-run %d of migrate: %v", run, err)
		}
		if v, want := readUserVersion(t, s.db), migrations[len(migrations)-1].version; v != want {
			t.Errorf("re-run %d: user_version = %d, want %d", run, v, want)
		}
		if !indexExists() {
			t.Errorf("re-run %d: the partial index is gone", run)
		}
		rows, err := s.ListUnreadableTracksForAdmin(ctx)
		if err != nil || len(rows) != 1 {
			t.Fatalf("re-run %d: list = (%d rows, %v), want the recorded verdict intact", run, len(rows), err)
		}
	}
}
