package manifest

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// The artist portraits that wait for the enricher's portrait batch (backlog
// B761): a lookup enrich.deezerEnabled refused, and one deferred, whose
// attempt failed or whose artist's retry by that name was not due.
//
// The enricher stamps such a track like any other, so enriched_at says nothing
// of it, and its memory does not outlive a restart. tracks.deezer_pending_query
// (v63) keeps the name the lookup asks Deezer by, MusicBrainz's canonical name
// for the artist, which no later read could recover (the tag is not that
// name), and tracks.deezer_pending_mbid (v64) the artist it was taken for. A
// query is the row's only while that artist is the one the row's tags_json
// names, and every writer and reader holds to it: MarkEnriched keeps one only
// for the artist its stamp writes, PendingPortraits and
// ArtistMBIDPortraitPending read one only beside its own artist, and each open
// drops the ones an older binary's stamp gave another artist, since its
// statements write the artist and leave these columns
// (reconcilePendingPortraits). MarkEnriched writes them in the stamp's own
// statement; a changed file's upsert drops them with the rest of the
// enrichment; "Retry missing" leaves them; and the enricher's portrait batch
// reads the rows that carry one (PendingPortraits) and clears each only at
// the version it read (ClearPendingPortraits), once the portrait is in the
// artwork cache or an answer ends the name.
//
// deezer_portrait_attempts (v64) keeps, per artist and name, the attempts that
// failed in a row and when the last did: the one backoff every path that asks
// Deezer for a portrait honours, and the bound past which the route no longer
// calls the portrait pending (PortraitPendingAttempts). It outlives a restart;
// a portrait held forgets the artist's attempts, an answer that ends a name
// that name's, and each open the ones no waiting row names. Never on the wire:
// a client learns of all this through /v1/artist-image's pending answer
// (ArtistMBIDPortraitPending).

// portraitWrite is what MarkEnriched writes of a row's pending portrait
// (Track.portrait).
type portraitWrite uint8

const (
	// portraitKept leaves the row's query and its artist as they are while
	// that artist is the one the stamp writes, and drops them otherwise: the
	// pass learnt nothing of the portrait (it made no attempt).
	portraitKept portraitWrite = iota
	// portraitPending keeps the name the lookup that waits would ask by, and
	// the artist it was taken for.
	portraitPending
	// portraitSettled clears both: the portrait is in the cache, or an
	// answer ended the name.
	portraitSettled
)

// PortraitPending records on t that the lookup of mbid's portrait by query
// waits for the portrait batch: enrich.deezerEnabled refused it, or it was
// deferred (its attempt failed, or the artist's retry by that name was not
// due). The next MarkEnriched keeps query and mbid in the row, for the batch
// to ask by once enrichment and Deezer are both on and the retry is due.
// Nothing is written for an mbid that is no MBID or not the artist the stamp
// gives the row, or for a name that is blank: the stamp then keeps the row's
// columns as a pass that learnt nothing would.
func (t *Track) PortraitPending(mbid, query string) {
	t.portrait, t.portraitQuery, t.portraitMBID = portraitPending, query, mbid
}

// PortraitSettled records on t that its artist's portrait is in the artwork
// cache, found there or fetched, or that an answer ended the name it waits by:
// the next MarkEnriched clears the row's pending query and its artist.
func (t *Track) PortraitSettled() {
	t.portrait, t.portraitQuery, t.portraitMBID = portraitSettled, "", ""
}

// portraitColumns is what MarkEnriched binds for the row's pending portrait:
// the write (markEnrichedSQL's CASE: 1 sets the query and its artist, 2 clears
// them, 0 keeps them while their artist is the stamp's) and the query.
func (t *Track) portraitColumns() (write int, query any) {
	switch t.portrait {
	case portraitPending:
		if !IsValidMBID(t.portraitMBID) || t.portraitMBID != t.ArtistMBID || strings.TrimSpace(t.portraitQuery) == "" {
			return 0, nil
		}
		return 1, portraitName(t.portraitQuery)
	case portraitSettled:
		return 2, nil
	}
	return 0, nil
}

