package manifest

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"
)

// Analysis-failure debounce (migration v46).
//
// # Why this exists
//
// A failed analysis writes no `track_analysis` row, and every candidate
// query selects tracks that LACK a fresh waveform. So a source that can never
// decode is re-selected on every sweep and every `bridge analyze` run,
// forever. Field report 2026-09-21: 30 truncated FLACs in the operator's
// library failed identically on every pass — `sox: decoded 51.5s of 357.2s
// probed — source appears truncated` — producing 1,385 WARN lines in 7 days
// on one host and re-failing at every start on the next. The files were
// byte-identical to the backup, i.e. genuinely truncated at the source, and
// nothing told the operator which ones to replace.
//
// This is the transcode debounce (v39, variant_failure.go) applied one
// subsystem over, and the three ideas behind it are the same ones — a
// CONSECUTIVE-failure threshold, a VERSION GATE on (size, mtime_ns), and a
// TTL. Read that file's docblock for why each was chosen. Two things differ,
// and both come from this path having something the transcode path does not:
// the decoder's own verdict.
//
//  1. CLASSIFICATION. `internal/analyze` tells a fact about the FILE from a
//     fact about everything else (analyze.ErrSourceUnreadable — see
//     failure.go), so only a decoder verdict is ever recorded here. A missing
//     sox, a faulted read, a killed child and a full output volume record
//     NOTHING. v39 could not make that distinction and debounces on any
//     failure; it is the more conservative position and the threshold is what
//     carries it.
//  2. A REASON and a FIRST-SEEN stamp, because the point is not only to stop
//     retrying — it is to tell the operator which files to replace. They are
//     what `GET /api/analysis/unreadable` renders.
//
// # What must NOT be recorded
//
// Only failures that are a property of the FILE. The pool excludes shutdown
// cancellation and the per-job timeout at the call site before it even asks
// the classifier, exactly as the transcode pool does: the first says nothing
// about the source and the second is as likely to mean a hung mount as a
// pathological file.
//
// The columns are COLUMN-ONLY: no `json:` tags, never spliced onto wire
// output — the same rule the v25 format facts and the v28/v37/v38/v39 columns
// follow. The admin surface reads them through a DTO.

// analysisFailureThreshold is how many CONSECUTIVE decode verdicts against
// the same file version suppress it. Three matches variantFailureThreshold
// and the scanner's missing-count threshold: enough that a transient
// environment fault has to recur across three separate sweeps before it can
// sideline a file, few enough that a genuinely broken source stops costing a
// full decode quickly.
//
// It costs no log noise to wait: the WARN is already deduplicated to one line
// per (path, size, mtime), so the threshold buys retries, not silence.
const analysisFailureThreshold = 3

// analysisFailureThresholdSQL is the threshold inlined as a literal so the
// predicates stay true Go consts (the shape that keeps SonarCloud's go:S2077
// quiet — see variantFailureThresholdSQL).
const analysisFailureThresholdSQL = "3"

// analysisFailureTTL is how long a suppression lasts before the source is
// tried once more.
//
// Shorter than variantFailureTTL's 30 days on purpose. Both exist because the
// TOOLCHAIN can change the answer, but a suppressed analysis also costs the
// operator something a suppressed transcode does not: no waveform, no
// loudness, no key or tempo, so the track is missing from the scrubber and
// from every smart mix. One decode per broken file per week is a cheap price
// for that not being permanent — 30 files is 30 decodes.
//
// Repairing the file needs no TTL at all: it changes (size, mtime_ns) and the
// version gate re-opens the candidate on the next sweep.
const analysisFailureTTL = 7 * 24 * time.Hour

// analysisFailureReasonMax bounds the stored decoder message. The reasons
// this path produces are one line (a truncation verdict, or a decoder's exit
// status plus its redacted stderr), but stderr is attacker-adjacent — it
// comes from a program parsing an untrusted file — and this column is read
// back into a console page.
const analysisFailureReasonMax = 500

