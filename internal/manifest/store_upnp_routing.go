package manifest

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// UPnPRouting is the volatile resolution data the file-serving proxy
// uses to fetch bytes from the upstream UPnP MediaServer. The owning
// `tracks` row's `path` IS the identity (FK PK), so this sidecar carries
// only the resolution hints — never identity.
//
// Fields mirror the wire shape the iOS-side ContentDirectoryClient would
// use to recover from a stale res URL on a 404 (Tier-2 BrowseMetadata by
// ObjectID, with the upstream's UDN driving live host:port lookup).
type UPnPRouting struct {
	SourcePath     string    // == tracks.path, the stable identity
	ServerUDN      string    // owning upstream MediaServer (uuid:...)
	ObjectID       string    // ContentDirectory item ID at last walk
	ParentObjectID string    // for amortized parent re-Browse cache heal
	ResURL         string    // last-known <res> URL (host:port floats)
	ProtocolInfo   string    // <res protocolInfo>, e.g. http-get:*:audio/x-flac:*
	LastSeenAt     time.Time // walk-time stamp for the reconcile sweep
}

// UpsertUPnPRouting inserts or replaces a routing row. Called once per
// track per walk by the upnpingest layer, AFTER the matching
// UpsertTrack/UpsertTrackBatch so the FK constraint is satisfied.
//
// Since upnp_track_routing references tracks(path) via a foreign key,
// the parent track row MUST exist in the database before the routing
// row can be inserted — inserting routing first would draw an
// SQLITE_CONSTRAINT_FOREIGNKEY error. The ingest layer's tracks-then-
// routing ordering is the load-bearing contract.
func (s *Store) UpsertUPnPRouting(ctx context.Context, r *UPnPRouting) error {
	if r == nil {
		return errors.New("manifest: nil UPnPRouting")
	}
	if r.SourcePath == "" {
		return errors.New("manifest: empty UPnPRouting.SourcePath")
	}
	if r.ServerUDN == "" {
		return errors.New("manifest: empty UPnPRouting.ServerUDN")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO upnp_track_routing(source_path, server_udn, object_id, parent_object_id, res_url, protocol_info, last_seen_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(source_path) DO UPDATE SET
			server_udn       = excluded.server_udn,
			object_id        = excluded.object_id,
			parent_object_id = excluded.parent_object_id,
			res_url          = excluded.res_url,
			protocol_info    = excluded.protocol_info,
			last_seen_at     = excluded.last_seen_at
	`,
		r.SourcePath,
		r.ServerUDN,
		r.ObjectID,
		r.ParentObjectID,
		r.ResURL,
		r.ProtocolInfo,
		r.LastSeenAt.UnixNano(),
	)
	return err
}

// UpsertUPnPRoutingBatch is the bulk-write twin used by the ingest
// layer; matches UpsertTrackBatch's "all under one transaction" shape.
// Empty input is a no-op.
func (s *Store) UpsertUPnPRoutingBatch(ctx context.Context, rs []*UPnPRouting) error {
	if len(rs) == 0 {
		return nil
	}
	for _, r := range rs {
		if r == nil || r.SourcePath == "" || r.ServerUDN == "" {
			return errors.New("manifest: invalid UPnPRouting in batch")
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("manifest: begin UpsertUPnPRoutingBatch tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO upnp_track_routing(source_path, server_udn, object_id, parent_object_id, res_url, protocol_info, last_seen_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(source_path) DO UPDATE SET
			server_udn       = excluded.server_udn,
			object_id        = excluded.object_id,
			parent_object_id = excluded.parent_object_id,
			res_url          = excluded.res_url,
			protocol_info    = excluded.protocol_info,
			last_seen_at     = excluded.last_seen_at
	`)
	if err != nil {
		return fmt.Errorf("manifest: prepare UpsertUPnPRoutingBatch: %w", err)
	}
	defer func() { _ = stmt.Close() }()
	for _, r := range rs {
		if _, err := stmt.ExecContext(ctx,
			r.SourcePath, r.ServerUDN, r.ObjectID, r.ParentObjectID,
			r.ResURL, r.ProtocolInfo, r.LastSeenAt.UnixNano(),
		); err != nil {
			return fmt.Errorf("manifest: UpsertUPnPRoutingBatch row %q: %w", r.SourcePath, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("manifest: commit UpsertUPnPRoutingBatch: %w", err)
	}
	return nil
}

// GetUPnPRouting returns the routing row for sourcePath. (nil, nil) when
// no row exists — callers (the file-serving proxy) treat that as "this
// is not a UPnP-sourced track" and route to the filesystem.
func (s *Store) GetUPnPRouting(ctx context.Context, sourcePath string) (*UPnPRouting, error) {
	if sourcePath == "" {
		return nil, nil
	}
	var (
		r     UPnPRouting
		lsNs  int64
		objID sql.NullString
		par   sql.NullString
		pi    sql.NullString
	)
	row := s.db.QueryRowContext(ctx, `
		SELECT source_path, server_udn, object_id, parent_object_id, res_url, protocol_info, last_seen_at
		  FROM upnp_track_routing
		 WHERE source_path = ?
	`, sourcePath)
	if err := row.Scan(&r.SourcePath, &r.ServerUDN, &objID, &par, &r.ResURL, &pi, &lsNs); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("manifest: GetUPnPRouting %q: %w", sourcePath, err)
	}
	r.ObjectID = objID.String
	r.ParentObjectID = par.String
	r.ProtocolInfo = pi.String
	r.LastSeenAt = time.Unix(0, lsNs)
	return &r, nil
}

// ListUPnPSourcePathsOlderThan returns the `source_path` values for the
// given server whose `last_seen_at` is strictly before `cutoff`. Used
// by the per-server reconcile sweep: tracks not refreshed in the
// current walk generation are reaped by passing each path to
// DeleteTrack (the FK CASCADE drops the routing row with it).
//
// Walking the result is the caller's responsibility — the ingest layer
// chunks the deletes so a giant library doesn't lock the writer for too
// long.
func (s *Store) ListUPnPSourcePathsOlderThan(ctx context.Context, serverUDN string, cutoff time.Time) ([]string, error) {
	if serverUDN == "" {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT source_path
		  FROM upnp_track_routing
		 WHERE server_udn = ?
		   AND last_seen_at < ?
		 ORDER BY source_path
	`, serverUDN, cutoff.UnixNano())
	if err != nil {
		return nil, fmt.Errorf("manifest: ListUPnPSourcePathsOlderThan: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("manifest: scan UPnP routing path: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// CountUPnPRoutingForServer returns the number of routing rows for the
// given server UDN. Cheap aggregate used by the admin "library" surface
// + by ingest telemetry.
func (s *Store) CountUPnPRoutingForServer(ctx context.Context, serverUDN string) (int, error) {
	var n int
	row := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM upnp_track_routing WHERE server_udn = ?`, serverUDN)
	if err := row.Scan(&n); err != nil {
		return 0, fmt.Errorf("manifest: CountUPnPRoutingForServer: %w", err)
	}
	return n, nil
}
