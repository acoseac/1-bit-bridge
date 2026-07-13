package manifest

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// PDF album booklets (v1.8): per-release availability + local fetch state,
// learned from Atlas via the harvest credential and served to iOS at
// GET /v1/booklet/{releaseMBID}. The `booklets` table (migration v24) is
// keyed by the release's MusicBrainz GID (`musicBrainzAlbumID` on tracks —
// NOT artworkMBID, since a locally-curated cover doesn't preclude a
// booklet). Provider identity never touches these rows or the wire.

// DistinctAlbumReleaseMBIDs enumerates the library's distinct
// musicBrainzAlbumID release GIDs — the booklet-check universe. Broader
// than DistinctReleaseMBIDs (which keys on artworkMBID, the cover set):
// a release with locally-curated artwork still has a booklet-eligible
// musicBrainzAlbumID. Full-table json_extract scan (backed by
// idx_tracks_release_mbid for the per-value lookups, but the DISTINCT
// enumeration walks the table) — slow-cadence callers only (the 6h
// booklet check), never a request hot path. Un-mutexed read.
func (s *Store) DistinctAlbumReleaseMBIDs(ctx context.Context) ([]string, error) {
	return collectStringColumn(s.db.QueryContext(ctx, `
		SELECT DISTINCT json_extract(tags_json, '$.musicBrainzAlbumID')
		  FROM tracks
		 WHERE json_extract(tags_json, '$.musicBrainzAlbumID') IS NOT NULL
		   AND json_extract(tags_json, '$.musicBrainzAlbumID') != ''
		   AND json_extract(tags_json, '$.musicBrainzAlbumID') NOT LIKE 'local-%'
	`))
}

// BookletRow mirrors one `booklets` row.
type BookletRow struct {
	ReleaseMBID   string
	Available     bool
	Etag          string
	Bytes         int64
	CheckAttempts int
	CheckedAt     time.Time
	// FetchedAt is nil until the PDF has been downloaded into the local
	// booklet cache dir.
	FetchedAt *time.Time
}

