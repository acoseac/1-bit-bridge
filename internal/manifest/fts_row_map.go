package manifest

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"strings"
	"sync/atomic"
)

// ftsRowMapVersion is the migration that addresses tracks_fts by rowid.
// A delete keyed on path scans the whole FTS table; the map makes it one
// rowid lookup. The constant is the version migrateFTSRowMap re-reads
// inside its transaction, so a second opener that arrives after the first
// has committed leaves the map alone.
const ftsRowMapVersion = 55

// The hooks and counters exist so a test can overlap two opens, and so it
// can fail the migration after the map is filled and still inside the
// transaction. They are nil in production.
var (
	ftsRowMapArrived      func()
	ftsRowMapAfterFill    func() error
	ftsRowMapBeforeCommit func()
	ftsRowMapBuilds       atomic.Int32
	ftsRowMapSkipped      atomic.Int32
)

// migrateFTSRowMap builds tracks_fts_row and replaces the FTS triggers.
// It runs as migration 55's post step, in one immediate transaction, and
// stamps user_version itself so a crash cannot leave the new triggers
// behind on the old version. A database with no tracks_fts (FTS5 was
// missing when v7 ran) is left alone.
//
// Any disagreement between tracks, tracks_fts, and the map — a duplicate
// path, an FTS row with no track, a track with no FTS row — rebuilds the
// FTS table from tracks and refills the map. A second disagreement rolls
// the transaction back.
func migrateFTSRowMap(db *sql.DB) error {
	ctx := context.Background()
	var present int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'tracks_fts'`).Scan(&present); err != nil {
		return fmt.Errorf("probe tracks_fts: %w", err)
	}
	if present == 0 {
		return nil
	}
	if hook := ftsRowMapArrived; hook != nil {
		hook()
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("tracks_fts row map: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		_ = conn.Close()
		return fmt.Errorf("tracks_fts row map: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			// A rollback that itself fails leaves the connection inside
			// the transaction. Discard it, or the pool hands the lock
			// to the next checkout.
			if _, rerr := conn.ExecContext(context.Background(), `ROLLBACK`); rerr != nil {
				_ = conn.Raw(func(any) error { return driver.ErrBadConn })
			}
		}
		_ = conn.Close()
	}()

	var version int
	if err := conn.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return fmt.Errorf("tracks_fts row map version: %w", err)
	}
	if version >= ftsRowMapVersion {
		ftsRowMapSkipped.Add(1)
		return nil
	}
	ftsRowMapBuilds.Add(1)
	if err := installFTSRowMap(ctx, conn); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version = %d`, ftsRowMapVersion)); err != nil {
		return fmt.Errorf("tracks_fts row map version: %w", err)
	}
	if hook := ftsRowMapBeforeCommit; hook != nil {
		hook()
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		return fmt.Errorf("tracks_fts row map commit: %w", err)
	}
	committed = true
	return nil
}

func installFTSRowMap(ctx context.Context, conn *sql.Conn) error {
	if _, err := conn.ExecContext(ctx, `
		DROP TRIGGER IF EXISTS tracks_fts_ai;
		DROP TRIGGER IF EXISTS tracks_fts_au;
		DROP TRIGGER IF EXISTS tracks_fts_ad;
		DROP TABLE IF EXISTS tracks_fts_row;
		CREATE TABLE tracks_fts_row (
			path TEXT PRIMARY KEY,
			fts_rowid INTEGER NOT NULL UNIQUE
		) WITHOUT ROWID`); err != nil {
		return fmt.Errorf("tracks_fts row map: %w", err)
	}
	// A duplicate path in tracks_fts fails this insert. The statement
	// rolls back and the transaction stays open, so the census below
	// sees an empty map and takes the rebuild.
	_, fillErr := conn.ExecContext(ctx, `INSERT INTO tracks_fts_row(path, fts_rowid) SELECT path, rowid FROM tracks_fts`)
	if fillErr != nil && !uniqueConstraint(fillErr) {
		return fmt.Errorf("tracks_fts row map fill: %w", fillErr)
	}
	census, err := readFTSCensus(ctx, conn)
	if err != nil {
		return err
	}
	if fillErr != nil || !census.consistent() {
		logger.Warn("tracks_fts row map rebuilt",
			"tracks", census.tracks,
			"fts", census.fts,
			"mapped", census.mapped,
			"map_without_track", census.mapWithoutTrack,
			"track_without_map", census.trackWithoutMap)
		if err := rebuildFTSFromTracks(ctx, conn); err != nil {
			return err
		}
		census, err = readFTSCensus(ctx, conn)
		if err != nil {
			return err
		}
		if !census.consistent() {
			return fmt.Errorf("tracks_fts row map still inconsistent: tracks=%d fts=%d mapped=%d map_without_track=%d track_without_map=%d",
				census.tracks, census.fts, census.mapped, census.mapWithoutTrack, census.trackWithoutMap)
		}
	}
	if hook := ftsRowMapAfterFill; hook != nil {
		if err := hook(); err != nil {
			return err
		}
	}
	if _, err := conn.ExecContext(ctx, ftsRowMapTriggers); err != nil {
		return fmt.Errorf("tracks_fts row map triggers: %w", err)
	}
	return nil
}

