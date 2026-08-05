package manifest

import (
	"context"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/dupes"
)

// TestMigrationV31IsIdempotent re-opens the same DB so the v31 post()
// runs against an already-migrated schema (the atlasColumnExists guard).
func TestMigrationV31IsIdempotent(t *testing.T) {
	dir := t.TempDir()
	s1, err := OpenStore(dir + "/bridge.db")
	if err != nil {
		t.Fatal(err)
	}
	if err := s1.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := OpenStore(dir + "/bridge.db")
	if err != nil {
		t.Fatalf("reopen after migration: %v", err)
	}
	defer s2.Close()
	if _, err := s2.db.Exec(`SELECT dupe_group_id, dupe_tier, dupe_suppressed FROM tracks LIMIT 0`); err != nil {
		t.Fatalf("v31 columns missing: %v", err)
	}
}

func stampOf(t *testing.T, s *Store, path string) DupeStampState {
	t.Helper()
	var st DupeStampState
	var sup int
	err := s.db.QueryRow(`SELECT dupe_group_id, dupe_tier, dupe_suppressed FROM tracks WHERE path = ?`,
		path).Scan(&st.GroupID, &st.Tier, &sup)
	if err != nil {
		t.Fatalf("stampOf(%q): %v", path, err)
	}
	st.Suppressed = sup != 0
	return st
}

func indexedAtOf(t *testing.T, s *Store, path string) int64 {
	t.Helper()
	var n int64
	if err := s.db.QueryRow(`SELECT indexed_at FROM tracks WHERE path = ?`, path).Scan(&n); err != nil {
		t.Fatalf("indexedAtOf(%q): %v", path, err)
	}
	return n
}

// TestApplyDupeStamps_BumpSemantics pins the delta contract: ONLY a
// suppressed→served transition (BumpIndexed) advances indexed_at, and it
// strict-advances so `indexed_at > since` can't miss it; plain stamp
// writes (including suppression itself) leave the watermark alone.
func TestApplyDupeStamps_BumpSemantics(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()
	ctx := context.Background()
	seedFSTrack(t, s, "a/b/x.flac")
	before := indexedAtOf(t, s, "a/b/x.flac")

	// Suppress: stamps land, watermark untouched.
	n, err := s.ApplyDupeStamps(ctx, []DupeStamp{{Path: "a/b/x.flac", GroupID: "g1", Tier: "same-format", Suppressed: true}})
	if err != nil || n != 1 {
		t.Fatalf("suppress: n=%d err=%v", n, err)
	}
	if got := indexedAtOf(t, s, "a/b/x.flac"); got != before {
		t.Fatalf("suppressing must NOT bump indexed_at (%d → %d)", before, got)
	}
	st := stampOf(t, s, "a/b/x.flac")
	if st.GroupID != "g1" || st.Tier != "same-format" || !st.Suppressed {
		t.Fatalf("stamp not written: %+v", st)
	}

	// Un-suppress with BumpIndexed: watermark strictly advances.
	n, err = s.ApplyDupeStamps(ctx, []DupeStamp{{Path: "a/b/x.flac", GroupID: "g1", Tier: "same-format", Suppressed: false, BumpIndexed: true}})
	if err != nil || n != 1 {
		t.Fatalf("unsuppress: n=%d err=%v", n, err)
	}
	after := indexedAtOf(t, s, "a/b/x.flac")
	if after <= before {
		t.Fatalf("unsuppress must strict-advance indexed_at (%d → %d)", before, after)
	}
	// And the served delta actually carries the row.
	since := time.Unix(0, before).UTC()
	rows, err := s.ListServedTracks(ctx, &since)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Path != "a/b/x.flac" {
		t.Fatalf("delta after unsuppress = %v", rows)
	}
	// enriched_at is never touched by stamp writes.
	var enrichedAt int64
	if err := s.db.QueryRow(`SELECT enriched_at FROM tracks WHERE path = 'a/b/x.flac'`).Scan(&enrichedAt); err != nil {
		t.Fatal(err)
	}
	if enrichedAt != 0 {
		t.Fatalf("stamp writes must not touch enriched_at, got %d", enrichedAt)
	}
}

