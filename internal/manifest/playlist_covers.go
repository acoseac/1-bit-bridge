package manifest

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
)

// Cover scopes for playlist_covers.key disambiguation.
const (
	CoverScopeSmartMix = "smartmix" // key = smart-mix family slug
	CoverScopePlaylist = "playlist" // key = user-playlist id (uuid string)
)

// PlaylistCoverDir is the on-disk directory (under DataDir) holding the
// resized cover JPEGs.
func PlaylistCoverDir(dataDir string) string {
	return filepath.Join(dataDir, "playlist-covers")
}

// PlaylistCoverFilename is the on-disk basename for a cover. scope + key are
// sanitized so a hostile/odd key can't escape the covers dir (path traversal)
// — keys are slugs (`[a-z0-9-]`) or UUIDs in practice, but defend anyway.
func PlaylistCoverFilename(scope, key, ext string) string {
	if ext == "" {
		ext = "jpg"
	}
	return SanitizeCoverKey(scope) + "-" + SanitizeCoverKey(key) + "." + SanitizeCoverKey(ext)
}

// PlaylistCoverPath is the full on-disk path for a cover image.
func PlaylistCoverPath(dataDir, scope, key, ext string) string {
	return filepath.Join(PlaylistCoverDir(dataDir), PlaylistCoverFilename(scope, key, ext))
}

// SanitizeCoverKey collapses anything outside [A-Za-z0-9._-] to '_' so a
// scope/key/ext can't introduce path separators or traversal sequences into
// the cover filename. Empty input yields "_".
func SanitizeCoverKey(s string) string {
	if s == "" {
		return "_"
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// PlaylistCover maps an operator-uploaded cover (scope,key) to the stored
// image's content hash + file extension + updated-at (UnixNano). The resized
// JPEG bytes live on disk under <DataDir>/playlist-covers/; this row lets the
// wire DTO advertise `imageHash` so iOS can cache-bust on re-upload.
type PlaylistCover struct {
	Scope     string
	Key       string
	ImageHash string
	Ext       string
	UpdatedAt int64
}

// SetPlaylistCover upserts a cover mapping. Writer contract — s.mu.
func (s *Store) SetPlaylistCover(ctx context.Context, c PlaylistCover) error {
	if c.Scope == "" || c.Key == "" || c.ImageHash == "" {
		return errors.New("manifest: SetPlaylistCover requires scope, key, image_hash")
	}
	if c.Ext == "" {
		c.Ext = "jpg"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO playlist_covers (scope, key, image_hash, ext, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(scope, key) DO UPDATE SET
			image_hash = excluded.image_hash,
			ext        = excluded.ext,
			updated_at = excluded.updated_at
	`, c.Scope, c.Key, c.ImageHash, c.Ext, c.UpdatedAt)
	return err
}

// GetPlaylistCover returns the cover mapping for (scope,key); ok=false when
// none exists. Read path — no s.mu.
func (s *Store) GetPlaylistCover(ctx context.Context, scope, key string) (PlaylistCover, bool, error) {
	var c PlaylistCover
	err := s.db.QueryRowContext(ctx, `
		SELECT scope, key, image_hash, ext, updated_at
		  FROM playlist_covers WHERE scope = ? AND key = ?
	`, scope, key).Scan(&c.Scope, &c.Key, &c.ImageHash, &c.Ext, &c.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return PlaylistCover{}, false, nil
	}
	if err != nil {
		return PlaylistCover{}, false, err
	}
	return c, true, nil
}

// DeletePlaylistCover removes a cover mapping and returns the removed row's
// (hash, ext) so the caller can unlink the on-disk JPEG; ok=false when there
// was nothing to remove. Writer contract — s.mu.
func (s *Store) DeletePlaylistCover(ctx context.Context, scope, key string) (hash, ext string, ok bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row := s.db.QueryRowContext(ctx,
		`SELECT image_hash, ext FROM playlist_covers WHERE scope = ? AND key = ?`, scope, key)
	switch e := row.Scan(&hash, &ext); {
	case errors.Is(e, sql.ErrNoRows):
		return "", "", false, nil
	case e != nil:
		return "", "", false, e
	}
	if _, e := s.db.ExecContext(ctx,
		`DELETE FROM playlist_covers WHERE scope = ? AND key = ?`, scope, key); e != nil {
		return "", "", false, e
	}
	return hash, ext, true, nil
}

// PlaylistCoversByScope returns key→cover for a scope (bulk-resolve imageHash
// for the wire DTO in one query). Read path — no s.mu.
func (s *Store) PlaylistCoversByScope(ctx context.Context, scope string) (map[string]PlaylistCover, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT scope, key, image_hash, ext, updated_at
		  FROM playlist_covers WHERE scope = ?
	`, scope)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]PlaylistCover{}
	for rows.Next() {
		var c PlaylistCover
		if err := rows.Scan(&c.Scope, &c.Key, &c.ImageHash, &c.Ext, &c.UpdatedAt); err != nil {
			return nil, err
		}
		out[c.Key] = c
	}
	return out, rows.Err()
}

// PrunePlaylistCoversExcept removes covers in `scope` whose key is NOT in the
// keep set (retiring a smart-mix family that dropped out of regeneration, or a
// deleted playlist). Returns the removed rows so the caller can unlink the
// on-disk JPEGs. Writer contract — s.mu.
func (s *Store) PrunePlaylistCoversExcept(ctx context.Context, scope string, keep map[string]struct{}) ([]PlaylistCover, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.QueryContext(ctx,
		`SELECT scope, key, image_hash, ext, updated_at FROM playlist_covers WHERE scope = ?`, scope)
	if err != nil {
		return nil, err
	}
	var removed []PlaylistCover
	for rows.Next() {
		var c PlaylistCover
		if err := rows.Scan(&c.Scope, &c.Key, &c.ImageHash, &c.Ext, &c.UpdatedAt); err != nil {
			rows.Close()
			return nil, err
		}
		if _, ok := keep[c.Key]; !ok {
			removed = append(removed, c)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, c := range removed {
		if _, err := s.db.ExecContext(ctx,
			`DELETE FROM playlist_covers WHERE scope = ? AND key = ?`, c.Scope, c.Key); err != nil {
			return removed, err
		}
	}
	return removed, nil
}
