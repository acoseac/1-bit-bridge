package manifest

// Library search via SQLite FTS5 (PR B in the v1.4 inspector rework).
//
// The migration ladder (version 7) creates a standalone `tracks_fts`
// virtual table mirroring path / title / artist / album extracted from
// the `tracks.tags_json` blob via triggers. This file is the read-side
// surface: query sanitisation, MATCH execution, hit shaping.
//
// Two public entry points:
//   - `SearchTracks(ctx, query, limit)` — flat list of track hits,
//     ordered by FTS5 rank (BM25). Each row carries the indexed
//     columns so the admin UI's flat-list view renders artist /
//     album context without a follow-up tags_json deserialise.
//   - `SearchFolders(ctx, query, limit)` — distinct folder hits,
//     derived from `dirname(path)` over the same query result,
//     ordered by hit count desc + path-length asc (shorter /
//     more canonical paths first).
//
// Both gracefully degrade to an empty slice when FTS5 is unavailable
// at this DB (see `SearchAvailable`). The admin handler maps that
// to a 503 response so the UI can surface a clear "search disabled"
// message.

import (
	"context"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"unicode"
)

// TrackHit is one row in the SearchTracks result. ParentPath is
// computed from Path via path.Dir; the API handler echoes it back
// to the UI so the flat-list view can navigate to the parent folder
// without re-parsing on the client.
type TrackHit struct {
	Path       string
	ParentPath string
	Title      string
	Artist     string
	Album      string
}

// FolderHit is one row in the SearchFolders result. HitCount is the
// number of underlying track matches that contributed to this folder
// rollup — drives the relevance ordering on the admin UI.
type FolderHit struct {
	Path     string
	Name     string
	HitCount int
}

// ErrSearchUnavailable surfaces when the FTS5 module is not
// compiled into the SQLite driver (graceful-degradation path from
// migration v7). Callers should map this to HTTP 503.
var ErrSearchUnavailable = errors.New("manifest: FTS5 library search not available on this bridge")

// SearchAvailable returns true iff the `tracks_fts` virtual table
// exists. Used by the admin search handler at request time so a
// bridge whose FTS5 probe failed at migration time can return a
// clean 503 rather than a confusing "no such table" surface.
func (s *Store) SearchAvailable(ctx context.Context) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='tracks_fts'`,
	).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("probe tracks_fts existence: %w", err)
	}
	return n > 0, nil
}

// SearchTracks runs an FTS5 MATCH query against the tracks_fts
// virtual table and returns up to `limit` ranked hits.
//
// Query sanitisation strips non-alphanumeric tokens (preserving
// Unicode letters via `unicode.IsLetter` / `IsDigit`), wraps each
// surviving token in double quotes, and appends `*` for prefix
// matching ONLY when the token is at least 3 chars. Short-token
// prefix expansion (`"a"*` / `"in"*`) would scan a massive fraction
// of the FTS vocabulary on a large library; the 3-char threshold is
// the documented trade-off in the v1.4 plan.
//
// Returns ErrSearchUnavailable if FTS5 isn't compiled in (probed
// via SearchAvailable up-front to give the caller a clear typed
// error rather than a SQL-level "no such table"). Empty query after
// sanitisation returns (nil, nil) — neither an error nor a hit.
func (s *Store) SearchTracks(ctx context.Context, query string, limit int) ([]TrackHit, error) {
	if avail, err := s.SearchAvailable(ctx); err != nil {
		return nil, err
	} else if !avail {
		return nil, ErrSearchUnavailable
	}
	matchExpr := buildFTSMatchExpr(query)
	if matchExpr == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT path,
		       COALESCE(title, ''),
		       COALESCE(artist, ''),
		       COALESCE(album, '')
		FROM tracks_fts
		WHERE tracks_fts MATCH ?
		ORDER BY rank
		LIMIT ?
	`, matchExpr, limit)
	if err != nil {
		return nil, fmt.Errorf("search tracks_fts: %w", err)
	}
	defer rows.Close()
	out := make([]TrackHit, 0, limit)
	for rows.Next() {
		var h TrackHit
		if err := rows.Scan(&h.Path, &h.Title, &h.Artist, &h.Album); err != nil {
			return nil, fmt.Errorf("scan track hit: %w", err)
		}
		h.ParentPath = path.Dir(h.Path)
		if h.ParentPath == "." {
			h.ParentPath = ""
		}
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate track hits: %w", err)
	}
	return out, nil
}

