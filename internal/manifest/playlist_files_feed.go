package manifest

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path"
	"time"
)

// PlaylistFile is one playlist file the index serves, as GET
// /v1/playlist-files lists it. It is a domain type: the handler wraps it in
// a DTO of its own.
type PlaylistFile struct {
	// Path is the file's manifest path.
	Path string
	// Name is the name the app gives a playlist it imports from a share:
	// the file name without its last extension (playlistFileDisplayName).
	Name string
	// ModTime is the file's mtime when the index read it.
	ModTime time.Time
	// Members are the manifest paths of the tracks the entries name, in
	// file order, duplicates kept. Never nil.
	Members []string
	// Unresolved are the entry lines that are no member, in file order, as
	// the parser keeps them (trimmed, "\" folded): a line the path rule
	// cannot place inside the manifest root, and a placed line no served
	// track answers. Never nil.
	Unresolved []string
	// TooLarge says the entries are not served: the file is
	// PlaylistFileMaxBytes or more, it holds more than
	// playlistFileMaxEntries entry lines, or its entries would take this
	// answer past PlaylistFileFeedBudget. Members and Unresolved are empty
	// then, which says nothing about the entries the file holds.
	TooLarge bool
}

// PlaylistFileFeedBudget bounds the entries one answer holds: each member
// path and unresolved line costs its length plus 4 (the JSON quotes, the
// comma, and some slack), and the playlists are filled in path order. A
// playlist whose entries would pass the budget is served TooLarge, and a
// later one that fits is still served. 8 MiB is some 80,000 entries of 100
// bytes, gzip'd on the wire.
const PlaylistFileFeedBudget = 8 << 20

// playlistLookupChunk bounds one json_each path list, as
// CatalogTrackRowsForPaths does.
const playlistLookupChunk = 400

// storedPlaylistFile is one playlist_files row as the feed reads it, and
// its lines through the path rule (place).
type storedPlaylistFile struct {
	path     string
	mtimeNs  int64
	tooLarge bool
	lines    []string
	placed   []placedPlaylistLine
}

// placedPlaylistLine is one entry line and where the path rule puts it; ok
// is false for a line it cannot place inside the manifest root.
type placedPlaylistLine struct {
	line string
	path string
	ok   bool
}

// playlistTrackMatch is the track row a placed entry names.
type playlistTrackMatch struct {
	path       string
	suppressed bool
	groupID    string
}

// PlaylistFiles is the playlist-file feed: every playlist file the index
// holds, in path order, with its entries resolved against the tracks the
// store holds NOW. Resolution happens here, at each request, never at scan
// time: a track indexed after its playlist was read (a later folder of the
// same scan, a later subtree scan, a root that came back) is a member
// without the file being read again, and a track that went is no member.
//
// Every read runs in one read-only transaction, so an answer is one
// snapshot of the store and the same store gives the same answer (the
// ETag is a hash of it).
//
// An entry the path rule places is looked up by its exact bytes first;
// with no row there, by unicode_lower (case and composition folded), and
// taken only when exactly one row answers: the store's read rule, exact
// first, folded fallback, refuse to pick (LookupTrack). The member is the
// row's own path. A row the duplicates pass suppressed is served as its
// group's one served row, or, when its group has none or several, its line
// is unresolved: the feed names only tracks the manifest serves.
//
// An album-folder playlist is skipped: one with at least one resolved
// entry, every resolved entry a track directly in the playlist's own
// folder, and every track directly in that folder among them, matched
// before the duplicate mapping, in any order (the owner's rule, the app's
// backlog B421). A playlist with no resolved entry is always served.
func (s *Store) PlaylistFiles(ctx context.Context) ([]PlaylistFile, error) {
	return s.playlistFiles(ctx, PlaylistFileFeedBudget)
}

// playlistFiles is PlaylistFiles under budget; a test passes a small one.
func (s *Store) playlistFiles(ctx context.Context, budget int) ([]PlaylistFile, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	stored, err := readStoredPlaylistFiles(ctx, tx)
	if err != nil {
		return nil, err
	}

	// Each file's lines through the path rule, then every distinct
	// placed path looked up once.
	distinct := map[string]struct{}{}
	for i := range stored {
		stored[i].place(distinct)
	}
	matches, err := matchPlaylistEntries(ctx, tx, distinct)
	if err != nil {
		return nil, err
	}
	served, err := servedCopies(ctx, tx, matches)
	if err != nil {
		return nil, err
	}
	out := make([]PlaylistFile, 0, len(stored))
	for _, f := range stored {
		pf, album, err := f.resolve(ctx, tx, matches, served, &budget)
		if err != nil {
			return nil, err
		}
		if !album {
			out = append(out, pf)
		}
	}
	return out, nil
}

