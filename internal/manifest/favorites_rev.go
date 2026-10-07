package manifest

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// favoriteTombstoneRetention is how long a removed favorite stays, so a
// 2.1 client that still hearts it can be told it was removed. Exactly this
// age is eligible. One legacy writer seen after the cutoff blocks every
// collection: that client has not been told.
const favoriteTombstoneRetention = 90 * 24 * time.Hour

// ErrFavoritesBaseRevision is a negative baseRevision. Zero is the
// never-stored revision and is valid.
var ErrFavoritesBaseRevision = errors.New("manifest: favorites baseRevision must not be negative")

// ErrFavoriteBoth is a key listed live and removed in one revisioned save.
// A legacy save does not send removals, so it cannot hit this.
var ErrFavoriteBoth = errors.New("manifest: a favorite cannot be both listed and removed")

// FavoriteTombstone is one removed track. Revision is the revision that
// introduced the tombstone; it is stored and not on the wire.
type FavoriteTombstone struct {
	Path              string
	OriginFingerprint string
	OriginPath        string
	RemovedAt         int64
	Revision          int64
}

// FavoriteAlbumTombstone is one removed album. Revision is the introducing
// revision, stored and not on the wire.
type FavoriteAlbumTombstone struct {
	AlbumArtist string
	Album       string
	Year        int
	RemovedAt   int64
	Revision    int64
}

// FavoritesDocument is the stored favorites, including removals.
// Stored is false when favorites_meta has no row (revision 0).
type FavoritesDocument struct {
	Stored          bool
	Epoch           string
	Revision        int64
	LastModifiedAt  int64
	UpdatedAt       int64
	DeviceToken     string
	Tracks          []FavoriteTrackRow
	Albums          []FavoriteAlbumRow
	Tombstones      []FavoriteTombstone
	AlbumTombstones []FavoriteAlbumTombstone
}

// FavoritesSave is one PUT. Legacy, or a nil BaseRevision, is additions
// only and never conflicts. A revisioned save replaces the live set and
// the tombstone set.
type FavoritesSave struct {
	Legacy          bool
	BaseRevision    *int64
	Tracks          []FavoriteTrackRow
	Albums          []FavoriteAlbumRow
	Tombstones      []FavoriteTombstone
	AlbumTombstones []FavoriteAlbumTombstone
}

// FavoritesSaveResult is the epoch and revision after an accepted save.
type FavoritesSaveResult struct {
	Epoch    string
	Revision int64
}

// FavoriteSyncDevice is one device's sync clock, without the token.
type FavoriteSyncDevice struct {
	LastSeenAt  int64
	LegacyPutAt int64
}

type favTrackKey struct {
	path, fp, origin string
}

type favAlbumKey struct {
	artist, album string
	year          int
}

func trackKeyOf(path, fp, origin string) favTrackKey {
	return favTrackKey{path: path, fp: fp, origin: origin}
}

func albumKeyOf(artist, album string, year int) favAlbumKey {
	return favAlbumKey{artist: artist, album: album, year: year}
}

// maxFavoriteTracks and maxFavoriteAlbums match the API body caps. A
// legacy merge adds a whole body onto what is already stored, so the
// merged result is capped here, leaving room for the tombstones already
// stored: a revisioned echo counts live rows and tombstones together.
// Existing keys are kept ahead of new ones.
const (
	maxFavoriteTracks = 50000
	maxFavoriteAlbums = 10000
)

func trackKeyOfRow(t FavoriteTrackRow) favTrackKey {
	return trackKeyOf(t.Path, t.OriginFingerprint, t.OriginPath)
}

func albumKeyOfRow(a FavoriteAlbumRow) favAlbumKey {
	return albumKeyOf(a.AlbumArtist, a.Album, a.Year)
}

// capFavoriteRows keeps existing keys first, then incoming keys, until
// cap. The kept row is the merged one, so an earlier stamp still wins.
// A result already within the cap is returned as given.
func capFavoriteRows[T any, K comparable](existing, incoming, merged []T, cap int, key func(T) K) []T {
	if len(merged) <= cap {
		return merged
	}
	have := make(map[K]T, len(merged))
	for _, row := range merged {
		have[key(row)] = row
	}
	out := make([]T, 0, cap)
	seen := make(map[K]bool, cap)
	take := func(rows []T) {
		for _, row := range rows {
			if len(out) >= cap {
				return
			}
			k := key(row)
			got, ok := have[k]
			if !ok || seen[k] {
				continue
			}
			seen[k] = true
			out = append(out, got)
		}
	}
	take(existing)
	take(incoming)
	return out
}

