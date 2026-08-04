package admin

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// TestGetEnrichmentSnapshot pins the dashboard enrichment breakdown + the ETA
// heuristic. Tracks are seeded through the real store write paths (UpsertTrack +
// MarkEnriched) so the snapshot exercises manifest.EnrichmentBreakdown
// end-to-end, and the ETA asserts the tracks-per-album divisor (not a raw
// per-track multiply).
func TestGetEnrichmentSnapshot(t *testing.T) {
	srv, _, _ := newTestServer(t)
	ctx := context.Background()

	// 20 pending (never enriched) → drives a non-zero ETA.
	for i := 0; i < 20; i++ {
		p := fmt.Sprintf("Music/Pending/%02d.flac", i)
		if err := srv.deps.Manifest.UpsertTrack(ctx, &manifest.Track{Path: p, Size: 1, ModTime: time.Unix(1, 0)}); err != nil {
			t.Fatalf("UpsertTrack pending %q: %v", p, err)
		}
	}

	// 1 matched (enriched, nothing left to fill) + 1 missing (enriched, no
	// artwork). "Matched" needs all THREE MBIDs — the breakdown's missing count
	// is defined as exactly the set "Retry missing" re-queues, so an artwork-only
	// row is still a gap.
	matched := &manifest.Track{
		Path: "Music/M/matched.flac", Size: 1, ModTime: time.Unix(1, 0),
		ArtworkMBID:        "12aae8a7-e814-4c46-94d7-5c9e053bda5b",
		ArtistMBID:         "22222222-2222-4222-8222-222222222222",
		MusicBrainzAlbumID: "33333333-3333-4333-8333-333333333333",
	}
	if err := srv.deps.Manifest.UpsertTrack(ctx, matched); err != nil {
		t.Fatalf("UpsertTrack matched: %v", err)
	}
	if err := srv.deps.Manifest.MarkEnriched(ctx, matched); err != nil {
		t.Fatalf("MarkEnriched matched: %v", err)
	}
	missing := &manifest.Track{Path: "Music/G/missing.flac", Size: 1, ModTime: time.Unix(1, 0)}
	if err := srv.deps.Manifest.UpsertTrack(ctx, missing); err != nil {
		t.Fatalf("UpsertTrack missing: %v", err)
	}
	if err := srv.deps.Manifest.MarkEnriched(ctx, missing); err != nil {
		t.Fatalf("MarkEnriched missing: %v", err)
	}

	snap := srv.getEnrichmentSnapshot()
	if snap.Pending != 20 || snap.Matched != 1 || snap.Missing != 1 {
		t.Fatalf("snapshot = (pending=%d matched=%d missing=%d), want (20,1,1)", snap.Pending, snap.Matched, snap.Missing)
	}
	// round((20 / avgTracksPerAlbum) * enrichPaceSeconds) = round((20/10)*1.1) = round(2.2) = 2.
	if snap.EtaSecondsEstimate != 2 {
		t.Errorf("EtaSecondsEstimate = %d, want 2 (round(20/10*1.1))", snap.EtaSecondsEstimate)
	}
	if snap.LastEnrichedAt == nil {
		t.Error("LastEnrichedAt = nil, want non-nil (two tracks enriched)")
	}
}
