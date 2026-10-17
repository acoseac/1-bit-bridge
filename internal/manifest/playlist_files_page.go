package manifest

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/acoseac/1-bit-bridge/internal/version"
)

// PlaylistFilePage is one page of the playlist-file feed (backlog B442).
// A client reads the pages in order, each from the cursor the one before
// gave, and all of them at one Version are one consistent read of the feed.
type PlaylistFilePage struct {
	// Version identifies the state the page was read from
	// (playlistFeedVersion): every page of one read carries the same, and
	// any change a page could show moves it.
	Version string
	// Complete says a full scan has brought the index up to date since it
	// was created (the upgrade that adds it) or emptied (a root flip's
	// wipe): every playlist file that scan saw has a row. Until then the
	// pages can lack files the library holds, and a client must not read a
	// file missing from them as removed (playlistFilesCompleteKey).
	Complete bool
	// Files are the page's playlist files, in path order. Never nil.
	Files []PlaylistFile
	// Next is the manifest path the next page starts after: the last file
	// this page covers, listed or left out as an album-folder playlist. ""
	// on the last page.
	Next string
}

// PlaylistFilePageBudget bounds one page by what its files take in the JSON
// (PlaylistFilePageCost). The files are taken in path order while they fit;
// one that does not starts the next page, so the budget never makes a file
// tooLarge. A page holds at least one file, so a file whose entries alone
// pass the budget has a page to itself, bounded by the per-file caps
// (PlaylistFileMaxBytes, playlistFileMaxEntries). 8 MiB is some 80,000
// entries of 100 bytes, gzip'd on the wire.
const PlaylistFilePageBudget = 8 << 20

// playlistFileOverhead is what a file's JSON takes besides its strings: the
// keys, the quotes, the mtime at its longest, the brackets, the comma, and
// "tooLarge":true (108 bytes), rounded up.
const playlistFileOverhead = 112

// PlaylistFilePageCost is what f costs a page: its path and its name as
// the JSON writes them (jsonStringBytes) and playlistFileOverhead besides,
// and each entry its JSON bytes plus 4 (its quotes, a comma, one spare). It
// is never less than the bytes f takes in a page's JSON, so a page's files
// take no more than the budget there (TestAPlaylistFileCostsAtLeastItsJSON).
func PlaylistFilePageCost(f PlaylistFile) int {
	cost := jsonStringBytes(f.Path) + jsonStringBytes(f.Name) + playlistFileOverhead
	for _, e := range f.Members {
		cost += jsonStringBytes(e) + 4
	}
	for _, e := range f.Unresolved {
		cost += jsonStringBytes(e) + 4
	}
	return cost
}

// jsonStringBytes is the bytes encoding/json writes for s between its
// quotes, with json.Encoder's defaults, which the feed is written with (HTML
// escaping on): a quote, a backslash, and \b \f \n \r \t take two bytes;
// any other control character, <, >, &, U+2028, U+2029 and each byte of
// invalid UTF-8 (written as \ufffd) take six; every other rune its UTF-8
// bytes. TestJSONStringBytesIsWhatEncodingJSONWrites holds it to the encoder.
func jsonStringBytes(s string) int {
	n := 0
	for i := 0; i < len(s); {
		if b := s[i]; b < utf8.RuneSelf {
			n += jsonASCIIBytes(b)
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size == 1, r == '\u2028', r == '\u2029':
			n += 6
		default:
			n += size
		}
		i += size
	}
	return n
}

// jsonASCIIBytes is what encoding/json writes for the ASCII byte b.
func jsonASCIIBytes(b byte) int {
	switch b {
	case '"', '\\', '\b', '\f', '\n', '\r', '\t':
		return 2
	case '<', '>', '&':
		return 6
	}
	if b < 0x20 {
		return 6
	}
	return 1
}

// playlistPageRowChunk bounds the rows' paths one step of a page reads: a
// page reads the rows after its cursor this many at a time, in path order,
// until it is full.
const playlistPageRowChunk = 1024

// ErrPlaylistFilesChanged is a page asked for at a version that is no
// longer the feed's: something a page could show changed since the cursor
// was made, so the client starts again from the first page.
var ErrPlaylistFilesChanged = errors.New("manifest: the playlist files changed since the cursor was made")

// playlistFeedRulesVersion is folded into the feed's version. Bump it with
// any change to what the feed answers for the same stored rows (the path
// rule, the lookup, the album-folder rule, the page budget), so a client
// paging across a bridge that took the change restarts. A release's own
// version is folded in as well.
const playlistFeedRulesVersion = 1