// place puts each of f's lines through the path rule, against f's own
// folder, and adds every placed path to distinct.
func (f *storedPlaylistFile) place(distinct map[string]struct{}) {
	dir := playlistFileDir(f.path)
	f.placed = make([]placedPlaylistLine, len(f.lines))
	for j, line := range f.lines {
		p, ok := playlistEntryPath(dir, line)
		f.placed[j] = placedPlaylistLine{line: line, path: p, ok: ok}
		if ok {
			distinct[p] = struct{}{}
		}
	}
}

// resolve is f's feed entry: each placed line that names a served track (or
// a suppressed one with one served copy) a member, every other line
// unresolved. album reports an album-folder playlist, which the feed does
// not list. Entries that would pass *budget leave the entry tooLarge;
// entries that fit are taken from it.
func (f *storedPlaylistFile) resolve(ctx context.Context, tx *sql.Tx, matches map[string]playlistTrackMatch,
	served map[string]string, budget *int) (pf PlaylistFile, album bool, err error) {
	pf = PlaylistFile{
		Path: f.path, Name: playlistFileDisplayName(f.path),
		ModTime: time.Unix(0, f.mtimeNs).UTC(),
		Members: []string{}, Unresolved: []string{},
		TooLarge: f.tooLarge,
	}
	if f.tooLarge {
		return pf, false, nil
	}
	var matched []string
	cost := 0
	for _, pl := range f.placed {
		m, hit := playlistTrackMatch{}, false
		if pl.ok {
			m, hit = matches[pl.path]
		}
		if hit {
			matched = append(matched, m.path)
		}
		member := m.path
		if hit && m.suppressed {
			member, hit = served[m.groupID]
		}
		if hit {
			pf.Members = append(pf.Members, member)
			cost += len(member) + 4
		} else {
			pf.Unresolved = append(pf.Unresolved, pl.line)
			cost += len(pl.line) + 4
		}
	}
	if album, err = isAlbumFolderPlaylist(ctx, tx, playlistFileDir(f.path), matched); err != nil || album {
		return pf, album, err
	}
	if cost > *budget {
		pf.Members, pf.Unresolved, pf.TooLarge = []string{}, []string{}, true
	} else {
		*budget -= cost
	}
	return pf, false, nil
}

