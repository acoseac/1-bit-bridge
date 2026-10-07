package manifest

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync/atomic"
	"time"
)

// firstIndexedBackfillChunk bounds one autocommit UPDATE so a library
// whose dates are still null does not grow the WAL by the whole table.
const firstIndexedBackfillChunk = 2000

// firstIndexedNullRowidsSQL is the fill's inner select. It is unordered:
// ORDER BY rowid makes SQLite skip idx_tracks_first_indexed_at_null.
// Rows that were filled leave the WHERE, so the loop still drains.
const firstIndexedNullRowidsSQL = `SELECT rowid FROM tracks WHERE first_indexed_at IS NULL LIMIT ?`

// firstIndexedBackfillSQL fills one chunk of rows whose date is still
// null. Its inner select is firstIndexedNullRowidsSQL.
const firstIndexedBackfillSQL = `UPDATE tracks SET first_indexed_at = CASE
		WHEN mtime_ns > 0 AND mtime_ns <= ? THEN mtime_ns
		ELSE ?
	END
	WHERE rowid IN (
		SELECT rowid FROM tracks
		WHERE first_indexed_at IS NULL
		LIMIT ?
	)`

// firstIndexedInsertNS is the INSERT arm's date. A caller that copied a
// previous date passes it; every other insert uses the store clock.
func firstIndexedInsertNS(carry, now int64) int64 {
	if carry > 0 {
		return carry
	}
	return now
}

// spliceFirstIndexedAt copies the column onto the wire field. A missing
// or non-positive value omits the field, which is how a row the backfill
// has not reached stays absent.
func spliceFirstIndexedAt(t *Track, ns sql.NullInt64) {
	if t == nil {
		return
	}
	if !ns.Valid || ns.Int64 <= 0 {
		t.FirstIndexedAt = nil
		return
	}
	at := time.Unix(0, ns.Int64).UTC()
	t.FirstIndexedAt = &at
}

// backfillFirstIndexedAt fills rows the column has not reached. A zero
// or future mtime becomes this store's clock. It does not assign
// indexed_at. Each statement covers one rowid chunk. The same fill runs
// at the end of every open, so a row a rolled-back binary inserted as
// NULL is filled when this binary opens the database again.
func (s *Store) backfillFirstIndexedAt(ctx context.Context) error {
	now := s.now().UnixNano()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		res, err := s.db.ExecContext(ctx, firstIndexedBackfillSQL, now, now, firstIndexedBackfillChunk)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return nil
		}
	}
}

