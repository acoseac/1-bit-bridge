// Duplicate-stamp persistence: the write half of the post-scan
// duplicate stamping pass (scanner_dupes.go) and the persisted summary
// the admin Duplicates page reads.
package manifest

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// DupeStampState is a row's CURRENT stamp projection, streamed alongside
// dupes.Row by StreamTrackDupeRefsUnderPrefix so the stamping pass can
// diff desired-vs-current and write only changed rows (a stable library
// produces zero writes, matching the reconciliation passes' idle cost).
type DupeStampState struct {
	GroupID    string
	Tier       string
	Suppressed bool
}

// DupeStamp is one row's desired stamp write. BumpIndexed is set ONLY on
// a suppressed→served transition: that is the one change a delta-syncing
// iOS client must be pushed (`WHERE indexed_at > since` is the delta
// watermark, and lifting suppression mutates no other row state). A row
// BECOMING suppressed is deliberately not bumped — it is excluded from
// the served stream, so a bump would be a wasted write; already-synced
// clients keep the row until their next full sync, invisible behind the
// client-side dedup.
type DupeStamp struct {
	Path        string
	GroupID     string
	Tier        string
	Suppressed  bool
	BumpIndexed bool
}

// ApplyDupeStamps writes the changed stamps in one transaction.
//
// Contract (the StampExtractorVersionBatch / applyReconciledTracks
// template): holds s.mu (writer contract), one tx, prepared statements;
// touches ONLY the three v31 dupe columns plus — for BumpIndexed rows —
// the strict-advance indexed_at CASE WHEN. NEVER touches enriched_at
// (suppression is a serving decision, not (re-)enrichment — this is
// deliberately NOT an enriched_at writer) and NEVER rewrites tags_json.
// Returns the number of rows actually updated.
func (s *Store) ApplyDupeStamps(ctx context.Context, stamps []DupeStamp) (int, error) {
	if len(stamps) == 0 {
		return 0, nil
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	plain, err := tx.PrepareContext(ctx, `
		UPDATE tracks
		SET dupe_group_id = ?, dupe_tier = ?, dupe_suppressed = ?
		WHERE path = ?
	`)
	if err != nil {
		return 0, err
	}
	defer plain.Close()
	bump, err := tx.PrepareContext(ctx, `
		UPDATE tracks
		SET dupe_group_id = ?, dupe_tier = ?, dupe_suppressed = ?,
		    indexed_at = CASE
		        WHEN indexed_at >= ? THEN indexed_at + 1
		        ELSE ?
		    END
		WHERE path = ?
	`)
	if err != nil {
		return 0, err
	}
	defer bump.Close()
	now := s.now().UnixNano()
	n := 0
	for _, st := range stamps {
		suppressed := 0
		if st.Suppressed {
			suppressed = 1
		}
		var res interface{ RowsAffected() (int64, error) }
		if st.BumpIndexed {
			res, err = bump.ExecContext(ctx, st.GroupID, st.Tier, suppressed, now, now, st.Path)
		} else {
			res, err = plain.ExecContext(ctx, st.GroupID, st.Tier, suppressed, st.Path)
		}
		if err != nil {
			return 0, err
		}
		if affected, _ := res.RowsAffected(); affected > 0 {
			n++
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return n, nil
}

// dupeSummaryScanStateKey is the scan_state row the stamping pass writes
// its summary under and the admin Duplicates page reads it from —
// zero-cost tiles that are exactly consistent with the stamps, no TTL
// machinery.
const dupeSummaryScanStateKey = "dupe_summary"

// DupeSummarySchemaVersion identifies the persisted summary JSON shape
// (a scan_state document, not the /v1 wire — bump this, never
// ProtocolVersion).
const DupeSummarySchemaVersion = 1

// DupeTierSummary is one tier's aggregate in the persisted summary.
type DupeTierSummary struct {
	Tier string `json:"tier"`
	// Groups / RedundantFiles / NonLargestBytes mirror the CLI report's
	// vocabulary: files beyond one per group, and bytes outside each
	// group's largest member — deliberately never named "wasted".
	Groups          int   `json:"groups"`
	RedundantFiles  int   `json:"redundantFiles"`
	NonLargestBytes int64 `json:"bytesInNonLargestCopies"`
	Suppressed      int   `json:"suppressed"`
}

// DupeSummary is the persisted output of one stamping pass.
//
// EVERY count here describes the population the STAMPING PASS WALKED,
// which is the filesystem library only: RestampDuplicates streams
// StreamTrackDupeRefsUnderPrefix with includeRouted=false, so UPnP-routed
// upstream rows are excluded — they are never grouped, stamped or
// suppressed, and their lifecycle belongs to the ingest reconcile.
//
// So these are NOT the /v1 served-track numbers, and on a hybrid
// deployment (filesystem roots plus a UPnP upstream) they differ by the
// whole routed catalogue: 122 filesystem tracks here against ~15.4k in
// health.tracksIndexed. Anything rendering them has to say which
// population it means. Do not "fix" that gap by sourcing Served from
// CountServedTracks — this is a stamp-time document whose whole value is
// describing applied state, and mixing a live cross-population count in
// would break Served + Suppressed == Scanned.
type DupeSummary struct {
	SchemaVersion int       `json:"schemaVersion"`
	StampedAt     time.Time `json:"stampedAt"`
	Policy        string    `json:"policy"`
	// Scanned is the row count the pass observed; Served is
	// Scanned - Suppressed. Both are scanned-library-scoped (see above).
	Scanned    int `json:"scanned"`
	Groups     int `json:"groups"`
	Suppressed int `json:"suppressed"`
	Served     int `json:"served"`
	// MD5Known/MD5Total: audio-MD5 evidence coverage across group
	// MEMBERS — the "evidence still arriving" signal while the
	// ExtractorVersion-3 re-extract backfills the library.
	MD5Known int               `json:"md5Known"`
	MD5Total int               `json:"md5Total"`
	Tiers    []DupeTierSummary `json:"tiers"`
}

// SaveDupeSummary persists the stamping-pass summary. scan_state writes
// hold s.mu via SetScanState.
func (s *Store) SaveDupeSummary(ctx context.Context, sum DupeSummary) error {
	raw, err := json.Marshal(sum)
	if err != nil {
		return fmt.Errorf("marshal dupe summary: %w", err)
	}
	return s.SetScanState(ctx, dupeSummaryScanStateKey, string(raw))
}

// LoadDupeSummary reads the last persisted summary; (nil, nil) when no
// stamping pass has run yet.
func (s *Store) LoadDupeSummary(ctx context.Context) (*DupeSummary, error) {
	raw, err := s.GetScanState(ctx, dupeSummaryScanStateKey)
	if err != nil {
		return nil, err
	}
	if raw == "" {
		return nil, nil
	}
	var sum DupeSummary
	if err := json.Unmarshal([]byte(raw), &sum); err != nil {
		return nil, fmt.Errorf("decode dupe summary: %w", err)
	}
	return &sum, nil
}

// DupeGroupMemberRow / DupeGroupRow are the admin Duplicates page's
// group-listing projection — store row structs, NO json tags (the wire
// DTO lives in internal/admin per the wire-type discipline).
type DupeGroupMemberRow struct {
	Path          string
	Suppressed    bool
	Codec         string
	SampleRate    int
	BitsPerSample int
	IsDSD         bool
	SizeBytes     int64
	DurationSec   float64
	Title         string
	Album         string
	AlbumArtist   string
}

type DupeGroupRow struct {
	GroupID string
	Tier    string
	Members []DupeGroupMemberRow
}

// ListDupeGroupsPage pages over stamped duplicate groups (cursor =
// dupe_group_id, exclusive), optionally narrowed to one tier, and
// materialises each selected group's members. The DISTINCT-group
// subquery rides the v31 partial index; the json_extract projection
// runs only on the selected groups' member rows (bounded by
// limit × group size), so this is a click-driven admin cost, not the
// AtlasMetaBreakdownCounts full-walk class. nextCursor is "" on the
// last page (limit+1 over-fetch on the group ids, the
// buildManifestPage idiom). Read-only; no s.mu.
func (s *Store) ListDupeGroupsPage(ctx context.Context, tier, afterGroupID string, limit int) ([]DupeGroupRow, string, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	idQuery := `SELECT DISTINCT dupe_group_id FROM tracks
	             WHERE dupe_group_id != '' AND dupe_group_id > ?`
	idArgs := []any{afterGroupID}
	if tier != "" {
		idQuery += ` AND dupe_tier = ?`
		idArgs = append(idArgs, tier)
	}
	idQuery += ` ORDER BY dupe_group_id LIMIT ?`
	idArgs = append(idArgs, limit+1)
	ids, err := collectStringColumn(s.db.QueryContext(ctx, idQuery, idArgs...))
	if err != nil {
		return nil, "", fmt.Errorf("list dupe group ids: %w", err)
	}
	next := ""
	if len(ids) > limit {
		ids = ids[:limit]
		next = ids[len(ids)-1]
	}
	if len(ids) == 0 {
		return nil, "", nil
	}
	blob, err := json.Marshal(ids)
	if err != nil {
		return nil, "", err
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT t.dupe_group_id, t.dupe_tier, t.path, t.dupe_suppressed,
		       COALESCE(t.codec, ''),
		       COALESCE(t.sample_rate, 0),
		       COALESCE(t.bits_per_sample, 0),
		       COALESCE(t.is_dsd, 0),
		       COALESCE(json_extract(t.tags_json, '$.size'),     0),
		       COALESCE(json_extract(t.tags_json, '$.duration'), 0),
		       COALESCE(json_extract(t.tags_json, '$.title'),       ''),
		       COALESCE(json_extract(t.tags_json, '$.album'),       ''),
		       COALESCE(json_extract(t.tags_json, '$.albumArtist'), '')
		  FROM tracks t
		 WHERE t.dupe_group_id IN (SELECT value FROM json_each(?))
		 ORDER BY t.dupe_group_id, t.path
	`, string(blob))
	if err != nil {
		return nil, "", fmt.Errorf("list dupe group members: %w", err)
	}
	defer rows.Close()
	var out []DupeGroupRow
	for rows.Next() {
		var (
			gid, tierVal      string
			m                 DupeGroupMemberRow
			suppressed, isDSD int
		)
		if err := rows.Scan(&gid, &tierVal, &m.Path, &suppressed,
			&m.Codec, &m.SampleRate, &m.BitsPerSample, &isDSD,
			&m.SizeBytes, &m.DurationSec, &m.Title, &m.Album, &m.AlbumArtist); err != nil {
			return nil, "", err
		}
		m.Suppressed = suppressed != 0
		m.IsDSD = isDSD != 0
		if len(out) == 0 || out[len(out)-1].GroupID != gid {
			out = append(out, DupeGroupRow{GroupID: gid, Tier: tierVal})
		}
		out[len(out)-1].Members = append(out[len(out)-1].Members, m)
	}
	return out, next, rows.Err()
}
