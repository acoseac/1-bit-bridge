package manifest

import (
	"context"
	"time"
)

// Transcode-failure debounce (migration v39).
//
// # Why this exists
//
// A failed conversion writes no `track_variants` row, and every candidate
// query selects tracks that LACK a fresh variant. So a source that fails
// permanently is re-selected on every batch and every auto-optimize sweep,
// forever — an unbounded retry loop burning CPU on work that cannot succeed.
//
// PR #716 closed the ALAC instance of this by refusing sources the installed
// sox cannot decode. This closes the GENERAL case: a corrupt or truncated
// file, a bad header, an unreadable-but-present file — anything that clears
// every gate and then fails in the decoder.
//
// # Shape
//
// Three ideas, each borrowed from an existing pattern in this codebase rather
// than invented:
//
//  1. A CONSECUTIVE-FAILURE DEBOUNCE, like the scanner's `missing_count`
//     threshold before it reaps a row. One failure proves nothing — a full
//     disk, a momentarily-dropped mount, or an OOM all produce a non-zero sox
//     exit for a perfectly good file. Suppressing only after
//     `variantFailureThreshold` consecutive strikes costs a bad file a bounded
//     handful of retries while keeping a transient environment failure from
//     sidelining real work. A single success clears the counter.
//
//  2. A VERSION GATE on (size, mtime_ns), like the acoustid no-match marker
//     (v37). The strikes describe ONE version of the file, so re-encoding or
//     repairing it re-opens the candidate automatically — the operator's
//     natural remedy needs no button. Both columns are kept current by the
//     upserts, so the comparison is a plain SQL predicate.
//
//  3. A TTL, like the acoustid markers. What changes over time here is the
//     TOOLCHAIN — a sox upgrade can add a format or fix a decoder bug — so a
//     suppression that never expired would outlive its own justification.
//
// # What must NOT be recorded
//
// Only failures that are a property of the FILE. The pool records a strike
// exclusively on a genuine sox failure: shutdown cancellation and the per-job
// timeout are both excluded at the call site, because the first says nothing
// about the source and the second is as likely to mean a hung mount as a
// pathological file. This mirrors the acoustid rule that a lookup ERROR (a
// fact about the upstream) never persists while a no-match (a fact about the
// audio) does.
//
// The columns are COLUMN-ONLY: no `json:` tags, never spliced onto wire
// output — the same rule the v25 format facts and the v28/v37/v38 acoustid
// columns follow.

// variantFailureThreshold is how many CONSECUTIVE failures of the same file
// version suppress it. Three matches the scanner's missing-count threshold:
// enough that a transient environment fault has to recur before it can
// sideline a file, few enough that a genuinely broken source stops costing
// CPU quickly.
const variantFailureThreshold = 3

// variantFailureTTL is how long a suppression lasts before the source is
// retried once more. A sox upgrade is the thing that can change the answer,
// and it needs no operator action to take effect.
const variantFailureTTL = 30 * 24 * time.Hour

// variantFailureSuppressedSQL is the shared predicate: this track has failed
// enough consecutive times, on THIS version of the file, recently enough to
// still count. Written once and referenced by every candidate query so the
// batch walks and the auto-optimize sweeper cannot drift apart.
//
// Takes one bind: the cutoff (now - TTL) in unix nanos.
//
// Columns are UNQUALIFIED so the predicate drops into queries that alias
// `tracks` (the auto-optimize candidate query uses `FROM tracks t`) as well as
// those that don't. Unambiguous in both: `tracks` is the only table in the
// outer FROM at every use site, and the joined tables carry differently-named
// columns (`track_variants.source_size`, not `size`).
const variantFailureSuppressedSQL = `(
	variant_fail_count >= ` + variantFailureThresholdSQL + `
	AND variant_fail_size = size
	AND variant_fail_mtime_ns = mtime_ns
	AND variant_fail_at > ?
)`

// variantFailureThresholdSQL is the threshold inlined as a literal so the
// predicate stays a true Go const (the shape that keeps SonarCloud's
// go:S2077 quiet — see the FavoritedTrackFeatures note in CLAUDE.md).
const variantFailureThresholdSQL = "3"

// VariantFailureCutoff is the bind value for variantFailureSuppressedSQL:
// strikes older than this no longer suppress.
func (s *Store) VariantFailureCutoff() int64 {
	return s.now().Add(-variantFailureTTL).UnixNano()
}

// RecordVariantFailure adds one strike against `path` for the file version
// described by (size, mtimeNS).
//
// A strike against a DIFFERENT version resets the count to 1 rather than
// incrementing: the previous strikes described a file that no longer exists
// at this path, and carrying them over would let an old failure plus one new
// one cross the threshold.
//
// Deliberately does NOT touch `indexed_at` — a suppressed conversion changes
// nothing a client can see, and bumping it would push a no-op delta row to
// every paired device.
func (s *Store) RecordVariantFailure(ctx context.Context, path string, size, mtimeNS int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx, `
		UPDATE tracks
		   SET variant_fail_count = CASE
		           WHEN variant_fail_size = ? AND variant_fail_mtime_ns = ?
		           THEN variant_fail_count + 1
		           ELSE 1
		       END,
		       variant_fail_at       = ?,
		       variant_fail_size     = ?,
		       variant_fail_mtime_ns = ?
		 WHERE path = ?`,
		size, mtimeNS, s.now().UnixNano(), size, mtimeNS, path)
	return err
}

// ClearVariantFailure zeroes the strike record for `path`. Called on every
// successful conversion, so the counter measures CONSECUTIVE failures rather
// than lifetime ones — without this a file that fails twice over a year, with
// successes in between, would eventually suppress itself.
func (s *Store) ClearVariantFailure(ctx context.Context, path string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx, `
		UPDATE tracks
		   SET variant_fail_count = 0, variant_fail_at = 0,
		       variant_fail_size = 0, variant_fail_mtime_ns = 0
		 WHERE path = ? AND variant_fail_count != 0`, path)
	return err
}

// ClearVariantFailuresUnderPrefix re-opens every suppressed source under a
// library-relative prefix, backing the operator's "retry" action. An empty
// prefix clears the whole library.
//
// Byte-ranged, never LIKE: this is a path-keyed WRITE, and SQLite's LIKE
// folds ASCII case by default, so the LIKE form would also clear a
// case-twin sibling directory (see the prefix-case entry in CLAUDE.md).
func (s *Store) ClearVariantFailuresUnderPrefix(ctx context.Context, prefix string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if base, scoped := subtreeRangeBase(prefix); scoped {
		res, err := s.db.ExecContext(ctx, `
			UPDATE tracks
			   SET variant_fail_count = 0, variant_fail_at = 0,
			       variant_fail_size = 0, variant_fail_mtime_ns = 0
			 WHERE variant_fail_count != 0
			   AND path COLLATE BINARY >= ? || '/'
			   AND path COLLATE BINARY <  ? || '0'`, base, base)
		if err != nil {
			return 0, err
		}
		return res.RowsAffected()
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE tracks
		   SET variant_fail_count = 0, variant_fail_at = 0,
		       variant_fail_size = 0, variant_fail_mtime_ns = 0
		 WHERE variant_fail_count != 0`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// SuppressedVariantFailureCount reports how many sources are currently
// suppressed, for the admin Jobs card — so a shrinking backlog that never
// reaches zero has a visible explanation instead of looking stuck.
func (s *Store) SuppressedVariantFailureCount(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM tracks WHERE `+variantFailureSuppressedSQL,
		s.VariantFailureCutoff()).Scan(&n)
	return n, err
}