// FirstIndexedAtReady reports whether every track row has a date. A
// failed count is not ready. The predicate matches the partial index.
func (s *Store) FirstIndexedAtReady(ctx context.Context) (bool, error) {
	if s == nil || s.db == nil {
		return false, sql.ErrConnDone
	}
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM tracks WHERE first_indexed_at IS NULL`).Scan(&n)
	if err != nil {
		return false, err
	}
	return n == 0, nil
}

// carryKey is the root's folder name plus the path within that root.
// Both storage forms save and look that key up. A collapse keeps a row
// only when the stored path already begins with the surviving root's
// folder name, and leaves that path as the key. A name that is empty or
// contains a slash is refused.
func carryKey(path string, fromMultiRoot bool, rootBase string) (string, bool) {
	if rootBase == "" || strings.Contains(rootBase, "/") || path == "" {
		return "", false
	}
	if !fromMultiRoot {
		return rootBase + "/" + path, true
	}
	prefix := rootBase + "/"
	if !strings.HasPrefix(path, prefix) || len(path) == len(prefix) {
		return "", false
	}
	return path, true
}

// RecordFirstIndexedCarry snapshots filesystem rows before a root flip
// that changes the stored path form. fromMultiRoot is the form the rows
// have now. rootBase is the existing root's folder name when a second
// root is added, and the surviving root's folder name when several roots
// collapse to one. The saved key is that folder name plus the path
// within the root, in either form. A snapshot with no rows writes
// nothing when no dates are saved yet, and the returned generation is 0.
// When dates are already saved it moves them onto the new generation and
// the form this flip is heading toward, and does not delete the keys. A
// later record merges: it keeps the earlier date for a key. The returned
// generation is the one this call wrote, so a wipe that then fails can
// drop exactly that generation.
func (s *Store) RecordFirstIndexedCarry(ctx context.Context, fromMultiRoot bool, rootBase string) (int64, error) {
	if rootBase == "" || strings.Contains(rootBase, "/") {
		return 0, fmt.Errorf("first-indexed carry: root name %q", rootBase)
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT path, first_indexed_at FROM tracks
		WHERE first_indexed_at IS NOT NULL
		  AND NOT EXISTS (
		      SELECT 1 FROM upnp_track_routing WHERE source_path = tracks.path
		  )`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	best := map[string]int64{}
	for rows.Next() {
		var path string
		var ns int64
		if err := rows.Scan(&path, &ns); err != nil {
			return 0, err
		}
		key, ok := carryKey(path, fromMultiRoot, rootBase)
		if !ok || ns <= 0 {
			continue
		}
		if prev, seen := best[key]; !seen || ns < prev {
			best[key] = ns
		}
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	targetMulti := 0
	if !fromMultiRoot {
		targetMulti = 1
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(best) == 0 {
		var n int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM first_indexed_carry`).Scan(&n); err != nil {
			return 0, err
		}
		if n == 0 {
			return 0, nil
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var maxGen sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT MAX(generation) FROM first_indexed_carry`).Scan(&maxGen); err != nil {
		return 0, err
	}
	next := int64(1)
	if maxGen.Valid {
		next = maxGen.Int64 + 1
	}
	if _, err := tx.ExecContext(ctx, `UPDATE first_indexed_carry SET generation = ?, target_multi = ?`, next, targetMulti); err != nil {
		return 0, err
	}
	const upsert = `INSERT INTO first_indexed_carry (path_key, first_indexed_at, generation, target_multi)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(path_key) DO UPDATE SET
			first_indexed_at = MIN(first_indexed_carry.first_indexed_at, excluded.first_indexed_at),
			generation = excluded.generation,
			target_multi = excluded.target_multi`
	for key, ns := range best {
		if _, err := tx.ExecContext(ctx, upsert, key, ns, next, targetMulti); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	noteRootFlipStage("record")
	return next, nil
}

// rootFlipStage is a test hook. Production leaves it unset. A test
// sets it to cancel the request once the carry has committed, or once
// the wipe has committed, which is the window a client timeout hits.
var rootFlipStage atomic.Value // func(string)

// SetRootFlipStageHookForTest installs fn, called with "record" after
// the carry commits and "wipe" after the filesystem wipe commits. The
// hook runs under the store lock and must not touch the database.
func SetRootFlipStageHookForTest(fn func(string)) {
	if fn == nil {
		rootFlipStage.Store((func(string))(nil))
		return
	}
	rootFlipStage.Store(fn)
}

func noteRootFlipStage(stage string) {
	fn, _ := rootFlipStage.Load().(func(string))
	if fn != nil {
		fn(stage)
	}
}

// abandonCarryTimeout bounds the cleanup of a flip the request did not
// finish. The request context may already be cancelled.
const abandonCarryTimeout = 5 * time.Second

// AbandonFirstIndexedCarry deletes the generation a flip recorded when
// the wipe that was supposed to follow it failed. A later scan then has
// nothing to copy onto a re-added file. A cleanup error is logged and
// does not replace the wipe error the caller returns.
func (s *Store) AbandonFirstIndexedCarry(ctx context.Context, generation int64) {
	if s == nil || generation <= 0 {
		return
	}
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), abandonCarryTimeout)
	defer cancel()
	if err := s.ClearFirstIndexedCarryGeneration(cctx, generation); err != nil {
		logger.Warn("first-indexed carry: could not drop an abandoned flip", "generation", generation, "err", err)
	}
}

// RetargetFirstIndexedCarry points a generation at the form the library
// still has, after the wipe succeeded and the config save did not. The
// compensating scan, or the next scan, then copies the dates and clears
// them. toMulti is false when the library is still a single root. A
// cleanup error is logged and does not replace the save error.
func (s *Store) RetargetFirstIndexedCarry(ctx context.Context, generation int64, toMulti bool) {
	if s == nil || generation <= 0 {
		return
	}
	target := 0
	if toMulti {
		target = 1
	}
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), abandonCarryTimeout)
	defer cancel()
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.db.ExecContext(cctx,
		`UPDATE first_indexed_carry SET target_multi = ? WHERE generation = ?`,
		target, generation); err != nil {
		logger.Warn("first-indexed carry: could not retarget an abandoned flip", "generation", generation, "err", err)
	}
}

