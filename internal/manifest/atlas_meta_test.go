package manifest

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func openAtlasTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestReleaseAtlasMetaRoundTrip(t *testing.T) {
	s := openAtlasTestStore(t)
	ctx := context.Background()
	const mbid = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	t0 := int64(1_700_000_000_000_000_000)
	s.now = func() time.Time { return time.Unix(0, t0) }

	in := ReleaseAtlasMeta{
		ReleaseMBID: mbid, Found: true,
		Description: "A great album.", RecordLabel: "Blue Note",
		Genres: []string{"Jazz", "Bebop"}, AtlasETag: "etag1",
		Source: "bandcamp", SourceURL: "https://x.bandcamp.com/album/y",
	}
	if err := s.UpsertReleaseAtlasMeta(ctx, in); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetReleaseAtlasMeta(ctx, mbid)
	if err != nil || got == nil {
		t.Fatalf("get = (%v, %v)", got, err)
	}
	if !got.Found || got.Description != in.Description || got.RecordLabel != in.RecordLabel || got.AtlasETag != "etag1" {
		t.Errorf("round-trip mismatch: %+v", got)
	}
	if got.Source != "bandcamp" || got.SourceURL != "https://x.bandcamp.com/album/y" {
		t.Errorf("attribution round-trip = (%q, %q), want (bandcamp, …)", got.Source, got.SourceURL)
	}
	if len(got.Genres) != 2 || got.Genres[0] != "Jazz" || got.Genres[1] != "Bebop" {
		t.Errorf("genres = %v", got.Genres)
	}
	if got.IngestedAt.UnixNano() != t0 {
		t.Errorf("ingestedAt = %d, want bridge-stamped %d", got.IngestedAt.UnixNano(), t0)
	}
}

func TestReleaseAtlasMetaUpsertRestampsIngestedAt(t *testing.T) {
	s := openAtlasTestStore(t)
	ctx := context.Background()
	const mbid = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	s.now = func() time.Time { return time.Unix(0, 1000) }
	_ = s.UpsertReleaseAtlasMeta(ctx, ReleaseAtlasMeta{ReleaseMBID: mbid, Found: true, Description: "v1"})
	s.now = func() time.Time { return time.Unix(0, 2000) }
	_ = s.UpsertReleaseAtlasMeta(ctx, ReleaseAtlasMeta{ReleaseMBID: mbid, Found: true, Description: "v2"})
	got, _ := s.GetReleaseAtlasMeta(ctx, mbid)
	if got.Description != "v2" {
		t.Errorf("description = %q, want v2 (upsert replaced)", got.Description)
	}
	if got.IngestedAt.UnixNano() != 2000 {
		t.Errorf("ingestedAt = %d, want 2000 (re-stamped on upsert)", got.IngestedAt.UnixNano())
	}
}

func TestReleaseAtlasMetaTombstone(t *testing.T) {
	s := openAtlasTestStore(t)
	ctx := context.Background()
	const mbid = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	if err := s.UpsertReleaseAtlasMeta(ctx, ReleaseAtlasMeta{ReleaseMBID: mbid, Found: false}); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetReleaseAtlasMeta(ctx, mbid)
	if err != nil || got == nil {
		t.Fatalf("tombstone get = (%v, %v); want a non-nil row with Found=false", got, err)
	}
	if got.Found {
		t.Error("tombstone row has Found=true, want false")
	}
}

func TestReleaseAtlasMetaAbsentReturnsNil(t *testing.T) {
	s := openAtlasTestStore(t)
	got, err := s.GetReleaseAtlasMeta(context.Background(), "dddddddd-dddd-4ddd-8ddd-dddddddddddd")
	if err != nil || got != nil {
		t.Errorf("absent get = (%v, %v), want (nil, nil)", got, err)
	}
}

func TestArtistAtlasMetaRoundTrip(t *testing.T) {
	s := openAtlasTestStore(t)
	ctx := context.Background()
	const mbid = "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"
	in := ArtistAtlasMeta{ArtistMBID: mbid, Found: true, Bio: "Long bio.", BioSummary: "Short.", Genres: []string{"Rock"}, Source: "wiki", SourceURL: "https://en.wikipedia.org/wiki/Z"}
	if err := s.UpsertArtistAtlasMeta(ctx, in); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetArtistAtlasMeta(ctx, mbid)
	if err != nil || got == nil || got.Bio != "Long bio." || got.BioSummary != "Short." || len(got.Genres) != 1 {
		t.Errorf("artist round-trip mismatch: %+v (err %v)", got, err)
	}
	if got.Source != "wiki" || got.SourceURL != "https://en.wikipedia.org/wiki/Z" {
		t.Errorf("attribution round-trip = (%q, %q), want (wiki, …)", got.Source, got.SourceURL)
	}
}