func readStoredPlaylistFiles(ctx context.Context, tx *sql.Tx) ([]storedPlaylistFile, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT path, mtime_ns, too_large, lines FROM playlist_files ORDER BY path COLLATE BINARY`)
	if err != nil {
		return nil, fmt.Errorf("read playlist files: %w", err)
	}
	defer rows.Close()
	var out []storedPlaylistFile
	for rows.Next() {
		var f storedPlaylistFile
		var blob []byte
		if err := rows.Scan(&f.path, &f.mtimeNs, &f.tooLarge, &blob); err != nil {
			return nil, fmt.Errorf("read playlist files: %w", err)
		}
		f.lines = splitPlaylistLines(blob)
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read playlist files: %w", err)
	}
	return out, nil
}

// matchPlaylistEntries maps each placed path to the track row it names:
// the row at its exact bytes, else the one row whose unicode_lower agrees
// with it. A path two rows fold to, or none, has no match.
func matchPlaylistEntries(ctx context.Context, tx *sql.Tx, paths map[string]struct{}) (map[string]playlistTrackMatch, error) {
	out := make(map[string]playlistTrackMatch, len(paths))
	if len(paths) == 0 {
		return out, nil
	}
	list := make([]string, 0, len(paths))
	for p := range paths {
		list = append(list, p)
	}
	if err := eachPlaylistLookup(ctx, tx, list,
		`SELECT path, dupe_suppressed, dupe_group_id FROM tracks
		  WHERE path IN (SELECT value FROM json_each(?))`,
		`SELECT path, dupe_suppressed, dupe_group_id FROM tracks WHERE path = ?`,
		func(m playlistTrackMatch) { out[m.path] = m }); err != nil {
		return nil, fmt.Errorf("match playlist entries: %w", err)
	}

	// The fold fallback, for what the exact lookup did not find.
	byFold := map[string][]string{}
	var folds []string
	for _, p := range list {
		if _, found := out[p]; found {
			continue
		}
		key := pathFold(p)
		if _, seen := byFold[key]; !seen {
			folds = append(folds, key)
		}
		byFold[key] = append(byFold[key], p)
	}
	if len(folds) == 0 {
		return out, nil
	}
	rowsByFold := map[string][]playlistTrackMatch{}
	if err := eachPlaylistLookup(ctx, tx, folds,
		`SELECT path, dupe_suppressed, dupe_group_id FROM tracks
		  WHERE unicode_lower(path) IN (SELECT value FROM json_each(?))`,
		`SELECT path, dupe_suppressed, dupe_group_id FROM tracks WHERE unicode_lower(path) = ?`,
		func(m playlistTrackMatch) {
			key := pathFold(m.path)
			rowsByFold[key] = append(rowsByFold[key], m)
		}); err != nil {
		return nil, fmt.Errorf("match playlist entries by fold: %w", err)
	}
	for key, rows := range rowsByFold {
		if len(rows) != 1 {
			continue // two rows fold alike: refuse to pick one
		}
		for _, p := range byFold[key] {
			out[p] = rows[0]
		}
	}
	return out, nil
}

// eachPlaylistLookup runs a track lookup over keys, a chunk at a time,
// through one bound JSON array (the json_each idiom). A key that is not
// valid UTF-8 cannot travel in JSON (encoding/json would substitute
// U+FFFD), so it is bound on its own through single.
func eachPlaylistLookup(ctx context.Context, tx *sql.Tx, keys []string, batch, single string, fn func(playlistTrackMatch)) error {
	valid, illFormed := splitIllFormedUTF8Paths(keys)
	scan := func(rows *sql.Rows) error {
		defer rows.Close()
		for rows.Next() {
			var m playlistTrackMatch
			if err := rows.Scan(&m.path, &m.suppressed, &m.groupID); err != nil {
				return err
			}
			fn(m)
		}
		return rows.Err()
	}
	for start := 0; start < len(valid); start += playlistLookupChunk {
		end := min(start+playlistLookupChunk, len(valid))
		blob, err := json.Marshal(valid[start:end])
		if err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, batch, string(blob))
		if err != nil {
			return err
		}
		if err := scan(rows); err != nil {
			return err
		}
	}
	for _, k := range illFormed {
		rows, err := tx.QueryContext(ctx, single, k)
		if err != nil {
			return err
		}
		if err := scan(rows); err != nil {
			return err
		}
	}
	return nil
}

// servedCopies maps the duplicate group of each suppressed match to its
// one served row. A group with no served row, or with several (a DSD and a
// PCM edition each keep one), maps to nothing, so its line is unresolved:
// with one served row, that row is every suppressed member's winner.
func servedCopies(ctx context.Context, tx *sql.Tx, matches map[string]playlistTrackMatch) (map[string]string, error) {
	groups := map[string]struct{}{}
	for _, m := range matches {
		if m.suppressed && m.groupID != "" {
			groups[m.groupID] = struct{}{}
		}
	}
	out := map[string]string{}
	if len(groups) == 0 {
		return out, nil
	}
	list := make([]string, 0, len(groups))
	for g := range groups {
		list = append(list, g)
	}
	count := map[string]int{}
	for start := 0; start < len(list); start += playlistLookupChunk {
		end := min(start+playlistLookupChunk, len(list))
		blob, err := json.Marshal(list[start:end])
		if err != nil {
			return nil, err
		}
		rows, err := tx.QueryContext(ctx,
			`SELECT dupe_group_id, path FROM tracks
			  WHERE dupe_suppressed = 0 AND dupe_group_id != ''
			    AND dupe_group_id IN (SELECT value FROM json_each(?))`, string(blob))
		if err != nil {
			return nil, fmt.Errorf("served copies: %w", err)
		}
		for rows.Next() {
			var g, p string
			if err := rows.Scan(&g, &p); err != nil {
				rows.Close()
				return nil, fmt.Errorf("served copies: %w", err)
			}
			count[g]++
			out[g] = p
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, fmt.Errorf("served copies: %w", err)
		}
	}
	for g, n := range count {
		if n != 1 {
			delete(out, g)
		}
	}
	return out, nil
}

// isAlbumFolderPlaylist reports the album-folder shape: matched (the track
// rows the entries named, before the duplicate mapping) is not empty, every
// one of them sits directly in dir, and every track directly in dir is one
// of them. Order and repeats do not count. The folder is read only when
// the first two hold, which no playlist naming another folder passes.
func isAlbumFolderPlaylist(ctx context.Context, tx *sql.Tx, dir string, matched []string) (bool, error) {
	if len(matched) == 0 {
		return false, nil
	}
	want := make(map[string]struct{}, len(matched))
	for _, m := range matched {
		if playlistFileDir(m) != dir {
			return false, nil
		}
		want[m] = struct{}{}
	}
	var rows *sql.Rows
	var err error
	if dir == "" {
		rows, err = tx.QueryContext(ctx, `SELECT path FROM tracks WHERE instr(path, '/') = 0`)
	} else {
		rows, err = tx.QueryContext(ctx,
			`SELECT path FROM tracks
			  WHERE path COLLATE BINARY >= ? || '/' AND path COLLATE BINARY < ? || '0'`, dir, dir)
	}
	if err != nil {
		return false, fmt.Errorf("album folder %q: %w", dir, err)
	}
	defer rows.Close()
	inFolder := 0
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return false, fmt.Errorf("album folder: %w", err)
		}
		if path.Dir(p) != pathDirOrDot(dir) {
			continue // deeper than the folder itself
		}
		if _, ok := want[p]; !ok {
			return false, rows.Err()
		}
		inFolder++
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("album folder: %w", err)
	}
	return inFolder == len(want), nil
}

// pathDirOrDot is dir as path.Dir spells it: "." for the manifest root.
func pathDirOrDot(dir string) string {
	if dir == "" {
		return "."
	}
	return dir
}
