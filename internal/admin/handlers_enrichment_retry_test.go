package admin

import (
	"context"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// TestDeriveEnrichSource pins the Settings picker's config-derivation rule,
// including the trailing-slash defense (env-var overrides reach the live
// config unnormalized).
func TestDeriveEnrichSource(t *testing.T) {
	cases := []struct {
		name, mb, ca     string
		wantSrc, wantURL string
	}{
		{"both empty → public", "", "", "musicbrainz", ""},
		{"atlas shape", "https://atlas.test/ws/2", "https://atlas.test", "atlas", "https://atlas.test"},
		{"atlas with trailing slashes", "https://atlas.test/ws/2/", "https://atlas.test/", "atlas", "https://atlas.test"},
		{"mismatched hosts → custom", "https://mb.test/ws/2", "https://caa.test", "custom", ""},
		{"MB only → custom", "https://mb.test/ws/2", "", "custom", ""},
		{"CoverArt only → custom", "", "https://caa.test", "custom", ""},
		{"non-ws2 MB path → custom", "https://atlas.test/api", "https://atlas.test", "custom", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src, url := deriveEnrichSource(tc.mb, tc.ca)
			if src != tc.wantSrc || url != tc.wantURL {
				t.Errorf("deriveEnrichSource(%q, %q) = (%q, %q), want (%q, %q)",
					tc.mb, tc.ca, src, url, tc.wantSrc, tc.wantURL)
			}
		})
	}
}

// TestAPIEnrichmentRetry pins the "Retry missing" handler: gap rows are
// re-queued, the harvest nudge closure fires, and a second click inside the
// rate window is refused with 429.
func TestAPIEnrichmentRetry(t *testing.T) {
	srv, _, _ := newTestServer(t)
	ctx := context.Background()

	const (
		artMBID = "11111111-1111-4111-8111-111111111111"
		relMBID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	)
	// "Complete" means all THREE MBIDs — the store's miss predicate gained a
	// release-MBID arm (manifest.enrichmentMissPredicateSQL), so artwork +
	// artist alone still leaves the row missing the release that the Atlas
	// description / booklet / premium cover key on.
	complete := &manifest.Track{Path: "A/complete.flac", Size: 1, ModTime: time.Unix(1, 0),
		ArtworkMBID: relMBID, ArtistMBID: artMBID, MusicBrainzAlbumID: relMBID}
	gap := &manifest.Track{Path: "B/gap.flac", Size: 1, ModTime: time.Unix(1, 0),
		ArtistMBID: artMBID, MusicBrainzAlbumID: relMBID}
	for _, tr := range []*manifest.Track{complete, gap} {
		if err := srv.deps.Manifest.UpsertTrack(ctx, tr); err != nil {
			t.Fatal(err)
		}
		if err := srv.deps.Manifest.MarkEnriched(ctx, tr); err != nil {
			t.Fatal(err)
		}
	}
	harvestNudged := false
	srv.deps.HarvestForceSubmit = func() bool { harvestNudged = true; return true }

	// Warm the breakdown cache BEFORE retrying. Without this the cache is
	// empty, so the handler recomputes regardless and the assertions below
	// would pass even with the invalidation removed — the stale-snapshot path
	// is the entire reason that invalidation exists, so it has to be the path
	// under test.
	warm := srv.getEnrichmentSnapshot()
	if warm.Pending != 0 {
		t.Fatalf("pre-retry pending = %d, want 0 (both seeded rows are enriched)", warm.Pending)
	}

	var resp enrichmentRetryResponse
	code := doJSON(t, srv.Handler(), "POST", "/api/enrichment/retry", nil, &resp)
	if code != 200 {
		t.Fatalf("retry: status %d, want 200", code)
	}
	if resp.ResetTracks != 1 {
		t.Errorf("resetTracks = %d, want 1 (only the artwork-gap row)", resp.ResetTracks)
	}
	if !resp.HarvestResubmitted || !harvestNudged {
		t.Errorf("harvest nudge = (resp=%v, called=%v), want both true", resp.HarvestResubmitted, harvestNudged)
	}

	// The ack carries the POST-reset snapshot so the card can repaint in one
	// trip. The enrichment SSE event rides the 30s slow ticker, so without this
	// the panel shows "0 tracks in the queue · all caught up" for up to half a
	// minute after a click that just queued work — which reads as the button
	// having done nothing.
	if resp.Enrichment == nil {
		t.Fatal("response carried no enrichment snapshot; the card cannot repaint until the next 30s tick")
	}
	if resp.Enrichment.Pending != 1 {
		t.Errorf("snapshot pending = %d, want 1 — it must reflect the row the retry just re-queued, "+
			"not the pre-reset cache", resp.Enrichment.Pending)
	}
	if resp.Enrichment.Missing != 0 {
		t.Errorf("snapshot missing = %d, want 0 (the gap row moved to pending)", resp.Enrichment.Missing)
	}

	// Immediate second click → refused by the rate guard.
	code = doJSON(t, srv.Handler(), "POST", "/api/enrichment/retry", nil, nil)
	if code != 429 {
		t.Errorf("second retry: status %d, want 429", code)
	}
}

