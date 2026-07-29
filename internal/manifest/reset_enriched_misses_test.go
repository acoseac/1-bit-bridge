package manifest

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestEnrichmentMissPredicateIsShared is what keeps the library-wide and
// folder-scoped "Retry missing" statements in step.
//
// The predicate is written out verbatim in both rather than concatenated in
// (see enrichmentMissPredicateSQL), so nothing at the language level stops the
// two from drifting — this test does. Drift is not hypothetical: the sibling
// Atlas search plans drifted exactly this way and cost a measured 18 points of
// album recall before anyone noticed.
func TestEnrichmentMissPredicateIsShared(t *testing.T) {
	for name, stmt := range map[string]string{
		"resetEnrichedMissesSQL":            resetEnrichedMissesSQL,
		"resetEnrichedMissesUnderPrefixSQL": resetEnrichedMissesUnderPrefixSQL,
		// The dashboard card counts `matched` as the NEGATION of this
		// predicate, so it has to embed the same text — otherwise the number
		// the operator reads stops describing what the button does.
		"enrichmentBreakdownSQL": enrichmentBreakdownSQL,
	} {
		// Whitespace-normalised: the three statements nest the predicate at
		// different depths, and forcing identical indentation on all of them
		// would couple the guard to layout rather than to meaning. Any
		// SEMANTIC edit — a dropped arm, a renamed tag path — still fails.
		if !strings.Contains(squashSpace(stmt), squashSpace(enrichmentMissPredicateSQL)) {
			t.Errorf("%s no longer embeds the shared miss-predicate — the retry statements\n"+
				"and the dashboard breakdown have drifted apart.\nStatement:\n%s\n\nwant it to contain:\n%s",
				name, stmt, enrichmentMissPredicateSQL)
		}
	}
}

// squashSpace collapses every run of whitespace to a single space so SQL
// fragments compare on content rather than indentation.
func squashSpace(s string) string { return strings.Join(strings.Fields(s), " ") }

// TestResetEnrichedMissesCoversAlbumMBID pins the third arm of the "Retry
// missing" predicate.
//
// artworkMBID also carries the scanner's `local-<sha256>` sentinel for embedded
// APIC / folder.jpg art, so a track whose album never resolved on MusicBrainz
// but which HAS local cover art reads as "not missing" on the artwork arm —
// while having no release MBID at all, and therefore no Atlas description,
// label, genres, booklet or premium cover, all of which key on it.
//
// Measured on the production bridge when this arm was added: 8,945 of 19,482
// tracks had no album MBID, and 6,801 of those — every one via a local-
// sentinel — were invisible to the old two-arm predicate.
func TestResetEnrichedMissesCoversAlbumMBID(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()

	rows := []struct {
		path string
		trk  Track
		want bool // should the retry re-queue it?
	}{
		{
			path: "complete.flac",
			trk: Track{
				ArtworkMBID: "11111111-1111-4111-8111-111111111111",
				ArtistMBID:  "22222222-2222-4222-8222-222222222222", MusicBrainzAlbumID: "33333333-3333-4333-8333-333333333333",
			},
			want: false, // nothing missing
		},
		{
			path: "local-art-no-album.flac",
			trk: Track{
				// The regression case: local artwork satisfies the artwork arm,
				// artist resolved, but the release MBID never landed.
				ArtworkMBID: "local-" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
				ArtistMBID:  "22222222-2222-4222-8222-222222222222",
			},
			want: true,
		},
		{
			path: "no-artwork.flac",
			trk: Track{
				ArtistMBID: "22222222-2222-4222-8222-222222222222", MusicBrainzAlbumID: "33333333-3333-4333-8333-333333333333",
			},
			want: true,
		},
		{
			path: "no-artist.flac",
			trk: Track{
				ArtworkMBID: "11111111-1111-4111-8111-111111111111", MusicBrainzAlbumID: "33333333-3333-4333-8333-333333333333",
			},
			want: true,
		},
	}

	for _, r := range rows {
		tr := r.trk
		tr.Path = r.path
		tr.Size = 1
		tr.ModTime = time.Now()
		if err := store.UpsertTrack(ctx, &tr); err != nil {
			t.Fatalf("UpsertTrack(%s): %v", r.path, err)
		}
		// UpsertTrack resets enriched_at to 0; stamp them "done" so the retry
		// has something to re-queue.
		if err := store.MarkEnriched(ctx, &tr); err != nil {
			t.Fatalf("MarkEnriched(%s): %v", r.path, err)
		}
	}

	// Capture indexed_at so we can assert the reset does not bump it.
	before := map[string]int64{}
	wantN := int64(0)
	for _, r := range rows {
		before[r.path] = indexedAtFor(t, store, r.path)
		if r.want {
			wantN++
		}
	}

	n, err := store.ResetEnrichedMisses(ctx)
	if err != nil {
		t.Fatalf("ResetEnrichedMisses: %v", err)
	}
	if n != wantN {
		t.Errorf("ResetEnrichedMisses re-queued %d rows, want %d", n, wantN)
	}

	for _, r := range rows {
		t.Run(r.path, func(t *testing.T) {
			assertRequeued(t, store, r.path, r.want)
			// indexed_at must not move — nothing about the row's content
			// changed yet; MarkEnriched bumps it when the retry lands data.
			if after := indexedAtFor(t, store, r.path); after != before[r.path] {
				t.Errorf("indexed_at moved %d -> %d; the reset must not bump it (iOS delta churn)",
					before[r.path], after)
			}
		})
	}
}

