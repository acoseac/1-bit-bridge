package manifest

import (
	"context"
	"database/sql"
	"strings"
)

// playlistFileStamp is what the scan pass compares a walked playlist file
// with: the size and mtime the stored lines were read at, the parser that
// read them, and the row's missing count.
type playlistFileStamp struct {
	size          int64
	mtimeNs       int64
	parserVersion int
	missingCount  int
}

// playlistFileRead is one completed read of a playlist file, which the scan
// pass writes: its manifest path, the stat of the file the read opened, and
// the entry lines parsePlaylistLines kept (none when tooLarge).
type playlistFileRead struct {
	path     string
	size     int64
	mtimeNs  int64
	tooLarge bool
	lines    []string
}

// playlistFilesCompleteKey is the scan_state key that marks the index as
// one a full scan has brought up to date: a full scan's pass sets it once
// every playlist file its walk saw has a row, the wipe of a root flip
// deletes it with the rows, and a store the migration just created has none.
// No later pass clears it: past that point a row leaves only as a removed
// file's, after the missing-count grace. The feed reports it as Complete.
const playlistFilesCompleteKey = "playlist_files_complete"

// markPlaylistFilesComplete sets playlistFilesCompleteKey.
func (s *Store) markPlaylistFilesComplete(ctx context.Context) error {
	return s.SetScanState(ctx, playlistFilesCompleteKey, "1")
}

// joinPlaylistLines is the stored form of a file's lines. A kept line never
// holds an LF, which ends a line, and never is empty, which is no entry, so
// LF separates them and an empty blob is no lines.
func joinPlaylistLines(lines []string) []byte {
	return []byte(strings.Join(lines, "\n"))
}

// splitPlaylistLines is joinPlaylistLines' inverse.
func splitPlaylistLines(blob []byte) []string {
	if len(blob) == 0 {
		return nil
	}
	return strings.Split(string(blob), "\n")
}

// playlistFileStampsUnder returns the stamp of every playlist-file row in a
// scan's scope, by path: every row for "" or "." (a full scan, or a
// subtree scan of a single root), and otherwise the rows under scope, with
// the bounds TrackPathsUnder uses ("<base>/." is the whole of that root).
// Byte ranges, never LIKE, for TrackPathsUnder's reason: a case twin of the
// scope is another folder, and pulling its rows into the snapshot would
// count them missing.
func (s *Store) playlistFileStampsUnder(ctx context.Context, scope string) (map[string]playlistFileStamp, error) {
	var rows *sql.Rows
	var err error
	if base := trackScopeBase(scope); scope == "" || scope == "." || base == "" {
		rows, err = s.db.QueryContext(ctx,
			`SELECT path, size, mtime_ns, parser_version, missing_count FROM playlist_files`)
	} else {
		rows, err = s.db.QueryContext(ctx,
			`SELECT path, size, mtime_ns, parser_version, missing_count FROM playlist_files
			  WHERE path COLLATE BINARY >= ? || '/' AND path COLLATE BINARY < ? || '0'`,
			base, base)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]playlistFileStamp{}
	for rows.Next() {
		var p string
		var st playlistFileStamp
		if err := rows.Scan(&p, &st.size, &st.mtimeNs, &st.parserVersion, &st.missingCount); err != nil {
			return nil, err
		}
		out[p] = st
	}
	return out, rows.Err()
}

// writePlaylistFiles stores a batch of reads in one transaction, each with
// its missing count cleared. It reports whether the batch changed what the
// feed serves: a new row, or lines or the too-large mark that differ from
// the stored ones. A file touched with its content unchanged stores the new
// stamp and changes nothing served. A change notes the library, so a
// library.changed goes out (once per full scan, debounced otherwise).
//
// Holds `s.mu` per the writer contract on Store.
func (s *Store) writePlaylistFiles(ctx context.Context, reads []playlistFileRead) (bool, error) {
	if len(reads) == 0 {
		return false, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO playlist_files (path, size, mtime_ns, parser_version, too_large, lines, missing_count)
		VALUES (?, ?, ?, ?, ?, ?, 0)
		ON CONFLICT(path) DO UPDATE SET
			size = excluded.size,
			mtime_ns = excluded.mtime_ns,
			parser_version = excluded.parser_version,
			too_large = excluded.too_large,
			lines = excluded.lines,
			missing_count = 0`)
	if err != nil {
		return false, err
	}
	defer stmt.Close()
	changed := false
	for _, r := range reads {
		blob := joinPlaylistLines(r.lines)
		var oldLines []byte
		var oldTooLarge bool
		switch err := tx.QueryRowContext(ctx,
			`SELECT lines, too_large FROM playlist_files WHERE path = ?`, r.path,
		).Scan(&oldLines, &oldTooLarge); {
		case err == sql.ErrNoRows:
			changed = true
		case err != nil:
			return false, err
		case oldTooLarge != r.tooLarge || string(oldLines) != string(blob):
			changed = true
		}
		if _, err := stmt.ExecContext(ctx, r.path, r.size, r.mtimeNs,
			playlistFileParserVersion, r.tooLarge, blob); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	if changed {
		s.noteLibrary(ctx)
	}
	return changed, nil
}

// settlePlaylistFiles is the scan pass's deletion step, in one transaction:
// it clears the missing count of the rows the walk saw again (seen), counts
// one more miss on each row it did not (missing) and deletes those at the
// threshold, scoped to the paths this pass counted (the tracks' rule: a row
// this pass did not observe is never reaped by a bare threshold), and
// deletes the old spelling of a case-only rename whose new spelling this
// pass read (renamed). It returns how many rows it deleted, and a deletion
// notes the library.
//
// Holds `s.mu` per the writer contract on Store.
func (s *Store) settlePlaylistFiles(ctx context.Context, seen, missing, renamed []string, threshold int) (int64, error) {
	if len(seen) == 0 && len(missing) == 0 && len(renamed) == 0 {
		return 0, nil
	}
	if threshold < 1 {
		threshold = 1
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	for _, p := range seen {
		if _, err := tx.ExecContext(ctx,
			`UPDATE playlist_files SET missing_count = 0 WHERE path = ? AND missing_count != 0`, p); err != nil {
			return 0, err
		}
	}
	var deleted int64
	for _, p := range missing {
		if _, err := tx.ExecContext(ctx,
			`UPDATE playlist_files SET missing_count = missing_count + 1 WHERE path = ?`, p); err != nil {
			return 0, err
		}
		res, err := tx.ExecContext(ctx,
			`DELETE FROM playlist_files WHERE path = ? AND missing_count >= ?`, p, threshold)
		if err != nil {
			return 0, err
		}
		if n, err := res.RowsAffected(); err == nil {
			deleted += n
		}
	}
	for _, p := range renamed {
		res, err := tx.ExecContext(ctx, `DELETE FROM playlist_files WHERE path = ?`, p)
		if err != nil {
			return 0, err
		}
		if n, err := res.RowsAffected(); err == nil {
			deleted += n
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	if deleted > 0 {
		s.noteLibrary(ctx)
	}
	return deleted, nil
}