// analysisFailureRecordedSQL is the shared predicate: this track has at least
// one decode verdict against THIS version of the file, recent enough to still
// count.
//
// Takes one bind: the cutoff (now - TTL) in unix nanos.
//
// The leading `analysis_fail_count != 0` term is not redundant with the
// version comparison. It is what lets SQLite use the partial index (v46,
// `WHERE analysis_fail_count != 0`) instead of scanning `tracks`, and it is
// the unambiguous "never failed" discriminator for a row whose zeroed
// fail_size/fail_mtime_ns could in principle equal a real file's.
//
// Columns are UNQUALIFIED so the predicate drops into queries that alias
// `tracks` as well as those that don't — same rule as
// variantFailureSuppressedSQL.
const analysisFailureRecordedSQL = `(
	analysis_fail_count != 0
	AND analysis_fail_size = size
	AND analysis_fail_mtime_ns = mtime_ns
	AND analysis_fail_at > ?
)`

// analysisFailureSuppressedSQL is analysisFailureRecordedSQL AND the
// threshold: enough consecutive verdicts to stop offering the track as a
// candidate. Written once and referenced by every candidate query and count
// so the CLI, the serve-side sweeper and the console cannot drift apart.
//
// The leading `analysis_fail_count != 0` is NOT redundant with the `>= 3`
// beside it, and this predicate did without it until the v46 index was
// measured against it. SQLite admits a partial index only when a query term
// matches the index's WHERE expression; it does not reason that `x >= 3`
// implies `x != 0`. So the term the index is built on has to APPEAR, and this
// predicate — which is the ENTIRE WHERE of SuppressedAnalysisPaths — dropped
// it while v46's own comment claimed "every predicate leads with
// analysis_fail_count != 0 so the planner can use it".
//
// Measured with EXPLAIN QUERY PLAN over 5,000 rows (sqlite 3.54.0):
//
//	recorded            SCAN tracks USING INDEX idx_tracks_analysis_fail
//	suppressed, before  SCAN tracks                       <- full table scan
//	suppressed, after   SEARCH tracks USING INDEX idx_tracks_analysis_fail
//	                                       (analysis_fail_count>?)
//
// A scan here steps every row of `tracks` — whose tags_json BLOB sits ahead
// of these columns in the record — to return the handful of suppressed paths,
// on the hourly serve-side sweep and on every `bridge analyze`.
//
// The earlier docblock said this was the recorded predicate "plus" the
// threshold. It REPLACED the `!= 0` term rather than adding to it, and that
// replacement is exactly what cost the index.
//
// Takes one bind: the cutoff, as above. The threshold is INLINED
// (analysisFailureThresholdSQL is the literal "3"), which is what keeps
// analysisFailureSuppressedSQL2's single-Replace derivation sound —
// TestAnalysisFailurePredicatesTakeOneBind pins it.
const analysisFailureSuppressedSQL = `(
	analysis_fail_count != 0
	AND analysis_fail_count >= ` + analysisFailureThresholdSQL + `
	AND analysis_fail_size = size
	AND analysis_fail_mtime_ns = mtime_ns
	AND analysis_fail_at > ?
)`

// analysisFailureSuppressedSQL2 is analysisFailureSuppressedSQL with its one
// placeholder numbered ?2, for a query that already uses ?1.
//
// DERIVED, never retyped. Two hand-written copies of a predicate is the
// "both wrong together" shape CLAUDE.md records against hand-copied
// cross-cycle contracts; deriving it means a change to the predicate reaches
// both call sites or neither. TestAnalysisFailurePredicatesTakeOneBind pins
// the single-placeholder assumption the Replace rests on.
//
// Numbering matters because SQLite assigns a BARE `?` the next positional
// index: dropped unchanged into AnalysisCoverage it would become ?2 today by
// accident and ?3 the moment a term is inserted above it, binding the schema
// version where a cutoff belongs.
var analysisFailureSuppressedSQL2 = strings.Replace(analysisFailureSuppressedSQL, "?", "?2", 1)

