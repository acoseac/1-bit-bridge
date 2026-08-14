package manifest

import (
	"context"
	"database/sql"
	"errors"
)

// Favorites sentinels surfaced to the API handler.
var (
	// ErrFavoritesStale signals an inbound PUT whose client wall-clock
	// lastModifiedAt is strictly older than the stored document's. The
	// handler re-reads the server copy and returns it in the 409 body so
	// iOS can union-merge without a second GET. Equal is accepted
	// (idempotent re-push after a partial multi-bridge flush).
	ErrFavoritesStale = errors.New("manifest: favorites are stale (server copy is newer)")
)

// FavoritesMeta is the singleton document header. No json tags (wire-type
// discipline): the API wraps it in a DTO. DeviceToken records the device
// that LAST WROTE the document (admin provenance only — reads are
// user-wide, the playlists convention).
type FavoritesMeta struct {
	LastModifiedAt int64 // client wall-clock UnixNano (LWW guard)
	DeviceToken    string
	UpdatedAt      int64 // server receipt UnixNano
}

// FavoriteTrackRow is one favorited track. Either Path (local, slashless,
// resolvable on this bridge) or OriginFingerprint+OriginPath (foreign /
// opaque) is set, never both — the playlist_items convention minus
// position, plus the favorited-at stamp.
type FavoriteTrackRow struct {
	Path              string
	OriginFingerprint string
	OriginPath        string
	Title             string
	Artist            string
	FavoritedAt       int64 // client wall-clock UnixNano
}

// FavoriteAlbumRow is one favorited album, keyed by the iOS album identity
// triple. Year 0 ≡ absent (the dhowden/tag sentinel).
type FavoriteAlbumRow struct {
	AlbumArtist string
	Album       string
	Year        int
	FavoritedAt int64 // client wall-clock UnixNano
}

