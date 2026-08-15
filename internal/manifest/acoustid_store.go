package manifest

import (
	"context"
	"encoding/json"
	"fmt"
)

// ResetEnrichedByPaths re-queues the given tracks for enrichment.
//
// # A new sanctioned enriched_at writer
//
// The closed list of writers exists because `WHERE enriched_at = 0` drives the
// enrichment worker, so anything that zeroes it decides what the bridge will
// re-crawl. This one is deliberately the narrowest of them: it takes an
// explicit path set rather than a predicate, so it can only ever re-queue rows
// a caller has already identified one by one.
//
// The caller is the fingerprint sweeper, handing over the tracks it just
// resolved. Those rows are about to gain MBIDs, so re-enriching them is not
// churn — it is the write actually landing.
//
// # Why not ResetEnrichedMisses
//
// The obvious alternative would be to have the sweeper call the existing
// library-wide reset. That predicate selects roughly half the library (~9,000
// rows on the production bridge), MarkEnriched strictly advances indexed_at,
// and every sweep would therefore push a ~9,000-track delta to every paired
// iOS device. That is the PR #369 wipe-loop class on a timer. The scoped form
// keeps the delta proportional to what actually changed.
//
// # Contract
//
//   - resets enriched_at ONLY; indexed_at is untouched here, and is advanced
//     later by MarkEnriched when the re-enrichment commits real data
//   - only rows already marked enriched are touched, so it cannot disturb work
//     the enricher has queued
//   - the path set travels as ONE bound JSON array consumed via json_each: a
//     single static statement, no placeholder construction (no go:S2077
//     surface) and no bind-variable-ceiling chunking
//
// Holds s.mu, like every other writer.
func (s *Store) ResetEnrichedByPaths(ctx context.Context, paths []string) (int64, error) {
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
		UPDATE tracks SET enriched_at = 0
		 WHERE enriched_at > 0
		   AND path IN (SELECT value FROM json_each(?))
	`, string(blob))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// SetAcoustIDMatch records which AcoustID cluster the sweeper ACCEPTED for a
// track, as provenance for the fingerprint fallback.
//
// Read the verb precisely, because the obvious undo depends on it. This is
// written when the gate accepts, which is before the enricher has applied
// anything — and the enricher can still refuse at apply time, where the
// local-tag veto runs against the row's own tags. A row can also reach the
// sweeper already holding a text-derived ArtistMBID, since a candidate only
// needs ONE of the two MBIDs missing. So presence means "a fingerprint answer
// was accepted for this path", NOT "every MBID on this row came from audio".
// An undo may therefore use this column to SELECT rows, but must not blindly
// clear their MBIDs — some of those are text-derived and predate the match.
//
// Column-only: it never reaches tags_json and never reaches the wire, so it
// costs no ProtocolVersion bump and no iOS mirror. See the v28 migration for
// why the feature needs provenance at all — briefly, a fingerprint match
// carries a residual error rate that text matching does not, and without this
// an MBID written from audio is indistinguishable from one written from tags
// forever.
//
// Deliberately does NOT touch enriched_at or indexed_at. It is a record of how
// a row was resolved, not a change to what the row says, and the enrichment
// write that accompanies it already advances indexed_at on its own.
func (s *Store) SetAcoustIDMatch(ctx context.Context, path, acoustID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx,
		`UPDATE tracks SET acoustid_match = ? WHERE path = ?`, acoustID, path)
	return err
}

// AcoustIDMatch returns the recorded provenance for a track, or "" when the
// row was not resolved by fingerprinting. Read-only, so no mutex — WAL handles
// concurrent readers.
func (s *Store) AcoustIDMatch(ctx context.Context, path string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx,
		`SELECT acoustid_match FROM tracks WHERE path = ?`, path).Scan(&v)
	return v, err
}

// AcoustIDMatchedPaths returns every track path carrying fingerprint
// provenance (a non-empty acoustid_match), as a set.
//
// The consumer is the sweeper's candidate pass. acoustid_match is column-only
// — never in tags_json — so the Track rows StreamTracks hands the sweeper
// cannot carry it; the sweep fetches this set once up front instead (the
// routedPathSet shape from the scanner's missing passes).
//
// Membership still means exactly what SetAcoustIDMatch's docblock says: a
// fingerprint answer was ACCEPTED for this path, not that it was applied. The
// apply-time veto can refuse the verdict, and a restart can lose the re-queue
// before the enricher consumes it. Whether the answer actually LANDED is
// therefore not this column's to say — the caller combines membership with row
// state (ArtistMBID on the streamed Track) to tell the two apart.
//
// Read-only, so no mutex — WAL handles concurrent readers.
func (s *Store) AcoustIDMatchedPaths(ctx context.Context) (map[string]struct{}, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT path FROM tracks WHERE acoustid_match != ''`)
	if err != nil {
		return nil, fmt.Errorf("manifest: AcoustIDMatchedPaths: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make(map[string]struct{})
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("manifest: scan acoustid-matched path: %w", err)
		}
		out[p] = struct{}{}
	}
	return out, rows.Err()
}