// AnalysisFailureCutoff is the bind value for both predicates: verdicts older
// than this no longer count.
func (s *Store) AnalysisFailureCutoff() int64 {
	return s.now().Add(-analysisFailureTTL).UnixNano()
}

// AnalysisFailureThreshold exposes the consecutive-verdict threshold so the
// admin surface can say WHY a track is listed but not yet suppressed without
// restating the number.
func AnalysisFailureThreshold() int { return analysisFailureThreshold }

// RecordAnalysisFailure adds one decode verdict against `path` and returns
// the resulting consecutive count.
//
// The returned count is the caller's LOG GATE: 1 means this is the first
// verdict against this version of this file, which is the one occurrence
// worth a WARN. Every later strike describes the same file failing the same
// way, and logging it again is what produced 1,385 lines for 30 files.
//
// A verdict against a DIFFERENT version resets the count to 1 rather than
// incrementing (and re-stamps first-seen): the previous strikes described a
// file that no longer exists at this path, and carrying them over would let
// an old failure plus one new one cross the threshold.
//
// # The version is taken FROM THE ROW, never from the caller
//
// `analysis_fail_size`/`analysis_fail_mtime_ns` are stamped from `size` and
// `mtime_ns` in the same statement that reads them, so the columns a strike
// records are by construction the columns the predicates compare it against.
// The alternative — binding the live stat the job decoded — stamps one
// world and compares in another, and a strike written while the scanner is
// behind would then never match and never suppress. That is the
// auto-optimize lesson stated exactly: staleness compares against the TRACK
// ROW, and the writer stamps from that same row.
//
// The candidate walk still holds a live stat and still gets to disagree —
// see SuppressedAnalysisPaths, which hands it the version it matched on so a
// file replaced since the last scan is retried rather than suppressed. The
// disagreement is resolved in the direction of doing the work.
//
// # The residual, and why it is left
//
// That disagreement does not converge while it lasts. A suppressed file
// that is TOUCHED and is still unreadable — same path, new mtime, no scan
// yet — is offered by the walk on every sweep (live mtime != the strike's),
// fails, and re-stamps from the row, which still matches the row, so the
// count climbs and nothing suppresses. One wasted decode per sweep per
// file, silent: the WARN is gated on `count == 1`, which never recurs.
//
// It IS self-terminating. The next scan writes the new mtime onto the
// track row, the strike then mismatches it, the count resets to 1, and
// three further failures suppress the file for good. So the cost is
// bounded by the scan interval (six hours by default), and the state
// needs a file that is corrupt, already suppressed, modified on disk,
// STILL corrupt, and racing the scanner.
//
// The obvious fix — stamp the live stat the job decoded — moves the bug
// rather than closing it: every predicate above compares the strike
// against `size`/`mtime_ns`, so a strike recorded against a version the
// row does not know fails `analysisFailureRecordedSQL` and the row leaves
// the suppressed set entirely. Closing it properly means the walk's query
// dropping the version terms (its live comparison is strictly better
// information) while `AnalysisCoverage` and `CountUnreadableTracks`, which
// have no filesystem, either keep them — and then transiently disagree
// with the walk, reintroducing the remainder-that-never-drains symptom
// #947 fixed — or drop them too and over-count suppression in the same
// window. Reviewed externally (Gemini, 2026-09-22) and declined on the
// balance: a predicate-shape change across three surfaces, plus divergence
// from the variant debounce this one deliberately mirrors, against a
// bounded and self-clearing cost.
//
// Deliberately does NOT touch `indexed_at`. A suppressed analysis changes
// nothing a client can see, and bumping it would push a no-op delta row to
// every paired device — the same reason RecordVariantFailure doesn't.
//
// Returns 0 when no row matched (the track was deleted mid-job), which reads
// as "not the first strike" and so logs nothing. That is the right direction:
// the file is gone, and a WARN naming it would be about a track the manifest
// no longer has.
func (s *Store) RecordAnalysisFailure(ctx context.Context, path, reason string) (int, error) {
	reason = trimReasonToRuneBoundary(reason, analysisFailureReasonMax)
	now := s.now().UnixNano()
	s.mu.Lock()
	defer s.mu.Unlock()
	// One UPDATE then one SELECT rather than RETURNING: every writer in this
	// package holds s.mu, so the pair is atomic against other writers, and
	// readers of a half-applied strike do not exist (the count is only ever
	// read to decide a log line and a candidacy).
	if _, err := s.db.ExecContext(ctx, `
		UPDATE tracks
		   SET analysis_fail_count = CASE
		           WHEN analysis_fail_count != 0
		            AND analysis_fail_size = size AND analysis_fail_mtime_ns = mtime_ns
		           THEN analysis_fail_count + 1
		           ELSE 1
		       END,
		       analysis_fail_first_at = CASE
		           WHEN analysis_fail_count != 0
		            AND analysis_fail_size = size AND analysis_fail_mtime_ns = mtime_ns
		           THEN analysis_fail_first_at
		           ELSE ?
		       END,
		       analysis_fail_at       = ?,
		       analysis_fail_size     = size,
		       analysis_fail_mtime_ns = mtime_ns,
		       analysis_fail_reason   = ?
		 WHERE path = ?`,
		now, now, reason, path); err != nil {
		return 0, err
	}
	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT analysis_fail_count FROM tracks WHERE path = ?`, path).Scan(&n); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// The track was removed between the job starting and the verdict
			// landing. Not an error the pool can act on, and not a first
			// strike either.
			return 0, nil
		}
		// Anything else is a fault, and the caller's log gate must not read
		// it as "no row": the UPDATE above has already committed a strike, so
		// reporting zero both loses the error and mis-sequences the WARN.
		return 0, err
	}
	return n, nil
}

// ClearAnalysisFailure zeroes the verdict record for `path`. Called on every
// successful analysis, so the counter measures CONSECUTIVE failures rather
// than lifetime ones — without this a file that fails twice over a year, with
// successes in between, would eventually suppress itself.
func (s *Store) ClearAnalysisFailure(ctx context.Context, path string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx, `
		UPDATE tracks
		   SET analysis_fail_count = 0, analysis_fail_at = 0,
		       analysis_fail_first_at = 0, analysis_fail_size = 0,
		       analysis_fail_mtime_ns = 0, analysis_fail_reason = ''
		 WHERE path = ? AND analysis_fail_count != 0`, path)
	return err
}

// ClearAllAnalysisFailures re-opens every recorded source in the library,
// backing the console's "Retry" button and `bridge analyze --retry-failed`
// with no filter.
//
// Deliberately a SEPARATE function from the by-paths form rather than its
// empty case. An empty path list means "nothing was selected"; clearing the
// library is a different instruction, and the two must not be spellable the
// same way — that is the `nil` vs `[]string{}` trap the retention reap
// records, where one spelling deleted zero rows and the other deleted every
// one.
func (s *Store) ClearAllAnalysisFailures(ctx context.Context) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.ExecContext(ctx, `
		UPDATE tracks
		   SET analysis_fail_count = 0, analysis_fail_at = 0,
		       analysis_fail_first_at = 0, analysis_fail_size = 0,
		       analysis_fail_mtime_ns = 0, analysis_fail_reason = ''
		 WHERE analysis_fail_count != 0`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ClearAnalysisFailuresByPaths re-opens exactly the named sources.
//
// The explicit-path form exists because the CLI's scope flag is `--filter`, a
// case-sensitive SUBSTRING match — not a prefix — so there is no byte range
// that expresses it and ClearAnalysisFailuresUnderPrefix would be the wrong
// tool. Same shape as ResetEnrichedByPaths, the fingerprint sweeper's
// explicit-path enrichment reset.
//
// An empty list clears NOTHING and is not an error: a filter that matched no
// recorded failure is an ordinary outcome, and the dangerous reading of an
// empty set — "so everything qualifies" — is the one every reaper in this
// tree is required to refuse.
func (s *Store) ClearAnalysisFailuresByPaths(ctx context.Context, paths []string) (int64, error) {
	if len(paths) == 0 {
		return 0, nil
	}
	blob, err := json.Marshal(paths)
	if err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.ExecContext(ctx, `
		UPDATE tracks
		   SET analysis_fail_count = 0, analysis_fail_at = 0,
		       analysis_fail_first_at = 0, analysis_fail_size = 0,
		       analysis_fail_mtime_ns = 0, analysis_fail_reason = ''
		 WHERE analysis_fail_count != 0
		   AND path IN (SELECT value FROM json_each(?))`, string(blob))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// AnalysisSuppression is the file version a suppression was recorded
// against — the manifest row's size and mtime at the time of the last
// verdict.
//
// It travels with the path so the candidate walk, which holds a LIVE stat,
// can check the claim instead of taking it. See SuppressedAnalysisPaths.
type AnalysisSuppression struct {
	SizeBytes int64
	MTimeNS   int64
}

// SuppressedAnalysisPaths returns the tracks the candidate walk should skip,
// each with the file version its verdicts were recorded against.
//
// A SET rather than a per-path query because collectAnalysisCandidates
// already runs one GetAnalysis per track; asking a second question per path
// would double the query count on a walk that visits the whole library. The
// set is bounded by how many files are broken, which is small by construction
// — and when it isn't, it is exactly the number the operator needs to see.
//
// The version travels back because the walk knows something this query
// cannot: what is on disk RIGHT NOW. Between an operator replacing a broken
// file and the next scan, the row still describes the old one, so a
// suppression keyed to it would hold against a file that no longer exists.
// The walk compares and, on any disagreement, analyses — the same posture
// the scanner's "we could not see this path" rule takes, resolved toward
// doing the work rather than toward the stored claim.
func (s *Store) SuppressedAnalysisPaths(ctx context.Context) (map[string]AnalysisSuppression, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT path, analysis_fail_size, analysis_fail_mtime_ns
		   FROM tracks WHERE `+analysisFailureSuppressedSQL,
		s.AnalysisFailureCutoff())
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := make(map[string]AnalysisSuppression)
	for rows.Next() {
		var (
			p string
			v AnalysisSuppression
		)
		if err := rows.Scan(&p, &v.SizeBytes, &v.MTimeNS); err != nil {
			return nil, err
		}
		out[p] = v
	}
	return out, rows.Err()
}

