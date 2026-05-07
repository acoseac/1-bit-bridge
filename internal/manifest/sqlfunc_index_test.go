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
	if !found {
		t.Errorf("query plan did not reference idx_tracks_path_unicode_lower — falling back to full table scan, which is the regression this index exists to prevent")
	}
}