// TestServedReadersExcludeSuppressed covers the whole Served* family
// against its full-store twins, including the count-consistency
// invariant `CountServedTracks == len(ListServedTracks(nil))` that
// backs manifest `total`.
func TestServedReadersExcludeSuppressed(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()
	ctx := context.Background()
	seedFSTrack(t, s, "a/b/kept.flac")
	seedFSTrack(t, s, "a/b/suppressed.flac")
	if _, err := s.ApplyDupeStamps(ctx, []DupeStamp{
		{Path: "a/b/suppressed.flac", GroupID: "g", Tier: "same-format", Suppressed: true},
	}); err != nil {
		t.Fatal(err)
	}

	served, err := s.ListServedTracks(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(served) != 1 || served[0].Path != "a/b/kept.flac" {
		t.Fatalf("ListServedTracks = %+v", served)
	}
	all, err := s.ListTracks(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("full-store ListTracks must keep seeing suppressed rows, got %d", len(all))
	}

	var streamed []string
	if err := s.StreamServedTracks(ctx, nil, func(tr *Track) error {
		streamed = append(streamed, tr.Path)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(streamed) != 1 || streamed[0] != "a/b/kept.flac" {
		t.Fatalf("StreamServedTracks = %v", streamed)
	}

	page, err := s.ListServedTracksPage(ctx, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 1 || page[0].Path != "a/b/kept.flac" {
		t.Fatalf("ListServedTracksPage = %+v", page)
	}

	nServed, err := s.CountServedTracks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if nServed != len(served) {
		t.Fatalf("CountServedTracks (%d) must equal the served row count (%d) — manifest total rides this", nServed, len(served))
	}
	nAll, err := s.CountTracks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if nAll != 2 {
		t.Fatalf("CountTracks must stay full-store, got %d", nAll)
	}
}

// TestEnrichmentCountsScopedToServed: the wire numerator counts served
// rows only, so tracksEnriched can never exceed tracksTotal.
func TestEnrichmentCountsScopedToServed(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()
	ctx := context.Background()
	seedFSTrack(t, s, "a/b/kept.flac")
	seedFSTrack(t, s, "a/b/suppressed.flac")
	for _, p := range []string{"a/b/kept.flac", "a/b/suppressed.flac"} {
		tr, err := s.GetTrack(ctx, p)
		if err != nil || tr == nil {
			t.Fatalf("GetTrack(%q): %v %v", p, tr, err)
		}
		if err := s.MarkEnriched(ctx, tr); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.ApplyDupeStamps(ctx, []DupeStamp{
		{Path: "a/b/suppressed.flac", GroupID: "g", Tier: "same-format", Suppressed: true},
	}); err != nil {
		t.Fatal(err)
	}
	enriched, last, err := s.EnrichmentCounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if enriched != 1 {
		t.Fatalf("enriched = %d, want 1 (served rows only)", enriched)
	}
	if last == nil {
		t.Fatal("lastEnrichedAt stays a freshness stamp (unscoped) and must be set")
	}
	total, err := s.CountServedTracks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if enriched > total {
		t.Fatalf("tracksEnriched (%d) may never exceed tracksTotal (%d)", enriched, total)
	}
}

// TestUpsertPreservesDupeStamps pins the fail-open upsert contract: a
// re-extracted (changed) row keeps its stamps until the next stamping
// pass; a fresh insert defaults to served.
func TestUpsertPreservesDupeStamps(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()
	ctx := context.Background()
	seedFSTrack(t, s, "a/b/x.flac")
	if _, err := s.ApplyDupeStamps(ctx, []DupeStamp{
		{Path: "a/b/x.flac", GroupID: "g", Tier: "same-format", Suppressed: true},
	}); err != nil {
		t.Fatal(err)
	}
	// Re-upsert with changed size/mtime (the scanner's changed-file shape).
	tr := &Track{Path: "a/b/x.flac", Size: 999, ModTime: time.Unix(5, 0).UTC(), Title: "retagged"}
	if err := s.UpsertTrack(ctx, tr); err != nil {
		t.Fatal(err)
	}
	st := stampOf(t, s, "a/b/x.flac")
	if st.GroupID != "g" || !st.Suppressed {
		t.Fatalf("upsert must preserve dupe stamps, got %+v", st)
	}
	// Batch path too.
	if err := s.UpsertTrackBatch(ctx, []*Track{{Path: "a/b/x.flac", Size: 1000, ModTime: time.Unix(6, 0).UTC()}}); err != nil {
		t.Fatal(err)
	}
	st = stampOf(t, s, "a/b/x.flac")
	if st.GroupID != "g" || !st.Suppressed {
		t.Fatalf("batch upsert must preserve dupe stamps, got %+v", st)
	}
	// Fresh insert defaults to served / unstamped.
	seedFSTrack(t, s, "a/b/fresh.flac")
	if st := stampOf(t, s, "a/b/fresh.flac"); st.GroupID != "" || st.Suppressed {
		t.Fatalf("fresh insert must default to served, got %+v", st)
	}
}

// TestScannerDeletionSnapshotsSeeSuppressedRows: the deletion-pass
// sources must stay UNFILTERED — filtering them would make the scanner
// count suppressed rows as missing-from-walk and reap them (the class
// the served/full reader split exists to prevent).
func TestScannerDeletionSnapshotsSeeSuppressedRows(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()
	ctx := context.Background()
	seedFSTrack(t, s, "a/b/suppressed.flac")
	if _, err := s.ApplyDupeStamps(ctx, []DupeStamp{
		{Path: "a/b/suppressed.flac", GroupID: "g", Tier: "same-format", Suppressed: true},
	}); err != nil {
		t.Fatal(err)
	}
	paths, err := s.TrackPaths(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range paths {
		if p == "a/b/suppressed.flac" {
			found = true
		}
	}
	if !found {
		t.Fatal("TrackPaths must include suppressed rows (deletion-pass snapshot)")
	}
	under, err := s.TrackPathsUnder(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	if len(under) != 1 {
		t.Fatalf("TrackPathsUnder must include suppressed rows, got %v", under)
	}
}

func TestDupeSummaryRoundTrip(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()
	ctx := context.Background()
	if got, err := s.LoadDupeSummary(ctx); err != nil || got != nil {
		t.Fatalf("empty store: got (%v, %v), want (nil, nil)", got, err)
	}
	in := DupeSummary{
		SchemaVersion: DupeSummarySchemaVersion,
		StampedAt:     time.Unix(1000, 0).UTC(),
		Policy:        "highest-quality",
		Scanned:       10, Groups: 2, Suppressed: 3, Served: 7,
		Tiers: []DupeTierSummary{{Tier: "same-format", Groups: 2, RedundantFiles: 3, NonLargestBytes: 42, Suppressed: 3}},
	}
	if err := s.SaveDupeSummary(ctx, in); err != nil {
		t.Fatal(err)
	}
	out, err := s.LoadDupeSummary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if out == nil || out.Suppressed != 3 || out.Policy != "highest-quality" || len(out.Tiers) != 1 {
		t.Fatalf("round-trip mismatch: %+v", out)
	}
}

// TestDupesLossyCodecsMirrorIsLossyCodec is the lockstep pin between
// internal/dupes' ranking vocabulary and manifest.IsLossyCodec (the
// single source of truth from PR #507). dupes can't import manifest
// (manifest imports dupes), so this package — the only one that sees
// both — holds the tripwire.
func TestDupesLossyCodecsMirrorIsLossyCodec(t *testing.T) {
	for _, c := range dupes.LossyCodecNames() {
		if !IsLossyCodec(c) {
			t.Errorf("dupes ranks %q lossy but manifest.IsLossyCodec disagrees", c)
		}
	}
	for _, c := range []string{"FLAC", "ALAC", "WAV", "AIFF", "DSF", "DFF"} {
		if IsLossyCodec(c) {
			t.Errorf("sanity: manifest.IsLossyCodec(%q) unexpectedly true", c)
		}
	}
}