// ClearFirstIndexedCarryGeneration drops the saved dates of one
// generation. A scan clears the generation it loaded, and only that one.
func (s *Store) ClearFirstIndexedCarryGeneration(ctx context.Context, generation int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx, `DELETE FROM first_indexed_carry WHERE generation = ?`, generation)
	return err
}

// foldedFirstIndexedSQL is the subtree insert's fold. Both sides go
// through unicode_lower so the lookup is the expression
// idx_tracks_path_unicode_lower indexes. MIN of no rows is NULL.
const foldedFirstIndexedSQL = `SELECT MIN(first_indexed_at) FROM tracks
	WHERE unicode_lower(path) = unicode_lower(?)
	  AND first_indexed_at IS NOT NULL
	  AND NOT EXISTS (
	      SELECT 1 FROM upnp_track_routing WHERE source_path = tracks.path
	  )`

// earliestFoldedFirstIndexed is the earliest date among filesystem rows
// whose path folds to path. A subtree insert asks this once, for that
// path, through the path index.
func (s *Store) earliestFoldedFirstIndexed(ctx context.Context, path string) (int64, bool) {
	var ns sql.NullInt64
	err := s.db.QueryRowContext(ctx, foldedFirstIndexedSQL, path).Scan(&ns)
	if err != nil || !ns.Valid || ns.Int64 <= 0 {
		return 0, false
	}
	return ns.Int64, true
}

// firstIndexedSnap is the dates a scan's inserts may copy. A full scan
// publishes it before the workers start and releases it after they join.
// loaded is set only when both reads completed. generation and
// targetMulti describe the newest saved-date generation.
type firstIndexedSnap struct {
	multi       bool
	loaded      bool
	hasCarry    bool
	generation  int64
	targetMulti bool
	exact       map[string]struct{}
	folded      map[string]int64
	root        map[string]int64
}