// SearchFolders runs the same MATCH but rolls hits up by directory.
// Returns up to `limit` distinct parent paths ranked by hit count
// desc then path-length asc (shorter / more-canonical paths first
// so a search that matches many tracks under a single album surfaces
// that album rather than its parent artist).
//
// Implemented as a derived aggregation over SearchTracks's query
// shape — we issue a single SQL statement combining the MATCH with
// GROUP BY path.Dir-equivalent (the SQLite scalar `substr` +
// `instr` are not used; we do the dirname split in Go for clarity,
// since the row count is bounded by the inner MATCH limit). For an
// admin-loopback surface bounded at limit ≤ 200, the cost is dust.
func (s *Store) SearchFolders(ctx context.Context, query string, limit int) ([]FolderHit, error) {
	if limit <= 0 {
		limit = 50
	}
	// Fetch a wider inner result so the GROUP BY can produce a full
	// set of distinct parents — 5× the requested folder limit keeps
	// the inner cost bounded while leaving headroom for typical
	// patterns (multiple tracks per matching folder).
	hits, err := s.SearchTracks(ctx, query, limit*5)
	if err != nil {
		return nil, err
	}
	if len(hits) == 0 {
		return nil, nil
	}
	type acc struct {
		count int
	}
	byParent := make(map[string]*acc, len(hits))
	order := make([]string, 0, len(hits))
	for _, h := range hits {
		entry, ok := byParent[h.ParentPath]
		if !ok {
			entry = &acc{}
			byParent[h.ParentPath] = entry
			order = append(order, h.ParentPath)
		}
		entry.count++
	}
	out := make([]FolderHit, 0, len(byParent))
	for _, p := range order {
		out = append(out, FolderHit{
			Path:     p,
			Name:     path.Base(p),
			HitCount: byParent[p].count,
		})
	}
	// Sort: hit count desc, then path length asc (Unicode-byte-len
	// is fine — shorter still wins between two equally-hit folders
	// regardless of how Unicode segments encode).
	sortFolderHits(out)
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// buildFTSMatchExpr turns user input into a safe FTS5 MATCH
// expression. See SearchTracks docblock for sanitisation rules.
// Pure / no I/O; testable in isolation.
func buildFTSMatchExpr(raw string) string {
	tokens := splitFTSTokens(raw)
	if len(tokens) == 0 {
		return ""
	}
	parts := make([]string, 0, len(tokens))
	for _, t := range tokens {
		// Double-quote each token defensively even though sanitisation
		// already stripped FTS5 metacharacters — defense in depth in
		// case a future caller passes pre-quoted input.
		quoted := `"` + t + `"`
		// Prefix-expand only on ≥3-char tokens. `"a"*` / `"in"*` would
		// scan a huge fraction of the FTS vocabulary.
		if utf8RuneLen(t) >= 3 {
			quoted += "*"
		}
		parts = append(parts, quoted)
	}
	return strings.Join(parts, " ")
}

// splitFTSTokens splits user input on whitespace AND any rune that
// isn't a Unicode letter or digit. The result is the list of
// surviving alphanumeric tokens — empty if the input was all
// punctuation / whitespace.
func splitFTSTokens(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	out := fields[:0]
	for _, f := range fields {
		if f != "" {
			out = append(out, f)
		}
	}
	return out
}

// utf8RuneLen returns the number of Unicode code points in s. Used
// for the ≥3-char prefix-expansion threshold — byte length would
// false-classify a single Chinese character (3 UTF-8 bytes) as
// "long enough" when it's structurally a single token.
func utf8RuneLen(s string) int {
	n := 0
	for range s {
		n++
	}
	return n
}

// sortFolderHits orders by HitCount desc, then path length asc, then
// Path asc (deterministic tiebreak). Extracted as a named function
// so tests can pin the contract.
func sortFolderHits(hits []FolderHit) {
	sort.SliceStable(hits, func(i, j int) bool {
		a, b := hits[i], hits[j]
		if a.HitCount != b.HitCount {
			return a.HitCount > b.HitCount
		}
		if len(a.Path) != len(b.Path) {
			return len(a.Path) < len(b.Path)
		}
		return a.Path < b.Path
	})
}