// playlistFeedVersion is the feed's version: a hash of the rules, the
// build, the install's backup epoch (a restored database is another
// history), the generation the v57 triggers keep (any track or playlist
// row change a page could show) and the completeness mark. Read in the
// page's own transaction.
func playlistFeedVersion(ctx context.Context, tx *sql.Tx) (string, bool, error) {
	epoch, err := readEpoch(ctx, tx)
	if err != nil {
		return "", false, fmt.Errorf("playlist feed version: %w", err)
	}
	var gen int64
	if err := tx.QueryRowContext(ctx, `SELECT gen FROM playlist_feed_generation WHERE id = 1`).Scan(&gen); err != nil {
		return "", false, fmt.Errorf("playlist feed version: %w", err)
	}
	var marked int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM scan_state WHERE k = ?`, playlistFilesCompleteKey).Scan(&marked); err != nil {
		return "", false, fmt.Errorf("playlist feed version: %w", err)
	}
	complete := marked > 0
	sum := sha256.Sum256(fmt.Appendf(nil, "playlist-files\x00%d\x00%s\x00%s\x00%d\x00%t",
		playlistFeedRulesVersion, version.ServerVersion, epoch, gen, complete))
	return hex.EncodeToString(sum[:8]), complete, nil
}

// PlaylistFilesPage is one page of the playlist-file feed: the playlist
// files after the manifest path after ("" for the first page), in path
// order, with their entries resolved against the tracks the store holds
// NOW, until the next file would take the page past PlaylistFilePageBudget.
// want is the version the client's cursor carries ("" for the first page):
// when the feed's version is another, the page is ErrPlaylistFilesChanged
// and the client starts again. The version check and the page are one
// read-only transaction, so a page is one snapshot.
//
// Resolution happens here, at each request, never at scan time: a track
// indexed after its playlist was read (a later folder of the same scan, a
// later subtree scan, a root that came back) is a member without the file
// being read again, and a track that went is no member. The rows are
// resolved playlistFeedBatchBytes of stored lines at a time, so a request
// holds one batch's lines, a chunk of the rows' paths and the page, however
// large the index.
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
// And it is judged only on whole evidence about its folder (the app's B421
// gate): the last scan that saw the file saw its folder whole
// (playlistFolderEvidence, the row's folder_whole). Without it the index can
// under-count the folder (a new file a read could not index, a listing cut
// short), and an ordinary playlist naming the indexed tracks would meet term
// 4; so an unsure file is listed. That evidence is a finished scan's, taken
// after its walk's every row landed, never a running scan's: a scan under
// way only adds rows to a folder until its pass, which can only take a file
// out of the album rule.
//
// A track there is any row the index holds, a suppressed duplicate
// included, and the rule reads the tracks the lines name, before the
// duplicate mapping.
func (s *Store) PlaylistFilesPage(ctx context.Context, want, after string) (PlaylistFilePage, error) {
	return s.playlistFilesPage(ctx, want, after, PlaylistFilePageBudget, playlistFeedBatchBytes)
}

// playlistFilesPage is PlaylistFilesPage under budget, resolving batchBytes
// of stored lines at a time; a test passes small ones.
func (s *Store) playlistFilesPage(ctx context.Context, want, after string, budget, batchBytes int) (PlaylistFilePage, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return PlaylistFilePage{}, err
	}
	defer tx.Rollback()
	current, complete, err := playlistFeedVersion(ctx, tx)
	if err != nil {
		return PlaylistFilePage{}, err
	}
	if want != "" && want != current {
		return PlaylistFilePage{}, ErrPlaylistFilesChanged
	}
	page := PlaylistFilePage{Version: current, Complete: complete, Files: []PlaylistFile{}}
	fill := playlistPageFill{page: &page, budget: budget, last: after}
	known := map[string]playlistTrackMatch{}
	folders := &playlistFolderTracks{tx: tx}
	defer folders.close()
	for from := after; ; {
		rows, err := readPlaylistFileRowsAfter(ctx, tx, from, playlistPageRowChunk)
		if err != nil {
			return PlaylistFilePage{}, err
		}
		if len(rows) == 0 {
			return page, nil
		}
		full, err := fill.takeRows(ctx, tx, rows, batchBytes, known, folders)
		if err != nil {
			return PlaylistFilePage{}, err
		}
		if full {
			page.Next = fill.last
			return page, nil
		}
		from = rows[len(rows)-1].path
	}
}

// playlistPageFill is a page being filled: what it has cost against its
// budget, and the last path it covers (taken, or left out as an
// album-folder playlist), where the next page starts.
type playlistPageFill struct {
	page   *PlaylistFilePage
	budget int
	cost   int
	last   string
}

// takeRows resolves rows a batch at a time and takes each file while the
// page has room. It reports true once a file does not fit: that file, and
// everything after it, is the next page's.
func (f *playlistPageFill) takeRows(ctx context.Context, tx *sql.Tx, rows []playlistFileRow, batchBytes int,
	known map[string]playlistTrackMatch, folders *playlistFolderTracks) (bool, error) {
	for start := 0; start < len(rows); {
		end := nextPlaylistFeedBatch(rows, start, batchBytes)
		resolved, err := resolvePlaylistBatch(ctx, tx, rows[start:end], known, folders)
		if err != nil {
			return false, err
		}
		for _, r := range resolved {
			if !f.take(r) {
				return true, nil
			}
		}
		start = end
	}
	return false, nil
}

// take adds r to the page when it fits, and reports false when it does not.
// An album-folder playlist is covered without a cost; the page's first file
// is taken whatever it costs.
func (f *playlistPageFill) take(r resolvedPlaylistFile) bool {
	if r.album {
		f.last = r.file.Path
		return true
	}
	if len(f.page.Files) > 0 && f.cost+r.cost > f.budget {
		return false
	}
	f.page.Files = append(f.page.Files, r.file)
	f.cost += r.cost
	f.last = r.file.Path
	return true
}

// readPlaylistFileRowsAfter reads up to limit playlist_files rows after the
// manifest path after, in path order, without their lines. Every stored
// path is longer than "", so after "" is the first rows.
func readPlaylistFileRowsAfter(ctx context.Context, tx *sql.Tx, after string, limit int) ([]playlistFileRow, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT path, mtime_ns, too_large, folder_whole, length(CAST(lines AS BLOB)) FROM playlist_files
		  WHERE path COLLATE BINARY > ? ORDER BY path COLLATE BINARY LIMIT ?`, after, limit)
	if err != nil {
		return nil, fmt.Errorf("read playlist files: %w", err)
	}
	defer rows.Close()
	var out []playlistFileRow
	for rows.Next() {
		var r playlistFileRow
		if err := rows.Scan(&r.path, &r.mtimeNs, &r.tooLarge, &r.folderWhole, &r.lineBytes); err != nil {
			return nil, fmt.Errorf("read playlist files: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read playlist files: %w", err)
	}
	return out, nil
}
