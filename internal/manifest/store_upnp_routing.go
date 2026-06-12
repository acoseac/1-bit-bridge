package manifest

import (
	"context"
	"database/sql"
	"encoding/json"
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
	// No ORDER BY: the composite index idx_upnp_routing_server_seen is
	// (server_udn, last_seen_at), so an ORDER BY source_path would force
	// a temp-B-tree filesort. The sole consumer is the reconcile sweep,
	// which feeds the paths into a delete batch (order-independent), so
	// the index's natural order is fine.
	rows, err := s.db.QueryContext(ctx, `
		SELECT source_path
		  FROM upnp_track_routing
		 WHERE server_udn = ?
		   AND last_seen_at < ?
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

// ListUPnPRoutedServerUDNs returns the distinct server_udn values that
// currently have routing rows. Used by the ingest orphan sweep: a UDN
// present here but absent from the configured server set belongs to a
// server the operator removed, and its rows would otherwise live
// forever (the fs scanner's missing pass deliberately spares routed
// rows — PR #370 — so the ingest is their ONLY lifecycle owner).
// Read path — no s.mu.
func (s *Store) ListUPnPRoutedServerUDNs(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT server_udn FROM upnp_track_routing ORDER BY server_udn
	`)
	if err != nil {
		return nil, fmt.Errorf("manifest: ListUPnPRoutedServerUDNs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			return nil, fmt.Errorf("manifest: scan UPnP routed server UDN: %w", err)
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// ListUPnPSourcePathsByServer returns every source_path routed via the
// given server, regardless of last_seen_at. Used by the ingest orphan
// sweep to reap ALL of a removed server's rows (the OlderThan variant
// above serves the per-walk reconcile, where the cutoff matters).
// Read path — no s.mu.
func (s *Store) ListUPnPSourcePathsByServer(ctx context.Context, serverUDN string) ([]string, error) {
	if serverUDN == "" {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT source_path
		  FROM upnp_track_routing
		 WHERE server_udn = ?
		 ORDER BY source_path
	`, serverUDN)
	if err != nil {
		return nil, fmt.Errorf("manifest: ListUPnPSourcePathsByServer: %w", err)
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

// CountUPnPRoutingTotal returns the total count of routing rows across
// every server. Powers the admin dashboard's "N UPnP-routed tracks" tile.
func (s *Store) CountUPnPRoutingTotal(ctx context.Context) (int, error) {
	var n int
	row := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM upnp_track_routing`)
	if err := row.Scan(&n); err != nil {
		return 0, fmt.Errorf("manifest: CountUPnPRoutingTotal: %w", err)
	}
	return n, nil
}

// AllUPnPRoutingPaths returns every `source_path` in `upnp_track_routing`
// — i.e. the set of manifest tracks whose bytes live on an upstream UPnP
// MediaServer rather than the local filesystem.
//
// Why a separate bulk-read API instead of calling `GetUPnPRouting` per
// track: the DLNA library-adapter rebuild iterates every manifest track
// (15k+ on the live test bridge with the 2Go ingested) and needs an
// "is this path routed?" check per row when `bridgefs.Resolver.Resolve`
// fails. A per-track point query inside that loop is an **N+1** under a
// strict 10 s context deadline — at 15k routed tracks the rebuild
// reliably tripped the timeout and silently dropped the remainder. This
// bulk read is one SELECT + one in-memory scan; caller builds a
// `map[string]struct{}` for O(1) lookup in the rebuild loop. Mirrors
// the existing `AllVariants` bulk-read pattern. Per Gemini HIGH on
// PR #356.
//
// Returns paths in `source_path` ASC for stable ordering — the caller
// (the dlna rebuild loop) is set-keyed, so any order works, but the
// ORDER BY makes the test fixture deterministic at trivial cost on
// SQLite (the table's PRIMARY KEY).
func (s *Store) AllUPnPRoutingPaths(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT source_path
		  FROM upnp_track_routing
		 ORDER BY source_path
	`)
	if err != nil {
		return nil, fmt.Errorf("manifest: AllUPnPRoutingPaths: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("manifest: scan AllUPnPRoutingPaths: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("manifest: iterate AllUPnPRoutingPaths: %w", err)
	}
	return out, nil
}

// UPnPRoutedSourcePaths returns every source_path currently present in
// the routing table, across all servers. The filesystem scanner's
// missing-tracks pass consumes it as an EXCLUSION set: routed rows are
// not filesystem-owned (they never appear in a disk walk by
// construction), so counting them "missing" every scan would march
// their missing_count to the threshold and mass-delete the routed
// catalog — exactly what happened once the ingest's skip-if-unchanged
// (PR #369) stopped resetting the counter via unconditional re-upserts
// (and what already happened pre-#369 whenever the upstream was offline
// for `threshold` consecutive filesystem scans). Routed-row lifecycle
// belongs to the ingest's own last_seen_at reconcile sweep.
//
// Read-only — no s.mu, matching the sibling routing readers.
func (s *Store) UPnPRoutedSourcePaths(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT source_path FROM upnp_track_routing`)
	if err != nil {
		return nil, fmt.Errorf("manifest: UPnPRoutedSourcePaths: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("manifest: scan routed source path: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ListUPnPTracksByServer returns the stored manifest Track for every
// path currently routed via serverUDN, keyed by Track.Path. The ingest
// layer loads this ONCE per walk as the skip-if-unchanged baseline: a
// walked track whose walk-authoritative fields match the stored row
// skips its UpsertTrack entirely, preserving `indexed_at` (so iOS
// delta-sync doesn't re-receive the whole routed library every walk)
// AND `enriched_at` (so the enricher doesn't re-process unchanged
// tracks — UpsertTrack resets it to 0 by design "on track change",
// and an unconditional re-upsert violated that contract for UPnP
// rows; see internal/upnpingest.walkFieldsEqual).
//
// Read-only — no s.mu, matching ListUPnPSourcePathsOlderThan /
// UnenrichedTracks. A ~15k-track upstream (Chord 2Go class) decodes
// in well under a second and the map lives only for the walk.
func (s *Store) ListUPnPTracksByServer(ctx context.Context, serverUDN string) (map[string]*Track, error) {
	if serverUDN == "" {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT t.tags_json
		  FROM tracks t
		  JOIN upnp_track_routing r ON r.source_path = t.path
		 WHERE r.server_udn = ?
	`, serverUDN)
	if err != nil {
		return nil, fmt.Errorf("manifest: ListUPnPTracksByServer: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make(map[string]*Track)
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("manifest: scan UPnP track row: %w", err)
		}
		var t Track
		if err := json.Unmarshal(raw, &t); err != nil {
			return nil, fmt.Errorf("manifest: decode UPnP track row: %w", err)
		}
		out[t.Path] = &t
	}
	return out, rows.Err()
}