// UpsertFavorites replaces the singleton favorites document wholesale,
// atomically, under the same client-wall-clock LWW guard playlists use —
// enforced inside the transaction so there is no TOCTOU gap against a
// concurrent writer:
//
//   - a stored last_modified_at STRICTLY newer than the incoming one
//     rejects with ErrFavoritesStale (the handler re-reads + 409s the
//     full server copy so iOS can union-merge);
//   - equal is accepted (idempotent re-push).
//
// An empty tracks+albums set is a valid document ("no favorites") — the
// unfavorite direction propagates via full-set replace; there is no
// DELETE route. Holds s.mu; timestamps via s.now().
func (s *Store) UpsertFavorites(ctx context.Context, deviceToken string, lastModifiedAt int64,
	tracks []FavoriteTrackRow, albums []FavoriteAlbumRow) error {
	if deviceToken == "" {
		return errors.New("manifest: UpsertFavorites requires a device token")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit; unwind guard otherwise

	var existingLMA int64
	row := tx.QueryRowContext(ctx, `SELECT last_modified_at FROM favorites_meta WHERE id = 1`)
	switch err := row.Scan(&existingLMA); {
	case errors.Is(err, sql.ErrNoRows):
		// never stored — fall through
	case err != nil:
		return err
	default:
		if existingLMA > lastModifiedAt {
			return ErrFavoritesStale
		}
	}

	now := s.now().UnixNano()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO favorites_meta (id, last_modified_at, device_token, updated_at)
		VALUES (1, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			last_modified_at = excluded.last_modified_at,
			device_token     = excluded.device_token,
			updated_at       = excluded.updated_at
	`, lastModifiedAt, deviceToken, now); err != nil {
		return err
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM favorite_tracks`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM favorite_albums`); err != nil {
		return err
	}

	// One prepared statement reused across each insert loop (the
	// UpsertPlaylist shape) — per-row ExecContext re-prepares the SQL on
	// every row, measurable at the 50k-track cap.
	trackStmt, err := tx.PrepareContext(ctx, `
		INSERT INTO favorite_tracks
			(path, origin_fingerprint, origin_path, title, artist, favorited_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`)
	if err != nil {
		return err
	}
	defer trackStmt.Close()
	for _, t := range tracks {
		// Store-level local-XOR-foreign validation, mirroring the v36
		// table CHECK — the API handler already enforces this on the wire,
		// but a future store caller must not be able to persist a row with
		// no identity, both identities, or a partial foreign identity
		// (CodeRabbit on PR #695).
		local := t.Path != ""
		foreign := t.OriginFingerprint != "" && t.OriginPath != ""
		partialForeign := (t.OriginFingerprint != "") != (t.OriginPath != "")
		if local == foreign || partialForeign {
			return errors.New("manifest: favorite track must be strictly local (path) XOR foreign (originFingerprint + originPath)")
		}
		if _, err := trackStmt.ExecContext(ctx, nullable(t.Path),
			nullable(t.OriginFingerprint), nullable(t.OriginPath),
			nullable(t.Title), nullable(t.Artist), t.FavoritedAt); err != nil {
			return err
		}
	}

	albumStmt, err := tx.PrepareContext(ctx, `
		INSERT INTO favorite_albums (album_artist, album, year, favorited_at)
		VALUES (?, ?, ?, ?)
	`)
	if err != nil {
		return err
	}
	defer albumStmt.Close()
	for _, a := range albums {
		if _, err := albumStmt.ExecContext(ctx, a.AlbumArtist, a.Album, a.Year, a.FavoritedAt); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// GetFavorites returns the stored favorites document. A nil meta means
// "never stored" — the handler serves an empty doc with lastModifiedAt 0
// (singleton semantics: never a 404-as-missing). Entries come back
// newest-favorited first. Read path — no s.mu; the three queries run in ONE
// read-only transaction so a PUT committing between them can't produce a
// torn document (an old lastModifiedAt over new entry sets — CodeRabbit on
// PR #695).
func (s *Store) GetFavorites(ctx context.Context) (*FavoritesMeta, []FavoriteTrackRow, []FavoriteAlbumRow, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, nil, nil, err
	}
	defer func() { _ = tx.Rollback() }()

	var meta FavoritesMeta
	err = tx.QueryRowContext(ctx, `
		SELECT last_modified_at, device_token, updated_at
		  FROM favorites_meta WHERE id = 1
	`).Scan(&meta.LastModifiedAt, &meta.DeviceToken, &meta.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, nil, nil
	}
	if err != nil {
		return nil, nil, nil, err
	}

	trackRows, err := tx.QueryContext(ctx, `
		SELECT COALESCE(path, ''), COALESCE(origin_fingerprint, ''),
		       COALESCE(origin_path, ''), COALESCE(title, ''), COALESCE(artist, ''),
		       favorited_at
		  FROM favorite_tracks
		 ORDER BY favorited_at DESC
	`)
	if err != nil {
		return nil, nil, nil, err
	}
	defer trackRows.Close()
	var tracks []FavoriteTrackRow
	for trackRows.Next() {
		var t FavoriteTrackRow
		if err := trackRows.Scan(&t.Path, &t.OriginFingerprint, &t.OriginPath,
			&t.Title, &t.Artist, &t.FavoritedAt); err != nil {
			return nil, nil, nil, err
		}
		tracks = append(tracks, t)
	}
	if err := trackRows.Err(); err != nil {
		return nil, nil, nil, err
	}

	albumRows, err := tx.QueryContext(ctx, `
		SELECT album_artist, album, year, favorited_at
		  FROM favorite_albums
		 ORDER BY favorited_at DESC
	`)
	if err != nil {
		return nil, nil, nil, err
	}
	defer albumRows.Close()
	var albums []FavoriteAlbumRow
	for albumRows.Next() {
		var a FavoriteAlbumRow
		if err := albumRows.Scan(&a.AlbumArtist, &a.Album, &a.Year, &a.FavoritedAt); err != nil {
			return nil, nil, nil, err
		}
		albums = append(albums, a)
	}
	if err := albumRows.Err(); err != nil {
		return nil, nil, nil, err
	}
	return &meta, tracks, albums, nil
}
