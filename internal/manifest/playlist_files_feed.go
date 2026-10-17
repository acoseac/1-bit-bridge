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

// PlaylistFileFeed is the playlist-file feed: the files, and whether the
// index is complete.
type PlaylistFileFeed struct {
	// Complete says a full scan has brought the index up to date since it
	// was created (the upgrade that adds it) or emptied (a root flip's
	// wipe): every playlist file that scan saw has a row. Until then Files
	// can lack files the library holds, and a client must not read a file
	// missing from it as removed (playlistFilesCompleteKey).
	Complete bool
	// Files are the listed playlist files, in path order. Never nil.
	Files []PlaylistFile
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
// An album-folder playlist is not listed (the owner's rule; the app's
// AlbumFolderPlaylist.isAlbumPlaylist, backlog B421, the same four terms).
// Its entries are its lines the path rule places (a line the rule drops is
// no entry), and it is an album playlist only when all four hold:
//  1. it has at least one entry;
//  2. EVERY entry names a track at exactly that path, byte for byte: one
//     that resolves to nothing, or only through the fold, keeps the file
//     listed (an unresolved line may be a file a scan could not read);
//  3. every entry's parent folder is the playlist's own folder, by bytes (a
//     track in a subfolder is not in the folder);
//  4. the tracks the entries name are exactly the tracks directly in that
//     folder, order and repeats ignored.
//
// A track there is any row the index holds, a suppressed duplicate
// included, and the rule reads the tracks the lines name, before the
// duplicate mapping.
func (s *Store) PlaylistFiles(ctx context.Context) (PlaylistFileFeed, error) {
	return s.playlistFiles(ctx, PlaylistFileFeedBudget)
}

// playlistFiles is PlaylistFiles under budget; a test passes a small one.
func (s *Store) playlistFiles(ctx context.Context, budget int) (PlaylistFileFeed, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return PlaylistFileFeed{}, err
	}
	defer tx.Rollback()

	var marked int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM scan_state WHERE k = ?`, playlistFilesCompleteKey).Scan(&marked); err != nil {
		return PlaylistFileFeed{}, fmt.Errorf("read playlist files: %w", err)
	}
	stored, err := readStoredPlaylistFiles(ctx, tx)
	if err != nil {
		return PlaylistFileFeed{}, err
	}

	// Each file's lines through the path rule, then every distinct
	// placed path looked up once.
	distinct := map[string]struct{}{}
	for i := range stored {
		stored[i].place(distinct)
	}
	matches, err := matchPlaylistEntries(ctx, tx, distinct)
	if err != nil {
		return PlaylistFileFeed{}, err
	}
	served, err := servedCopies(ctx, tx, matches)
	if err != nil {
		return PlaylistFileFeed{}, err
	}
	feed := PlaylistFileFeed{Complete: marked > 0, Files: make([]PlaylistFile, 0, len(stored))}
	for _, f := range stored {
		pf, album, err := f.resolve(ctx, tx, matches, served, &budget)
		if err != nil {
			return PlaylistFileFeed{}, err
		}
		if !album {
			feed.Files = append(feed.Files, pf)
		}
	}
	return feed, nil
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
	lines := resolvedPlaylistLines{members: []string{}, unresolved: []string{}, allExact: true}
	for _, pl := range f.placed {
		lines.add(pl, matches, served)
	}
	if lines.entries > 0 && lines.allExact {
		if album, err = isAlbumFolderPlaylist(ctx, tx, playlistFileDir(f.path), lines.named); err != nil || album {
			return pf, album, err
		}
	}
	if lines.cost > *budget {
		pf.TooLarge = true
		return pf, false, nil
	}
	*budget -= lines.cost
	pf.Members, pf.Unresolved = lines.members, lines.unresolved
	return pf, false, nil
}

// resolvedPlaylistLines is a playlist's lines resolved: the members and
// unresolved lines the feed serves and what they cost against the budget,
// and the album-folder rule's inputs. Its entries are the placed lines (a
// line the path rule drops is no entry); named holds the tracks they name
// at their exact paths, and allExact goes false at the first entry that
// names none there (unresolved, or found only by the fold).
type resolvedPlaylistLines struct {
	members, unresolved []string
	cost                int
	entries             int
	named               []string
	allExact            bool
}

// add resolves one line and files it as a member or an unresolved line.
func (r *resolvedPlaylistLines) add(pl placedPlaylistLine, matches map[string]playlistTrackMatch, served map[string]string) {
	if member, ok := r.member(pl, matches, served); ok {
		r.members = append(r.members, member)
		r.cost += len(member) + 4
		return
	}
	r.unresolved = append(r.unresolved, pl.line)
	r.cost += len(pl.line) + 4
}

// member is the member a placed line names, if any, and records the line
// for the album-folder rule.
func (r *resolvedPlaylistLines) member(pl placedPlaylistLine, matches map[string]playlistTrackMatch, served map[string]string) (string, bool) {
	if !pl.ok {
		return "", false
	}
	r.entries++
	m, hit := matches[pl.path]
	if hit && m.path == pl.path {
		r.named = append(r.named, m.path)
	} else {
		r.allExact = false
	}
	if hit && m.suppressed {
		copyPath, ok := served[m.groupID]
		return copyPath, ok
	}
	return m.path, hit
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
// through one bound JSON array (the json_each idiom), each statement
// prepared once. A key that is not valid UTF-8 cannot travel in JSON
// (encoding/json would substitute U+FFFD), so it is bound on its own
// through single.
func eachPlaylistLookup(ctx context.Context, tx *sql.Tx, keys []string, batch, single string, fn func(playlistTrackMatch)) error {
	valid, illFormed := splitIllFormedUTF8Paths(keys)
	var blobs []string
	for start := 0; start < len(valid); start += playlistLookupChunk {
		blob, err := json.Marshal(valid[start:min(start+playlistLookupChunk, len(valid))])
		if err != nil {
			return err
		}
		blobs = append(blobs, string(blob))
	}
	if err := queryEachArg(ctx, tx, batch, blobs, fn); err != nil {
		return err
	}
	return queryEachArg(ctx, tx, single, illFormed, fn)
}

// queryEachArg prepares query once, runs it with each arg, and hands fn
// every row.
func queryEachArg(ctx context.Context, tx *sql.Tx, query string, args []string, fn func(playlistTrackMatch)) error {
	if len(args) == 0 {
		return nil
	}
	stmt, err := tx.PrepareContext(ctx, query)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, arg := range args {
		if err := scanPlaylistMatches(ctx, stmt, arg, fn); err != nil {
			return err
		}
	}
	return nil
}

// scanPlaylistMatches runs stmt with arg and hands fn each row.
func scanPlaylistMatches(ctx context.Context, stmt *sql.Stmt, arg string, fn func(playlistTrackMatch)) error {
	rows, err := stmt.QueryContext(ctx, arg)
	if err != nil {
		return err
	}
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

// servedCopies maps the duplicate group of each suppressed match to its
// one served row. A group with no served row, or with several (a DSD and a
// PCM edition each keep one), maps to nothing, so its line is unresolved:
// with one served row, that row is every suppressed member's winner.
func servedCopies(ctx context.Context, tx *sql.Tx, matches map[string]playlistTrackMatch) (map[string]string, error) {
	var groups []string
	listed := map[string]bool{}
	for _, m := range matches {
		if m.suppressed && m.groupID != "" && !listed[m.groupID] {
			listed[m.groupID] = true
			groups = append(groups, m.groupID)
		}
	}
	out := map[string]string{}
	if len(groups) == 0 {
		return out, nil
	}
	stmt, err := tx.PrepareContext(ctx,
		`SELECT dupe_group_id, path FROM tracks
		  WHERE dupe_suppressed = 0 AND dupe_group_id != ''
		    AND dupe_group_id IN (SELECT value FROM json_each(?))`)
	if err != nil {
		return nil, fmt.Errorf("served copies: %w", err)
	}
	defer stmt.Close()
	count := map[string]int{}
	for start := 0; start < len(groups); start += playlistLookupChunk {
		blob, err := json.Marshal(groups[start:min(start+playlistLookupChunk, len(groups))])
		if err != nil {
			return nil, err
		}
		if err := servedCopiesChunk(ctx, stmt, string(blob), out, count); err != nil {
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

// servedCopiesChunk reads one chunk of groups' served rows into out, and
// counts them by group.
func servedCopiesChunk(ctx context.Context, stmt *sql.Stmt, blob string, out map[string]string, count map[string]int) error {
	rows, err := stmt.QueryContext(ctx, blob)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var g, p string
		if err := rows.Scan(&g, &p); err != nil {
			return err
		}
		count[g]++
		out[g] = p
	}
	return rows.Err()
}

// isAlbumFolderPlaylist is terms 3 and 4 of the album-folder rule, for a
// playlist whose every entry names a track at its exact path (terms 1 and 2,
// resolve): named, those tracks, every one sits directly in dir (its parent
// folder is dir, by bytes; a subfolder is not dir), and they are exactly the
// tracks directly in dir, order and repeats ignored. A track is any row the
// index holds, a suppressed duplicate included, as a share's listing holds
// every file. The folder is read only when term 3 holds.
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
