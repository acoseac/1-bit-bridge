package manifest

import (
	"context"
	"testing"
	"time"
)

const (
	bkRel1 = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	bkRel2 = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
)

// mustGetBooklet is the test-side fetch helper (fatal on error/absent).
func mustGetBooklet(t *testing.T, s *Store, mbid string) *BookletRow {
	t.Helper()
	row, err := s.GetBooklet(context.Background(), mbid)
	if err != nil || row == nil {
		t.Fatalf("GetBooklet(%s) = (%v, %v)", mbid, row, err)
	}
	return row
}

func TestUpsertBookletAvailabilityMissesAccumulate(t *testing.T) {
	s := openAtlasTestStore(t)
	ctx := context.Background()
	for i := 1; i <= 2; i++ {
		if err := s.UpsertBookletAvailability(ctx, bkRel1, false, "", 0); err != nil {
			t.Fatal(err)
		}
		row := mustGetBooklet(t, s, bkRel1)
		if row.Available || row.CheckAttempts != i {
			t.Errorf("after miss %d: available=%v attempts=%d", i, row.Available, row.CheckAttempts)
		}
	}
	// Availability resets attempts + stores tag/size.
	if err := s.UpsertBookletAvailability(ctx, bkRel1, true, "etag-1", 1234); err != nil {
		t.Fatal(err)
	}
	row := mustGetBooklet(t, s, bkRel1)
	if !row.Available || row.CheckAttempts != 0 || row.Etag != "etag-1" || row.Bytes != 1234 {
		t.Errorf("after available: %+v", row)
	}
}

func TestUpsertBookletAvailabilityEtagLifecycle(t *testing.T) {
	s := openAtlasTestStore(t)
	ctx := context.Background()
	if err := s.UpsertBookletAvailability(ctx, bkRel1, true, "etag-1", 1234); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkBookletFetched(ctx, bkRel1); err != nil {
		t.Fatal(err)
	}
	// Same-etag re-check keeps fetched_at; a changed etag clears it (stale
	// cached PDF must re-download).
	if err := s.UpsertBookletAvailability(ctx, bkRel1, true, "etag-1", 1234); err != nil {
		t.Fatal(err)
	}
	if mustGetBooklet(t, s, bkRel1).FetchedAt == nil {
		t.Error("same-etag re-check cleared fetched_at, want preserved")
	}
	if err := s.UpsertBookletAvailability(ctx, bkRel1, true, "etag-2", 2222); err != nil {
		t.Fatal(err)
	}
	if mustGetBooklet(t, s, bkRel1).FetchedAt != nil {
		t.Error("etag change kept fetched_at, want cleared (cached PDF is stale)")
	}
	// MarkBookletUnavailable flips + clears.
	if err := s.MarkBookletUnavailable(ctx, bkRel1); err != nil {
		t.Fatal(err)
	}
	row := mustGetBooklet(t, s, bkRel1)
	if row.Available || row.Etag != "" || row.FetchedAt != nil {
		t.Errorf("after unavailable: %+v", row)
	}
}

func TestSetBookletTagAndBumpIndexWholeAlbumStrictAdvance(t *testing.T) {
	s := openAtlasTestStore(t)
	ctx := context.Background()
	base := time.Unix(0, 1_700_000_000_000_000_000)
	s.now = func() time.Time { return base }

	// Two sibling tracks on the release + one unrelated track.
	for _, tr := range []*Track{
		{Path: "A/01.flac", Size: 1, ModTime: time.Unix(1, 0), MusicBrainzAlbumID: bkRel1},
		{Path: "A/02.flac", Size: 1, ModTime: time.Unix(1, 0), MusicBrainzAlbumID: bkRel1},
		{Path: "B/01.flac", Size: 1, ModTime: time.Unix(1, 0), MusicBrainzAlbumID: bkRel2},
	} {
		if err := s.UpsertTrack(ctx, tr); err != nil {
			t.Fatal(err)
		}
	}

	// Freeze the clock at a PAST instant relative to the upsert stamps —
	// the CASE-WHEN must still STRICTLY advance indexed_at.
	s.now = func() time.Time { return base.Add(-time.Hour) }
	n, err := s.SetBookletTagAndBumpIndex(ctx, bkRel1, "tag-1")
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("bump touched %d rows, want 2 (whole album, sibling untouched)", n)
	}

	// STRICT advance despite the rewound clock: the siblings' indexed_at
	// moved past their upsert stamp (base), so a delta anchored exactly at
	// base returns them — and ONLY them (the unrelated track stays at
	// base, excluded by the strict `>`). The tag is spliced on reads.
	since := base
	tracks, err := s.ListTracks(ctx, &since)
	if err != nil {
		t.Fatal(err)
	}
	if len(tracks) != 2 {
		t.Fatalf("delta since base returned %d rows, want exactly the 2 bumped siblings: %+v", len(tracks), tracks)
	}
	for _, tr := range tracks {
		if tr.MusicBrainzAlbumID != bkRel1 || tr.BookletTag != "tag-1" {
			t.Errorf("%s: (mbid=%q, bookletTag=%q), want the tagged rel1 siblings", tr.Path, tr.MusicBrainzAlbumID, tr.BookletTag)
		}
	}

	// Unchanged tag → no-op (no indexed_at churn).
	n2, err := s.SetBookletTagAndBumpIndex(ctx, bkRel1, "tag-1")
	if err != nil {
		t.Fatal(err)
	}
	if n2 != 0 {
		t.Errorf("same-tag bump touched %d rows, want 0", n2)
	}

	// Clearing the tag ("") touches the rows again.
	n3, err := s.SetBookletTagAndBumpIndex(ctx, bkRel1, "")
	if err != nil {
		t.Fatal(err)
	}
	if n3 != 2 {
		t.Errorf("clear touched %d rows, want 2", n3)
	}
}

