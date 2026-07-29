package manifest

import (
	"context"
	"testing"
)

// seedMetaTrack upserts a track carrying the given MBID refs.
func seedMetaTrack(t *testing.T, s *Store, path, artworkMBID, artistMBID, releaseMBID string) {
	t.Helper()
	if err := s.UpsertTrack(context.Background(), &Track{
		Path:               path,
		Size:               100,
		ArtworkMBID:        artworkMBID,
		ArtistMBID:         artistMBID,
		MusicBrainzAlbumID: releaseMBID,
	}); err != nil {
		t.Fatalf("UpsertTrack %q: %v", path, err)
	}
}

const (
	uuidA = "11111111-1111-1111-1111-111111111111"
	uuidB = "22222222-2222-2222-2222-222222222222"
	uuidC = "33333333-3333-3333-3333-333333333333"
)

// TestStreamTrackMetaRefsUnderPrefix pins the projection: prefix
// scoping, whole-library on empty prefix, local-<sha> artwork refs
// passing through, and the artwork_version column splice.
func TestStreamTrackMetaRefsUnderPrefix(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })

	local := "local-0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	seedMetaTrack(t, s, "ArtistX/Album1/01.flac", uuidA, uuidB, uuidA)
	seedMetaTrack(t, s, "ArtistX/Album1/02.flac", local, uuidB, "")
	seedMetaTrack(t, s, "ArtistY/Album2/01.flac", uuidC, "", uuidC)
	if _, err := s.db.Exec(
		`UPDATE tracks SET artwork_version = 'v7' WHERE path = ?`,
		"ArtistX/Album1/01.flac"); err != nil {
		t.Fatalf("set artwork_version: %v", err)
	}

	collect := func(prefix string) map[string]TrackMetaRef {
		t.Helper()
		out := map[string]TrackMetaRef{}
		if err := s.StreamTrackMetaRefsUnderPrefix(context.Background(), prefix, func(r TrackMetaRef) error {
			out[r.Path] = r
			return nil
		}); err != nil {
			t.Fatalf("stream %q: %v", prefix, err)
		}
		return out
	}

	all := collect("")
	if len(all) != 3 {
		t.Fatalf("root stream = %d rows, want 3", len(all))
	}
	if got := all["ArtistX/Album1/01.flac"]; got.ArtworkMBID != uuidA ||
		got.ArtistMBID != uuidB || got.ReleaseMBID != uuidA || got.ArtworkVersion != "v7" {
		t.Errorf("ref = %+v, want artwork=%s artist=%s release=%s version=v7", got, uuidA, uuidB, uuidA)
	}
	if got := all["ArtistX/Album1/02.flac"]; got.ArtworkMBID != local {
		t.Errorf("local- artwork ref = %q, want passthrough %q", got.ArtworkMBID, local)
	}

	scoped := collect("ArtistX")
	if len(scoped) != 2 {
		t.Errorf("scoped stream = %d rows, want 2", len(scoped))
	}
	if _, leaked := scoped["ArtistY/Album2/01.flac"]; leaked {
		t.Errorf("scoped stream leaked an out-of-prefix row")
	}
}

// TestDistinctMBIDsUnderPrefix pins both scoped enumerators: artist
// dedup + scope; release = UNION of UUID artworkMBIDs and
// musicBrainzAlbumIDs with local- excluded.
func TestDistinctMBIDsUnderPrefix(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })

	local := "local-0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	seedMetaTrack(t, s, "ArtistX/Album1/01.flac", uuidA, uuidB, uuidA)
	seedMetaTrack(t, s, "ArtistX/Album1/02.flac", local, uuidB, uuidC) // local cover, release via albumID
	seedMetaTrack(t, s, "ArtistY/Album2/01.flac", uuidC, uuidA, "")

	artists, err := s.DistinctArtistMBIDsUnderPrefix(context.Background(), "ArtistX")
	if err != nil {
		t.Fatalf("artists: %v", err)
	}
	if len(artists) != 1 || artists[0] != uuidB {
		t.Errorf("artists = %v, want [%s]", artists, uuidB)
	}

	releases, err := s.DistinctReleaseMBIDsUnderPrefix(context.Background(), "ArtistX")
	if err != nil {
		t.Fatalf("releases: %v", err)
	}
	got := map[string]bool{}
	for _, m := range releases {
		got[m] = true
	}
	if len(got) != 2 || !got[uuidA] || !got[uuidC] {
		t.Errorf("releases = %v, want {%s, %s} (UNION, local- excluded)", releases, uuidA, uuidC)
	}

	// Whole library: artworkMBIDs {A, C} ∪ albumIDs {A, C} = 2 distinct
	// (uuidB is an ARTIST mbid and must not appear here).
	allReleases, err := s.DistinctReleaseMBIDsUnderPrefix(context.Background(), "")
	if err != nil {
		t.Fatalf("releases(all): %v", err)
	}
	gotAll := map[string]bool{}
	for _, m := range allReleases {
		gotAll[m] = true
	}
	if len(gotAll) != 2 || !gotAll[uuidA] || !gotAll[uuidC] || gotAll[uuidB] {
		t.Errorf("whole-library releases = %v, want exactly {%s, %s}", allReleases, uuidA, uuidC)
	}
}

