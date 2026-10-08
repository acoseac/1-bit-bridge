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
// body, a tombstone, a restore, or a cover hash on a live playlist. A
// smart-mix cover, a cover pruned from a playlist that is already
// tombstoned, and an identical body that writes nothing do not. A
// restore of the backup epoch runs in a process that holds no broker
// and publishes nothing.
//
// Library fires when a delta client would see a change: indexed_at
// moved, or a journaled deletion or suppression landed. The api
// publisher turns these notes into library.changed: one event at the
// end of a full scan, and one shared trailing debounce for every
// writer outside that scan.
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
// the clients already compare. The read uses a context the caller's cancel
// cannot stop: the commit has already landed.
func (s *Store) notePlaylists(ctx context.Context) {
	if s.syncHooks == nil || s.syncHooks.Playlists == nil {
		return
	}
	epoch, err := s.BackupEpoch(context.WithoutCancel(ctx))
	if err != nil || epoch == "" {
		return
	}
	s.syncHooks.Playlists(epoch)
}

// noteLibrary runs only after a commit that changed what a delta client
// sees. The watermark is the later of MAX(indexed_at),
// MAX(manifest_deletions.deleted_at) and the deletion-journal coverage
// start. A query error publishes nothing. An empty library with no
// coverage start publishes nothing from here; ScanEnded publishes the
// publisher clock when no watermark exists.
func (s *Store) noteLibrary(ctx context.Context) {
	if s.syncHooks == nil || s.syncHooks.Library == nil {
		return
	}
	ns, ok, err := libraryWatermark(context.WithoutCancel(ctx), s.db)
	if err != nil || !ok || ns <= 0 {
		return
	}
	s.syncHooks.Library(ns)
}

// LibraryWatermark is the later of MAX(indexed_at),
// MAX(manifest_deletions.deleted_at) and the deletion-journal coverage
// start. ok is false when all three are absent. A query error is
// returned and is not an empty library.
func (s *Store) LibraryWatermark(ctx context.Context) (int64, bool, error) {
	return libraryWatermark(ctx, s.db)
}

func libraryWatermark(ctx context.Context, db *sql.DB) (int64, bool, error) {
	var latest sql.NullInt64
	err := db.QueryRowContext(ctx, `
		SELECT MAX(v) FROM (
			SELECT MAX(indexed_at) AS v FROM tracks
			UNION ALL
			SELECT MAX(deleted_at) AS v FROM manifest_deletions
			UNION ALL
			SELECT CAST(v AS INTEGER) AS v FROM scan_state WHERE k = ?
		)`, deletionJournalCoverageKey).Scan(&latest)
	if err != nil {
		return 0, false, err
	}
	if !latest.Valid || latest.Int64 <= 0 {
		return 0, false, nil
	}
	return latest.Int64, true, nil
}