// portraitName is a name as the store keeps it, in a row and as an attempts
// key alike: valid UTF-8.
func portraitName(query string) string { return strings.ToValidUTF8(query, "�") }

// PendingPortrait is a row whose portrait lookup waits, as the portrait batch
// reads it: its path, the artist the lookup was taken for (the row's own), and
// the name it asks by. version is the row's indexed_at when it was read, which
// ClearPendingPortraits requires the row to still have.
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
// ordinary pass's, which asks for its portrait itself, and a row whose query
// was taken for another artist than the one it names now is no lookup at all.
const pendingPortraitsSQL = `
		SELECT path, indexed_at, deezer_pending_mbid, deezer_pending_query
		  FROM tracks
		 WHERE deezer_pending_query IS NOT NULL
		   AND enriched_at > 0
		   AND path > ?
		   AND deezer_pending_mbid = json_extract(tags_json, '$.artistMBID')
		 ORDER BY path
		 LIMIT ?`

// PendingPortraits returns up to limit enriched rows that carry a pending
// portrait query for their own artist, in path order from after the path
// after ("" is the start). Each carries the version it was read at.
// Read-only, un-mutexed (WAL).
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
		UPDATE tracks SET deezer_pending_query = NULL, deezer_pending_mbid = NULL
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

// PortraitPendingAttempts is how many failed attempts at an artist's portrait
// by one name the route still calls pending: a lookup whose name has failed
// that many times in a row answers no_image, though the row keeps its query
// and the batch asks again at each retry (a minute after the first failure,
// doubling to six hours), so a portrait that arrives later is served. At five
// the route stops after about a quarter of an hour of failures (a minute, then
// two, four and eight before the fifth attempt).
const PortraitPendingAttempts = 5

// artistPortraitPendingSQL binds (mbid, PortraitPendingAttempts). It seeks
// the artist's waiting rows in idx_tracks_deezer_pending_artist, whose WHERE
// it carries verbatim, takes a row only beside its own artist, and only while
// the name it waits by has failed fewer times than the bound.
const artistPortraitPendingSQL = `
		SELECT EXISTS(SELECT 1 FROM tracks
		 WHERE deezer_pending_mbid = ?
		   AND deezer_pending_query IS NOT NULL
		   AND json_extract(tags_json, '$.artistMBID') = deezer_pending_mbid
		   AND NOT EXISTS (SELECT 1 FROM deezer_portrait_attempts
		                    WHERE artist_mbid = tracks.deezer_pending_mbid
		                      AND query = tracks.deezer_pending_query
		                      AND failures >= ?))`

// ArtistMBIDPortraitPending reports whether any track of the artist
// `artistMBID` names waits for its portrait by a name that has failed fewer
// than PortraitPendingAttempts times, which the enricher's portrait batch asks
// for once enrichment and Deezer are both on. /v1/artist-image asks it beside
// ArtistMBIDEnrichmentPending, and only while Deezer is on (internal/api's
// artistImagePending). A database fault is returned, never folded into the
// bool: the handler fails open to 202.
func (s *Store) ArtistMBIDPortraitPending(ctx context.Context, mbid string) (bool, error) {
	if mbid == "" {
		return false, nil
	}
	var pending bool
	err := s.db.QueryRowContext(ctx, artistPortraitPendingSQL, mbid, PortraitPendingAttempts).Scan(&pending)
	if err != nil {
		// Log-only ctx gate, as pendingEnrichmentWithJSONField's.
		if ctx.Err() == nil {
			logger.Error("ArtistMBIDPortraitPending", "err", err)
		}
		return false, err
	}
	return pending, nil
}

// PortraitAttempts is what the store keeps of the attempts at an artist's
// portrait by one name that failed in a row: how many, and when the last did.
// The zero value is none.
type PortraitAttempts struct {
	Failures     int
	LastFailedAt time.Time
}

