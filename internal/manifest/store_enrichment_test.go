package manifest

import (
	"context"
	"testing"
	"time"
)

// TestUnenrichedTracksOrdersByIndexedAtDesc pins the LIFO ordering contract:
// UnenrichedTracks yields newest-`indexed_at` first (so a freshly-added album
// enriches ahead of the older backlog) with `path ASC` as the deterministic
// tie-break within an equal clock. The paths are chosen so alphabetical order
// (the pre-LIFO behaviour) and indexed_at order DISAGREE — proving the sort is
// indexed_at-driven, not path-driven.
func TestUnenrichedTracksOrdersByIndexedAtDesc(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()

	// Insert in an order that is neither the final order nor the alphabetical
	// order. Clock is injected per-insert so indexed_at is deterministic.
	// Note the paths: the NEWEST track sorts FIRST alphabetically and the
	// OLDEST sorts LAST — so `ORDER BY path ASC` (old behaviour) and
	// `ORDER BY indexed_at DESC` (new) produce opposite results.
	insert := func(ns int64, path string) {
		s.now = func() time.Time { return time.Unix(0, ns) }
		if err := s.UpsertTrack(ctx, &Track{Path: path, Size: 1, ModTime: time.Unix(1, 0)}); err != nil {
			t.Fatalf("UpsertTrack(%q): %v", path, err)
		}
	}
	insert(1000, "Music/Z/old.flac")    // oldest, sorts last by path
	insert(3000, "Music/M/b.flac")      // middle clock, tie with a.flac
	insert(3000, "Music/M/a.flac")      // same clock as b.flac — tie-break case
	insert(5000, "Music/A/newest.flac") // newest, sorts first by path

	got, err := s.UnenrichedTracks(ctx, 100)
	if err != nil {
		t.Fatalf("UnenrichedTracks: %v", err)
	}
	want := []string{
		"Music/A/newest.flac", // indexed_at 5000 (newest) — beats its alphabetical-first path
		"Music/M/a.flac",      // indexed_at 3000, path a < b (tie-break)
		"Music/M/b.flac",      // indexed_at 3000
		"Music/Z/old.flac",    // indexed_at 1000 (oldest) — last despite alphabetical-last path
	}
	if len(got) != len(want) {
		t.Fatalf("got %d rows, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].Path != w {
			gotPaths := make([]string, len(got))
			for j := range got {
				gotPaths[j] = got[j].Path
			}
			t.Fatalf("ordering mismatch at index %d: got %q, want %q\nfull order: %v", i, got[i].Path, w, gotPaths)
		}
	}
}

// TestEnrichmentBreakdown pins the derived three-state split: pending
// (enriched_at = 0), matched (enriched + artworkMBID present), missing
// (enriched + artworkMBID absent). Every state is produced through the REAL
// store write paths (UpsertTrack + MarkEnriched) so the test locks the
// derivation against what enrichOne actually writes.
func TestEnrichmentBreakdown(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()

	// pending: never marked enriched.
	if err := s.UpsertTrack(ctx, &Track{Path: "Music/P/pending.flac", Size: 1, ModTime: time.Unix(1, 0)}); err != nil {
		t.Fatalf("UpsertTrack pending: %v", err)
	}

	// matched: enriched AND carries an artwork MBID (a real cover was cached).
	matched := &Track{
		Path: "Music/M/matched.flac", Size: 1, ModTime: time.Unix(1, 0),
		ArtworkMBID: "12aae8a7-e814-4c46-94d7-5c9e053bda5b",
	}
	if err := s.UpsertTrack(ctx, matched); err != nil {
		t.Fatalf("UpsertTrack matched: %v", err)
	}
	if err := s.MarkEnriched(ctx, matched); err != nil {
		t.Fatalf("MarkEnriched matched: %v", err)
	}

	// missing: enriched but no artwork MBID — the coverage-gap state.
	missing := &Track{Path: "Music/G/missing.flac", Size: 1, ModTime: time.Unix(1, 0)}
	if err := s.UpsertTrack(ctx, missing); err != nil {
		t.Fatalf("UpsertTrack missing: %v", err)
	}
	if err := s.MarkEnriched(ctx, missing); err != nil {
		t.Fatalf("MarkEnriched missing: %v", err)
	}

	pending, matchedCnt, missingCnt, last, err := s.EnrichmentBreakdown(ctx)
	if err != nil {
		t.Fatalf("EnrichmentBreakdown: %v", err)
	}
	if pending != 1 || matchedCnt != 1 || missingCnt != 1 {
		t.Fatalf("breakdown = (pending=%d matched=%d missing=%d), want (1,1,1)", pending, matchedCnt, missingCnt)
	}
	if last == nil {
		t.Fatalf("lastEnrichedAt = nil, want non-nil (two tracks were enriched)")
	}
}

// TestEnrichmentBreakdownEmptyAndAllPending pins the two zero-ish edges:
// an empty library returns all-zero + nil lastEnrichedAt, and a library with
// ONLY pending rows must ALSO return nil lastEnrichedAt — MAX(enriched_at) is a
// valid 0 (not SQL NULL) there, so the `!= 0` guard is load-bearing (a naive
// map would surface a 1970 "last enriched" timestamp).
func TestEnrichmentBreakdownEmptyAndAllPending(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()

	pending, matched, missing, last, err := s.EnrichmentBreakdown(ctx)
	if err != nil {
		t.Fatalf("EnrichmentBreakdown (empty): %v", err)
	}
	if pending != 0 || matched != 0 || missing != 0 || last != nil {
		t.Fatalf("empty store = (%d,%d,%d,last=%v), want (0,0,0,nil)", pending, matched, missing, last)
	}

	for _, p := range []string{"Music/A/1.flac", "Music/B/2.flac"} {
		if err := s.UpsertTrack(ctx, &Track{Path: p, Size: 1, ModTime: time.Unix(1, 0)}); err != nil {
			t.Fatalf("UpsertTrack(%q): %v", p, err)
		}
	}
	pending, matched, missing, last, err = s.EnrichmentBreakdown(ctx)
	if err != nil {
		t.Fatalf("EnrichmentBreakdown (all pending): %v", err)
	}
	if pending != 2 || matched != 0 || missing != 0 {
		t.Fatalf("all-pending = (pending=%d matched=%d missing=%d), want (2,0,0)", pending, matched, missing)
	}
	if last != nil {
		t.Fatalf("all-pending lastEnrichedAt = %v, want nil (MAX(enriched_at)=0 must map to nil)", last)
	}
}