// NoMatchRecord is the file version a persisted no-match verdict was computed
// from. It is deliberately NOT a Track field: like acoustid_match, these are
// columns only, so nothing here can reach tags_json or the wire.
type NoMatchRecord struct {
	Size    int64
	MTimeNS int64
}

// SetAcoustIDNoMatch records that AcoustID answered cleanly and knew nothing
// about this file version.
//
// # Why this is persisted at all
//
// The in-memory outcome cache already stops a repeat decode inside one
// process, and the Cache docblock records that a persistent marker was
// considered and REJECTED — it fights "Retry missing", and AcoustID's database
// grows, so an old no-match deserves re-checking. Production measurement
// reopened that decision: on a bridge whose library is an rclone/B2 FUSE mount,
// every restart re-decoded ~500 candidates, and because a no-match wrote
// nothing, the same unanswerable rows were paid for again on the next restart,
// forever. The saving is not the HTTP call; it is the whole-object read.
//
// Both original objections are answered rather than ignored: the `_at` stamp is
// what the TTL is measured against (see fingerprintNoMatchTTL), and the retry
// paths clear these rows outright, so the button still MEANS "try again".
//
// The (size, mtimeNS) pair is the file version the verdict applies to. Storing
// it — rather than trusting the row's current values — is what makes the marker
// self-invalidating: after a re-encode or tag edit the scanner rewrites
// tags_json, the pair no longer matches, and the row re-enters the candidate
// pool with no upsert-path change and no migration backfill.
//
// Deliberately touches NEITHER enriched_at NOR indexed_at. This records what
// was learned about a file, not a change to what the row says — a bump here
// would push a delta to every paired device for a column they never receive.
func (s *Store) SetAcoustIDNoMatch(ctx context.Context, path string, size, mtimeNS int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx, `
		UPDATE tracks
		   SET acoustid_nomatch_at       = ?,
		       acoustid_nomatch_size     = ?,
		       acoustid_nomatch_mtime_ns = ?
		 WHERE path = ?`,
		s.now().UnixNano(), size, mtimeNS, path)
	return err
}