// ftsRowMapTriggers keeps tracks_fts and tracks_fts_row together.
// last_insert_rowid is read in the same trigger that inserted the FTS
// row: SQLite restores the previous value when the trigger returns.
// The update trigger fires only when path, title, artist, or album
// changes. A tags_json write that changes something else (an MBID)
// leaves the FTS row where it is. Invalid JSON cannot match, so the
// body runs instead of skipping a document that may be stale.
// json_extract raises on malformed JSON in this SQLite, so each
// extract sits inside a json_valid CASE. On valid JSON the value is
// the same as v7's expression:
//
//	COALESCE(json_extract(...), '')
const ftsRowMapTriggers = `
CREATE TRIGGER tracks_fts_ai AFTER INSERT ON tracks BEGIN
  INSERT INTO tracks_fts(path, title, artist, album) VALUES (
    new.path,
    COALESCE(CASE WHEN json_valid(new.tags_json) THEN json_extract(new.tags_json, '$.title') END, ''),
    COALESCE(CASE WHEN json_valid(new.tags_json) THEN json_extract(new.tags_json, '$.artist') END, ''),
    COALESCE(CASE WHEN json_valid(new.tags_json) THEN json_extract(new.tags_json, '$.album') END, '')
  );
  INSERT INTO tracks_fts_row(path, fts_rowid) VALUES (new.path, last_insert_rowid());
END;
CREATE TRIGGER tracks_fts_ad AFTER DELETE ON tracks BEGIN
  DELETE FROM tracks_fts WHERE rowid = (SELECT fts_rowid FROM tracks_fts_row WHERE path = old.path);
  DELETE FROM tracks_fts_row WHERE path = old.path;
END;
CREATE TRIGGER tracks_fts_au
AFTER UPDATE OF path, tags_json ON tracks
WHEN old.path != new.path
  OR json_valid(old.tags_json) IS NOT 1
  OR json_valid(new.tags_json) IS NOT 1
  OR COALESCE(CASE WHEN json_valid(old.tags_json) THEN json_extract(old.tags_json, '$.title') END, '') != COALESCE(CASE WHEN json_valid(new.tags_json) THEN json_extract(new.tags_json, '$.title') END, '')
  OR COALESCE(CASE WHEN json_valid(old.tags_json) THEN json_extract(old.tags_json, '$.artist') END, '') != COALESCE(CASE WHEN json_valid(new.tags_json) THEN json_extract(new.tags_json, '$.artist') END, '')
  OR COALESCE(CASE WHEN json_valid(old.tags_json) THEN json_extract(old.tags_json, '$.album') END, '') != COALESCE(CASE WHEN json_valid(new.tags_json) THEN json_extract(new.tags_json, '$.album') END, '')
BEGIN
  DELETE FROM tracks_fts WHERE rowid = (SELECT fts_rowid FROM tracks_fts_row WHERE path = old.path);
  DELETE FROM tracks_fts_row WHERE path = old.path;
  INSERT INTO tracks_fts(path, title, artist, album) VALUES (
    new.path,
    COALESCE(CASE WHEN json_valid(new.tags_json) THEN json_extract(new.tags_json, '$.title') END, ''),
    COALESCE(CASE WHEN json_valid(new.tags_json) THEN json_extract(new.tags_json, '$.artist') END, ''),
    COALESCE(CASE WHEN json_valid(new.tags_json) THEN json_extract(new.tags_json, '$.album') END, '')
  );
  INSERT INTO tracks_fts_row(path, fts_rowid) VALUES (new.path, last_insert_rowid());
END;
`

func rebuildFTSFromTracks(ctx context.Context, conn *sql.Conn) error {
	if _, err := conn.ExecContext(ctx, `DELETE FROM tracks_fts`); err != nil {
		return fmt.Errorf("tracks_fts rebuild: %w", err)
	}
	// The same expressions v7 used to fill tracks_fts from tracks.
	if _, err := conn.ExecContext(ctx, `
		INSERT INTO tracks_fts(path, title, artist, album)
		SELECT path,
			COALESCE(CASE WHEN json_valid(tags_json) THEN json_extract(tags_json, '$.title') END, ''),
			COALESCE(CASE WHEN json_valid(tags_json) THEN json_extract(tags_json, '$.artist') END, ''),
			COALESCE(CASE WHEN json_valid(tags_json) THEN json_extract(tags_json, '$.album') END, '')
		FROM tracks`); err != nil {
		return fmt.Errorf("tracks_fts rebuild: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `DELETE FROM tracks_fts_row`); err != nil {
		return fmt.Errorf("tracks_fts rebuild: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO tracks_fts_row(path, fts_rowid) SELECT path, rowid FROM tracks_fts`); err != nil {
		return fmt.Errorf("tracks_fts rebuild: %w", err)
	}
	return nil
}

type censusQuery interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

type ftsCensus struct {
	tracks, fts, mapped, mapWithoutTrack, trackWithoutMap int
}

func (c ftsCensus) consistent() bool {
	return c.tracks == c.fts && c.fts == c.mapped && c.mapWithoutTrack == 0 && c.trackWithoutMap == 0
}

func readFTSCensus(ctx context.Context, q censusQuery) (ftsCensus, error) {
	var c ftsCensus
	err := q.QueryRowContext(ctx, `
		SELECT
			(SELECT COUNT(*) FROM tracks),
			(SELECT COUNT(*) FROM tracks_fts),
			(SELECT COUNT(*) FROM tracks_fts_row),
			(SELECT COUNT(*) FROM tracks_fts_row m WHERE NOT EXISTS (SELECT 1 FROM tracks t WHERE t.path = m.path)),
			(SELECT COUNT(*) FROM tracks t WHERE NOT EXISTS (SELECT 1 FROM tracks_fts_row m WHERE m.path = t.path))`).Scan(
		&c.tracks, &c.fts, &c.mapped, &c.mapWithoutTrack, &c.trackWithoutMap)
	if err != nil {
		return ftsCensus{}, fmt.Errorf("tracks_fts census: %w", err)
	}
	return c, nil
}

func uniqueConstraint(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}