// TestAPIEnrichmentRetryNilHarvest pins the nil-closure shape:
// harvestResubmitted comes back false, the reset still runs.
func TestAPIEnrichmentRetryNilHarvest(t *testing.T) {
	srv, _, _ := newTestServer(t)
	var resp enrichmentRetryResponse
	code := doJSON(t, srv.Handler(), "POST", "/api/enrichment/retry", nil, &resp)
	if code != 200 {
		t.Fatalf("retry: status %d, want 200", code)
	}
	if resp.HarvestResubmitted {
		t.Error("harvestResubmitted = true with no closure wired, want false")
	}
}

// TestEnrichmentSnapshotExtendedFields pins the composed card payload: the
// config-derived source label, the bios/descriptions coverage from the
// atlas-meta tables, and the artist-image coverage from the injected
// file-set closure. Facets with an empty universe stay nil (omitted).
func TestEnrichmentSnapshotExtendedFields(t *testing.T) {
	srv, _, _ := newTestServer(t)
	ctx := context.Background()

	const (
		artMBID = "11111111-1111-4111-8111-111111111111"
		art2    = "22222222-2222-4222-8222-222222222222"
		relMBID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	)
	tr := &manifest.Track{Path: "A/a.flac", Size: 1, ModTime: time.Unix(1, 0),
		ArtistMBID: artMBID, ArtworkMBID: relMBID}
	tr2 := &manifest.Track{Path: "B/b.flac", Size: 1, ModTime: time.Unix(1, 0),
		ArtistMBID: art2}
	for _, x := range []*manifest.Track{tr, tr2} {
		if err := srv.deps.Manifest.UpsertTrack(ctx, x); err != nil {
			t.Fatal(err)
		}
	}
	if err := srv.deps.Manifest.UpsertArtistAtlasMeta(ctx, manifest.ArtistAtlasMeta{ArtistMBID: artMBID, Found: true, Bio: "b"}); err != nil {
		t.Fatal(err)
	}
	// Artist-image closure: only artMBID has a cached image.
	srv.deps.ArtistImageMBIDs = func() (map[string]struct{}, error) {
		return map[string]struct{}{artMBID: {}}, nil
	}

	snap := srv.getEnrichmentSnapshot()
	if snap.Source != "musicbrainz" {
		t.Errorf("source = %q, want musicbrainz (no enrich URLs configured)", snap.Source)
	}
	if snap.ArtistBios == nil || snap.ArtistBios.Have != 1 || snap.ArtistBios.Missing != 1 {
		t.Errorf("artistBios = %+v, want have=1 missing=1", snap.ArtistBios)
	}
	if snap.AlbumDescriptions == nil || snap.AlbumDescriptions.Have != 0 || snap.AlbumDescriptions.Missing != 1 {
		t.Errorf("albumDescriptions = %+v, want have=0 missing=1", snap.AlbumDescriptions)
	}
	if snap.ArtistImages == nil || snap.ArtistImages.Have != 1 || snap.ArtistImages.Missing != 1 {
		t.Errorf("artistImages = %+v, want have=1 missing=1", snap.ArtistImages)
	}
}

// TestEnrichmentSnapshotOmitsEmptyFacets pins the fresh-library shape: no
// MBIDs anywhere → every coverage facet is nil so the card renders no
// zero-noise rows.
func TestEnrichmentSnapshotOmitsEmptyFacets(t *testing.T) {
	srv, _, _ := newTestServer(t)
	snap := srv.getEnrichmentSnapshot()
	if snap.ArtistBios != nil || snap.AlbumDescriptions != nil || snap.ArtistImages != nil {
		t.Errorf("empty library facets = (%+v, %+v, %+v), want all nil",
			snap.ArtistBios, snap.AlbumDescriptions, snap.ArtistImages)
	}
	if snap.Source != "musicbrainz" {
		t.Errorf("source = %q, want musicbrainz", snap.Source)
	}
}
