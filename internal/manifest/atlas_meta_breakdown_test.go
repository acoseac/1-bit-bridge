package manifest

import (
	"context"
	"testing"
	"time"
)

// TestAtlasMetaBreakdownCounts pins the coverage-summary CTE: tombstones
// (found=0) count as missing, `local-` artwork sentinels are excluded from
// the release universe, and the artworkMBID ∪ musicBrainzAlbumID union
// dedupes a release reachable through both fields.
func TestAtlasMetaBreakdownCounts(t *testing.T) {
	s := openAtlasTestStore(t)
	ctx := context.Background()

	const (
		artistWithBio = "11111111-1111-4111-8111-111111111111"
		artistNoBio   = "22222222-2222-4222-8222-222222222222"
		relWithDesc   = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
		relNoDesc     = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
		localArt      = "local-0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	)

	seed := []*Track{
		// Release reachable via BOTH artworkMBID and musicBrainzAlbumID —
		// the union must count it once.
		{Path: "A/dup1.flac", Size: 1, ModTime: time.Unix(1, 0),
			ArtistMBID: artistWithBio, ArtworkMBID: relWithDesc},
		{Path: "A/dup2.flac", Size: 1, ModTime: time.Unix(1, 0),
			ArtistMBID: artistWithBio, MusicBrainzAlbumID: relWithDesc},
		// Local-curated artwork sentinel must NOT enter the release
		// universe; the release still counts via musicBrainzAlbumID.
		{Path: "B/local.flac", Size: 1, ModTime: time.Unix(1, 0),
			ArtistMBID: artistNoBio, ArtworkMBID: localArt, MusicBrainzAlbumID: relNoDesc},
		// No MBIDs at all — contributes to neither universe.
		{Path: "C/bare.flac", Size: 1, ModTime: time.Unix(1, 0)},
	}
	for _, tr := range seed {
		if err := s.UpsertTrack(ctx, tr); err != nil {
			t.Fatalf("UpsertTrack %q: %v", tr.Path, err)
		}
	}

	// Bio present for one artist; explicit tombstone for the other (must
	// count as missing, not found).
	if err := s.UpsertArtistAtlasMeta(ctx, ArtistAtlasMeta{ArtistMBID: artistWithBio, Found: true, Bio: "a bio"}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertArtistAtlasMeta(ctx, ArtistAtlasMeta{ArtistMBID: artistNoBio, Found: false}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertReleaseAtlasMeta(ctx, ReleaseAtlasMeta{ReleaseMBID: relWithDesc, Found: true, Description: "desc"}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertReleaseAtlasMeta(ctx, ReleaseAtlasMeta{ReleaseMBID: relNoDesc, Found: false}); err != nil {
		t.Fatal(err)
	}

	b, err := s.AtlasMetaBreakdownCounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if b.ArtistsTotal != 2 || b.ArtistBiosFound != 1 {
		t.Errorf("artists = (total=%d found=%d), want (2, 1)", b.ArtistsTotal, b.ArtistBiosFound)
	}
	if b.ReleasesTotal != 2 || b.ReleaseDescsFound != 1 {
		t.Errorf("releases = (total=%d found=%d), want (2, 1) — union must dedupe and exclude local-", b.ReleasesTotal, b.ReleaseDescsFound)
	}
}

// TestAtlasMetaBreakdownCountsEmptyLibrary pins the zero shape (fresh DB).
func TestAtlasMetaBreakdownCountsEmptyLibrary(t *testing.T) {
	s := openAtlasTestStore(t)
	b, err := s.AtlasMetaBreakdownCounts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if b != (AtlasMetaBreakdown{}) {
		t.Errorf("empty library breakdown = %+v, want all zeros", b)
	}
}