// FreshAcoustIDNoMatches returns the no-match verdicts stamped at or after
// notBefore, keyed by track path.
//
// Bounded by the TTL cutoff on purpose: a verdict older than that is due for
// re-checking anyway, so fetching it would grow the map without ever
// suppressing anything. The caller compares each record against the row's
// CURRENT size+mtime — that comparison cannot be pushed into SQL cheaply,
// because a track's live size and mtime live inside the tags_json blob (an
// RFC3339Nano string for the time), not in comparable columns.
//
// Read-only, so no mutex — WAL handles concurrent readers.
func (s *Store) FreshAcoustIDNoMatches(ctx context.Context, notBefore int64) (map[string]NoMatchRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT path, acoustid_nomatch_size, acoustid_nomatch_mtime_ns
		  FROM tracks
		 WHERE acoustid_nomatch_at >= ?
		   AND acoustid_nomatch_at > 0`, notBefore)
	if err != nil {
		return nil, fmt.Errorf("manifest: FreshAcoustIDNoMatches: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make(map[string]NoMatchRecord)
	for rows.Next() {
		var p string
		var rec NoMatchRecord
		if err := rows.Scan(&p, &rec.Size, &rec.MTimeNS); err != nil {
			return nil, fmt.Errorf("manifest: scan no-match verdict: %w", err)
		}
		out[p] = rec
	}
	return out, rows.Err()
}

// ClearAcoustIDNoMatches drops every persisted no-match verdict, putting those
// rows back in the fingerprint candidate pool on the next sweep.
//
// This is what keeps the operator's "Retry missing" button honest — it MEANS
// "try again", and a persisted negative that survived it would silently narrow
// the retry to whatever the enricher alone could fix. Touches no timestamp
// column: clearing a verdict is not a change to what the row says.
func (s *Store) ClearAcoustIDNoMatches(ctx context.Context) (int64, error) {
	return s.execClearNoMatch(ctx, clearNoMatchAllSQL)
}

// ClearAcoustIDNoMatchesUnderPrefix is the folder-scoped twin, for the
// Inspector's per-folder retry.
//
// Uses the BYTE-RANGE bound, never LIKE: this is a WRITE, and SQLite's default
// case-insensitive LIKE would reach a case-twin sibling directory, which on a
// case-sensitive filesystem is a different directory entirely. An unscoped
// prefix delegates to the library-wide form — decided AFTER the trim, so a
// slash-only value cannot fall through to a range that silently clears
// nothing.
func (s *Store) ClearAcoustIDNoMatchesUnderPrefix(ctx context.Context, prefix string) (int64, error) {
	base, scoped := subtreeRangeBase(prefix)
	if !scoped {
		return s.ClearAcoustIDNoMatches(ctx)
	}
	return s.execClearNoMatch(ctx, clearNoMatchUnderPrefixSQL, base, base)
}

// execClearNoMatch runs one of the two clear statements under the writer lock.
// Shared so the scope is the ONLY thing that differs between them.
func (s *Store) execClearNoMatch(ctx context.Context, stmt string, args ...any) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.ExecContext(ctx, stmt, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// clearNoMatchSQL is the shared UPDATE head. Split out so the two statements
// below cannot drift in WHICH columns they reset while differing only in
// scope.
//
// The two full statements are compile-time `const`s rather than a
// concatenation performed at the call site. That is deliberate: both forms are
// equally static, but only this one keeps SonarCloud's go:S2077
// ("dynamically formatted SQL query") quiet, and a suppression comment is not
// honoured for Go. Same shape as trackFeatureSelect's callers. Neither carries
// an interpolated value — the scope travels as bind parameters.
const clearNoMatchSQL = `
	UPDATE tracks
	   SET acoustid_nomatch_at       = 0,
	       acoustid_nomatch_size     = 0,
	       acoustid_nomatch_mtime_ns = 0`

const clearNoMatchAllSQL = clearNoMatchSQL + `
	 WHERE acoustid_nomatch_at > 0`

const clearNoMatchUnderPrefixSQL = clearNoMatchSQL + `
	 WHERE acoustid_nomatch_at > 0
	   AND path COLLATE BINARY >= ? || '/'
	   AND path COLLATE BINARY <  ? || '0'`

// CountAcoustIDMatches reports how many rows carry fingerprint provenance —
// the number an operator wants when deciding whether the feature is pulling
// its weight, and the number to check before undoing it.
func (s *Store) CountAcoustIDMatches(ctx context.Context) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM tracks WHERE acoustid_match != ''`).Scan(&n)
	return n, err
}
