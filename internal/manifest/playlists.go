package manifest

import (
	"context"
	"database/sql"
	"errors"
)

// Playlist-backup sentinels surfaced to the API handler.
var (
	// ErrPlaylistStale signals an inbound PUT whose client wall-clock
	// last_modified_at is strictly older than the stored copy. The handler
	// re-reads the server copy and returns it in a 409 body so iOS can
	// reconcile. Backup hygiene only — single-device backup rarely hits it.
	ErrPlaylistStale = errors.New("manifest: playlist is stale (server copy is newer)")
	// ErrPlaylistOwnedByOther signals a PUT/DELETE for a playlist id that
	// already exists under a different device token. UUID collisions across
	// devices are practically impossible; this guards a malicious client
	// from overwriting another device's backup by guessing its id.
	ErrPlaylistOwnedByOther = errors.New("manifest: playlist is owned by another device")
)

// PlaylistRow is the playlists-table row shape. No json tags (wire-type
// discipline): the API wraps it in a DTO. Timestamps are UnixNano integers
// (the wire form for last_modified_at is an integer too — no time.Time
// round-trip, no truncation risk on the LWW-critical field).
type PlaylistRow struct {
	ID             string
	DeviceToken    string
	Name           string
	LastModifiedAt int64 // client wall-clock UnixNano (LWW guard)
	UpdatedAt      int64 // server receipt UnixNano
	Deleted        bool
}

// PlaylistItemRow is one ordered entry. Either Path (local, resolvable on
// this bridge) or OriginFingerprint+OriginPath (foreign/opaque) is set,
// never both. Title/Artist are render fallback for the admin surface.
type PlaylistItemRow struct {
	Position          int
	Path              string
	OriginFingerprint string
	OriginPath        string
	Title             string
	Artist            string
}

// PlaylistSummary is the per-device list row (no items).
type PlaylistSummary struct {
	ID             string
	Name           string
	TrackCount     int
	LastModifiedAt int64
}

