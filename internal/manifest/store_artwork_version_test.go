package manifest

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ResolveArtworkVersionMBID backs the /v1/artwork 16-hex alias: iOS keys
// cover fetches on `artworkVersion ?? artworkMBID`, so a bare version
// tag must resolve to the servable MBID server-side.
func TestResolveArtworkVersionMBID(t *testing.T) {
	s, err := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()

	const mbid = "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
	const version = "aa20c66194bcbe9e"
	tr := &Track{Path: "a.flac", Size: 1, ModTime: time.Now(), Artist: "A", Album: "B", ArtworkMBID: mbid}
	if err := s.UpsertTrack(ctx, tr); err != nil {
		t.Fatal(err)
	}
	if n, err := s.SetArtworkVersionAndBumpIndex(ctx, mbid, version); err != nil || n != 1 {
		t.Fatalf("stamp version: n=%d err=%v", n, err)
	}

	got, err := s.ResolveArtworkVersionMBID(ctx, version)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != mbid {
		t.Errorf("resolved %q, want %q", got, mbid)
	}

	// Unknown version tag → ("", nil), never an error: the handler
	// answers not_found, not 500.
	got, err = s.ResolveArtworkVersionMBID(ctx, "0000000000000000")
	if err != nil || got != "" {
		t.Errorf("unknown tag: got (%q, %v), want (\"\", nil)", got, err)
	}
	// Empty input short-circuits.
	got, err = s.ResolveArtworkVersionMBID(ctx, "")
	if err != nil || got != "" {
		t.Errorf("empty tag: got (%q, %v), want (\"\", nil)", got, err)
	}
}

// The v40 partial index must back the resolve — the lookup runs on the
// /v1/artwork request path, so a table scan over a 20k-row library per
// aliased request is the regression this pin exists to catch.
func TestResolveArtworkVersionMBIDUsesIndex(t *testing.T) {
	s, err := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	const mbid = "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
	// Enough rows that a full-table SCAN is measurably worse than the
	// index seek — on a near-empty table the planner legitimately
	// prefers SCAN and the pin would be vacuous. Production shape:
	// artwork_version is NULL on all but the premium-refetched
	// minority, which is exactly what the partial index covers.
	tracks := make([]*Track, 0, 120)
	for i := 0; i < 120; i++ {
		tracks = append(tracks, &Track{
			Path: fmt.Sprintf("t%03d.flac", i), Size: 1, ModTime: time.Now(),
			Artist: "A", Album: "B",
		})
	}
	tracks[0].ArtworkMBID = mbid
	if err := s.UpsertTrackBatch(ctx, tracks); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetArtworkVersionAndBumpIndex(ctx, mbid, "aa20c66194bcbe9e"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`ANALYZE`); err != nil {
		t.Fatal(err)
	}

	rows, err := s.db.Query(`EXPLAIN QUERY PLAN
		SELECT json_extract(tags_json, '$.artworkMBID')
		  FROM tracks
		 WHERE artwork_version = ?
		 LIMIT 1`, "aa20c66194bcbe9e")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatal(err)
		}
		plan.WriteString(detail)
		plan.WriteString("\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	planStr := plan.String()
	t.Logf("EXPLAIN QUERY PLAN:\n%s", planStr)
	if !strings.Contains(planStr, "idx_tracks_artwork_version") {
		t.Errorf("plan does not use idx_tracks_artwork_version:\n%s", planStr)
	}
}