// ReadFavorites returns the document from one read-only transaction. It
// does not take s.mu and it does not collect tombstones: a GET must not
// queue behind a writer, and collection runs in SaveFavorites.
func (s *Store) ReadFavorites(ctx context.Context) (FavoritesDocument, error) {
	doc, _, err := s.ReadFavoritesIfUnchanged(ctx, nil)
	return doc, err
}

// ReadFavoritesIfUnchanged reads the epoch and the revision first. When
// unchanged reports that those already match what the caller holds, it
// returns that header and true without reading favorite rows. Otherwise
// it returns the full document from the same transaction. It does not
// take s.mu and it does not collect tombstones.
func (s *Store) ReadFavoritesIfUnchanged(ctx context.Context, unchanged func(epoch string, revision int64, stored bool) bool) (FavoritesDocument, bool, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return FavoritesDocument{}, false, err
	}
	defer tx.Rollback() //nolint:errcheck // read-only
	if unchanged != nil {
		epoch, rev, stored, err := readFavoritesHeader(ctx, tx)
		if err != nil {
			return FavoritesDocument{}, false, err
		}
		if unchanged(epoch, rev, stored) {
			return FavoritesDocument{Epoch: epoch, Revision: rev, Stored: stored}, true, nil
		}
	}
	doc, err := readFavoritesDocument(ctx, tx)
	return doc, false, err
}