// UpsertBookletAvailability records one availability-check verdict.
// Available=true resets the attempt counter and stores the content tag;
// a miss increments the counter (the check loop stops re-asking at its
// attempt cap). fetched_at survives an available→available re-check but is
// CLEARED when the etag changes (the cached PDF is stale) or availability
// drops. Holds s.mu (writer contract).
func (s *Store) UpsertBookletAvailability(ctx context.Context, mbid string, available bool, etag string, size int64) error {
	if mbid == "" {
		return errors.New("UpsertBookletAvailability: empty mbid")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now().UnixNano()
	if available {
		_, err := s.db.ExecContext(ctx, `
			INSERT INTO booklets (release_mbid, available, etag, bytes, check_attempts, checked_at, fetched_at)
			VALUES (?, 1, ?, ?, 0, ?, NULL)
			ON CONFLICT(release_mbid) DO UPDATE SET
				available      = 1,
				bytes          = excluded.bytes,
				check_attempts = 0,
				checked_at     = excluded.checked_at,
				fetched_at     = CASE WHEN booklets.etag = excluded.etag THEN booklets.fetched_at ELSE NULL END,
				etag           = excluded.etag
		`, mbid, etag, size, now)
		return err
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO booklets (release_mbid, available, etag, bytes, check_attempts, checked_at, fetched_at)
		VALUES (?, 0, '', 0, 1, ?, NULL)
		ON CONFLICT(release_mbid) DO UPDATE SET
			available      = 0,
			etag           = '',
			bytes          = 0,
			check_attempts = booklets.check_attempts + 1,
			checked_at     = excluded.checked_at,
			fetched_at     = NULL
	`, mbid, now)
	return err
}

// SetBookletTagAndBumpIndex stamps the wire tag on EVERY track of the
// release (whole-album UPDATE keyed on $.musicBrainzAlbumID — index-backed
// by idx_tracks_release_mbid) and strict-advances indexed_at so iOS
// delta-sync re-receives the rows. Clone of SetArtworkVersionAndBumpIndex:
// same CASE-WHEN monotonic form, same no-op guard on an unchanged tag, and
// deliberately NO enriched_at touch (this is not (re-)enrichment — touching
// it would re-trigger the MB/CAA/Deezer treadmill). Holds s.mu.
func (s *Store) SetBookletTagAndBumpIndex(ctx context.Context, releaseMBID, tag string) (int64, error) {
	if releaseMBID == "" {
		return 0, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now().UnixNano()
	res, err := s.db.ExecContext(ctx, `
		UPDATE tracks SET
			booklet_tag = ?,
			indexed_at = CASE
				WHEN indexed_at >= ? THEN indexed_at + 1
				ELSE ?
			END
		WHERE json_extract(tags_json, '$.musicBrainzAlbumID') = ?
		  AND COALESCE(booklet_tag, '') <> COALESCE(?, '')
	`, nullifyEmpty(tag), now, now, releaseMBID, nullifyEmpty(tag))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// nullifyEmpty maps "" to SQL NULL so a cleared tag stores NULL (matching
// the never-set state) instead of a distinct empty-string value.
func nullifyEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// BookletsToCheck filters the given candidate release MBIDs (the library's
// distinct musicBrainzAlbumID universe, enumerated by the caller) down to
// the ones worth asking Atlas about: not yet available AND under the
// attempt cap. Unknown MBIDs (no row yet) are included. Read-only, no s.mu.
func (s *Store) BookletsToCheck(ctx context.Context, candidates []string, maxAttempts int) ([]string, error) {
	if len(candidates) == 0 {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT release_mbid, available, check_attempts FROM booklets
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type state struct {
		available bool
		attempts  int
	}
	known := make(map[string]state)
	for rows.Next() {
		var mbid string
		var avail, attempts int
		if err := rows.Scan(&mbid, &avail, &attempts); err != nil {
			return nil, err
		}
		known[mbid] = state{available: avail != 0, attempts: attempts}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(candidates))
	for _, m := range candidates {
		st, ok := known[m]
		if !ok {
			out = append(out, m)
			continue
		}
		if !st.available && st.attempts < maxAttempts {
			out = append(out, m)
		}
	}
	return out, nil
}

// BookletsToFetch returns available-but-not-yet-downloaded rows,
// oldest-checked first, bounded by limit. Read-only, no s.mu.
func (s *Store) BookletsToFetch(ctx context.Context, limit int) ([]BookletRow, error) {
	if limit <= 0 {
		limit = 10
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT release_mbid, available, etag, bytes, check_attempts, checked_at, fetched_at
		  FROM booklets
		 WHERE available = 1 AND fetched_at IS NULL
		 ORDER BY checked_at ASC
		 LIMIT ?
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BookletRow
	for rows.Next() {
		b, err := scanBookletRow(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// MarkBookletFetched stamps fetched_at after the PDF landed on disk.
// Holds s.mu.
func (s *Store) MarkBookletFetched(ctx context.Context, mbid string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx, `
		UPDATE booklets SET fetched_at = ? WHERE release_mbid = ?
	`, s.now().UnixNano(), mbid)
	return err
}

// MarkBookletUnavailable flips a row unavailable (Atlas 404'd the fetch —
// e.g. the asset was evicted upstream between check and fetch). The caller
// clears the wire tag separately via SetBookletTagAndBumpIndex(mbid, "").
// Holds s.mu.
func (s *Store) MarkBookletUnavailable(ctx context.Context, mbid string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx, `
		UPDATE booklets SET available = 0, etag = '', bytes = 0, fetched_at = NULL,
		       checked_at = ?
		 WHERE release_mbid = ?
	`, s.now().UnixNano(), mbid)
	return err
}

// GetBooklet returns one row, or (nil, nil) when absent. Read-only.
func (s *Store) GetBooklet(ctx context.Context, mbid string) (*BookletRow, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT release_mbid, available, etag, bytes, check_attempts, checked_at, fetched_at
		  FROM booklets WHERE release_mbid = ?
	`, mbid)
	b, err := scanBookletRow(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &b, nil
}

// BookletCounts reports (available, cached-on-disk) for the admin card.
// Read-only.
func (s *Store) BookletCounts(ctx context.Context) (available, cached int, err error) {
	err = s.db.QueryRowContext(ctx, `
		SELECT
			(SELECT COUNT(*) FROM booklets WHERE available = 1),
			(SELECT COUNT(*) FROM booklets WHERE available = 1 AND fetched_at IS NOT NULL)
	`).Scan(&available, &cached)
	return available, cached, err
}

// DeleteBookletsNotIn is the orphan GC: removes rows whose release_mbid is
// no longer in the library's universe (the caller passes the same
// enumeration the check loop just used, so no extra scan) and returns the
// removed MBIDs so the caller can delete the cached PDFs — rows and files
// go together, mirroring the removeSidecarFiles discipline. An empty
// universe is a NO-OP, not "delete everything": a transient enumeration
// failure upstream must never wipe the cache. Holds s.mu.
func (s *Store) DeleteBookletsNotIn(ctx context.Context, universe []string) ([]string, error) {
	if len(universe) == 0 {
		return nil, nil
	}
	blob, err := json.Marshal(universe)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Collect-then-delete inside one lock hold so the returned set exactly
	// matches the deleted rows.
	orphans, err := collectStringColumn(s.db.QueryContext(ctx, `
		SELECT release_mbid FROM booklets
		 WHERE release_mbid NOT IN (SELECT value FROM json_each(?))
	`, string(blob)))
	if err != nil {
		return nil, err
	}
	if len(orphans) == 0 {
		return nil, nil
	}
	if _, err := s.db.ExecContext(ctx, `
		DELETE FROM booklets
		 WHERE release_mbid NOT IN (SELECT value FROM json_each(?))
	`, string(blob)); err != nil {
		return nil, err
	}
	return orphans, nil
}

// scanBookletRow scans the canonical 7-column booklet SELECT.
func scanBookletRow(scan func(...any) error) (BookletRow, error) {
	var b BookletRow
	var avail int
	var checkedAt int64
	var fetchedAt sql.NullInt64
	if err := scan(&b.ReleaseMBID, &avail, &b.Etag, &b.Bytes, &b.CheckAttempts, &checkedAt, &fetchedAt); err != nil {
		return BookletRow{}, err
	}
	b.Available = avail != 0
	b.CheckedAt = time.Unix(0, checkedAt)
	if fetchedAt.Valid {
		t := time.Unix(0, fetchedAt.Int64)
		b.FetchedAt = &t
	}
	return b, nil
}
