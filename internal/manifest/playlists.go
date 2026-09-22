package manifest

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"time"
)

// Playlist-backup sentinels surfaced to the API handler.
var (
	// ErrPlaylistStale signals an inbound PUT whose client wall-clock
	// last_modified_at is strictly older than the stored copy. The handler
	// re-reads the server copy and returns it in a 409 body so iOS can
	// reconcile. Backup hygiene only — single-device backup rarely hits it.
	ErrPlaylistStale = errors.New("manifest: playlist is stale (server copy is newer)")
)

// PlaylistRow is the playlists-table row shape. No json tags (wire-type
// discipline): the API wraps it in a DTO. Timestamps are UnixNano integers
// (the wire form for last_modified_at is an integer too — no time.Time
// round-trip, no truncation risk on the LWW-critical field).
//
// DeviceToken records the device that LAST WROTE the playlist (provenance
// for the admin surface), not an access scope: every paired device belongs
// to the bridge operator, so playlists are user-wide — readable, writable
// and deletable from any device. A future multi-user mode would re-scope
// by a user id grouping several device tokens; the column stays for that.
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

// PlaylistSummary is the list row (no items).
type PlaylistSummary struct {
	ID             string
	Name           string
	TrackCount     int
	LastModifiedAt int64
}

// (nullable moved to sqlhelpers.go — shared by playlists.go + history.go.)

