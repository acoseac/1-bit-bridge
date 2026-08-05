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
type DupeSummary struct {
	SchemaVersion int               `json:"schemaVersion"`
	StampedAt     time.Time         `json:"stampedAt"`
	Policy        string            `json:"policy"`
	Scanned       int               `json:"scanned"`
	Groups        int               `json:"groups"`
	Suppressed    int               `json:"suppressed"`
	Served        int               `json:"served"`
	Tiers         []DupeTierSummary `json:"tiers"`
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