// nullable returns a *string for SQL binding: nil for "" (stored as NULL,
// so a local item's empty origin columns and a foreign item's empty path
// stay distinguishable), the value otherwise.
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// UpsertPlaylist stores (or replaces) a playlist + its items for a device,
// atomically. It enforces two guards inside the transaction so there is no
// TOCTOU gap against a concurrent writer:
//
//   - ownership: an existing row under a different device token rejects
//     with ErrPlaylistOwnedByOther;
//   - LWW: an existing row with a strictly-newer last_modified_at rejects
//     with ErrPlaylistStale (the handler re-reads + 409s the server copy).
//
// On success the row is (re)written with deleted=0 and updated_at=now, and
// its items are fully replaced. Holds s.mu; timestamps via s.now().
func (s *Store) UpsertPlaylist(ctx context.Context, deviceToken string, p PlaylistRow, items []PlaylistItemRow) error {
	if deviceToken == "" {
		return errors.New("manifest: UpsertPlaylist requires a device token")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit; unwind guard otherwise

	var existingOwner string
	var existingLMA int64
	row := tx.QueryRowContext(ctx, `SELECT device_token, last_modified_at FROM playlists WHERE id = ?`, p.ID)
	switch err := row.Scan(&existingOwner, &existingLMA); {
	case errors.Is(err, sql.ErrNoRows):
		// fresh insert — fall through
	case err != nil:
		return err
	default:
		if existingOwner != deviceToken {
			return ErrPlaylistOwnedByOther
		}
		if existingLMA > p.LastModifiedAt {
			return ErrPlaylistStale
		}
	}

	now := s.now().UnixNano()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO playlists (id, device_token, name, last_modified_at, updated_at, deleted)
		VALUES (?, ?, ?, ?, ?, 0)
		ON CONFLICT(id) DO UPDATE SET
			name             = excluded.name,
			last_modified_at = excluded.last_modified_at,
			updated_at       = excluded.updated_at,
			deleted          = 0
	`, p.ID, deviceToken, p.Name, p.LastModifiedAt, now); err != nil {
		return err
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM playlist_items WHERE playlist_id = ?`, p.ID); err != nil {
		return err
	}
	for _, it := range items {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO playlist_items
				(playlist_id, position, path, origin_fingerprint, origin_path, title, artist)
			VALUES (?, ?, ?, ?, ?, ?, ?)
		`, p.ID, it.Position, nullable(it.Path), nullable(it.OriginFingerprint),
			nullable(it.OriginPath), nullable(it.Title), nullable(it.Artist)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// GetPlaylist returns a device's playlist + ordered items, or (nil, nil,
// nil) when the id is unknown, tombstoned, or owned by another device
// (scoping makes other devices' rows invisible). Read path — no s.mu.
func (s *Store) GetPlaylist(ctx context.Context, deviceToken, id string) (*PlaylistRow, []PlaylistItemRow, error) {
	var p PlaylistRow
	err := s.db.QueryRowContext(ctx, `
		SELECT id, device_token, name, last_modified_at, updated_at
		  FROM playlists
		 WHERE id = ? AND device_token = ? AND deleted = 0
	`, id, deviceToken).Scan(&p.ID, &p.DeviceToken, &p.Name, &p.LastModifiedAt, &p.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT position,
		       COALESCE(path, ''), COALESCE(origin_fingerprint, ''),
		       COALESCE(origin_path, ''), COALESCE(title, ''), COALESCE(artist, '')
		  FROM playlist_items
		 WHERE playlist_id = ?
		 ORDER BY position ASC
	`, id)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var items []PlaylistItemRow
	for rows.Next() {
		var it PlaylistItemRow
		if err := rows.Scan(&it.Position, &it.Path, &it.OriginFingerprint,
			&it.OriginPath, &it.Title, &it.Artist); err != nil {
			return nil, nil, err
		}
		items = append(items, it)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	return &p, items, nil
}

// ListPlaylists returns a device's live (non-tombstoned) playlist
// summaries, newest-modified first. Read path — no s.mu.
func (s *Store) ListPlaylists(ctx context.Context, deviceToken string) ([]PlaylistSummary, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT p.id, p.name, p.last_modified_at,
		       (SELECT COUNT(*) FROM playlist_items i WHERE i.playlist_id = p.id) AS track_count
		  FROM playlists p
		 WHERE p.device_token = ? AND p.deleted = 0
		 ORDER BY p.last_modified_at DESC
	`, deviceToken)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PlaylistSummary
	for rows.Next() {
		var s PlaylistSummary
		if err := rows.Scan(&s.ID, &s.Name, &s.LastModifiedAt, &s.TrackCount); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// TombstonePlaylist marks a device's playlist deleted (so the delete
// propagates instead of the row reappearing on the next backup sweep).
// Returns false when no live row matched (unknown / already-deleted /
// owned by another device). Holds s.mu; updated_at via s.now().
func (s *Store) TombstonePlaylist(ctx context.Context, deviceToken, id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.ExecContext(ctx, `
		UPDATE playlists SET deleted = 1, updated_at = ?
		 WHERE id = ? AND device_token = ? AND deleted = 0
	`, s.now().UnixNano(), id, deviceToken)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// ListAllPlaylistsForAdmin returns every device's live playlist summary
// for the loopback admin surface, paired with the owning device token.
// Read path — no s.mu. Admin-only; never exposed on /v1.
func (s *Store) ListAllPlaylistsForAdmin(ctx context.Context) ([]AdminPlaylistSummary, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT p.id, p.device_token, p.name, p.last_modified_at, p.updated_at,
		       (SELECT COUNT(*) FROM playlist_items i WHERE i.playlist_id = p.id) AS track_count
		  FROM playlists p
		 WHERE p.deleted = 0
		 ORDER BY p.updated_at DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AdminPlaylistSummary
	for rows.Next() {
		var a AdminPlaylistSummary
		if err := rows.Scan(&a.ID, &a.DeviceToken, &a.Name, &a.LastModifiedAt, &a.UpdatedAt, &a.TrackCount); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// AdminPlaylistSummary is the admin-surface row (carries the owning device
// token, unlike the per-device wire summary). No json tags — admin wraps it.
type AdminPlaylistSummary struct {
	ID             string
	DeviceToken    string
	Name           string
	LastModifiedAt int64
	UpdatedAt      int64
	TrackCount     int
}