func (s *Store) loadFirstIndexedSnap(ctx context.Context, multiRoot bool) (*firstIndexedSnap, error) {
	snap := &firstIndexedSnap{
		multi:  multiRoot,
		exact:  map[string]struct{}{},
		folded: map[string]int64{},
		root:   map[string]int64{},
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT path, first_indexed_at FROM tracks
		WHERE first_indexed_at IS NOT NULL
		  AND NOT EXISTS (
		      SELECT 1 FROM upnp_track_routing WHERE source_path = tracks.path
		  )`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var path string
		var ns int64
		if err := rows.Scan(&path, &ns); err != nil {
			_ = rows.Close()
			return nil, err
		}
		snap.exact[path] = struct{}{}
		if ns <= 0 {
			continue
		}
		key := pathFold(path)
		if prev, ok := snap.folded[key]; !ok || ns < prev {
			snap.folded[key] = ns
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	carried, err := s.db.QueryContext(ctx, `SELECT path_key, first_indexed_at, generation, target_multi FROM first_indexed_carry`)
	if err != nil {
		return nil, err
	}
	defer carried.Close()
	seenGen := false
	for carried.Next() {
		var key string
		var ns, gen, multi int64
		if err := carried.Scan(&key, &ns, &gen, &multi); err != nil {
			return nil, err
		}
		if ns > 0 && key != "" {
			snap.root[key] = ns
			snap.hasCarry = true
		}
		if !seenGen || gen > snap.generation {
			seenGen = true
			snap.generation = gen
			snap.targetMulti = multi != 0
		}
	}
	if err := carried.Err(); err != nil {
		return nil, err
	}
	snap.loaded = true
	return snap, nil
}

func (s *Scanner) publishFirstIndexed(ctx context.Context, multiRoot bool) {
	snap, err := s.store.loadFirstIndexedSnap(ctx, multiRoot)
	if err != nil {
		scanLogger.Warn("first-indexed snapshot", "err", err)
		s.firstIndexed = nil
		return
	}
	s.firstIndexed = snap
}

// releaseFirstIndexed drops the snapshot after the workers have joined.
// The caller clears the generation only when this scan loaded it and the
// library is already in the form that record was saving toward.
func (s *Scanner) releaseFirstIndexed() (clear bool, generation int64) {
	snap := s.firstIndexed
	s.firstIndexed = nil
	if snap == nil || !snap.loaded || !snap.hasCarry || snap.multi != snap.targetMulti {
		return false, 0
	}
	return true, snap.generation
}

// savedDateKey is the carry key for a path this scan is about to insert.
// A multi-root scan already stores the root's folder name. A single-root
// scan prefixes the folder name of the root the walk started with.
func (s *Scanner) savedDateKey(path string, multiRoot bool) string {
	if multiRoot || s.scanSingleRootBase == "" || path == "" {
		return path
	}
	return s.scanSingleRootBase + "/" + path
}

// noteFirstIndexed copies a previous row's date onto an insert whose
// exact path is not already stored. A saved date is looked up by the
// root's folder name plus the path within it, which is the same key in
// both storage forms. A case-only fold of the stored path is the
// fallback. An existing path is an update and records nothing. A full
// scan reads the snapshot it published. A subtree scan, and a full scan
// whose snapshot failed, looks the one path up in the store.
func (s *Scanner) noteFirstIndexed(ctx context.Context, t *Track, multiRoot bool) {
	if t == nil || t.carryFirstIndexedNS > 0 || t.Path == "" {
		return
	}
	key := s.savedDateKey(t.Path, multiRoot)
	if snap := s.firstIndexed; snap != nil {
		if _, exists := snap.exact[t.Path]; exists {
			return
		}
		if ns, ok := snap.root[key]; ok && ns > 0 {
			t.carryFirstIndexedNS = ns
			return
		}
		if ns, ok := snap.folded[pathFold(t.Path)]; ok && ns > 0 {
			t.carryFirstIndexedNS = ns
		}
		return
	}
	s.noteFirstIndexedFromStore(ctx, t, key)
}

func (s *Scanner) noteFirstIndexedFromStore(ctx context.Context, t *Track, key string) {
	var one int
	err := s.store.db.QueryRowContext(ctx, `SELECT 1 FROM tracks WHERE path = ?`, t.Path).Scan(&one)
	if err == nil {
		return
	}
	if err != sql.ErrNoRows {
		return
	}
	var ns sql.NullInt64
	err = s.store.db.QueryRowContext(ctx, `SELECT first_indexed_at FROM first_indexed_carry WHERE path_key = ?`, key).Scan(&ns)
	if err == nil && ns.Valid && ns.Int64 > 0 {
		t.carryFirstIndexedNS = ns.Int64
		return
	}
	if err != nil && err != sql.ErrNoRows {
		return
	}
	if got, ok := s.store.earliestFoldedFirstIndexed(ctx, t.Path); ok && got > 0 {
		t.carryFirstIndexedNS = got
	}
}
