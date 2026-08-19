// The deletion journal: server-side tombstones that let iOS DELTA syncs
// apply track removals.
//
// Before this table, `/v1/manifest?since=` could only ADD or UPDATE rows —
// deletions reached a paired client exclusively via a user-initiated Full
// rescan (iOS's Phase-5 full-scan diff), so server-side removals (a deleted
// album, a dupe-suppression pass, a threshold reap) lingered on every synced
// phone indefinitely. Every track-DELETE writer in the store now records the
// reaped paths into `manifest_deletions` (migration v41) inside the SAME
// transaction as its DELETE, and the manifest's since-leg emits
// `deleted: [paths]` for tombstones newer than the client's cursor.
//
// Same-predicate-same-binds discipline: each journal INSERT derives its WHERE
// from the exact predicate its sibling DELETE uses — compile-time (const +
// const) where the DELETE's predicate is a const, the `buildPathInQuery`
// runtime form where the DELETE itself already assembles at runtime — so the
// journaled set and the deleted set cannot diverge (the
// `thresholdReapBatchWhereSQL` convention; `TestDeletionJournalPredicatesAreShared`
// pins the const derivations).
//
// Coverage semantics: `deletion_journal_coverage_start_ns` (scan_state) is
// the instant from which the journal is COMPLETE. A client whose `since`
// predates it cannot be given a trustworthy deletion list, so the delta
// response answers `deltaIncomplete: true` and the client escalates to a
// full sync. Coverage resets (start = now, tombstones wiped) on the
// mass-delete paths where per-path tombstones would be wrong-shaped anyway:
// WipeAllTracks / WipeFilesystemTracks (the single↔multi-root flip — every
// path changes shape, a full resync is the only correct answer) and a
// DeleteTracksBatch call past the mass-op guard. Retention pruning advances
// the coverage start to the prune horizon for the same reason.
//
// Suppression rides the same table: a dupe-suppression 0→1 transition
// journals a tombstone (the row leaves the SERVED set — before this, iOS
// deltas never learned and suppressed copies lingered until a full sync);
// serving a row again (1→0, or any upsert of a served row) clears its
// tombstone. `indexed_at` is still NEVER bumped on suppression — the
// tombstone is the delta signal now, and the existing served-transition
// bump stays the re-appearance signal.
package manifest

import (
	"context"
	"database/sql"
	"strconv"
	"time"
)

// deletionJournalCoverageKey is the scan_state key holding the UnixNano
// instant from which the journal is complete. Seeded by migration v41's
// post(); reset by the wipe paths + the mass-op guard; advanced by
// PruneDeletionJournal.
const deletionJournalCoverageKey = "deletion_journal_coverage_start_ns"

// deletionJournalRetention bounds tombstone age. A client syncing less
// often than this gets `deltaIncomplete` (→ full sync) instead of a
// deletion list with holes.
const deletionJournalRetention = 180 * 24 * time.Hour

// Mass-op guard (DeleteTracksBatch): past either bound the call resets
// journal coverage instead of writing per-path tombstones — a deletion
// this large is a reorganization, and a full resync is both cheaper and
// safer for clients than a five-digit tombstone list.
const (
	deletionJournalMassOpAbsolute = 10_000
	// deletionJournalMassOpLibraryFraction: >25% of the library.
	deletionJournalMassOpLibraryDivisor = 4
)

// manifestDeltaDeletedCap bounds the `deleted` array a single delta
// response will carry. Past it the response answers `deltaIncomplete`
// instead — cheap insurance against a pathological journal.
const manifestDeltaDeletedCap = 20_000

// Journal INSERT fragments. Prefix + <the sibling DELETE's WHERE> +
// suffix, concatenated at COMPILE time where the predicate is a const —
// the same const-derivation that keeps thresholdReap's unlink set and
// row set in lockstep (and keeps SonarCloud go:S2077 quiet).
const (
	journalInsertPrefixSQL = `INSERT INTO manifest_deletions(path, deleted_at)
		SELECT path, ? FROM tracks WHERE `
	journalInsertSuffixSQL = `
		ON CONFLICT(path) DO UPDATE SET deleted_at = excluded.deleted_at`

	// Threshold reap (both legs) — predicates shared verbatim with the
	// DELETE arms in store.go.
	journalThresholdReapBatchSQL = journalInsertPrefixSQL + thresholdReapBatchWhereSQL + journalInsertSuffixSQL
	journalThresholdReapOneSQL   = journalInsertPrefixSQL + thresholdReapOneWhereSQL + journalInsertSuffixSQL

	// DeleteTracksByPrefix — predicate shared with its DELETE via
	// deleteByPrefixWhereSQL.
	journalDeleteByPrefixSQL = journalInsertPrefixSQL + deleteByPrefixWhereSQL + journalInsertSuffixSQL

	// ClearMissingCounts — predicate shared with its DELETE via
	// clearMissingTracksWhereSQL.
	journalClearMissingSQL = journalInsertPrefixSQL + clearMissingTracksWhereSQL + journalInsertSuffixSQL

	// Single-path sites (DeleteTrack; ApplyDupeStamps' suppression leg).
	journalSinglePathSQL = journalInsertPrefixSQL + `path = ?` + journalInsertSuffixSQL
)

// clearTombstoneServedGuardSQL is the ONE spelling of "a served row
// exists for this tombstone's path". The `dupe_suppressed = 0` term is
// load-bearing: a content-changed-but-still-suppressed upsert must KEEP
// its tombstone (the row is still absent from the served set). Both
// clear forms below derive from it at compile time so the per-path and
// batch variants cannot drift (the journal-INSERT const-derivation
// convention).
const clearTombstoneServedGuardSQL = `EXISTS (SELECT 1 FROM tracks
	                WHERE tracks.path = manifest_deletions.path
	                  AND tracks.dupe_suppressed = 0)`