// TestResetEnrichedMissesUnderPrefix pins the scoped sanctioned
// writer: only enriched-but-incomplete rows UNDER the prefix reset;
// complete rows and out-of-prefix rows stay; indexed_at never moves.
func TestResetEnrichedMissesUnderPrefix(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })

	seedMetaTrack(t, s, "ArtistX/Album1/01.flac", "", "", "") // incomplete, in prefix
	// Complete means all THREE MBIDs — the predicate gained a release-MBID arm
	// (see enrichmentMissPredicateSQL), so a row with artwork + artist but no
	// release MBID is genuinely still missing something.
	seedMetaTrack(t, s, "ArtistX/Album1/02.flac", uuidA, uuidB, uuidC) // complete, in prefix
	seedMetaTrack(t, s, "ArtistY/Album2/01.flac", "", "", "")          // incomplete, OUT of prefix
	if _, err := s.db.Exec(`UPDATE tracks SET enriched_at = 5`); err != nil {
		t.Fatal(err)
	}
	type row struct{ enriched, indexed int64 }
	read := func() map[string]row {
		t.Helper()
		rows, err := s.db.Query(`SELECT path, enriched_at, indexed_at FROM tracks`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[string]row{}
		for rows.Next() {
			var p string
			var r row
			if err := rows.Scan(&p, &r.enriched, &r.indexed); err != nil {
				t.Fatal(err)
			}
			out[p] = r
		}
		return out
	}
	before := read()

	n, err := s.ResetEnrichedMissesUnderPrefix(context.Background(), "ArtistX")
	if err != nil {
		t.Fatalf("reset: %v", err)
	}
	if n != 1 {
		t.Errorf("reset = %d rows, want 1", n)
	}
	after := read()
	if after["ArtistX/Album1/01.flac"].enriched != 0 {
		t.Errorf("in-prefix incomplete row not reset")
	}
	if after["ArtistX/Album1/02.flac"].enriched != 5 {
		t.Errorf("complete row was reset — the incomplete-only predicate regressed")
	}
	if after["ArtistY/Album2/01.flac"].enriched != 5 {
		t.Errorf("out-of-prefix row was reset — scoping regressed")
	}
	for p, b := range before {
		if after[p].indexed != b.indexed {
			t.Errorf("indexed_at moved for %q — retry must not cause iOS delta churn", p)
		}
	}

	// Empty prefix delegates to the library-wide reset.
	if _, err := s.db.Exec(`UPDATE tracks SET enriched_at = 5`); err != nil {
		t.Fatal(err)
	}
	n, err = s.ResetEnrichedMissesUnderPrefix(context.Background(), "")
	if err != nil {
		t.Fatalf("reset(all): %v", err)
	}
	if n != 2 {
		t.Errorf("library-wide reset = %d rows, want 2 (both incomplete rows)", n)
	}
}

// TestResetBookletChecks pins the re-arm: attempts zero for
// unavailable rows only; available rows untouched.
func TestResetBookletChecks(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()

	if err := s.UpsertBookletAvailability(ctx, uuidA, false, "", 0); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertBookletAvailability(ctx, uuidB, true, "etag-b", 100); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE booklets SET check_attempts = 7`); err != nil {
		t.Fatal(err)
	}

	n, err := s.ResetBookletChecks(ctx, []string{uuidA, uuidB, uuidC})
	if err != nil {
		t.Fatalf("ResetBookletChecks: %v", err)
	}
	if n != 1 {
		t.Errorf("reset = %d, want 1 (only the unavailable row)", n)
	}
	var attempts int
	if err := s.db.QueryRow(`SELECT check_attempts FROM booklets WHERE release_mbid = ?`, uuidA).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if attempts != 0 {
		t.Errorf("unavailable row attempts = %d, want 0", attempts)
	}
	if err := s.db.QueryRow(`SELECT check_attempts FROM booklets WHERE release_mbid = ?`, uuidB).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if attempts != 7 {
		t.Errorf("available row attempts = %d, want 7 (untouched)", attempts)
	}
}

// TestBookletStatesIn pins the bulk probe: absent rows = zero value,
// available/fetched split surfaces.
func TestBookletStatesIn(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()

	if err := s.UpsertBookletAvailability(ctx, uuidA, true, "etag-a", 100); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertBookletAvailability(ctx, uuidB, true, "etag-b", 100); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkBookletFetched(ctx, uuidB); err != nil {
		t.Fatal(err)
	}

	states, err := s.BookletStatesIn(ctx, []string{uuidA, uuidB, uuidC})
	if err != nil {
		t.Fatalf("BookletStatesIn: %v", err)
	}
	if st := states[uuidA]; !st.Available || st.Fetched {
		t.Errorf("A = %+v, want available, not fetched", st)
	}
	if st := states[uuidB]; !st.Available || !st.Fetched {
		t.Errorf("B = %+v, want available AND fetched", st)
	}
	if st, present := states[uuidC]; present && (st.Available || st.Fetched) {
		t.Errorf("C = %+v, want absent/zero (never checked)", st)
	}
}
