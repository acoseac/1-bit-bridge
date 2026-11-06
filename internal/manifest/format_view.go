package manifest

// The client format gate on the served set (backlog B489): which rows a
// client's view takes. A view (formatgate.Set) is the formats one request
// declared in X-Bridge-Formats; the zero view, a request that declares none,
// is every app from before WavPack and Monkey's Audio were indexed, and no
// row, tombstone or count of theirs reaches it. Every /v1 reader of the
// served set takes a view; the operator's surfaces and the bridge's own
// machinery read every row (formatgate.All).
//
// A row is the view's by its path's extension, in any case, whatever its
// origin or codec (formatgate.Of): a file its extractor refused has no codec,
// a tombstone no row to read one from, and an upstream server's row only the
// extension its URL gave it. The SQL below is that rule, held to Of by
// TestTheFormatViewSQLIsFormatgatesRule over every shape of path.
//
// A page cursor continues only the view that made it (pageCursor): a cursor
// of another view would carry a client past rows its own view has and the
// other did not deliver.

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"github.com/acoseac/1-bit-bridge/internal/formatgate"
)

// formatViewSQL keeps a view's rows of a table whose path column is path:
// one term per gated format, each true when the view declares the format or
// the path's extension is not the format's. Binds formatViewArgs, in order.
// A constant, so every query holding it stays one the static analysis reads
// (the queries are assembled from constants alone).
const formatViewSQL = `(? OR lower(substr(path, -3)) <> '.wv') AND (? OR lower(substr(path, -4)) <> '.ape')`

// formatViewSQLT is formatViewSQL over t.path, for a query that joins
// tracks as t to another table with a path column (search's).
const formatViewSQLT = `(? OR lower(substr(t.path, -3)) <> '.wv') AND (? OR lower(substr(t.path, -4)) <> '.ape')`

// ungatedPathSQLT is true for a row of tracks as t that no gated format
// names (formatgate.Gated is false): formatViewSQLT for the zero view, with
// no binds, for a predicate whose binds are fixed already (the upscale
// eligibility mirror, which leaves a WavPack or Monkey's Audio file out of
// every rendition).
const ungatedPathSQLT = `(lower(substr(t.path, -3)) <> '.wv' AND lower(substr(t.path, -4)) <> '.ape')`

// countGatedFormatTracksSQL is CountGatedFormatTracks' query.
const countGatedFormatTracksSQL = `SELECT COUNT(*) FROM tracks t WHERE NOT ` + ungatedPathSQLT

// CountGatedFormatTracks counts the indexed rows the client format gate
// holds back from an app that declares no gated format (backlog B489): a
// WavPack or Monkey's Audio file, by its path as formatgate.Gated reads it,
// whatever its origin, a suppressed duplicate included. The operator's
// surfaces say how many are indexed and which apps play them: bridge scan's
// summary and the dashboard's composition. Read-only, no s.mu.
func (s *Store) CountGatedFormatTracks(ctx context.Context) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, countGatedFormatTracksSQL).Scan(&n); err != nil {
		return 0, fmt.Errorf("count gated-format tracks: %w", err)
	}
	return n, nil
}

// formatViewArgs are the binds of formatViewSQL and formatViewSQLT.
func formatViewArgs(view formatgate.Set) []any {
	return []any{view.WavPack(), view.APE()}
}

// ErrCursorOfAnotherView is a page cursor presented with a view other than
// the one that made it: a request that declares other formats than the one
// it continues. The client starts its walk again from the first page
// (PROTOCOL.md: 410 manifest_cursor_view).
var ErrCursorOfAnotherView = errors.New("the page cursor was made for another set of formats")

// declaredCursorPrefix begins the page cursor of a view that declares a
// format. The zero view's cursor is the last path a page held, as it always
// was, so an app from before B489 pages as it always did.
const declaredCursorPrefix = "f1."

// pageCursor is the cursor a page of view hands back after the path last:
// the path itself for the zero view, and for any other the view's key and the
// path, base64url-encoded behind declaredCursorPrefix.
func pageCursor(view formatgate.Set, last string) string {
	if view.IsZero() {
		return last
	}
	return declaredCursorPrefix + base64.RawURLEncoding.EncodeToString([]byte(view.Key()+"\x00"+last))
}

// pagePath reads a cursor a page of view was asked for back into the path
// the page continues after, and ErrCursorOfAnotherView for a cursor another
// view made: a declared view's cursor presented with the zero view, a path
// presented with a declared view, or one view's cursor with another's. ""
// is the first page of every view.
func pagePath(view formatgate.Set, cursor string) (string, error) {
	if cursor == "" {
		return "", nil
	}
	key, last, declared := parseDeclaredCursor(cursor)
	switch {
	case view.IsZero() && declared:
		return "", ErrCursorOfAnotherView
	case view.IsZero():
		return cursor, nil
	case !declared || key != view.Key():
		return "", ErrCursorOfAnotherView
	}
	return last, nil
}

// parseDeclaredCursor reads a declared view's cursor: its view's key and its
// path, and false for a string pageCursor never makes for a declared view.
func parseDeclaredCursor(cursor string) (key, last string, ok bool) {
	rest, found := strings.CutPrefix(cursor, declaredCursorPrefix)
	if !found {
		return "", "", false
	}
	raw, err := base64.RawURLEncoding.DecodeString(rest)
	if err != nil {
		return "", "", false
	}
	key, last, found = strings.Cut(string(raw), "\x00")
	if !found {
		return "", "", false
	}
	if set, ok := formatgate.ParseKey(key); !ok || set.IsZero() {
		return "", "", false
	}
	return key, last, true
}