// clearTombstoneIfServedSQL removes a single path's tombstone when a
// SERVED row exists for it — the single-row upsert path (UpsertTrack).
const clearTombstoneIfServedSQL = `DELETE FROM manifest_deletions
	 WHERE path = ?
	   AND ` + clearTombstoneServedGuardSQL

// clearAllServedTombstonesSQL sweeps EVERY tombstone whose path has a
// served row — UpsertTrackBatch runs it once at the end of its
// transaction instead of a per-row point delete (O(1) statements
// instead of O(batch); the journal is usually tiny, so the correlated
// EXISTS scan is cheap). Semantically a superset of the per-row clear:
// it enforces the same invariant ("no served path carries a tombstone")
// table-wide, which can only remove tombstones that should not exist.
const clearAllServedTombstonesSQL = `DELETE FROM manifest_deletions
	 WHERE ` + clearTombstoneServedGuardSQL

// clearTombstoneSQL removes a path's tombstone unconditionally —
// ApplyDupeStamps' served-transition leg, where the row's stamp write in
// the same transaction is what makes it served.
const clearTombstoneSQL = `DELETE FROM manifest_deletions WHERE path = ?`

// resetDeletionJournalCoverageTx wipes every tombstone and restarts
// coverage at nowNs, inside the caller's transaction. Direct scan_state
// upsert (NOT SetScanState — the callers already hold s.mu and
// SetScanState would deadlock re-acquiring it).
func resetDeletionJournalCoverageTx(ctx context.Context, tx *sql.Tx, nowNs int64) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM manifest_deletions`); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO scan_state(k, v) VALUES(?, ?)
		ON CONFLICT(k) DO UPDATE SET v = excluded.v
	`, deletionJournalCoverageKey, strconv.FormatInt(nowNs, 10))
	return err
}

// PruneDeletionJournal drops tombstones older than the retention window
// and advances the coverage start to the prune horizon (a `since` older
// than the horizon can no longer be answered completely). Called from
// Scan's success tail. Returns the pruned row count.
func (s *Store) PruneDeletionJournal(ctx context.Context) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := s.now().Add(-deletionJournalRetention).UnixNano()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `DELETE FROM manifest_deletions WHERE deleted_at < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		// Coverage start = max(existing, cutoff). Only advances when
		// something was actually pruned — an empty prune leaves the
		// (older, still complete) coverage window intact.
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO scan_state(k, v) VALUES(?, ?)
			ON CONFLICT(k) DO UPDATE SET v = MAX(CAST(excluded.v AS INTEGER), CAST(scan_state.v AS INTEGER))
		`, deletionJournalCoverageKey, strconv.FormatInt(cutoff, 10)); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return n, nil
}

// DeletedSince returns the tombstoned paths whose deletion is newer than
// `since`, capped at manifestDeltaDeletedCap (+1 probe). overflow=true
// means the cap was exceeded and the caller should answer
// `deltaIncomplete` instead of a truncated list.
//
// The NOT EXISTS guard keeps a path out of the list while a SERVED row
// carries it — a delta response must never name a path in BOTH `tracks`
// and `deleted` (the upsert-side tombstone clear makes overlap
// structurally rare; this makes the wire contract airtight regardless).
func (s *Store) DeletedSince(ctx context.Context, since time.Time) ([]string, bool, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT path FROM manifest_deletions
		 WHERE deleted_at > ?
		   AND NOT EXISTS (SELECT 1 FROM tracks
		                    WHERE tracks.path = manifest_deletions.path
		                      AND tracks.dupe_suppressed = 0)
		 ORDER BY path
		 LIMIT ?`, since.UnixNano(), manifestDeltaDeletedCap+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var paths []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, false, err
		}
		paths = append(paths, p)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if len(paths) > manifestDeltaDeletedCap {
		return nil, true, nil
	}
	return paths, false, nil
}

// DeltaSinceCovered reports whether the journal is complete for a client
// syncing from `since`. False when the coverage marker is absent (a
// pre-v41 store state that should not occur post-migration — answered
// conservatively as NOT covered) or when `since` predates it.
func (s *Store) DeltaSinceCovered(ctx context.Context, since time.Time) (bool, error) {
	raw, err := s.GetScanState(ctx, deletionJournalCoverageKey)
	if err != nil {
		return false, err
	}
	if raw == "" {
		return false, nil
	}
	startNs, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return false, nil // unparseable marker → conservative
	}
	return since.UnixNano() >= startNs, nil
}

// deltaDeletionFields resolves the since-leg's `deleted` +
// `deltaIncomplete` pair — shared by BuildManifest and
// writeManifestGated so the buffered and streaming legs cannot drift
// (the parity test compares both).
//
// Failure direction: any read error answers `deltaIncomplete: true`
// with no list. A client escalating to a full sync on a transient DB
// blip is expensive but SAFE; silently omitting deletions is the exact
// divergence the journal exists to close.
func deltaDeletionFields(ctx context.Context, store *Store, since time.Time) ([]string, bool) {
	covered, err := store.DeltaSinceCovered(ctx, since)
	if err != nil {
		logger.Warn("delta coverage probe failed", "err", err)
		return nil, true
	}
	if !covered {
		return nil, true
	}
	deleted, overflow, err := store.DeletedSince(ctx, since)
	if err != nil {
		logger.Warn("delta deleted-since query failed", "err", err)
		return nil, true
	}
	if overflow {
		return nil, true
	}
	return deleted, false
}