// AnalysisFailureCounts is the operator-facing summary: how many tracks
// carry a current decode verdict, and how many of those have crossed the
// threshold and are no longer being retried.
//
// Recorded >= Suppressed always. The gap is files on their way to being
// given up on, and showing only the suppressed number would make a file that
// has failed twice invisible until the third sweep.
type AnalysisFailureCounts struct {
	Recorded   int
	Suppressed int
}

// CountUnreadableTracks reports both numbers in ONE query, so the summary and
// the list it heads cannot disagree about the library they describe.
func (s *Store) CountUnreadableTracks(ctx context.Context) (AnalysisFailureCounts, error) {
	var c AnalysisFailureCounts
	cutoff := s.AnalysisFailureCutoff()
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*),
		       COALESCE(SUM(CASE WHEN `+analysisFailureSuppressedSQL+` THEN 1 ELSE 0 END), 0)
		  FROM tracks
		 WHERE `+analysisFailureRecordedSQL,
		cutoff, cutoff).Scan(&c.Recorded, &c.Suppressed)
	if err != nil {
		return AnalysisFailureCounts{}, err
	}
	return c, nil
}

// AdminUnreadableTrack is one row of the console's unreadable list. A domain
// type, not a wire DTO — internal/admin wraps it (the wire-type rule).
type AdminUnreadableTrack struct {
	Path string
	// Reason is the decoder's own message, already redacted of absolute
	// paths at the point it was produced (analyze.decodeFramesWith).
	Reason string
	// Strikes is how many consecutive verdicts this file version has drawn.
	Strikes int
	// Suppressed is whether it has crossed the threshold, i.e. whether the
	// bridge has stopped retrying it. Derived from the SAME predicate the
	// candidate walk uses, in the same query, rather than recomputed from
	// Strikes — a second copy of the rule is a second thing to keep in step.
	Suppressed bool
	// FirstSeenAt / LastSeenAt are unix nanos, both for THIS file version.
	FirstSeenAt int64
	LastSeenAt  int64
	// SizeBytes is the size the verdicts were drawn against, which is also
	// the size on disk (the version gate is what put this row in the list).
	SizeBytes int64
}

// ListUnreadableTracksForAdmin returns every track carrying a current decode
// verdict, most-recently-failed first.
//
// Unbounded on purpose, for the reason apiDeletedPlaylistsList gives: the set
// is one row per file the decoders have refused three sweeps running, which
// is small by construction, and a cap would hide exactly the rows a mass
// import just created — the case this list exists for.
func (s *Store) ListUnreadableTracksForAdmin(ctx context.Context) ([]AdminUnreadableTrack, error) {
	cutoff := s.AnalysisFailureCutoff()
	rows, err := s.db.QueryContext(ctx, `
		SELECT path, analysis_fail_reason, analysis_fail_count,
		       `+analysisFailureSuppressedSQL+`,
		       analysis_fail_first_at, analysis_fail_at, size
		  FROM tracks
		 WHERE `+analysisFailureRecordedSQL+`
		 ORDER BY analysis_fail_at DESC, path ASC`, cutoff, cutoff)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []AdminUnreadableTrack
	for rows.Next() {
		var t AdminUnreadableTrack
		if err := rows.Scan(&t.Path, &t.Reason, &t.Strikes, &t.Suppressed,
			&t.FirstSeenAt, &t.LastSeenAt, &t.SizeBytes); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// trimReasonToRuneBoundary caps a decoder message at max BYTES without
// leaving a partial rune at the cut.
//
// Trims at most utf8.UTFMax-1 trailing bytes, never "until the whole string
// validates": this input is a decoder's stderr about a corrupt file, so it
// can carry INTERIOR invalid bytes, and the validate-the-world loop would
// discard everything after the first bad byte at O(N²). Lockstep twin of
// tailscale.trimPartialTrailingRune and transcode's copy; a string that still
// ends in genuinely invalid bytes is returned as-is, the same posture those
// take for interior garbage.
//
// A review asked it to inspect the original suffix first, so a PRE-EXISTING
// invalid byte that happens to sit at the cut is preserved rather than
// dropped (DecodeLastRuneInString returns RuneError,1 for both cases and
// cannot tell them apart). Declined: the effect is at most three bytes off
// the end of a DISPLAY-ONLY string, and the change would make this copy
// diverge from the two it is deliberately identical to — which is the more
// expensive defect, since the whole point of naming them lockstep twins is
// that a reader can check one and trust the others. (CodeRabbit on #947.)
func trimReasonToRuneBoundary(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	s = s[:maxBytes]
	for i := 0; i < utf8.UTFMax-1 && len(s) > 0; i++ {
		r, size := utf8.DecodeLastRuneInString(s)
		if r != utf8.RuneError || size != 1 {
			return s
		}
		s = s[:len(s)-1]
	}
	return s
}
