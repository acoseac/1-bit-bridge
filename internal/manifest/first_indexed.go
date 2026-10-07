package manifest

import (
	"context"
	"database/sql"
	"time"

	"golang.org/x/text/cases"
	"golang.org/x/text/language"
)

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
	tm := time.Unix(0, ns.Int64).UTC()
	t.FirstIndexedAt = &tm
}

// firstIndexedInsertNS is the value an INSERT records. A caller that
// carried a previous row's date passes it; every other insert uses now.
func firstIndexedInsertNS(carry, now int64) int64 {
	if carry > 0 {
		return carry
	}
	return now
}

// backfillFirstIndexedAt fills rows the column has not reached, once,
// from the stored file mtime. A zero or future mtime becomes now. It
// does not move the row's change cursor.
func (s *Store) backfillFirstIndexedAt(ctx context.Context) error {
	now := s.now().UnixNano()
	_, err := s.db.ExecContext(ctx, `
		UPDATE tracks
		SET first_indexed_at = CASE
			WHEN mtime_ns > 0 AND mtime_ns <= ? THEN mtime_ns
			ELSE ?
		END
		WHERE first_indexed_at IS NULL`, now, now)
	return err
}

// FirstIndexedAtReady reports whether every track row has a first-indexed
// date. An empty library is ready. A query error is not.
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

// rootFlipKey is the library-relative path with a multi-root basename
// taken off. Record uses the form the rows are stored in; lookup uses
// the form the scan is about to write.
func rootFlipKey(path string, multiRoot bool) string {
	if !multiRoot {
		return path
	}
	for i := 0; i < len(path); i++ {
		if path[i] == '/' {
			return path[i+1:]
		}
	}
	return path
}

// RecordFirstIndexedCarry snapshots each filesystem row's date, keyed by
// the path with the current root-basename prefix removed, before a
// single/multi root flip deletes those rows. A path collision keeps the
// earliest date. An empty snapshot leaves a snapshot already stored: a
// retry after the wipe must not replace it with nothing.
func (s *Store) RecordFirstIndexedCarry(ctx context.Context, fromMultiRoot bool) error {
	rows, err := s.db.QueryContext(ctx, `
		SELECT path, first_indexed_at FROM tracks
		WHERE first_indexed_at IS NOT NULL
		  AND NOT EXISTS (
		      SELECT 1 FROM upnp_track_routing WHERE source_path = tracks.path
		  )`)
	if err != nil {
		return err
	}
	best := map[string]int64{}
	for rows.Next() {
		var path string
		var ns int64
		if err := rows.Scan(&path, &ns); err != nil {
			_ = rows.Close()
			return err
		}
		if ns <= 0 {
			continue
		}
		key := rootFlipKey(path, fromMultiRoot)
		if prev, ok := best[key]; !ok || ns < prev {
			best[key] = ns
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if len(best) == 0 {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM first_indexed_carry`); err != nil {
		return err
	}
	for key, ns := range best {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO first_indexed_carry (path_key, first_indexed_at) VALUES (?, ?)`,
			key, ns); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ClearFirstIndexedCarry drops a snapshot a successful full scan has applied.
func (s *Store) ClearFirstIndexedCarry(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx, `DELETE FROM first_indexed_carry`)
	return err
}

// firstIndexedSnap is the dates a scan's inserts may copy. Published
// before the workers start and only read until they join.
type firstIndexedSnap struct {
	multi  bool
	exact  map[string]struct{}
	folded map[string]int64
	root   map[string]int64
}

func (s *Store) loadFirstIndexedSnap(ctx context.Context, multiRoot bool) (*firstIndexedSnap, error) {
	snap := &firstIndexedSnap{
		multi:  multiRoot,
		exact:  map[string]struct{}{},
		folded: map[string]int64{},
		root:   map[string]int64{},
	}
	fold := cases.Lower(language.Und)
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
		key := fold.String(path)
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
	carried, err := s.db.QueryContext(ctx, `SELECT path_key, first_indexed_at FROM first_indexed_carry`)
	if err != nil {
		return nil, err
	}
	defer carried.Close()
	for carried.Next() {
		var key string
		var ns int64
		if err := carried.Scan(&key, &ns); err != nil {
			return nil, err
		}
		if ns > 0 {
			snap.root[key] = ns
		}
	}
	if err := carried.Err(); err != nil {
		return nil, err
	}
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

// noteFirstIndexed copies a previous row's date onto an insert whose
// exact path is not already stored. A root-flip key wins over a
// case-only fold. An existing path is an update and records nothing.
func (s *Scanner) noteFirstIndexed(t *Track) {
	snap := s.firstIndexed
	if snap == nil || t == nil || t.carryFirstIndexedNS > 0 {
		return
	}
	if _, exists := snap.exact[t.Path]; exists {
		return
	}
	if ns, ok := snap.root[rootFlipKey(t.Path, snap.multi)]; ok && ns > 0 {
		t.carryFirstIndexedNS = ns
		return
	}
	if ns, ok := snap.folded[cases.Lower(language.Und).String(t.Path)]; ok && ns > 0 {
		t.carryFirstIndexedNS = ns
	}
}