// assertRequeued checks whether ResetEnrichedMisses re-queued path, against
// whether it should have.
func assertRequeued(t *testing.T, s *Store, path string, want bool) {
	t.Helper()
	got := enrichedAtFor(t, s, path)
	switch {
	case want && got != 0:
		t.Errorf("enriched_at = %d, want 0 (should have been re-queued)", got)
	case !want && got == 0:
		t.Errorf("enriched_at = 0, want it left alone (nothing was missing)")
	}
}

func enrichedAtFor(t *testing.T, s *Store, path string) int64 {
	t.Helper()
	var v int64
	if err := s.db.QueryRow(`SELECT enriched_at FROM tracks WHERE path = ?`, path).Scan(&v); err != nil {
		t.Fatalf("read enriched_at(%s): %v", path, err)
	}
	return v
}

func indexedAtFor(t *testing.T, s *Store, path string) int64 {
	t.Helper()
	var v int64
	if err := s.db.QueryRow(`SELECT indexed_at FROM tracks WHERE path = ?`, path).Scan(&v); err != nil {
		t.Fatalf("read indexed_at(%s): %v", path, err)
	}
	return v
}

// TestEnrichmentBreakdownMissingEqualsRetryScope is the behavioural half of the
// card↔button contract: whatever the dashboard reports as "missing" must be
// exactly the row count "Retry missing" re-queues.
//
// They used to disagree badly. `matched` counted any non-NULL `$.artworkMBID`,
// so a `local-<sha256>` sentinel from embedded art counted as complete even
// with no release MBID at all — on the production library the card read 553
// missing while the retry re-queued 10,194.
func TestEnrichmentBreakdownMissingEqualsRetryScope(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()

	const (
		art    = "11111111-1111-4111-8111-111111111111"
		artist = "22222222-2222-4222-8222-222222222222"
		rel    = "33333333-3333-4333-8333-333333333333"
		local  = "local-0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	)
	seed := []struct {
		path string
		trk  Track
		done bool
	}{
		{"complete.flac", Track{ArtworkMBID: art, ArtistMBID: artist, MusicBrainzAlbumID: rel}, true},
		// The blind spot: complete-looking on the old artwork-only test.
		{"local-no-release.flac", Track{ArtworkMBID: local, ArtistMBID: artist}, true},
		{"no-artist.flac", Track{ArtworkMBID: art, MusicBrainzAlbumID: rel}, true},
		{"no-artwork.flac", Track{ArtistMBID: artist, MusicBrainzAlbumID: rel}, true},
		{"never-enriched.flac", Track{}, false}, // pending, not missing
	}
	for _, s := range seed {
		tr := s.trk
		tr.Path, tr.Size, tr.ModTime = s.path, 1, time.Now()
		if err := store.UpsertTrack(ctx, &tr); err != nil {
			t.Fatalf("UpsertTrack(%s): %v", s.path, err)
		}
		if s.done {
			if err := store.MarkEnriched(ctx, &tr); err != nil {
				t.Fatalf("MarkEnriched(%s): %v", s.path, err)
			}
		}
	}

	pending, matched, missing, _, err := store.EnrichmentBreakdown(ctx)
	if err != nil {
		t.Fatalf("EnrichmentBreakdown: %v", err)
	}
	if pending != 1 {
		t.Errorf("pending = %d, want 1", pending)
	}
	if matched != 1 {
		t.Errorf("matched = %d, want 1 (only the all-three-MBID row is complete)", matched)
	}
	if missing != 3 {
		t.Errorf("missing = %d, want 3 (local-no-release + no-artist + no-artwork)", missing)
	}

	// The equality that matters: the reset re-queues precisely `missing` rows.
	requeued, err := store.ResetEnrichedMisses(ctx)
	if err != nil {
		t.Fatalf("ResetEnrichedMisses: %v", err)
	}
	if int(requeued) != missing {
		t.Errorf("card says %d missing but the retry re-queued %d — the dashboard number "+
			"no longer predicts what the button does", missing, requeued)
	}
}