// TestMarshalForStorageZeroesBookletTag pins the column-only discipline: a
// read Track round-tripped through a write path must not freeze the spliced
// tag into tags_json.
func TestMarshalForStorageZeroesBookletTag(t *testing.T) {
	s := openAtlasTestStore(t)
	ctx := context.Background()
	tr := &Track{Path: "A/01.flac", Size: 1, ModTime: time.Unix(1, 0), MusicBrainzAlbumID: bkRel1}
	if err := s.UpsertTrack(ctx, tr); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetBookletTagAndBumpIndex(ctx, bkRel1, "tag-1"); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListTracks(ctx, nil)
	if err != nil || len(list) != 1 {
		t.Fatalf("list: (%d, %v)", len(list), err)
	}
	got := list[0]
	if got.BookletTag != "tag-1" {
		t.Fatalf("spliced tag = %q", got.BookletTag)
	}
	// Round-trip through the write path, then read the JSON-only view.
	if err := s.MarkEnriched(ctx, &got); err != nil {
		t.Fatal(err)
	}
	fromJSON, err := s.GetTrack(ctx, "A/01.flac")
	if err != nil || fromJSON == nil {
		t.Fatalf("GetTrack: (%v, %v)", fromJSON, err)
	}
	if fromJSON.BookletTag != "" {
		t.Errorf("tags_json leaked bookletTag = %q, want empty (column-only)", fromJSON.BookletTag)
	}
	// The column splice still serves it.
	list, _ = s.ListTracks(ctx, nil)
	if list[0].BookletTag != "tag-1" {
		t.Errorf("post-roundtrip splice = %q, want tag-1", list[0].BookletTag)
	}
}

func TestBookletsToCheckAttemptCapAndFetchQueue(t *testing.T) {
	s := openAtlasTestStore(t)
	ctx := context.Background()

	// rel1: capped out; rel2: available (excluded from checks, queued for
	// fetch); an unknown MBID passes through.
	for i := 0; i < 3; i++ {
		if err := s.UpsertBookletAvailability(ctx, bkRel1, false, "", 0); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.UpsertBookletAvailability(ctx, bkRel2, true, "e", 9); err != nil {
		t.Fatal(err)
	}

	const unknown = "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
	got, err := s.BookletsToCheck(ctx, []string{bkRel1, bkRel2, unknown}, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != unknown {
		t.Errorf("to-check = %v, want just the unknown MBID (rel1 capped, rel2 available)", got)
	}
	// Under a higher cap, rel1 is retried.
	got, _ = s.BookletsToCheck(ctx, []string{bkRel1}, 5)
	if len(got) != 1 {
		t.Errorf("under cap 5, to-check = %v, want rel1", got)
	}

	fetch, err := s.BookletsToFetch(ctx, 10, 8)
	if err != nil {
		t.Fatal(err)
	}
	if len(fetch) != 1 || fetch[0].ReleaseMBID != bkRel2 {
		t.Errorf("to-fetch = %+v, want rel2 only", fetch)
	}
	if err := s.MarkBookletFetched(ctx, bkRel2); err != nil {
		t.Fatal(err)
	}
	fetch, _ = s.BookletsToFetch(ctx, 10, 8)
	if len(fetch) != 0 {
		t.Errorf("to-fetch after mark = %+v, want empty", fetch)
	}

	avail, cached, err := s.BookletCounts(ctx)
	if err != nil || avail != 1 || cached != 1 {
		t.Errorf("counts = (%d, %d, %v), want (1, 1, nil)", avail, cached, err)
	}
}

func TestDeleteBookletsNotIn(t *testing.T) {
	s := openAtlasTestStore(t)
	ctx := context.Background()
	if err := s.UpsertBookletAvailability(ctx, bkRel1, true, "e", 9); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertBookletAvailability(ctx, bkRel2, true, "e", 9); err != nil {
		t.Fatal(err)
	}

	// Empty universe is a NO-OP (transient enumeration failure must never
	// wipe the cache).
	orphans, err := s.DeleteBookletsNotIn(ctx, nil)
	if err != nil || len(orphans) != 0 {
		t.Fatalf("empty universe: (%v, %v), want no-op", orphans, err)
	}
	if avail, _, _ := s.BookletCounts(ctx); avail != 2 {
		t.Fatalf("rows vanished on empty universe")
	}

	orphans, err = s.DeleteBookletsNotIn(ctx, []string{bkRel1})
	if err != nil {
		t.Fatal(err)
	}
	if len(orphans) != 1 || orphans[0] != bkRel2 {
		t.Errorf("orphans = %v, want [rel2]", orphans)
	}
	if row, _ := s.GetBooklet(ctx, bkRel2); row != nil {
		t.Error("rel2 row survived GC")
	}
	if row, _ := s.GetBooklet(ctx, bkRel1); row == nil {
		t.Error("rel1 row was wrongly GC'd")
	}
}