// SaveFavorites applies one PUT under s.mu. A revisioned save whose base
// is not the stored revision and whose body differs returns
// ErrFavoritesStale and writes nothing. An equal body, a matching base,
// and every legacy save are accepted. The revision moves only when the
// document changes.
func (s *Store) SaveFavorites(ctx context.Context, deviceToken string, save FavoritesSave) (FavoritesSaveResult, error) {
	if deviceToken == "" {
		return FavoritesSaveResult{}, errors.New("manifest: SaveFavorites requires a device token")
	}
	if save.BaseRevision != nil && *save.BaseRevision < 0 {
		return FavoritesSaveResult{}, ErrFavoritesBaseRevision
	}
	legacy := save.Legacy || save.BaseRevision == nil
	if !legacy {
		if err := rejectFavoriteBoth(save); err != nil {
			return FavoritesSaveResult{}, err
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.collectFavoriteTombstonesLocked(ctx); err != nil {
		return FavoritesSaveResult{}, err
	}
	doc, err := readFavoritesDocument(ctx, s.db)
	if err != nil {
		return FavoritesSaveResult{}, err
	}

	nowNS := s.now().UnixNano()
	nextRev := doc.Revision + 1
	var next FavoritesDocument
	if legacy {
		next = mergeLegacyFavorites(doc, save)
	} else {
		next = mergeRevisionedFavorites(doc, save, nextRev, nowNS)
	}
	equal := favoritesEqual(doc, next)
	if !legacy && save.BaseRevision != nil && *save.BaseRevision != doc.Revision && !equal {
		return FavoritesSaveResult{}, ErrFavoritesStale
	}

	changed := !doc.Stored || !equal
	if !changed {
		if err := s.noteFavoriteDevice(ctx, deviceToken, nowNS, legacy); err != nil {
			return FavoritesSaveResult{}, err
		}
		return FavoritesSaveResult{Epoch: doc.Epoch, Revision: doc.Revision}, nil
	}
	if err := s.writeFavoritesDocument(ctx, deviceToken, nowNS, nextRev, legacy, next); err != nil {
		return FavoritesSaveResult{}, err
	}
	return FavoritesSaveResult{Epoch: doc.Epoch, Revision: nextRev}, nil
}

// ListFavoriteTombstones returns stored removals, including ones past the
// retention until the next SaveFavorites collects them.
func (s *Store) ListFavoriteTombstones(ctx context.Context) ([]FavoriteTombstone, []FavoriteAlbumTombstone, error) {
	tracks, err := readTrackTombstones(ctx, s.db)
	if err != nil {
		return nil, nil, err
	}
	albums, err := readAlbumTombstones(ctx, s.db)
	if err != nil {
		return nil, nil, err
	}
	return tracks, albums, nil
}

// ListFavoriteSyncDevices returns each device's clocks, token omitted,
// oldest last-seen first.
func (s *Store) ListFavoriteSyncDevices(ctx context.Context) ([]FavoriteSyncDevice, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT last_seen_at, legacy_put_at
		  FROM favorite_sync_devices
		 ORDER BY last_seen_at, device_token
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FavoriteSyncDevice
	for rows.Next() {
		var d FavoriteSyncDevice
		var legacy sql.NullInt64
		if err := rows.Scan(&d.LastSeenAt, &legacy); err != nil {
			return nil, err
		}
		if legacy.Valid {
			d.LegacyPutAt = legacy.Int64
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// sqlQueryer is the read surface *sql.DB and *sql.Tx share, so one
// favorites read can stay inside a single transaction.
type sqlQueryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func readEpoch(ctx context.Context, q sqlQueryer) (string, error) {
	var epoch string
	err := q.QueryRowContext(ctx, `SELECT epoch FROM backup_epoch WHERE id = 1`).Scan(&epoch)
	if errors.Is(err, sql.ErrNoRows) {
		return "", errors.New("manifest: backup epoch is missing")
	}
	return epoch, err
}

func readFavoritesHeader(ctx context.Context, q sqlQueryer) (string, int64, bool, error) {
	epoch, err := readEpoch(ctx, q)
	if err != nil {
		return "", 0, false, err
	}
	var rev int64
	err = q.QueryRowContext(ctx, `SELECT revision FROM favorites_meta WHERE id = 1`).Scan(&rev)
	if errors.Is(err, sql.ErrNoRows) {
		return epoch, 0, false, nil
	}
	if err != nil {
		return "", 0, false, err
	}
	return epoch, rev, true, nil
}

func readFavoritesDocument(ctx context.Context, q sqlQueryer) (FavoritesDocument, error) {
	epoch, err := readEpoch(ctx, q)
	if err != nil {
		return FavoritesDocument{}, err
	}
	doc := FavoritesDocument{Epoch: epoch}
	var rev int64
	err = q.QueryRowContext(ctx, `
		SELECT revision, last_modified_at, updated_at, device_token
		  FROM favorites_meta WHERE id = 1
	`).Scan(&rev, &doc.LastModifiedAt, &doc.UpdatedAt, &doc.DeviceToken)
	if errors.Is(err, sql.ErrNoRows) {
		doc.Tracks = []FavoriteTrackRow{}
		doc.Albums = []FavoriteAlbumRow{}
		doc.Tombstones = []FavoriteTombstone{}
		doc.AlbumTombstones = []FavoriteAlbumTombstone{}
		return doc, nil
	}
	if err != nil {
		return FavoritesDocument{}, err
	}
	doc.Stored = true
	doc.Revision = rev

	trows, err := q.QueryContext(ctx, `
		SELECT COALESCE(path, ''), COALESCE(origin_fingerprint, ''),
		       COALESCE(origin_path, ''), COALESCE(title, ''), COALESCE(artist, ''),
		       favorited_at
		  FROM favorite_tracks
		 ORDER BY favorited_at DESC
	`)
	if err != nil {
		return FavoritesDocument{}, err
	}
	defer trows.Close()
	for trows.Next() {
		var t FavoriteTrackRow
		if err := trows.Scan(&t.Path, &t.OriginFingerprint, &t.OriginPath,
			&t.Title, &t.Artist, &t.FavoritedAt); err != nil {
			return FavoritesDocument{}, err
		}
		doc.Tracks = append(doc.Tracks, t)
	}
	if err := trows.Err(); err != nil {
		return FavoritesDocument{}, err
	}

	arows, err := q.QueryContext(ctx, `
		SELECT album_artist, album, year, favorited_at
		  FROM favorite_albums
		 ORDER BY favorited_at DESC
	`)
	if err != nil {
		return FavoritesDocument{}, err
	}
	defer arows.Close()
	for arows.Next() {
		var a FavoriteAlbumRow
		if err := arows.Scan(&a.AlbumArtist, &a.Album, &a.Year, &a.FavoritedAt); err != nil {
			return FavoritesDocument{}, err
		}
		doc.Albums = append(doc.Albums, a)
	}
	if err := arows.Err(); err != nil {
		return FavoritesDocument{}, err
	}

	doc.Tombstones, err = readTrackTombstones(ctx, q)
	if err != nil {
		return FavoritesDocument{}, err
	}
	doc.AlbumTombstones, err = readAlbumTombstones(ctx, q)
	if err != nil {
		return FavoritesDocument{}, err
	}
	if doc.Tracks == nil {
		doc.Tracks = []FavoriteTrackRow{}
	}
	if doc.Albums == nil {
		doc.Albums = []FavoriteAlbumRow{}
	}
	return doc, nil
}

func readTrackTombstones(ctx context.Context, db sqlQueryer) ([]FavoriteTombstone, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT COALESCE(path, ''), COALESCE(origin_fingerprint, ''),
		       COALESCE(origin_path, ''), removed_at, revision
		  FROM favorite_tombstones
		 ORDER BY removed_at DESC, path
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FavoriteTombstone
	for rows.Next() {
		var t FavoriteTombstone
		if err := rows.Scan(&t.Path, &t.OriginFingerprint, &t.OriginPath,
			&t.RemovedAt, &t.Revision); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	if out == nil {
		out = []FavoriteTombstone{}
	}
	return out, rows.Err()
}

func readAlbumTombstones(ctx context.Context, db sqlQueryer) ([]FavoriteAlbumTombstone, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT album_artist, album, year, removed_at, revision
		  FROM favorite_album_tombstones
		 ORDER BY removed_at DESC, album_artist, album, year
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FavoriteAlbumTombstone
	for rows.Next() {
		var a FavoriteAlbumTombstone
		if err := rows.Scan(&a.AlbumArtist, &a.Album, &a.Year, &a.RemovedAt, &a.Revision); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	if out == nil {
		out = []FavoriteAlbumTombstone{}
	}
	return out, rows.Err()
}

// collectFavoriteTombstonesLocked deletes tombstones at least
// favoriteTombstoneRetention old, unless any device has a legacy PUT
// after the cutoff. The caller holds s.mu. The delete commits on its own,
// before a compare-and-swap that may still fail.
func (s *Store) collectFavoriteTombstonesLocked(ctx context.Context) error {
	cutoff := s.now().Add(-favoriteTombstoneRetention).UnixNano()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	var removed int64
	for _, q := range []string{
		`DELETE FROM favorite_tombstones
		  WHERE removed_at <= ?
		    AND NOT EXISTS (
		      SELECT 1 FROM favorite_sync_devices
		       WHERE legacy_put_at IS NOT NULL AND legacy_put_at > ?
		    )`,
		`DELETE FROM favorite_album_tombstones
		  WHERE removed_at <= ?
		    AND NOT EXISTS (
		      SELECT 1 FROM favorite_sync_devices
		       WHERE legacy_put_at IS NOT NULL AND legacy_put_at > ?
		    )`,
	} {
		res, err := tx.ExecContext(ctx, q, cutoff, cutoff)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		removed += n
	}
	if removed > 0 {
		nowNS := s.now().UnixNano()
		if _, err := tx.ExecContext(ctx, `
			UPDATE favorites_meta
			   SET revision = revision + 1,
			       last_modified_at = ?,
			       updated_at = ?
			 WHERE id = 1
		`, nowNS, nowNS); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if removed > 0 {
		var rev int64
		// The request may already be cancelled: the commit has landed, and
		// this read only publishes it. A cancelled context here drops the
		// event while the document has moved.
		if err := s.db.QueryRowContext(context.WithoutCancel(ctx), `SELECT revision FROM favorites_meta WHERE id = 1`).Scan(&rev); err == nil {
			s.noteFavorites(rev)
		}
	}
	return nil
}

func (s *Store) noteFavoriteDevice(ctx context.Context, deviceToken string, nowNS int64, legacy bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	if err := noteFavoriteDeviceTx(ctx, tx, deviceToken, nowNS, legacy); err != nil {
		return err
	}
	return tx.Commit()
}

func noteFavoriteDeviceTx(ctx context.Context, tx *sql.Tx, deviceToken string, nowNS int64, legacy bool) error {
	var legacyAt *int64
	if legacy {
		legacyAt = &nowNS
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO favorite_sync_devices (device_token, last_seen_at, legacy_put_at)
		VALUES (?, ?, ?)
		ON CONFLICT(device_token) DO UPDATE SET
			last_seen_at = excluded.last_seen_at,
			legacy_put_at = COALESCE(excluded.legacy_put_at, favorite_sync_devices.legacy_put_at)
	`, deviceToken, nowNS, legacyAt)
	return err
}

func (s *Store) writeFavoritesDocument(ctx context.Context, deviceToken string, nowNS, revision int64, legacy bool, doc FavoritesDocument) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	for _, q := range []string{
		`DELETE FROM favorite_tracks`,
		`DELETE FROM favorite_albums`,
		`DELETE FROM favorite_tombstones`,
		`DELETE FROM favorite_album_tombstones`,
	} {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO favorites_meta (id, last_modified_at, device_token, updated_at, revision)
		VALUES (1, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			last_modified_at = excluded.last_modified_at,
			device_token     = excluded.device_token,
			updated_at       = excluded.updated_at,
			revision         = excluded.revision
	`, nowNS, deviceToken, nowNS, revision); err != nil {
		return err
	}
	if err := insertFavoriteTracks(ctx, tx, doc.Tracks); err != nil {
		return err
	}
	if err := insertFavoriteAlbums(ctx, tx, doc.Albums); err != nil {
		return err
	}
	if err := insertFavoriteTombstones(ctx, tx, doc.Tombstones); err != nil {
		return err
	}
	if err := insertFavoriteAlbumTombstones(ctx, tx, doc.AlbumTombstones); err != nil {
		return err
	}
	if err := noteFavoriteDeviceTx(ctx, tx, deviceToken, nowNS, legacy); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.noteFavorites(revision)
	return nil
}

func insertFavoriteTracks(ctx context.Context, tx *sql.Tx, tracks []FavoriteTrackRow) error {
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO favorite_tracks
			(path, origin_fingerprint, origin_path, title, artist, favorited_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, t := range tracks {
		if !favoriteTrackIdentityOK(t.Path, t.OriginFingerprint, t.OriginPath) {
			return errors.New("manifest: favorite track must be strictly local (path) XOR foreign (originFingerprint + originPath)")
		}
		if _, err := stmt.ExecContext(ctx, nullable(t.Path), nullable(t.OriginFingerprint),
			nullable(t.OriginPath), nullable(t.Title), nullable(t.Artist), t.FavoritedAt); err != nil {
			return err
		}
	}
	return nil
}

func insertFavoriteAlbums(ctx context.Context, tx *sql.Tx, albums []FavoriteAlbumRow) error {
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO favorite_albums (album_artist, album, year, favorited_at)
		VALUES (?, ?, ?, ?)
	`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, a := range albums {
		if _, err := stmt.ExecContext(ctx, a.AlbumArtist, a.Album, a.Year, a.FavoritedAt); err != nil {
			return err
		}
	}
	return nil
}

func insertFavoriteTombstones(ctx context.Context, tx *sql.Tx, tombs []FavoriteTombstone) error {
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO favorite_tombstones
			(path, origin_fingerprint, origin_path, removed_at, revision)
		VALUES (?, ?, ?, ?, ?)
	`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, t := range tombs {
		if !favoriteTrackIdentityOK(t.Path, t.OriginFingerprint, t.OriginPath) {
			return errors.New("manifest: favorite track must be strictly local (path) XOR foreign (originFingerprint + originPath)")
		}
		if _, err := stmt.ExecContext(ctx, nullable(t.Path), nullable(t.OriginFingerprint),
			nullable(t.OriginPath), t.RemovedAt, t.Revision); err != nil {
			return err
		}
	}
	return nil
}

func insertFavoriteAlbumTombstones(ctx context.Context, tx *sql.Tx, tombs []FavoriteAlbumTombstone) error {
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO favorite_album_tombstones
			(album_artist, album, year, removed_at, revision)
		VALUES (?, ?, ?, ?, ?)
	`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, a := range tombs {
		if _, err := stmt.ExecContext(ctx, a.AlbumArtist, a.Album, a.Year, a.RemovedAt, a.Revision); err != nil {
			return err
		}
	}
	return nil
}

func favoriteTrackIdentityOK(path, fp, origin string) bool {
	local := path != "" && fp == "" && origin == ""
	foreign := path == "" && fp != "" && origin != ""
	return local || foreign
}

func rejectFavoriteBoth(save FavoritesSave) error {
	live := map[favTrackKey]struct{}{}
	for _, t := range save.Tracks {
		live[trackKeyOf(t.Path, t.OriginFingerprint, t.OriginPath)] = struct{}{}
	}
	for _, t := range save.Tombstones {
		if _, ok := live[trackKeyOf(t.Path, t.OriginFingerprint, t.OriginPath)]; ok {
			return ErrFavoriteBoth
		}
	}
	albums := map[favAlbumKey]struct{}{}
	for _, a := range save.Albums {
		albums[albumKeyOf(a.AlbumArtist, a.Album, a.Year)] = struct{}{}
	}
	for _, a := range save.AlbumTombstones {
		if _, ok := albums[albumKeyOf(a.AlbumArtist, a.Album, a.Year)]; ok {
			return ErrFavoriteBoth
		}
	}
	return nil
}

func mergeLegacyFavorites(doc FavoritesDocument, save FavoritesSave) FavoritesDocument {
	next := doc
	tracks := indexTracks(doc.Tracks)
	tombs := indexTombs(doc.Tombstones)
	for _, t := range save.Tracks {
		k := trackKeyOf(t.Path, t.OriginFingerprint, t.OriginPath)
		if _, dead := tombs[k]; dead {
			continue
		}
		if cur, ok := tracks[k]; ok {
			if t.FavoritedAt < cur.FavoritedAt {
				tracks[k] = t
			}
			continue
		}
		tracks[k] = t
	}

	albums := indexAlbums(doc.Albums)
	atombs := indexAlbumTombs(doc.AlbumTombstones)
	for _, a := range save.Albums {
		k := albumKeyOf(a.AlbumArtist, a.Album, a.Year)
		if _, dead := atombs[k]; dead {
			continue
		}
		if cur, ok := albums[k]; ok {
			if a.FavoritedAt < cur.FavoritedAt {
				albums[k] = a
			}
			continue
		}
		albums[k] = a
	}
	next.Tracks = capFavoriteRows(doc.Tracks, save.Tracks, tracksFromIndex(tracks), maxFavoriteTracks-len(doc.Tombstones), trackKeyOfRow)
	next.Albums = capFavoriteRows(doc.Albums, save.Albums, albumsFromIndex(albums), maxFavoriteAlbums-len(doc.AlbumTombstones), albumKeyOfRow)
	return next
}

func mergeRevisionedFavorites(doc FavoritesDocument, save FavoritesSave, nextRev, nowNS int64) FavoritesDocument {
	next := FavoritesDocument{
		Tracks:          save.Tracks,
		Albums:          save.Albums,
		Tombstones:      tombstonesForSave(doc.Tracks, doc.Tombstones, save.Tombstones, nextRev, nowNS),
		AlbumTombstones: albumTombstonesForSave(doc.Albums, doc.AlbumTombstones, save.AlbumTombstones, nextRev, nowNS),
	}
	return next
}

func tombstonesForSave(live []FavoriteTrackRow, stored, incoming []FavoriteTombstone, nextRev, nowNS int64) []FavoriteTombstone {
	wasLive := indexTracks(live)
	kept := indexTombs(stored)
	var out []FavoriteTombstone
	seen := map[favTrackKey]int{}
	for _, t := range incoming {
		k := trackKeyOf(t.Path, t.OriginFingerprint, t.OriginPath)
		var row FavoriteTombstone
		if prev, ok := kept[k]; ok {
			row = prev
		} else if _, ok := wasLive[k]; ok {
			row = FavoriteTombstone{
				Path: t.Path, OriginFingerprint: t.OriginFingerprint, OriginPath: t.OriginPath,
				RemovedAt: nowNS, Revision: nextRev,
			}
		} else {
			continue
		}
		if i, dup := seen[k]; dup {
			out[i] = row
			continue
		}
		seen[k] = len(out)
		out = append(out, row)
	}
	if out == nil {
		out = []FavoriteTombstone{}
	}
	return out
}

func albumTombstonesForSave(live []FavoriteAlbumRow, stored, incoming []FavoriteAlbumTombstone, nextRev, nowNS int64) []FavoriteAlbumTombstone {
	wasLive := indexAlbums(live)
	kept := indexAlbumTombs(stored)
	var out []FavoriteAlbumTombstone
	seen := map[favAlbumKey]int{}
	for _, a := range incoming {
		k := albumKeyOf(a.AlbumArtist, a.Album, a.Year)
		var row FavoriteAlbumTombstone
		if prev, ok := kept[k]; ok {
			row = prev
		} else if _, ok := wasLive[k]; ok {
			row = FavoriteAlbumTombstone{
				AlbumArtist: a.AlbumArtist, Album: a.Album, Year: a.Year,
				RemovedAt: nowNS, Revision: nextRev,
			}
		} else {
			continue
		}
		if i, dup := seen[k]; dup {
			out[i] = row
			continue
		}
		seen[k] = len(out)
		out = append(out, row)
	}
	if out == nil {
		out = []FavoriteAlbumTombstone{}
	}
	return out
}

func favoritesEqual(a, b FavoritesDocument) bool {
	return sameTracks(a.Tracks, b.Tracks) &&
		sameAlbums(a.Albums, b.Albums) &&
		sameTombs(a.Tombstones, b.Tombstones) &&
		sameAlbumTombs(a.AlbumTombstones, b.AlbumTombstones)
}

func sameTracks(a, b []FavoriteTrackRow) bool {
	if len(a) != len(b) {
		return false
	}
	idx := indexTracks(a)
	for _, t := range b {
		got, ok := idx[trackKeyOf(t.Path, t.OriginFingerprint, t.OriginPath)]
		if !ok || got.Title != t.Title || got.Artist != t.Artist || got.FavoritedAt != t.FavoritedAt {
			return false
		}
	}
	return true
}

func sameAlbums(a, b []FavoriteAlbumRow) bool {
	if len(a) != len(b) {
		return false
	}
	idx := indexAlbums(a)
	for _, row := range b {
		got, ok := idx[albumKeyOf(row.AlbumArtist, row.Album, row.Year)]
		if !ok || got.FavoritedAt != row.FavoritedAt {
			return false
		}
	}
	return true
}

func sameTombs(a, b []FavoriteTombstone) bool {
	if len(a) != len(b) {
		return false
	}
	idx := indexTombs(a)
	for _, t := range b {
		got, ok := idx[trackKeyOf(t.Path, t.OriginFingerprint, t.OriginPath)]
		if !ok || got.RemovedAt != t.RemovedAt {
			return false
		}
	}
	return true
}

func sameAlbumTombs(a, b []FavoriteAlbumTombstone) bool {
	if len(a) != len(b) {
		return false
	}
	idx := indexAlbumTombs(a)
	for _, row := range b {
		got, ok := idx[albumKeyOf(row.AlbumArtist, row.Album, row.Year)]
		if !ok || got.RemovedAt != row.RemovedAt {
			return false
		}
	}
	return true
}

func indexTracks(in []FavoriteTrackRow) map[favTrackKey]FavoriteTrackRow {
	out := make(map[favTrackKey]FavoriteTrackRow, len(in))
	for _, t := range in {
		out[trackKeyOf(t.Path, t.OriginFingerprint, t.OriginPath)] = t
	}
	return out
}

func indexAlbums(in []FavoriteAlbumRow) map[favAlbumKey]FavoriteAlbumRow {
	out := make(map[favAlbumKey]FavoriteAlbumRow, len(in))
	for _, a := range in {
		out[albumKeyOf(a.AlbumArtist, a.Album, a.Year)] = a
	}
	return out
}

func indexTombs(in []FavoriteTombstone) map[favTrackKey]FavoriteTombstone {
	out := make(map[favTrackKey]FavoriteTombstone, len(in))
	for _, t := range in {
		out[trackKeyOf(t.Path, t.OriginFingerprint, t.OriginPath)] = t
	}
	return out
}

func indexAlbumTombs(in []FavoriteAlbumTombstone) map[favAlbumKey]FavoriteAlbumTombstone {
	out := make(map[favAlbumKey]FavoriteAlbumTombstone, len(in))
	for _, a := range in {
		out[albumKeyOf(a.AlbumArtist, a.Album, a.Year)] = a
	}
	return out
}

func tracksFromIndex(in map[favTrackKey]FavoriteTrackRow) []FavoriteTrackRow {
	out := make([]FavoriteTrackRow, 0, len(in))
	for _, t := range in {
		out = append(out, t)
	}
	return out
}

func albumsFromIndex(in map[favAlbumKey]FavoriteAlbumRow) []FavoriteAlbumRow {
	out := make([]FavoriteAlbumRow, 0, len(in))
	for _, a := range in {
		out = append(out, a)
	}
	return out
}
