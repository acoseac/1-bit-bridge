package manifest

import (
	"context"
	"testing"
	"time"
)

// TestResetEnrichedMisses pins the "Retry missing" reset: only ENRICHED rows
// missing artwork OR an artist match are re-queued (enriched_at → 0);
// complete rows and never-enriched rows are untouched; a second call is a
// no-op.
func TestResetEnrichedMisses(t *testing.T) {
	s := openAtlasTestStore(t)
	ctx := context.Background()

	const (
		artMBID = "11111111-1111-4111-8111-111111111111"
		relMBID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	)
	upsert := func(tr *Track, enrich bool) {
		t.Helper()
		if err := s.UpsertTrack(ctx, tr); err != nil {
			t.Fatalf("UpsertTrack %q: %v", tr.Path, err)
		}
		if enrich {
			if err := s.MarkEnriched(ctx, tr); err != nil {
				t.Fatalf("MarkEnriched %q: %v", tr.Path, err)
			}
		}
	}

	upsert(&Track{Path: "A/complete.flac", Size: 1, ModTime: time.Unix(1, 0),
		ArtworkMBID: relMBID, ArtistMBID: artMBID}, true)
	upsert(&Track{Path: "B/no-artwork.flac", Size: 1, ModTime: time.Unix(1, 0),
		ArtistMBID: artMBID}, true)
	upsert(&Track{Path: "C/no-artist.flac", Size: 1, ModTime: time.Unix(1, 0),
		ArtworkMBID: relMBID}, true)
	upsert(&Track{Path: "D/pending.flac", Size: 1, ModTime: time.Unix(1, 0)}, false)
	// Explicit-empty MBID regression (CodeRabbit on PR #495): omitempty means
	// UpsertTrack never writes "", so plant it with json_set directly — the
	// COALESCE predicate must treat it as missing like JSON-null/absent.
	upsert(&Track{Path: "E/empty-mbid.flac", Size: 1, ModTime: time.Unix(1, 0),
		ArtworkMBID: relMBID, ArtistMBID: artMBID}, true)
	if _, err := s.db.ExecContext(ctx,
		`UPDATE tracks SET tags_json = json_set(tags_json, '$.artworkMBID', '') WHERE path = 'E/empty-mbid.flac'`); err != nil {
		t.Fatal(err)
	}

	n, err := s.ResetEnrichedMisses(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("reset = %d rows, want 3 (no-artwork + no-artist + empty-string artwork)", n)
	}
	pending, _, _, _, err := s.EnrichmentBreakdown(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pending != 4 {
		t.Errorf("pending after reset = %d, want 4 (3 reset + 1 already pending)", pending)
	}

	// Idempotent: the reset rows are now pending (enriched_at = 0), so a
	// second sweep matches nothing.
	n2, err := s.ResetEnrichedMisses(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n2 != 0 {
		t.Errorf("second reset = %d rows, want 0", n2)
	}
}

// TestResetEnrichedByArtistMBIDs pins the artist-image retry overload: only
// enriched rows whose artistMBID is in the given set are re-queued.
func TestResetEnrichedByArtistMBIDs(t *testing.T) {
	s := openAtlasTestStore(t)
	ctx := context.Background()

	const (
		artistA = "11111111-1111-4111-8111-111111111111"
		artistB = "22222222-2222-4222-8222-222222222222"
		relMBID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	)
	for _, tr := range []*Track{
		{Path: "A/a.flac", Size: 1, ModTime: time.Unix(1, 0), ArtistMBID: artistA, ArtworkMBID: relMBID},
		{Path: "B/b.flac", Size: 1, ModTime: time.Unix(1, 0), ArtistMBID: artistB, ArtworkMBID: relMBID},
	} {
		if err := s.UpsertTrack(ctx, tr); err != nil {
			t.Fatal(err)
		}
		if err := s.MarkEnriched(ctx, tr); err != nil {
			t.Fatal(err)
		}
	}

	if n, err := s.ResetEnrichedByArtistMBIDs(ctx, nil); err != nil || n != 0 {
		t.Fatalf("empty set reset = (%d, %v), want (0, nil)", n, err)
	}
	n, err := s.ResetEnrichedByArtistMBIDs(ctx, []string{artistA})
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("reset = %d rows, want 1 (artistA only)", n)
	}
	pending, _, _, _, err := s.EnrichmentBreakdown(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pending != 1 {
		t.Errorf("pending = %d, want 1 (artistB untouched)", pending)
	}
}
