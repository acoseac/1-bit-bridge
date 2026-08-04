package manifest

import (
	"context"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

// missSeedRow is one seeded track plus the facets it should report short.
type missSeedRow struct {
	path       string
	trk        Track
	wantFacets []string
}

const (
	seedArtworkMBID = "11111111-1111-4111-8111-111111111111"
	seedArtistMBID  = "22222222-2222-4222-8222-222222222222"
	seedReleaseMBID = "33333333-3333-4333-8333-333333333333"
	seedLocalArt    = "local-0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

func missSeedRows() []missSeedRow {
	return []missSeedRow{
		{
			path: "Artist/Album/complete.flac",
			trk: Track{
				ArtworkMBID: seedArtworkMBID, ArtistMBID: seedArtistMBID,
				MusicBrainzAlbumID: seedReleaseMBID,
			},
			wantFacets: nil,
		},
		{
			// The #595 regression shape: local art satisfies the artwork arm
			// while the release MBID never landed.
			path: "Artist/Album/local-art-no-release.flac",
			trk: Track{
				ArtworkMBID: seedLocalArt, ArtistMBID: seedArtistMBID,
			},
			wantFacets: []string{MissFacetRelease},
		},
		{
			path: "Artist/Album/no-artwork.flac",
			trk: Track{
				ArtistMBID: seedArtistMBID, MusicBrainzAlbumID: seedReleaseMBID,
			},
			wantFacets: []string{MissFacetArtwork},
		},
		{
			path: "Artist/Album/no-artist.flac",
			trk: Track{
				ArtworkMBID: seedArtworkMBID, MusicBrainzAlbumID: seedReleaseMBID,
			},
			wantFacets: []string{MissFacetArtist},
		},
		{
			// "An Unknown Artist / CD 02" shape — nothing resolved at all.
			path: "Unknown/CD 02/track.flac",
			trk:  Track{},
			wantFacets: []string{
				MissFacetArtwork, MissFacetArtist, MissFacetRelease,
			},
		},
	}
}

func seedMissRows(t *testing.T, store *Store, rows []missSeedRow) {
	t.Helper()
	ctx := context.Background()
	for _, r := range rows {
		tr := r.trk
		tr.Path = r.path
		tr.Size = 1
		tr.ModTime = time.Now()
		if err := store.UpsertTrack(ctx, &tr); err != nil {
			t.Fatalf("UpsertTrack(%s): %v", r.path, err)
		}
		// UpsertTrack resets enriched_at to 0; stamp them done so the
		// SQL-side retry has something to re-queue.
		if err := store.MarkEnriched(ctx, &tr); err != nil {
			t.Fatalf("MarkEnriched(%s): %v", r.path, err)
		}
	}
}

// TestMissFacetsMirrorsTheMissPredicate is the lockstep guard between the
// Go-side miss classification (TrackMetaRef.MissFacets / IsMiss, which back
// the operator-facing "which tracks are short, and of what?" surfaces) and
// enrichmentMissPredicateSQL (which backs the dashboard count and the "Retry
// missing" button).
//
// It does not compare strings — it seeds real rows and asserts the two
// mechanisms select the SAME set of paths. That is the property that matters:
// a listing that disagrees with the button is how #596 happened, where the
// card read 553 while the button re-queued 10,194.
func TestMissFacetsMirrorsTheMissPredicate(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()

	rows := missSeedRows()
	seedMissRows(t, store, rows)

	// Go side: everything IsMiss() flags, via the shared projection.
	var goMisses []string
	if err := store.StreamTrackMetaRefsUnderPrefix(ctx, "", func(ref TrackMetaRef) error {
		if ref.IsMiss() {
			goMisses = append(goMisses, ref.Path)
		}
		return nil
	}); err != nil {
		t.Fatalf("StreamTrackMetaRefsUnderPrefix: %v", err)
	}
	sort.Strings(goMisses)

	// SQL side: whatever the retry statement actually re-queues. Read the
	// paths back by enriched_at, which ResetEnrichedMisses zeroes.
	n, err := store.ResetEnrichedMisses(ctx)
	if err != nil {
		t.Fatalf("ResetEnrichedMisses: %v", err)
	}
	sqlMisses := pathsWithEnrichedAtZero(t, store)
	sort.Strings(sqlMisses)

	if int64(len(sqlMisses)) != n {
		t.Fatalf("ResetEnrichedMisses reported %d rows but %d have enriched_at=0", n, len(sqlMisses))
	}
	if len(goMisses) != len(sqlMisses) {
		t.Fatalf("miss sets differ in size:\n  Go  (%d): %v\n  SQL (%d): %v",
			len(goMisses), goMisses, len(sqlMisses), sqlMisses)
	}
	for i := range goMisses {
		if goMisses[i] != sqlMisses[i] {
			t.Fatalf("miss sets diverged — the Go classification and the retry\n"+
				"predicate no longer describe the same rows.\n  Go : %v\n  SQL: %v",
				goMisses, sqlMisses)
		}
	}
}

// TestMissFacetsNamesTheRightFacet pins the per-facet attribution, which is
// the part the operator actually reads ("3,557 short a release MBID" is
// actionable; "5,435 short something" is not).
func TestMissFacetsNamesTheRightFacet(t *testing.T) {
	ctx := context.Background()
	store, err := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer store.Close()

	rows := missSeedRows()
	seedMissRows(t, store, rows)

	want := map[string][]string{}
	for _, r := range rows {
		want[r.path] = r.wantFacets
	}

	got := map[string][]string{}
	if err := store.StreamTrackMetaRefsUnderPrefix(ctx, "", func(ref TrackMetaRef) error {
		got[ref.Path] = ref.MissFacets()
		return nil
	}); err != nil {
		t.Fatalf("StreamTrackMetaRefsUnderPrefix: %v", err)
	}

	for path, wantFacets := range want {
		gotFacets, ok := got[path]
		if !ok {
			t.Fatalf("path %q never surfaced in the projection", path)
		}
		if len(gotFacets) != len(wantFacets) {
			t.Errorf("%s: facets = %v, want %v", path, gotFacets, wantFacets)
			continue
		}
		for i := range wantFacets {
			if gotFacets[i] != wantFacets[i] {
				t.Errorf("%s: facets = %v, want %v", path, gotFacets, wantFacets)
				break
			}
		}
	}
}

// pathsWithEnrichedAtZero reads back the rows the retry statement re-queued.
func pathsWithEnrichedAtZero(t *testing.T, s *Store) []string {
	t.Helper()
	rows, err := s.db.Query(`SELECT path FROM tracks WHERE enriched_at = 0`)
	if err != nil {
		t.Fatalf("query enriched_at=0: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}
