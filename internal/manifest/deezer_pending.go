package manifest

import (
	"context"
	"strings"
)

// The artist portraits enrich.deezerEnabled refused (backlog B761).
//
// The enricher stamps a track whose portrait lookup the Deezer switch refused
// like any other, so enriched_at says nothing of it, and Deezer's answers live
// only in the process. tracks.deezer_pending_query (v63) is what remembers it:
// non-NULL means the lookup was refused, and the value is the name it would
// have asked Deezer by, MusicBrainz's canonical name for the artist the row's
// artistMBID names, which no later read could recover (the tag is not that
// name, and the enricher's caches do not outlive a restart). MarkEnriched
// writes it in the stamp's own statement; a changed file's upsert drops it
// with the rest of the enrichment; "Retry missing" leaves it; and the
// enricher's portrait batch reads the rows that carry one (PendingPortraits)
// and clears each only at the version it read (ClearPendingPortraits), once
// the portrait is in the artwork cache or Deezer has answered it has none.
// Never on the wire: a client learns of it through /v1/artist-image's pending
// answer (ArtistMBIDPortraitPending).

// portraitWrite is what MarkEnriched writes of a row's pending portrait
// (Track.portrait).
type portraitWrite uint8

const (
	// portraitKept leaves the row's query as it is: the pass made no
	// portrait attempt, or its attempt failed (a transient failure, a
	// cancellation, any other error), which settles nothing.
	portraitKept portraitWrite = iota
	// portraitRefused keeps the name the refused lookup would have asked by.
	portraitRefused
	// portraitSettled clears the query: the portrait is in the cache, or
	// Deezer answered that it has none.
	portraitSettled
)

// PortraitRefused records on t that enrich.deezerEnabled refused the lookup
// of its artist's portrait, which would have asked Deezer by query: the next
// MarkEnriched keeps query in the row for the enricher's portrait batch to ask
// by once enrichment and Deezer are both on. Nothing is written for a Track
// with no artist MBID, which the batch would have nothing to file a portrait
// under, or for a name that is blank.
func (t *Track) PortraitRefused(query string) {
	t.portrait, t.portraitQuery = portraitRefused, query
}

// PortraitSettled records on t that its artist's portrait is in the artwork
// cache, found there or fetched, or that Deezer answered it has none: the
// next MarkEnriched clears the row's pending query.
func (t *Track) PortraitSettled() {
	t.portrait, t.portraitQuery = portraitSettled, ""
}

// portraitColumn is what MarkEnriched binds for the row's pending query: the
// write (markEnrichedSQL's CASE: 0 keeps the column, 1 sets it to query, 2
// clears it) and the query. A refusal for a Track with no artist MBID or no
// name keeps the column.
func (t *Track) portraitColumn() (write int, query any) {
	switch t.portrait {
	case portraitRefused:
		if !IsValidMBID(t.ArtistMBID) || strings.TrimSpace(t.portraitQuery) == "" {
			return 0, nil
		}
		return 1, strings.ToValidUTF8(t.portraitQuery, "�")
	case portraitSettled:
		return 2, nil
	}
	return 0, nil
}

// PendingPortrait is a row whose portrait lookup the Deezer switch refused,
// as the portrait batch reads it: its path, the artist MBID it carries, and
// the name the lookup would have asked by. version is the row's indexed_at
// when it was read, which ClearPendingPortraits requires the row to still
// have.
type PendingPortrait struct {
	Path       string
	ArtistMBID string
	Query      string
	version    int64
}

// pendingPortraitsSQL binds (after, limit). Its predicate carries
// idx_tracks_deezer_pending's WHERE verbatim, so the read is a range of that
// partial index in path order, past the batch's cursor. A row that waits for
// the enricher (enriched_at = 0, "Retry missing" among the ways there) is the
// ordinary pass's, which asks for its portrait itself.
const pendingPortraitsSQL = `
		SELECT path, indexed_at, COALESCE(json_extract(tags_json, '$.artistMBID'), ''), deezer_pending_query
		  FROM tracks
		 WHERE deezer_pending_query IS NOT NULL
		   AND enriched_at > 0
		   AND path > ?
		 ORDER BY path
		 LIMIT ?`

// PendingPortraits returns up to limit enriched rows that carry a pending
// portrait query, in path order from after the path after ("" is the
// start). Each carries the version it was read at. Read-only, un-mutexed (WAL).
func (s *Store) PendingPortraits(ctx context.Context, after string, limit int) ([]PendingPortrait, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, pendingPortraitsSQL, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PendingPortrait
	for rows.Next() {
		var p PendingPortrait
		if err := rows.Scan(&p.Path, &p.version, &p.ArtistMBID, &p.Query); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// clearPendingPortraitSQL binds (path, version). It clears the query only
// while the row is the one the batch read (the MarkEnriched pattern, backlog
// B187): a row the scanner rewrote since, or deleted and inserted again, has
// another indexed_at, and keeps whatever query its own enrichment wrote. It
// moves no indexed_at and leaves enriched_at: nothing a client reads changed.
const clearPendingPortraitSQL = `
		UPDATE tracks SET deezer_pending_query = NULL
		 WHERE path = ? AND indexed_at = ? AND deezer_pending_query IS NOT NULL`

// ClearPendingPortraits clears the pending query of each row, each only
// while it is still at the version PendingPortraits read it at, in one
// transaction, and returns how many it cleared. Holds s.mu, like every writer.
func (s *Store) ClearPendingPortraits(ctx context.Context, rows []PendingPortrait) (int, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.PrepareContext(ctx, clearPendingPortraitSQL)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()
	cleared := 0
	for _, r := range rows {
		res, err := stmt.ExecContext(ctx, r.Path, r.version)
		if err != nil {
			return 0, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return 0, err
		}
		cleared += int(n)
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return cleared, nil
}

// ArtistMBIDPortraitPending reports whether any track carrying the MBID in
// `artistMBID` keeps a portrait lookup the Deezer switch refused, which the
// enricher's portrait batch fetches once enrichment and Deezer are both on.
// /v1/artist-image asks it beside ArtistMBIDEnrichmentPending, and only while
// Deezer is on (internal/api's artistImagePending). Seeks on
// idx_tracks_artist_mbid, as the other probes do. A database fault is
// returned, never folded into the bool: the handler fails open to 202.
func (s *Store) ArtistMBIDPortraitPending(ctx context.Context, mbid string) (bool, error) {
	if mbid == "" {
		return false, nil
	}
	var pending bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM tracks
		 WHERE json_extract(tags_json, '$.artistMBID') = ? AND deezer_pending_query IS NOT NULL)`, mbid).Scan(&pending)
	if err != nil {
		// Log-only ctx gate, as pendingEnrichmentWithJSONField's.
		if ctx.Err() == nil {
			logger.Error("ArtistMBIDPortraitPending", "err", err)
		}
		return false, err
	}
	return pending, nil
}