// UpsertPlaylist stores (or replaces) a playlist + its items, atomically.
// Playlists are user-wide: any paired device may overwrite any playlist
// (all devices belong to the bridge operator), so there is no ownership
// guard — the LWW check is the only gate, enforced inside the transaction
// so there is no TOCTOU gap against a concurrent writer:
//
//   - LWW: an existing row with a strictly-newer last_modified_at rejects
//     with ErrPlaylistStale (the handler re-reads + 409s the server copy).
//
// On success the row is (re)written with deleted=0, updated_at=now and
// device_token=the writing device (last-writer provenance), and its items
// are fully replaced. Holds s.mu; timestamps via s.now().
func (s *Store) UpsertPlaylist(ctx context.Context, deviceToken string, p PlaylistRow, items []PlaylistItemRow) error {
	if deviceToken == "" {
		return errors.New("manifest: UpsertPlaylist requires a device token")
	}
	if p.ID == "" {
		return errors.New("manifest: UpsertPlaylist requires a playlist id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit; unwind guard otherwise

	var existingLMA int64
	row := tx.QueryRowContext(ctx, `SELECT last_modified_at FROM playlists WHERE id = ?`, p.ID)
	switch err := row.Scan(&existingLMA); {
	case errors.Is(err, sql.ErrNoRows):
		// fresh insert — fall through
	case err != nil:
		return err
	default:
		if existingLMA > p.LastModifiedAt {
			return ErrPlaylistStale
		}
	}

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
	`, p.ID, deviceToken, p.Name, p.LastModifiedAt, now); err != nil {
		return err
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM playlist_items WHERE playlist_id = ?`, p.ID); err != nil {
		return err
	}
	// One prepared statement reused across the item loop (same shape as
	// InsertHistoryBatch) — per-item ExecContext re-prepares the SQL on
	// every row, which is measurable at the 50k-item cap.
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
	return tx.Commit()
}

// GetPlaylist returns a playlist + ordered items, or (nil, nil, nil) when
// the id is unknown or tombstoned. User-wide: any paired device can read
// any playlist (restore is initiable from any of the operator's devices).
// Read path — no s.mu.
func (s *Store) GetPlaylist(ctx context.Context, id string) (*PlaylistRow, []PlaylistItemRow, error) {
	var p PlaylistRow
	err := s.db.QueryRowContext(ctx, `
		SELECT id, device_token, name, last_modified_at, updated_at
		  FROM playlists
		 WHERE id = ? AND deleted = 0
	`, id).Scan(&p.ID, &p.DeviceToken, &p.Name, &p.LastModifiedAt, &p.UpdatedAt)
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

// ListPlaylists returns every live (non-tombstoned) playlist summary,
// newest-modified first, across ALL devices — playlists are user-wide.
// Read path — no s.mu.
func (s *Store) ListPlaylists(ctx context.Context) ([]PlaylistSummary, error) {
	// LEFT JOIN + GROUP BY rather than a correlated COUNT subquery per row:
	// the subquery form ran one playlist_items B-tree lookup per returned
	// playlist (N+1). COUNT(i.position) is 0 for an item-less playlist (the
	// LEFT JOIN yields a single all-NULL row; COUNT ignores NULLs), matching
	// the old COUNT(*) semantics.
	rows, err := s.db.QueryContext(ctx, `
		SELECT p.id, p.name, p.last_modified_at, COUNT(i.position) AS track_count
		  FROM playlists p
		  LEFT JOIN playlist_items i ON i.playlist_id = p.id
		 WHERE p.deleted = 0
		 GROUP BY p.id
		 ORDER BY p.last_modified_at DESC
	`)
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

// ListPlaylistTombstoneIDs returns the id of every tombstoned playlist —
// ids deleted via TombstonePlaylist and not since revived by an upsert.
// Served as `deletedIds` on GET /v1/playlists so a delete initiated on one
// device propagates to the user's other devices on their next sweep
// (absence from the live list alone is NOT a delete signal — clients must
// never infer deletion from a missing summary). Newest-deleted first.
// Read path — no s.mu.
func (s *Store) ListPlaylistTombstoneIDs(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id FROM playlists WHERE deleted = 1 ORDER BY updated_at DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// TombstonePlaylist marks a playlist deleted (so the delete propagates
// instead of the row reappearing on the next backup sweep). User-wide:
// any paired device can delete any playlist. Returns false when no live
// row matched (unknown / already-deleted). Holds s.mu; updated_at via
// s.now().
//
// deletedBy is the DELETING device's token — not device_token, which
// records the last WRITER and is a different device more often than not
// (a phone that never wrote a playlist can delete it, and did: 2026-09-20).
// It is what the console attributes a tombstone to and what
// CountRecentPlaylistTombstonesBy groups a mass delete on. Empty is
// accepted rather than rejected: the tombstone is the operator's data and
// must not fail to land because provenance was unavailable.
//
// updated_at IS the delete time for as long as the row stays tombstoned —
// this is the only writer of `deleted = 1` in the tree, and it stamps both
// in one statement. See migration v45 for why there is no deleted_at.
func (s *Store) TombstonePlaylist(ctx context.Context, id, deletedBy string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.ExecContext(ctx, `
		UPDATE playlists SET deleted = 1, deleted_by = ?, updated_at = ?
		 WHERE id = ? AND deleted = 0
	`, deletedBy, s.now().UnixNano(), id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// RestorePlaylist lifts a tombstone: the row goes live again with its
// stored items, and the id LEAVES the `deletedIds` feed on the next
// GET /v1/playlists so paired devices re-import it. Returns false when no
// TOMBSTONED row matched (unknown id, or already live — a restore of a
// live playlist is a no-op, never an error). Holds s.mu; updated_at via
// s.now().
//
// last_modified_at is deliberately NOT touched. It is the CLIENT's
// wall-clock and the LWW guard key, so moving it to now would make the
// bridge's copy outrank every device's — an operator undoing a delete has
// not authored a new version, and a device holding a genuinely newer copy
// must still win on its next flush exactly as it would have if the delete
// had never happened.
//
// deleted_by is cleared with the flag: it is provenance for THIS tombstone,
// and leaving it behind would attribute a future delete to whoever made the
// last one.
//
// The custom cover is NOT restored — DELETE /v1/playlists/{id} unlinks the
// JPEG (api.pruneCover), and nothing keeps a copy. Restored playlists fall
// back to the auto-mosaic; the console says so where the button is.
func (s *Store) RestorePlaylist(ctx context.Context, id string) (bool, error) {
	if id == "" {
		return false, errors.New("manifest: RestorePlaylist requires a playlist id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.ExecContext(ctx, `
		UPDATE playlists SET deleted = 0, deleted_by = '', updated_at = ?
		 WHERE id = ? AND deleted = 1
	`, s.now().UnixNano(), id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// ListAllPlaylistsForAdmin returns every live playlist summary for the
// loopback admin surface, paired with the device token that last wrote it.
// Read path — no s.mu. Admin-only; never exposed on /v1.
func (s *Store) ListAllPlaylistsForAdmin(ctx context.Context) ([]AdminPlaylistSummary, error) {
	// LEFT JOIN + GROUP BY (see ListPlaylists) to avoid the per-row
	// correlated-subquery N+1.
	rows, err := s.db.QueryContext(ctx, `
		SELECT p.id, p.device_token, p.name, p.last_modified_at, p.updated_at, COUNT(i.position) AS track_count
		  FROM playlists p
		  LEFT JOIN playlist_items i ON i.playlist_id = p.id
		 WHERE p.deleted = 0
		 GROUP BY p.id
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

// AdminPlaylistSummary is the admin-surface row (carries the last-writer
// device token, unlike the wire summary). No json tags — admin wraps it.
type AdminPlaylistSummary struct {
	ID             string
	DeviceToken    string
	Name           string
	LastModifiedAt int64
	UpdatedAt      int64
	TrackCount     int
}

// AdminDeletedPlaylist is one tombstoned playlist as the console's
// "Recently deleted" panel needs it. No json tags — admin wraps it.
//
// Two devices, not one: WroteLast is the device that last PUT the
// playlist, DeletedBy the one that asked for the tombstone. They are
// frequently different, and the second is the interesting one — a
// mass delete is attributed to whoever sent the DELETEs, not to whoever
// happened to author the playlists.
//
// DeletedAt is the row's updated_at, which on a tombstoned row IS the
// delete time (see migration v45). Names are best-effort: a device that
// has only ever used the header path registers with an empty name, and a
// DELETE from a caller that sent no X-Device-Token leaves DeletedBy empty
// entirely.
type AdminDeletedPlaylist struct {
	ID             string
	Name           string
	TrackCount     int
	LastModifiedAt int64
	DeletedAt      int64

	WroteLastToken string
	WroteLastName  string

	DeletedByToken   string
	DeletedByName    string
	DeletedByTokenID string
}

// ListDeletedPlaylistsForAdmin returns every tombstoned playlist,
// most-recently-deleted first, for the console's restore panel. Read path
// — no s.mu. Admin-only; never exposed on /v1, which serves tombstones as
// bare ids (`deletedIds`) and nothing else.
//
// The two LEFT JOINs against device_registrations resolve display names
// for both devices; both are on that table's PRIMARY KEY, so each group
// sees at most one row and the bare columns beside COUNT() are
// unambiguous. An unregistered or empty token simply yields ”.
func (s *Store) ListDeletedPlaylistsForAdmin(ctx context.Context) ([]AdminDeletedPlaylist, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT p.id, p.name, p.last_modified_at, p.updated_at,
		       p.device_token, COALESCE(w.device_name, ''),
		       p.deleted_by, COALESCE(d.device_name, ''), COALESCE(d.token_id, ''),
		       COUNT(i.position) AS track_count
		  FROM playlists p
		  LEFT JOIN playlist_items i ON i.playlist_id = p.id
		  LEFT JOIN device_registrations w ON w.device_token = p.device_token
		  LEFT JOIN device_registrations d ON d.device_token = p.deleted_by
		 WHERE p.deleted = 1
		 GROUP BY p.id
		 ORDER BY p.updated_at DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AdminDeletedPlaylist
	for rows.Next() {
		var a AdminDeletedPlaylist
		if err := rows.Scan(&a.ID, &a.Name, &a.LastModifiedAt, &a.DeletedAt,
			&a.WroteLastToken, &a.WroteLastName,
			&a.DeletedByToken, &a.DeletedByName, &a.DeletedByTokenID,
			&a.TrackCount); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// Mass-delete warning thresholds. ONE definition, because the number in
// the log line and the number the console reasons about have to be the
// same number — and ONE predicate too (LargestPlaylistDeleteRun below),
// because two readings of "N within W" drift the moment either moves.
//
// Five in a minute is well clear of a person deleting a couple of
// playlists by hand and well under a client sweep draining a queue: the
// 2026-09-20 incident was 17 in 26 seconds, which clears it four times
// over. The window is deliberately short — it is asking "is a client
// looping", not "has a lot been deleted today".
const (
	// PlaylistDeleteBurstThreshold is the tombstone count, within
	// PlaylistDeleteBurstWindow and from one device, that makes a delete
	// run worth a warning.
	PlaylistDeleteBurstThreshold = 5
	// PlaylistDeleteBurstWindow is how far back the count looks.
	PlaylistDeleteBurstWindow = 60 * time.Second
)

// PlaylistDeleteBurst is what CountRecentPlaylistTombstonesBy found: how
// many playlists this device has tombstoned inside the window, plus enough
// identity to name it in a log line. Zero Count means the device has no
// live tombstones in the window at all.
type PlaylistDeleteBurst struct {
	Count      int
	DeviceName string // best-effort; '' for a device that never sent a name
	TokenID    string // the auth token currently bound to that device
}

// CountRecentPlaylistTombstonesBy counts the playlists one device has
// tombstoned since `since`, for the mass-delete warning on the DELETE
// path. Read path — no s.mu.
//
// Counted from the TABLE rather than from an in-process ring: the rows are
// the same ones the console's restore panel lists, so the warning and the
// panel cannot tell different stories, and a burst spanning a restart is
// still one burst. The playlists table holds one row per playlist — tens
// to hundreds on a real bridge — and a DELETE is a rare, human-paced
// request, so the scan is not worth an index.
//
// An empty deviceToken counts nothing: unattributed deletes must not pool
// into one phantom device. The caller gets Count 0 and logs nothing.
func (s *Store) CountRecentPlaylistTombstonesBy(ctx context.Context, deviceToken string, since time.Time) (PlaylistDeleteBurst, error) {
	var b PlaylistDeleteBurst
	if deviceToken == "" {
		return b, nil
	}
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*),
		       COALESCE(MAX(d.device_name), ''), COALESCE(MAX(d.token_id), '')
		  FROM playlists p
		  LEFT JOIN device_registrations d ON d.device_token = p.deleted_by
		 WHERE p.deleted = 1 AND p.deleted_by = ? AND p.updated_at >= ?
	`, deviceToken, since.UnixNano()).Scan(&b.Count, &b.DeviceName, &b.TokenID)
	if err != nil {
		return PlaylistDeleteBurst{}, err
	}
	return b, nil
}

// PlaylistDeleteRun is the largest mass delete visible in a set of
// tombstones: how many one device deleted inside one
// PlaylistDeleteBurstWindow, and how long that took.
//
// SpanNS is the distance between the first and last delete in the winning
// window, so it is 0 for a run that landed inside a single clock tick and
// is never larger than the window.
type PlaylistDeleteRun struct {
	Count         int
	SpanNS        int64
	DeviceToken   string
	DeviceName    string
	LastDeletedAt int64
}

// LargestPlaylistDeleteRun finds the biggest run of tombstones one device
// wrote inside PlaylistDeleteBurstWindow, or reports that there is none
// worth calling a mass delete.
//
// This is the console's half of the predicate CountRecentPlaylistTombstonesBy
// evaluates live on the DELETE path, and it exists so the panel's sentence
// and the journal's "playlist mass delete" line cannot describe different
// events. The console used to reconstruct the run in JavaScript from
// `deletedByPrefix` and pairwise gaps, which could not agree with the
// server even in principle: the prefix is eight characters of a redacted
// token, and a chain of gaps is not a fixed window — a second device
// deleting one playlist in the middle of a run ended it, so the panel
// would say "3 deleted" while the journal said 6 (CodeRabbit on #942).
// Grouping happens HERE, on the whole `deleted_by` token, and the console
// renders what it is told.
//
// A true sliding window rather than an anchor at the device's newest
// tombstone: a device that deleted twenty playlists this morning and one
// more just now must still report twenty, which is what the WARN said at
// the time.
//
// Rows with no recorded deleter are skipped — a tombstone from before
// migration v45, or a DELETE that carried no device token. Attributing a
// mass delete to "unknown" is worse than not claiming one.
//
// The comparison is a strict TOTAL order (count, then recency, then
// token), for the dupe-elector's reason: the grouping walks a Go map, so
// any pair the comparator leaves equal would flip the reported device
// between two renders of the same data.
func LargestPlaylistDeleteRun(rows []AdminDeletedPlaylist) (PlaylistDeleteRun, bool) {
	byDevice := make(map[string][]AdminDeletedPlaylist)
	for _, r := range rows {
		if r.DeletedByToken == "" {
			continue
		}
		byDevice[r.DeletedByToken] = append(byDevice[r.DeletedByToken], r)
	}
	window := int64(PlaylistDeleteBurstWindow)
	var best PlaylistDeleteRun
	for token, group := range byDevice {
		// Ascending, so the two pointers walk forward together. The
		// caller's ORDER BY is not relied on: a function that is correct
		// only for one query's ordering is a trap for the next caller.
		sort.Slice(group, func(i, j int) bool { return group[i].DeletedAt < group[j].DeletedAt })
		lo := 0
		for hi := range group {
			for group[hi].DeletedAt-group[lo].DeletedAt > window {
				lo++
			}
			run := PlaylistDeleteRun{
				Count:         hi - lo + 1,
				SpanNS:        group[hi].DeletedAt - group[lo].DeletedAt,
				DeviceToken:   token,
				DeviceName:    group[hi].DeletedByName,
				LastDeletedAt: group[hi].DeletedAt,
			}
			if runOutranks(run, best) {
				best = run
			}
		}
	}
	if best.Count < PlaylistDeleteBurstThreshold {
		return PlaylistDeleteRun{}, false
	}
	return best, true
}

// runOutranks is LargestPlaylistDeleteRun's strict total order: more
// deletes wins, then the more recent run, then the token as the tie-break
// that makes the answer independent of map iteration order.
func runOutranks(a, b PlaylistDeleteRun) bool {
	if a.Count != b.Count {
		return a.Count > b.Count
	}
	if a.LastDeletedAt != b.LastDeletedAt {
		return a.LastDeletedAt > b.LastDeletedAt
	}
	return a.DeviceToken > b.DeviceToken
}

// PlaylistHeadPaths returns the first `perPlaylist` LOCAL item paths of
// every live playlist, keyed by playlist id.
//
// One query for the whole set, not one per playlist: the player's
// playlist grid builds a mosaic from each playlist's leading covers, and
// a GetPlaylist per tile would be N round trips to answer a question
// about the first handful of rows.
//
// FOREIGN items (another bridge's tracks, carried as
// origin_fingerprint + origin_path) are excluded: nothing here can
// resolve them to a local album, so they cannot contribute artwork. They
// still COUNT — track_count comes from ListAllPlaylistsForAdmin and
// includes them — so a playlist of entirely foreign refs shows its real
// size with no mosaic, which is the honest rendering.
func (s *Store) PlaylistHeadPaths(ctx context.Context, perPlaylist int) (map[string][]string, error) {
	if perPlaylist <= 0 {
		return map[string][]string{}, nil
	}
	const q = `SELECT i.playlist_id, i.path
	             FROM playlist_items i
	             JOIN playlists p ON p.id = i.playlist_id
	            WHERE p.deleted = 0 AND i.path IS NOT NULL AND i.path != ''
	              AND i.position < ?
	         ORDER BY i.playlist_id, i.position`
	rows, err := s.db.QueryContext(ctx, q, perPlaylist)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string][]string{}
	for rows.Next() {
		var id, p string
		if err := rows.Scan(&id, &p); err != nil {
			return nil, err
		}
		out[id] = append(out[id], p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
