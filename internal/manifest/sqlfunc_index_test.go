package manifest

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestUnicodeLowerIndexIsSelected verifies SQLite's query planner
// actually chooses the v4 functional index for the LookupTrack
// fallback query. Without the determinism flag at registration —
// or without a byte-for-byte expression match between WHERE clause
// and CREATE INDEX — the planner silently falls back to a full
// table scan, and Pi-class hosts pay a 50k-row read on every iOS-
// shaped path lookup. This test fails loud if either invariant
// breaks.
func TestUnicodeLowerIndexIsSelected(t *testing.T) {
	s, err := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer s.Close()

	rows, err := s.db.Query(
		`EXPLAIN QUERY PLAN SELECT tags_json FROM tracks WHERE unicode_lower(path) = unicode_lower(?) LIMIT 2`,
		"any/path.flac",
	)
	if err != nil {
		t.Fatalf("EXPLAIN QUERY PLAN: %v", err)
	}
	defer rows.Close()

	var found bool
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatalf("scan: %v", err)
		}
		t.Logf("plan: %s", detail)
		if strings.Contains(detail, "idx_tracks_path_unicode_lower") {
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows iteration error: %v", err)
	}
	if !found {
		t.Errorf("query plan did not reference idx_tracks_path_unicode_lower — falling back to full table scan, which is the regression this index exists to prevent")
	}
}

// TestUnicodeLowerVariantIndexIsSelected mirrors the trap above for
// the `track_variants` table. LookupVariant is a paired-with-LookupTrack
// hot path on every iOS-shaped /v1/download?variant=... call; if the
// v4 migration's `idx_track_variants_source_path_unicode_lower` is
// silently dropped or the WHERE expression drifts (refactored to
// `lower()` by mistake), the planner falls back to a full table scan
// of track_variants and the regression goes undetected because the
// tracks-side trap above still passes. Greptile bot review on PR #182.
func TestUnicodeLowerVariantIndexIsSelected(t *testing.T) {
	s, err := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer s.Close()

	rows, err := s.db.Query(
		`EXPLAIN QUERY PLAN SELECT source_path FROM track_variants
		 WHERE unicode_lower(source_path) = unicode_lower(?) AND variant_id = ?
		 LIMIT 2`,
		"any/path.flac", "upscaled-v2-44khz-16bit",
	)
	if err != nil {
		t.Fatalf("EXPLAIN QUERY PLAN: %v", err)
	}
	defer rows.Close()

	var found bool
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatalf("scan: %v", err)
		}
		t.Logf("plan: %s", detail)
		if strings.Contains(detail, "idx_track_variants_source_path_unicode_lower") {
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows iteration error: %v", err)
	}
	if !found {
		t.Errorf("query plan did not reference idx_track_variants_source_path_unicode_lower — falling back to full table scan, which is the regression this index exists to prevent")
	}
}
