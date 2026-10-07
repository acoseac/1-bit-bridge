package manifest

import (
	"context"
	"database/sql"
)

// SyncHooks are called after a user-data commit has landed. A nil hook
// publishes nothing, which is what the CLI and every test that does not
// install one does. The callbacks must not re-enter a Store writer: they
// run while the writer still holds s.mu, and the broker publish they reach
// is non-blocking.
//
// Favorites fires when favorites_meta.revision moved: a revisioned or
// legacy save that changed the document, and a tombstone collection that
// removed at least one row. A collection commits its own transaction
// before the save's compare-and-swap, so a later 409 or no-op save still
// leaves that revision event in place.
//
// Playlists fires when the playlist list changed: a PUT that stored a
// body, a tombstone, a restore, a cover hash that changed, or a backup
// epoch replacement. An identical body that writes nothing does not.
//
// Library fires when indexed_at moved. The api publisher turns these
// notes into library.changed: one event at the end of a full scan, and
// one shared trailing debounce for every writer outside that scan.
type SyncHooks struct {
	Favorites func(revision int64)
	Playlists func(epoch string)
	Library   func(watermarkNS int64)
}

// SetSyncHooks installs the callbacks. The zero value publishes nothing.
func (s *Store) SetSyncHooks(h SyncHooks) {
	s.mu.Lock()
	s.syncHooks = &h
	s.mu.Unlock()
}

// noteFavorites runs only after the revision commit. revision is the value
// just stored.
func (s *Store) noteFavorites(revision int64) {
	if s.syncHooks == nil || s.syncHooks.Favorites == nil || revision <= 0 {
		return
	}
	s.syncHooks.Favorites(revision)
}

// notePlaylists runs only after a list-changing commit. It reads the epoch
// the clients already compare.
func (s *Store) notePlaylists(ctx context.Context) {
	if s.syncHooks == nil || s.syncHooks.Playlists == nil {
		return
	}
	epoch, err := s.BackupEpoch(ctx)
	if err != nil || epoch == "" {
		return
	}
	s.syncHooks.Playlists(epoch)
}

// noteLibrary runs only after a commit that advanced indexed_at. The
// watermark is MAX(indexed_at), the same instant GET /v1/manifest?since=
// filters on.
func (s *Store) noteLibrary(ctx context.Context) {
	if s.syncHooks == nil || s.syncHooks.Library == nil {
		return
	}
	ns, ok := libraryWatermark(ctx, s.db)
	if !ok {
		return
	}
	s.syncHooks.Library(ns)
}

// LibraryWatermark is MAX(indexed_at) across tracks. ok is false when the
// table is empty.
func (s *Store) LibraryWatermark(ctx context.Context) (int64, bool) {
	return libraryWatermark(ctx, s.db)
}

func libraryWatermark(ctx context.Context, db *sql.DB) (int64, bool) {
	var max sql.NullInt64
	if err := db.QueryRowContext(ctx, `SELECT MAX(indexed_at) FROM tracks`).Scan(&max); err != nil || !max.Valid || max.Int64 <= 0 {
		return 0, false
	}
	return max.Int64, true
}

// ReplaceBackupEpochAndNotify writes a new epoch and, when hooks are
// installed, publishes playlists.changed. bridge restore opens a store of
// its own in a second process, where the hook is nil: a serving bridge
// learns the new epoch on the next GET. A test installs the hook on the
// store it calls.
func (s *Store) ReplaceBackupEpochAndNotify(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	epoch, err := ReplaceBackupEpoch(ctx, s.db)
	if err != nil {
		return "", err
	}
	s.notePlaylists(ctx)
	return epoch, nil
}