// PortraitAttemptsOf is the failed attempts the store keeps at mbid's
// portrait by query. Read-only, un-mutexed (WAL).
func (s *Store) PortraitAttemptsOf(ctx context.Context, mbid, query string) (PortraitAttempts, error) {
	var failures int
	var last int64
	err := s.db.QueryRowContext(ctx, `SELECT failures, last_failed_at FROM deezer_portrait_attempts
		 WHERE artist_mbid = ? AND query = ?`, mbid, portraitName(query)).Scan(&failures, &last)
	if errors.Is(err, sql.ErrNoRows) {
		return PortraitAttempts{}, nil
	}
	if err != nil {
		return PortraitAttempts{}, err
	}
	return PortraitAttempts{Failures: failures, LastFailedAt: time.Unix(0, last)}, nil
}

// NotePortraitFailure adds one to the failed attempts at mbid's portrait by
// query, the last at at, and returns what the store now keeps. Holds s.mu.
func (s *Store) NotePortraitFailure(ctx context.Context, mbid, query string, at time.Time) (PortraitAttempts, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var failures int
	var last int64
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO deezer_portrait_attempts (artist_mbid, query, failures, last_failed_at)
		VALUES (?, ?, 1, ?)
		ON CONFLICT(artist_mbid, query) DO UPDATE SET failures = failures + 1, last_failed_at = excluded.last_failed_at
		RETURNING failures, last_failed_at`, mbid, portraitName(query), at.UnixNano()).Scan(&failures, &last)
	if err != nil {
		return PortraitAttempts{}, err
	}
	return PortraitAttempts{Failures: failures, LastFailedAt: time.Unix(0, last)}, nil
}

// ForgetPortraitAttempts forgets the failed attempts at mbid's portrait by
// query: an answer ended that name. Holds s.mu.
func (s *Store) ForgetPortraitAttempts(ctx context.Context, mbid, query string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx, `DELETE FROM deezer_portrait_attempts WHERE artist_mbid = ? AND query = ?`,
		mbid, portraitName(query))
	return err
}

// ForgetArtistPortraitAttempts forgets the failed attempts at mbid's portrait
// by every name: the portrait is in the cache. Holds s.mu.
func (s *Store) ForgetArtistPortraitAttempts(ctx context.Context, mbid string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx, `DELETE FROM deezer_portrait_attempts WHERE artist_mbid = ?`, mbid)
	return err
}

// strayPendingPortraitsSQL drops the pending portraits no longer bound to
// their row's artist: an older binary's stamp wrote another artist and left
// these columns, which it does not know, or the query names no artist at all.
// Its predicate carries the partial indexes' WHERE verbatim, so it reads the
// waiting rows alone. It moves no indexed_at and leaves enriched_at.
const strayPendingPortraitsSQL = `
		UPDATE tracks SET deezer_pending_query = NULL, deezer_pending_mbid = NULL
		 WHERE deezer_pending_query IS NOT NULL
		   AND (deezer_pending_mbid IS NULL
		        OR deezer_pending_mbid IS NOT json_extract(tags_json, '$.artistMBID'))`

// unnamedPortraitAttemptsSQL forgets the failed attempts no waiting row names
// any more: its rows were settled, retagged or deleted.
const unnamedPortraitAttemptsSQL = `
		DELETE FROM deezer_portrait_attempts
		 WHERE NOT EXISTS (SELECT 1 FROM tracks
		                    WHERE deezer_pending_mbid = deezer_portrait_attempts.artist_mbid
		                      AND deezer_pending_query = deezer_portrait_attempts.query
		                      AND deezer_pending_query IS NOT NULL)`

// reconcilePendingPortraits is each open's check of the pending portraits
// (backlog B761): an older binary run against the data dir between two opens
// of this one writes a row's artist and leaves the columns it does not know,
// so a query kept for one artist would stand beside another, which the batch
// would ask Deezer by and file under the other. It drops those, and the failed
// attempts no waiting row names. Every reader asks the binding as well, so a
// row that slips past here is still no lookup.
func (s *Store) reconcilePendingPortraits(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, strayPendingPortraitsSQL); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, unnamedPortraitAttemptsSQL)
	return err
}
