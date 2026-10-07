package manifest

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"sort"
)

// PlaylistPutResult is the stamp a PUT left in place. Unchanged means the
// statement wrote nothing because the body already matched.
type PlaylistPutResult struct {
	LastModifiedAt int64
	Unchanged      bool
}

// PlaylistBaseMismatch is a revisioned PUT whose baseLastModifiedAt is not
// the stored stamp and whose body differs. Row is the stored playlist,
// including a deleted one (GetPlaylist hides those).
type PlaylistBaseMismatch struct {
	Row   PlaylistRow
	Items []PlaylistItemRow
}

func (e *PlaylistBaseMismatch) Error() string {
	return "manifest: playlist base does not match the stored stamp"
}

// PutPlaylist is UpsertPlaylist with an optional baseLastModifiedAt.
// A nil base keeps the stamp guard. A base equal to the stored stamp
// skips that guard: a behind-clock client is accepted, and a stamp that
// is not strictly greater becomes the stored stamp plus one nanosecond.
// An identical body writes nothing when a base is present, or when the
// row is deleted, and that is decided before the base is compared, so a
// stale base on the same body does not answer 409 and a repeat does not
// revive. A live playlist with no base stays on the stamp guard even
// when the body already matches: an older stamp is stale and a newer
// one is stored. A matching base with a different body is stored, which
// revives a deleted row: DELETE does not move last_modified_at, so that
// base is the pre-deletion version, and dropping the edit would leave
// the client believing the old playlist was accepted.
func (s *Store) PutPlaylist(ctx context.Context, deviceToken string, p PlaylistRow, items []PlaylistItemRow, base *int64) (PlaylistPutResult, error) {
	if deviceToken == "" {
		return PlaylistPutResult{}, errors.New("manifest: UpsertPlaylist requires a device token")
	}
	if p.ID == "" {
		return PlaylistPutResult{}, errors.New("manifest: UpsertPlaylist requires a playlist id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return PlaylistPutResult{}, err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit; unwind guard otherwise

	var name string
	var existingLMA, updatedAt int64
	var deleted int
	err = tx.QueryRowContext(ctx, `
		SELECT name, last_modified_at, updated_at, deleted
		  FROM playlists WHERE id = ?
	`, p.ID).Scan(&name, &existingLMA, &updatedAt, &deleted)
	if errors.Is(err, sql.ErrNoRows) {
		if err := writePlaylist(ctx, tx, s, deviceToken, p, items, p.LastModifiedAt); err != nil {
			return PlaylistPutResult{}, err
		}
		if err := tx.Commit(); err != nil {
			return PlaylistPutResult{}, err
		}
		return PlaylistPutResult{LastModifiedAt: p.LastModifiedAt}, nil
	}
	if err != nil {
		return PlaylistPutResult{}, err
	}
	stored, err := loadPlaylistItems(ctx, tx, p.ID)
	if err != nil {
		return PlaylistPutResult{}, err
	}
	same := playlistBodyEqual(name, stored, p.Name, items)

	// An identical body writes nothing when a base is present or the row
	// is deleted. A live playlist with no base keeps the stamp guard,
	// including when the body already matches.
	if same && (base != nil || deleted != 0) {
		return PlaylistPutResult{LastModifiedAt: existingLMA, Unchanged: true}, nil
	}

	if base != nil {
		if *base != existingLMA {
			return PlaylistPutResult{}, &PlaylistBaseMismatch{
				Row: PlaylistRow{
					ID:             p.ID,
					Name:           name,
					LastModifiedAt: existingLMA,
					UpdatedAt:      updatedAt,
					Deleted:        deleted != 0,
				},
				Items: stored,
			}
		}
		stamp := p.LastModifiedAt
		if stamp <= existingLMA {
			stamp = existingLMA + 1
		}
		p.LastModifiedAt = stamp
		if err := writePlaylist(ctx, tx, s, deviceToken, p, items, stamp); err != nil {
			return PlaylistPutResult{}, err
		}
		if err := tx.Commit(); err != nil {
			return PlaylistPutResult{}, err
		}
		return PlaylistPutResult{LastModifiedAt: stamp}, nil
	}

	if existingLMA > p.LastModifiedAt {
		return PlaylistPutResult{}, ErrPlaylistStale
	}
	if err := writePlaylist(ctx, tx, s, deviceToken, p, items, p.LastModifiedAt); err != nil {
		return PlaylistPutResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return PlaylistPutResult{}, err
	}
	return PlaylistPutResult{LastModifiedAt: p.LastModifiedAt}, nil
}

func loadPlaylistItems(ctx context.Context, tx *sql.Tx, id string) ([]PlaylistItemRow, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT position,
		       COALESCE(path, ''), COALESCE(origin_fingerprint, ''),
		       COALESCE(origin_path, ''), COALESCE(title, ''), COALESCE(artist, '')
		  FROM playlist_items
		 WHERE playlist_id = ?
		 ORDER BY position ASC
	`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []PlaylistItemRow
	for rows.Next() {
		var it PlaylistItemRow
		if err := rows.Scan(&it.Position, &it.Path, &it.OriginFingerprint,
			&it.OriginPath, &it.Title, &it.Artist); err != nil {
			return nil, err
		}
		items = append(items, it)
	}
	return items, rows.Err()
}

func playlistBodyEqual(storedName string, stored []PlaylistItemRow, name string, items []PlaylistItemRow) bool {
	if storedName != name {
		return false
	}
	if len(stored) != len(items) {
		return false
	}
	a := append([]PlaylistItemRow(nil), stored...)
	b := append([]PlaylistItemRow(nil), items...)
	sort.Slice(a, func(i, j int) bool { return a[i].Position < a[j].Position })
	sort.Slice(b, func(i, j int) bool { return b[i].Position < b[j].Position })
	return slices.Equal(a, b)
}

func writePlaylist(ctx context.Context, tx *sql.Tx, s *Store, deviceToken string, p PlaylistRow, items []PlaylistItemRow, stamp int64) error {
	now := s.now().UnixNano()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO playlists (id, device_token, name, last_modified_at, updated_at, deleted)
		VALUES (?, ?, ?, ?, ?, 0)
		ON CONFLICT(id) DO UPDATE SET
			device_token     = excluded.device_token,
			name             = excluded.name,
			last_modified_at = excluded.last_modified_at,
			updated_at       = excluded.updated_at,
			deleted          = 0,
			-- Cleared WITH the flag, exactly as RestorePlaylist does it.
			-- deleted_by is provenance for a tombstone, and this arm is
			-- the client's own revive-by-PUT: leaving it behind means a
			-- LIVE row carrying the token of a device whose delete has
			-- already been undone. Nothing reads it on a live row today
			-- (every reader filters deleted = 1) and the next tombstone
			-- overwrites it, so this is latent rather than live — but
			-- the burst warning and the Recently-deleted panel both
			-- attribute FROM this column, and a stale value on a revived
			-- row is the kind that names the wrong device once something
			-- does read it. The 2026-09-20 incident is on record
			-- precisely because attribution was the half that was
			-- missing.
			deleted_by       = ''
	`, p.ID, deviceToken, p.Name, stamp, now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM playlist_items WHERE playlist_id = ?`, p.ID); err != nil {
		return err
	}
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO playlist_items
			(playlist_id, position, path, origin_fingerprint, origin_path, title, artist)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, it := range items {
		if _, err := stmt.ExecContext(ctx, p.ID, it.Position, nullable(it.Path),
			nullable(it.OriginFingerprint), nullable(it.OriginPath),
			nullable(it.Title), nullable(it.Artist)); err != nil {
			return err
		}
	}
	return nil
}
