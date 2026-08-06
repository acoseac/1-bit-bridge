package manifest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// --- extractors ---

func TestExtractFLACTagsAndFormat(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "t.flac")
	writeMinimalFLAC(t, p, 96000, 24, map[string]string{
		"TITLE":                 "Song",
		"ARTIST":                "An Artist",
		"ALBUM":                 "An Album",
		"TRACKNUMBER":           "3",
		"DATE":                  "2024",
		"GENRE":                 "Jazz",
		"MUSICBRAINZ_ALBUMID":   "album-mbid-1",
		"REPLAYGAIN_TRACK_GAIN": "-8.4 dB",
	})
	tr := &Track{Path: "t.flac", Size: 1, ModTime: time.Now()}
	if err := Extract(p, tr); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if tr.Title != "Song" || tr.Artist != "An Artist" || tr.Album != "An Album" {
		t.Errorf("tags: %+v", tr)
	}
	if tr.TrackNumber == nil || *tr.TrackNumber != 3 ||
		tr.Year == nil || *tr.Year != 2024 || tr.Genre != "Jazz" {
		t.Errorf("tags: %+v", tr)
	}
	if tr.MusicBrainzAlbumID != "album-mbid-1" {
		t.Errorf("MB album ID: %q", tr.MusicBrainzAlbumID)
	}
	if tr.ReplayGainTrackDB == nil || *tr.ReplayGainTrackDB != -8.4 {
		t.Errorf("replay gain: %v", tr.ReplayGainTrackDB)
	}
	if tr.SampleRate == nil || *tr.SampleRate != 96000 {
		t.Errorf("sample rate: %v", tr.SampleRate)
	}
	if tr.BitsPerSample == nil || *tr.BitsPerSample != 24 {
		t.Errorf("bit depth: %v", tr.BitsPerSample)
	}
	if tr.Duration == nil || *tr.Duration < 4.9 || *tr.Duration > 5.1 {
		t.Errorf("duration: %v (want ~5)", tr.Duration)
	}
	if tr.IsDSD == nil || *tr.IsDSD {
		t.Errorf("FLAC IsDSD = %v, want pointer to false (extractFLACFormat must set the explicit PCM flag so iOS can trust isDSD:false)", tr.IsDSD)
	}
}

func TestExtractDSFTagsAndFormat(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "t.dsf")
	writeMinimalDSF(t, p, 2822400, map[string]string{
		"title":  "DSF Song",
		"artist": "DSF Artist",
		"album":  "DSF Album",
		"track":  "5",
		"year":   "2023",
	})
	tr := &Track{Path: "t.dsf", Size: 1, ModTime: time.Now()}
	if err := Extract(p, tr); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if tr.Title != "DSF Song" {
		t.Errorf("title = %q", tr.Title)
	}
	if tr.Artist != "DSF Artist" {
		t.Errorf("artist = %q", tr.Artist)
	}
	if tr.Album != "DSF Album" {
		t.Errorf("album = %q", tr.Album)
	}
	if tr.TrackNumber == nil || *tr.TrackNumber != 5 {
		t.Errorf("track = %v", tr.TrackNumber)
	}
	if tr.IsDSD == nil || !*tr.IsDSD {
		t.Error("DSF should have IsDSD = true")
	}
	if tr.SampleRate == nil || *tr.SampleRate != 2822400 {
		t.Errorf("DSD rate = %v, want 2822400", tr.SampleRate)
	}
	if tr.BitsPerSample == nil || *tr.BitsPerSample != 1 {
		t.Errorf("bit depth = %v, want 1", tr.BitsPerSample)
	}
	if tr.Duration == nil || *tr.Duration < 4.9 || *tr.Duration > 5.1 {
		t.Errorf("duration: %v", tr.Duration)
	}
}

func TestExtractMP3Tags(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "t.mp3")
	writeMinimalMP3(t, p, map[string]string{
		"title":  "MP3 Song",
		"artist": "MP3 Artist",
		"album":  "MP3 Album",
	})
	tr := &Track{Path: "t.mp3", Size: 1, ModTime: time.Now()}
	if err := Extract(p, tr); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if tr.Title != "MP3 Song" || tr.Artist != "MP3 Artist" || tr.Album != "MP3 Album" {
		t.Errorf("mp3 tags: %+v", tr)
	}
}

func TestExtractUntaggedFileIsNotAnError(t *testing.T) {
	// A file that has no tags at all should still index without error;
	// the scanner falls back to path-derived values.
	dir := t.TempDir()
	p := filepath.Join(dir, "empty.flac")
	writeMinimalFLAC(t, p, 44100, 16, nil)
	tr := &Track{Path: "empty.flac", Size: 1, ModTime: time.Now()}
	if err := Extract(p, tr); err != nil {
		t.Errorf("Extract on tag-less FLAC: %v", err)
	}
	if tr.SampleRate == nil || *tr.SampleRate != 44100 {
		t.Errorf("sample rate from tag-less FLAC: %v", tr.SampleRate)
	}
}

// --- store ---

func TestStoreRoundTrip(t *testing.T) {
	s, err := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	now := time.Now().UTC().Truncate(time.Second)
	tr := &Track{Path: "a/b.flac", Size: 100, ModTime: now, Title: "X", Artist: "Y"}
	if err := s.UpsertTrack(context.Background(), tr); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetTrack(context.Background(), "a/b.flac")
	if err != nil || got == nil {
		t.Fatalf("GetTrack: %v / %v", err, got)
	}
	if got.Title != "X" || got.Artist != "Y" {
		t.Errorf("roundtrip: %+v", got)
	}

	// Update via upsert.
	tr.Title = "XX"
	if err := s.UpsertTrack(context.Background(), tr); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetTrack(context.Background(), "a/b.flac")
	if got.Title != "XX" {
		t.Errorf("update: %+v", got)
	}

	// List: one entry.
	all, _ := s.ListTracks(context.Background(), nil)
	if len(all) != 1 {
		t.Errorf("list: %d", len(all))
	}

	// Delete.
	if err := s.DeleteTrack(context.Background(), "a/b.flac"); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetTrack(context.Background(), "a/b.flac")
	if got != nil {
		t.Errorf("delete didn't take: %+v", got)
	}
}

// TestUpsertTrackBatchHappyPath covers the BEGIN/COMMIT-wrapped
// multi-row insert path used by the scanner's writer goroutine.
// All rows must land in one transaction (verified via post-commit row
// count + per-path GetTrack), and a follow-up batch with overlapping
// paths must update in place (ON CONFLICT semantics match UpsertTrack).
func TestUpsertTrackBatchHappyPath(t *testing.T) {
	s, err := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	now := time.Now().UTC().Truncate(time.Second)
	first := []*Track{
		{Path: "A/1.flac", Size: 10, ModTime: now, Title: "one"},
		{Path: "A/2.flac", Size: 20, ModTime: now, Title: "two"},
		{Path: "B/3.flac", Size: 30, ModTime: now, Title: "three"},
	}
	if err := s.UpsertTrackBatch(context.Background(), first); err != nil {
		t.Fatalf("first batch: %v", err)
	}
	got, _ := s.CountTracks(context.Background())
	if got != 3 {
		t.Errorf("post-batch count = %d, want 3", got)
	}
	one, _ := s.GetTrack(context.Background(), "A/1.flac")
	if one == nil || one.Title != "one" {
		t.Errorf("first batch didn't persist A/1.flac: %+v", one)
	}

	// Overlap + new: A/1 updates in place, B/3 unchanged, C/4 new.
	second := []*Track{
		{Path: "A/1.flac", Size: 99, ModTime: now, Title: "one-updated"},
		{Path: "C/4.flac", Size: 40, ModTime: now, Title: "four"},
	}
	if err := s.UpsertTrackBatch(context.Background(), second); err != nil {
		t.Fatalf("second batch: %v", err)
	}
	got, _ = s.CountTracks(context.Background())
	if got != 4 {
		t.Errorf("post-overlap-batch count = %d, want 4", got)
	}
	updated, _ := s.GetTrack(context.Background(), "A/1.flac")
	if updated == nil || updated.Title != "one-updated" || updated.Size != 99 {
		t.Errorf("overlap row not updated: %+v", updated)
	}
}

// TestUpsertTrackBatchEmptyIsNoOp confirms that the scanner's writer
// can flush a zero-row batch without erroring out — happens at
// scan-end when the final batch boundary aligns with the last row.
func TestUpsertTrackBatchEmptyIsNoOp(t *testing.T) {
	s, err := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.UpsertTrackBatch(context.Background(), nil); err != nil {
		t.Errorf("nil batch: %v", err)
	}
	if err := s.UpsertTrackBatch(context.Background(), []*Track{}); err != nil {
		t.Errorf("empty batch: %v", err)
	}
	got, _ := s.CountTracks(context.Background())
	if got != 0 {
		t.Errorf("empty batch wrote rows: count = %d", got)
	}
}

// TestStreamTracksRejectsNilCallback locks the defensive guard added
// after PR #70 review — calling StreamTracks with a nil fn must error
// out cleanly instead of nil-derefing inside the row loop.
func TestStreamTracksRejectsNilCallback(t *testing.T) {
	s, err := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.StreamTracks(context.Background(), nil, nil); err == nil {
		t.Error("StreamTracks(nil callback) returned nil error, want explicit failure")
	}
}

// TestStreamTracksCancelledCtxStopsIteration pins the
// ctx-propagation contract added in the senior-audit follow-up:
// a cancelled ctx surfaces as a ctx-error from StreamTracks
// without invoking the row callback. Pre-fix, StreamTracks used
// `db.Query` (no ctx), so a client-disconnect mid-manifest-pull
// would let the SQLite scan run to completion holding the read
// lock; iOS would just discard the body but the bridge wasted
// CPU + IO for the rest of the stream. With QueryContext the
// query itself cancels at the next row boundary.
func TestStreamTracksCancelledCtxStopsIteration(t *testing.T) {
	s, err := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	// Insert a few tracks so the query has rows to potentially
	// emit — without rows the cancellation could trivially
	// "succeed" by emitting zero callbacks.
	for i := 0; i < 5; i++ {
		if err := s.UpsertTrack(context.Background(), &Track{
			Path:    fmt.Sprintf("track-%d.flac", i),
			Size:    100,
			ModTime: time.Now(),
		}); err != nil {
			t.Fatalf("UpsertTrack: %v", err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel so the query rejects immediately
	called := 0
	err = s.StreamTracks(ctx, nil, func(*Track) error {
		called++
		return nil
	})
	// Either the QueryContext returns context.Canceled OR the
	// row iteration sees it on the first row.Next() — both are
	// acceptable. What matters: the callback didn't run for
	// EVERY row.
	if called >= 5 {
		t.Errorf("callback fired for all rows despite ctx cancellation; got %d", called)
	}
	if err == nil {
		t.Error("StreamTracks returned nil; expected context error to propagate")
	}
}

// TestStoreHasTrackWithArtworkMBID pins the SQL contract the
// /v1/artwork 202-vs-404 handler depends on. A track tagged with a
// given ArtworkMBID reports true; an arbitrary MBID that no track
// references reports false. Empty MBID always returns false
// (short-circuit). Parallel coverage for ArtistMBID.
func TestStoreHasTrackWithArtworkMBID(t *testing.T) {
	s, _ := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	defer s.Close()

	known := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	unknown := "99999999-9999-4999-8999-999999999999"

	s.UpsertTrack(context.Background(), &Track{
		Path: "a/b.flac", Size: 1, ModTime: time.Now(),
		Artist: "A", Album: "B", ArtworkMBID: known,
	})

	if !s.HasTrackWithArtworkMBID(context.Background(), known) {
		t.Errorf("known MBID should report true")
	}
	if s.HasTrackWithArtworkMBID(context.Background(), unknown) {
		t.Errorf("unknown MBID should report false")
	}
	if s.HasTrackWithArtworkMBID(context.Background(), "") {
		t.Errorf("empty MBID should short-circuit to false")
	}
}

func TestStoreHasTrackWithArtistMBID(t *testing.T) {
	s, _ := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	defer s.Close()

	known := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"

	s.UpsertTrack(context.Background(), &Track{
		Path: "a/b.flac", Size: 1, ModTime: time.Now(),
		Artist: "A", Album: "B", ArtistMBID: known,
	})

	if !s.HasTrackWithArtistMBID(context.Background(), known) {
		t.Errorf("known artist MBID should report true")
	}
	if s.HasTrackWithArtistMBID(context.Background(), "not-a-uuid") {
		t.Errorf("unknown artist MBID should report false")
	}
}

// TestStoreMBIDEnrichmentPending pins the SQL contract behind the
// /v1/artwork + /v1/artist-image 202-vs-terminal-404 split: an
// upserted track (enriched_at = 0) reports pending; after
// MarkEnriched it reports complete; an MBID nobody references and
// the empty MBID both report not-pending. A second still-unenriched
// track sharing the MBID keeps it pending — the answer is "ANY track
// awaiting the enricher", not "the first one".
func TestStoreMBIDEnrichmentPending(t *testing.T) {
	s, _ := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	defer s.Close()
	ctx := context.Background()

	artist := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	release := "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"

	t1 := &Track{
		Path: "a/1.flac", Size: 1, ModTime: time.Now(),
		Artist: "A", Album: "B", ArtistMBID: artist, ArtworkMBID: release,
	}
	t2 := &Track{
		Path: "a/2.flac", Size: 1, ModTime: time.Now(),
		Artist: "A", Album: "B", ArtistMBID: artist, ArtworkMBID: release,
	}
	s.UpsertTrack(ctx, t1)
	s.UpsertTrack(ctx, t2)

	if !s.ArtistMBIDEnrichmentPending(ctx, artist) {
		t.Errorf("freshly-upserted tracks: artist MBID should report pending")
	}
	if !s.ArtworkMBIDEnrichmentPending(ctx, release) {
		t.Errorf("freshly-upserted tracks: artwork MBID should report pending")
	}

	if err := s.MarkEnriched(ctx, t1); err != nil {
		t.Fatalf("MarkEnriched(t1): %v", err)
	}
	if !s.ArtistMBIDEnrichmentPending(ctx, artist) {
		t.Errorf("one of two tracks enriched: should STILL report pending")
	}

	if err := s.MarkEnriched(ctx, t2); err != nil {
		t.Fatalf("MarkEnriched(t2): %v", err)
	}
	if s.ArtistMBIDEnrichmentPending(ctx, artist) {
		t.Errorf("all tracks enriched: artist MBID should report complete")
	}
	if s.ArtworkMBIDEnrichmentPending(ctx, release) {
		t.Errorf("all tracks enriched: artwork MBID should report complete")
	}

	if s.ArtistMBIDEnrichmentPending(ctx, "99999999-9999-4999-8999-999999999999") {
		t.Errorf("unreferenced MBID should report not-pending")
	}
	if s.ArtistMBIDEnrichmentPending(ctx, "") {
		t.Errorf("empty MBID should short-circuit to not-pending")
	}
}

// TestEnrichmentCountsCorrectness pins the basic contract: number of
// rows with `enriched_at != 0`, and the MAX of those timestamps. The
// query was rewritten to two scalar subqueries (per Gemini A9 / iOS
// bug review #9) so SQLite's planner uses `idx_tracks_enriched` for
// both — but the surface contract must stay the same.
func TestEnrichmentCountsCorrectness(t *testing.T) {
	s, err := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// 5 tracks: 3 enriched (with distinct timestamps), 2 unenriched.
	now := time.Now().Truncate(time.Second)
	for i := 1; i <= 5; i++ {
		tr := &Track{
			Path:    fmt.Sprintf("a/t%d.flac", i),
			Size:    int64(i),
			ModTime: now,
			Artist:  "A", Album: "B",
		}
		if err := s.UpsertTrack(context.Background(), tr); err != nil {
			t.Fatal(err)
		}
		if i <= 3 {
			// Stagger enriched timestamps so MAX() has something to find.
			tr.Title = fmt.Sprintf("T%d", i)
			// MarkEnriched stamps `enriched_at = NOW()`. Use distinct
			// monotonic offsets so the latest is deterministic.
			if err := s.MarkEnriched(context.Background(), tr); err != nil {
				t.Fatal(err)
			}
		}
	}

	enriched, last, err := s.EnrichmentCounts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if enriched != 3 {
		t.Errorf("enriched count: got %d, want 3", enriched)
	}
	if last == nil {
		t.Errorf("lastEnrichedAt: got nil, want non-nil")
	}
	if last != nil && last.Before(now) {
		t.Errorf("lastEnrichedAt should be >= test start, got %v", last)
	}
}

// TestEnrichmentCountsEmpty exercises the all-zero branch: no rows at
// all means count=0 and lastEnrichedAt=nil (the `MAX(enriched_at)`
// returns NULL, which sql.NullInt64 surfaces correctly via the
// `lastNs.Valid && lastNs.Int64 != 0` gate).
func TestEnrichmentCountsEmpty(t *testing.T) {
	s, err := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	enriched, last, err := s.EnrichmentCounts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if enriched != 0 {
		t.Errorf("empty count: got %d, want 0", enriched)
	}
	if last != nil {
		t.Errorf("empty lastEnrichedAt: got %v, want nil", *last)
	}
}

// TestEnrichmentCountsAllUnenriched covers the case where rows exist
// but none have run through enrichment (enriched_at == 0). count=0,
// lastEnrichedAt=nil — the `lastNs.Int64 != 0` gate is load-bearing
// here: SQLite's MAX over all-zero returns 0 (Valid:true, Int64:0),
// and we'd otherwise emit `time.Unix(0,0)` instead of nil.
func TestEnrichmentCountsAllUnenriched(t *testing.T) {
	s, err := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	for i := 1; i <= 3; i++ {
		if err := s.UpsertTrack(context.Background(), &Track{
			Path: fmt.Sprintf("a/t%d.flac", i), Size: int64(i),
			ModTime: time.Now(), Artist: "A", Album: "B",
		}); err != nil {
			t.Fatal(err)
		}
	}

	enriched, last, err := s.EnrichmentCounts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if enriched != 0 {
		t.Errorf("all-unenriched count: got %d, want 0", enriched)
	}
	if last != nil {
		t.Errorf("all-unenriched lastEnrichedAt: got %v, want nil", *last)
	}
}

// TestEnrichmentCountsUsesIndex pins the perf contract: SQLite's
// query planner must use `idx_tracks_enriched` for BOTH the count
// subquery AND the MAX subquery, NOT a full table scan. Pre-fix
// the SUM(CASE WHEN enriched_at != 0 THEN 1 ELSE 0 END) form
// forced a full SCAN tracks on every `/v1/health` poll — measurable
// CPU spike per ~15 s on Pi-class hosts with 100k-row libraries.
//
// 95%-enriched fixture per Gemini's recommendation (older SQLite
// planners can sometimes fall back to a full scan when the index
// would return >50% of rows; the 95% fixture forces the worst-case
// planner choice).
func TestEnrichmentCountsUsesIndex(t *testing.T) {
	s, err := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// 100 rows, 95 enriched. Beyond the per-row coalesce-into-batch
	// upsert path so the index actually has populated tuples.
	for i := 1; i <= 100; i++ {
		tr := &Track{
			Path:    fmt.Sprintf("a/t%03d.flac", i),
			Size:    int64(i),
			ModTime: time.Now(),
			Artist:  "A", Album: "B",
		}
		if err := s.UpsertTrack(context.Background(), tr); err != nil {
			t.Fatal(err)
		}
		if i <= 95 {
			tr.Title = fmt.Sprintf("T%d", i)
			if err := s.MarkEnriched(context.Background(), tr); err != nil {
				t.Fatal(err)
			}
		}
	}

	// Populate sqlite_stat1 so the planner has selectivity stats.
	// Without this, SQLite's planner falls back to a heuristic that
	// chooses SCAN-with-COVERING-INDEX over SEARCH for high-cardinality
	// matches — both touch every row, but SEARCH is the strictly
	// better shape. The contract this test locks is that the index
	// CAN be used for a bounded range lookup; ANALYZE makes the
	// planner act on that capability. (CodeRabbit Major round-1.)
	if _, err := s.db.Exec(`ANALYZE`); err != nil {
		t.Fatal(err)
	}

	rows, err := s.db.Query(`EXPLAIN QUERY PLAN
		SELECT
			(SELECT COUNT(*) FROM tracks WHERE enriched_at > 0),
			(SELECT MAX(enriched_at) FROM tracks)
	`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	var plan strings.Builder
	for rows.Next() {
		// EXPLAIN QUERY PLAN row shape: (id, parent, notused, detail).
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatal(err)
		}
		plan.WriteString(detail)
		plan.WriteString("\n")
	}
	planStr := plan.String()
	t.Logf("EXPLAIN QUERY PLAN:\n%s", planStr)

	// Both scalar subqueries must reference the index. The COUNT(*)
	// subquery uses `WHERE enriched_at > 0` (sargeable, becomes a
	// SEARCH range scan), and the MAX(enriched_at) subquery uses
	// the index B-tree's tail (also a SEARCH). We require the index
	// name to appear AT LEAST TWICE in the plan — once per subquery —
	// to lock the contract that NEITHER one degrades to a full
	// table walk (CodeRabbit Major round-1 on PR #164). The
	// `strings.Count` form catches the regression where the COUNT
	// subquery silently falls back to `SCAN tracks USING COVERING
	// INDEX` (which still touches every row) — the previous
	// `strings.Contains` form let that pass.
	if c := strings.Count(planStr, "idx_tracks_enriched"); c < 2 {
		t.Errorf("expected both scalar subqueries to reference idx_tracks_enriched (>=2 mentions), got %d in:\n%s", c, planStr)
	}
	// Any `SCAN tracks` line — even with COVERING INDEX — still
	// traverses every row. The perf contract requires SEARCH for
	// keyed/range lookups against the index. Reject SCAN tracks
	// unconditionally (CodeRabbit Major round-1 on PR #164).
	for _, line := range strings.Split(planStr, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "SCAN tracks") {
			t.Errorf("EXPLAIN line %q uses SCAN, not keyed SEARCH — contract requires range index lookup", line)
		}
	}
}

// --- Pagination (v1.1 §8) ---

// TestStoreListTracksPageWalksAllRowsExactlyOnce is the core
// correctness guarantee: iterating with `afterPath=""` and feeding
// each page's last path back in must cover every track exactly once,
// in ascending path order, even when the page size doesn't divide
// the track count evenly.
func TestStoreListTracksPageWalksAllRowsExactlyOnce(t *testing.T) {
	s, _ := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	defer s.Close()

	// 7 tracks, page size 3 → pages of 3, 3, 1. Non-divisible on
	// purpose.
	for i := 1; i <= 7; i++ {
		s.UpsertTrack(context.Background(), &Track{
			Path:    fmt.Sprintf("Music/Artist/%02d.flac", i),
			Size:    int64(i),
			ModTime: time.Now(),
		})
	}

	var seen []string
	cursor := ""
	pages := 0
	for {
		page, err := s.ListTracksPage(context.Background(), cursor, 3)
		if err != nil {
			t.Fatal(err)
		}
		pages++
		if len(page) == 0 {
			break
		}
		for _, tr := range page {
			seen = append(seen, tr.Path)
		}
		if len(page) < 3 {
			break
		}
		cursor = page[len(page)-1].Path
	}

	if len(seen) != 7 {
		t.Errorf("seen %d tracks across %d pages, want 7", len(seen), pages)
	}
	// Sorted + unique.
	for i := 1; i < len(seen); i++ {
		if seen[i] <= seen[i-1] {
			t.Errorf("out-of-order or duplicate at %d: %q then %q", i, seen[i-1], seen[i])
		}
	}
}

// TestStoreListTracksPageEmptyStoreReturnsEmpty covers the zero-row
// edge — the pagination loop must terminate immediately, not spin.
func TestStoreListTracksPageEmptyStoreReturnsEmpty(t *testing.T) {
	s, _ := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	defer s.Close()

	page, err := s.ListTracksPage(context.Background(), "", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 0 {
		t.Errorf("empty store should return empty page, got %d", len(page))
	}
}

// TestStoreListTracksPageZeroLimitDefaults locks in the "no foot-gun"
// contract: a caller passing limit=0 or negative gets a sensible
// default rather than a zero-row page that stalls their loop.
func TestStoreListTracksPageZeroLimitDefaults(t *testing.T) {
	s, _ := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	defer s.Close()
	for i := 1; i <= 5; i++ {
		s.UpsertTrack(context.Background(), &Track{
			Path:    fmt.Sprintf("Music/%02d.flac", i),
			Size:    int64(i),
			ModTime: time.Now(),
		})
	}
	page, err := s.ListTracksPage(context.Background(), "", 0) // 0 → default 1000
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 5 {
		t.Errorf("limit=0 should fall back to default and return all 5 tracks, got %d", len(page))
	}
}

// TestBuildManifestPageSetsNextCursorAndTotal checks the envelope-
// building contract:
//   - First page (cursor=="") carries Folders + Total.
//   - Subsequent pages carry neither (reduces bandwidth on large libs).
//   - NextCursor set iff there's another page.
func TestBuildManifestPageSetsNextCursorAndTotal(t *testing.T) {
	dir := t.TempDir()
	s, _ := OpenStore(filepath.Join(dir, "bridge.db"))
	defer s.Close()
	for i := 1; i <= 5; i++ {
		s.UpsertTrack(context.Background(), &Track{
			Path:    fmt.Sprintf("Music/%02d.flac", i),
			Size:    int64(i),
			ModTime: time.Now(),
		})
	}

	// Page size 2 → first page is full, NextCursor non-nil, Total set.
	m, err := BuildManifestPage(context.Background(), s, []string{filepath.Join(dir, "Music")}, "", 2)
	if err != nil {
		t.Fatal(err)
	}
	if m.Total == nil || *m.Total != 5 {
		t.Errorf("Total = %v, want pointer to 5", m.Total)
	}
	if m.NextCursor == nil || *m.NextCursor != "Music/02.flac" {
		t.Errorf("NextCursor = %v, want pointer to Music/02.flac", m.NextCursor)
	}
	if len(m.Tracks) != 2 {
		t.Errorf("first page tracks = %d, want 2", len(m.Tracks))
	}

	// Second page (cursor=="Music/02.flac") — still mid-run, so
	// NextCursor non-nil, but Folders + Total absent.
	m, err = BuildManifestPage(context.Background(), s, []string{filepath.Join(dir, "Music")}, "Music/02.flac", 2)
	if err != nil {
		t.Fatal(err)
	}
	if m.Total != nil {
		t.Errorf("mid-run Total should be nil, got %v", *m.Total)
	}
	if m.Folders != nil {
		t.Errorf("mid-run Folders should be nil, got %v", m.Folders)
	}
	if m.NextCursor == nil {
		t.Errorf("second page should still have NextCursor")
	}

	// Last page (cursor=="Music/04.flac" → only Music/05.flac left).
	// Short read, NextCursor nil.
	m, err = BuildManifestPage(context.Background(), s, []string{filepath.Join(dir, "Music")}, "Music/04.flac", 2)
	if err != nil {
		t.Fatal(err)
	}
	if m.NextCursor != nil {
		t.Errorf("NextCursor should be nil on last page, got %v", *m.NextCursor)
	}
	if len(m.Tracks) != 1 {
		t.Errorf("last page tracks = %d, want 1", len(m.Tracks))
	}
}

// TestBuildManifestPageExactMultipleOfLimitStopsAtCorrectPage locks
// in the limit+1 query trick: when the track count is an exact
// multiple of the page limit, the iteration must terminate on the
// last full page (NextCursor nil) rather than requiring an extra
// round-trip that returns zero rows.
func TestBuildManifestPageExactMultipleOfLimitStopsAtCorrectPage(t *testing.T) {
	dir := t.TempDir()
	s, _ := OpenStore(filepath.Join(dir, "bridge.db"))
	defer s.Close()
	// 4 tracks, page size 2 — exactly two full pages.
	for i := 1; i <= 4; i++ {
		s.UpsertTrack(context.Background(), &Track{
			Path:    fmt.Sprintf("Music/%02d.flac", i),
			Size:    int64(i),
			ModTime: time.Now(),
		})
	}

	// Page 1: 2 tracks, NextCursor set to Music/02.flac.
	p1, _ := BuildManifestPage(context.Background(), s, []string{filepath.Join(dir, "Music")}, "", 2)
	if p1.NextCursor == nil {
		t.Fatalf("page 1 should have NextCursor")
	}

	// Page 2: 2 tracks, NextCursor MUST be nil — we've reached the
	// end. Pre-fix this would have set NextCursor to Music/04.flac
	// and forced a third empty round-trip.
	p2, _ := BuildManifestPage(context.Background(), s, []string{filepath.Join(dir, "Music")}, *p1.NextCursor, 2)
	if len(p2.Tracks) != 2 {
		t.Errorf("page 2 tracks = %d, want 2", len(p2.Tracks))
	}
	if p2.NextCursor != nil {
		t.Errorf("page 2 NextCursor should be nil (exact-multiple termination), got %v",
			*p2.NextCursor)
	}
}

func TestStoreSinceFilterIndexedAt(t *testing.T) {
	// ListTracks's `since` is indexed_at, not mtime_ns, so a track with
	// an old file-mtime still surfaces in a delta if it was newly
	// indexed. This covers the "rip from years ago, copy into library
	// today" scenario that the mtime-based filter couldn't see.
	s, _ := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	defer s.Close()

	oldMtime := time.Now().Add(-10 * 365 * 24 * time.Hour).UTC().Truncate(time.Second)
	s.UpsertTrack(context.Background(), &Track{Path: "old.flac", Size: 1, ModTime: oldMtime})
	// Sleep spans a couple of Go-time ticks so the cursor lands
	// strictly between the two UpsertTrack calls' indexed_at values.
	time.Sleep(10 * time.Millisecond)
	cursor := time.Now().UTC()
	time.Sleep(10 * time.Millisecond)
	s.UpsertTrack(context.Background(), &Track{Path: "mid.flac", Size: 1, ModTime: oldMtime})
	s.UpsertTrack(context.Background(), &Track{Path: "new.flac", Size: 1, ModTime: time.Now()})

	all, _ := s.ListTracks(context.Background(), nil)
	if len(all) != 3 {
		t.Errorf("all: %d", len(all))
	}
	newer, _ := s.ListTracks(context.Background(), &cursor)
	if len(newer) != 2 {
		t.Fatalf("newer than cursor: want 2 (mid+new), got %d", len(newer))
	}
	// mid.flac has an ancient file-mtime but a fresh indexed_at — it
	// MUST surface. An mtime-based filter would silently drop it.
	foundMid := false
	for _, tr := range newer {
		if tr.Path == "mid.flac" {
			foundMid = true
		}
	}
	if !foundMid {
		t.Error("mid.flac (ancient mtime, fresh indexed_at) missing from delta")
	}
}

func TestStorePersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bridge.db")
	s1, _ := OpenStore(path)
	s1.UpsertTrack(context.Background(), &Track{Path: "p.flac", Size: 1, ModTime: time.Now(), Title: "persists"})
	s1.Close()

	s2, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	got, _ := s2.GetTrack(context.Background(), "p.flac")
	if got == nil || got.Title != "persists" {
		t.Errorf("didn't persist: %+v", got)
	}
}

func TestStoreFolderRoundTrip(t *testing.T) {
	s, _ := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	defer s.Close()
	now := time.Now().UTC().Truncate(time.Second)
	s.UpsertFolder(context.Background(), &Folder{Path: "a", ModTime: now})
	got, _ := s.FolderMTime(context.Background(), "a")
	if !got.Equal(now) {
		t.Errorf("folder mtime: got %v, want %v", got, now)
	}
	folders, _ := s.ListFolders(context.Background())
	if len(folders) != 1 {
		t.Errorf("folders: %+v", folders)
	}
}

// --- scanner ---

func TestScannerIndexesAllTracks(t *testing.T) {
	root, expected := tempLibrary(t)
	s, _ := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	defer s.Close()
	sc := NewScanner([]string{root}, s, "")

	n, err := sc.Scan(context.Background())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if n != expected {
		t.Errorf("scanned %d, want %d", n, expected)
	}
	all, _ := s.ListTracks(context.Background(), nil)
	if len(all) != expected {
		t.Errorf("stored %d, want %d", len(all), expected)
	}
	// Verify tags propagated.
	found := map[string]bool{}
	for _, tr := range all {
		found[tr.Title] = true
	}
	for _, want := range []string{"Flac Title", "DSF Title", "Mp3 Title"} {
		if !found[want] {
			t.Errorf("missing title %q after scan; found = %v", want, found)
		}
	}
}

func TestScannerSkipsDotFilesAndNonAudio(t *testing.T) {
	root, _ := tempLibrary(t)
	s, _ := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	defer s.Close()
	sc := NewScanner([]string{root}, s, "")
	sc.Scan(context.Background())
	for _, p := range mustPaths(t, s) {
		if strings.Contains(p, ".DS_Store") || strings.HasSuffix(p, ".txt") {
			t.Errorf("scanner indexed non-audio path: %q", p)
		}
	}
}

func TestScannerRemovesDeletedTracks(t *testing.T) {
	root, _ := tempLibrary(t)
	s, _ := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	defer s.Close()
	sc := NewScanner([]string{root}, s, "")
	sc.Scan(context.Background())

	// Delete one track from disk, rescan, verify its row is gone.
	target := filepath.Join(root, "Artist A", "Album 1", "01 FlacTrack.flac")
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	sc.Scan(context.Background())
	got, _ := s.GetTrack(context.Background(), "Artist A/Album 1/01 FlacTrack.flac")
	if got != nil {
		t.Errorf("deleted track still in DB: %+v", got)
	}
	// The sibling DSF track should still be there.
	sibling, _ := s.GetTrack(context.Background(), "Artist A/Album 1/02 DsfTrack.dsf")
	if sibling == nil {
		t.Error("sibling track lost during deletion pass")
	}
}

func TestScannerSkipsUnchangedFiles(t *testing.T) {
	root, _ := tempLibrary(t)
	s, _ := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	defer s.Close()
	sc := NewScanner([]string{root}, s, "")
	sc.Scan(context.Background())

	// Second scan should report the same count; since nothing changed, it
	// doesn't re-extract. We can't easily observe "skipped", but we can
	// verify the DB is still consistent.
	n, _ := sc.Scan(context.Background())
	// n reflects tracks touched/upserted. In a no-change scan, our
	// current implementation still walks but skips the upsert — so n is 0.
	if n != 0 {
		t.Errorf("no-change rescan touched %d tracks, want 0", n)
	}
}

// TestScannerReextractsSameSizeOlderMtime pins the skip-gate fix
// (2026-07-21 review Low): the early-skip used to fire whenever the
// stored mtime was >= the file's, so a same-size replacement carrying
// an OLDER mtime (cp -p restore, backup rollback) was never
// re-extracted and kept serving stale tags. The gate now skips only on
// exact mtime equality — a mismatch in EITHER direction re-extracts.
func TestScannerReextractsSameSizeOlderMtime(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "track.flac")
	if err := os.WriteFile(p, []byte("not-a-real-flac"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	sc := NewScanner([]string{root}, s, "")
	if _, err := sc.Scan(context.Background()); err != nil {
		t.Fatalf("initial Scan: %v", err)
	}

	// Same-size replacement (15 bytes → 15 bytes) with an OLDER mtime.
	older := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.WriteFile(p, []byte("NOT-A-REAL-FLAC"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, older, older); err != nil {
		t.Fatal(err)
	}

	n, err := sc.Scan(context.Background())
	if err != nil {
		t.Fatalf("rescan: %v", err)
	}
	if n != 1 {
		t.Errorf("rescan touched %d tracks, want 1 (older-mtime replacement re-extracted)", n)
	}
	got, err := s.GetTrack(context.Background(), "track.flac")
	if err != nil {
		t.Fatalf("GetTrack: %v", err)
	}
	if got == nil {
		t.Fatal("track row missing after rescan")
	}
	if !got.ModTime.Equal(older) {
		t.Errorf("stored ModTime = %v, want re-extracted older mtime %v (skip gate kept the stale row)", got.ModTime, older)
	}
}

// TestScannerWorkerPoolCommitsAllTracks confirms the new worker-pool +
// batched-writer pipeline persists every walked file. Synthesises a
// flat directory of N audio-extension files (Extract fails on each but
// the row is still upserted via fillFromPath, matching legacy behaviour),
// then asserts the post-scan row count and the progress counter both
// reach N.
func TestScannerWorkerPoolCommitsAllTracks(t *testing.T) {
	const n = 1200 // > 2 batches at scanBatchSize=500
	root := t.TempDir()
	for i := 0; i < n; i++ {
		p := filepath.Join(root, fmt.Sprintf("track-%05d.flac", i))
		if err := os.WriteFile(p, []byte("not-a-real-flac"), 0o644); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	s, err := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	sc := NewScanner([]string{root}, s, "")

	count, err := sc.Scan(context.Background())
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if count != n {
		t.Errorf("Scan returned count = %d, want %d", count, n)
	}
	if got := sc.ScanProgress(); got != int64(n) {
		t.Errorf("ScanProgress = %d, want %d", got, n)
	}
	got, err := s.CountTracks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got != n {
		t.Errorf("CountTracks = %d, want %d (some rows lost in batching pipeline)", got, n)
	}
}

// TestScannerCancellationLeavesCommittedBatchesIntact verifies that
// cancelling a scan mid-walk doesn't corrupt the DB. The walker honours
// ctx.Done, the writer flushes its in-flight batch, and the row count
// is whatever-was-committed-so-far (≤ N). This is the safety contract
// for "user backgrounded the app during a fresh scan".
func TestScannerCancellationLeavesCommittedBatchesIntact(t *testing.T) {
	const n = 2000
	root := t.TempDir()
	for i := 0; i < n; i++ {
		p := filepath.Join(root, fmt.Sprintf("track-%05d.flac", i))
		if err := os.WriteFile(p, []byte("nope"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	s, err := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	sc := NewScanner([]string{root}, s, "")

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before Scan starts

	// Cancelling the parent ctx still lets Scan complete its setup;
	// it propagates cancellation through the walker. The exact row
	// count is racy but must be in [0, n] and must equal CountTracks
	// (no leaked partial-batch state).
	count, _ := sc.Scan(ctx)
	if count < 0 || count > n {
		t.Errorf("count = %d, want 0..%d", count, n)
	}
	got, _ := s.CountTracks(context.Background())
	if got != count {
		t.Errorf("CountTracks (%d) != Scan count (%d) — partial batch leaked", got, count)
	}
}

func TestScannerIsScanningFlag(t *testing.T) {
	root, _ := tempLibrary(t)
	s, _ := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	defer s.Close()
	sc := NewScanner([]string{root}, s, "")
	if sc.IsScanning() {
		t.Error("fresh scanner reports scanning")
	}
	sc.Scan(context.Background())
	if sc.IsScanning() {
		t.Error("scanner reports scanning after Scan returned")
	}
	if sc.LastFullScan().IsZero() {
		t.Error("LastFullScan is zero after successful scan")
	}
}

func TestNewScannerSeedsLastFullScanFromStore(t *testing.T) {
	// Pre-fix `LastFullScan()` only read the in-memory `s.lastFull`;
	// the matching SQLite write at `last_full_scan` was a dead-code
	// orphan with no reader. After a fresh process, the dashboard
	// showed "never" until the next scan completed — even if the
	// previous run had a successful scan minutes earlier. Now
	// NewScanner seeds the in-memory atomic from the SQLite key so
	// the timestamp survives restarts.
	dbPath := filepath.Join(t.TempDir(), "bridge.db")
	s, err := OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	want := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
	if err := s.SetScanState(context.Background(), "last_full_scan", want.Format(time.RFC3339Nano)); err != nil {
		t.Fatalf("SetScanState: %v", err)
	}
	s.Close()

	// Re-open as if a fresh process started.
	s2, err := OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	sc := NewScanner([]string{t.TempDir()}, s2, "")
	got := sc.LastFullScan()
	if got.IsZero() {
		t.Fatalf("LastFullScan is zero after fresh-process construction; expected the SQLite-seeded value (%s)", want.Format(time.RFC3339))
	}
	if !got.Equal(want) {
		t.Errorf("LastFullScan = %s, want %s", got.Format(time.RFC3339Nano), want.Format(time.RFC3339Nano))
	}
}

func TestNewScannerLastFullScanZeroOnFreshDB(t *testing.T) {
	// Cold install: nothing in scan_state. Seed must NOT panic and
	// MUST leave LastFullScan as the zero time so the dashboard
	// renders "never" — same as before the seeding code existed.
	s, err := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	sc := NewScanner([]string{t.TempDir()}, s, "")
	if !sc.LastFullScan().IsZero() {
		t.Errorf("LastFullScan = %v, want zero on a fresh database", sc.LastFullScan())
	}
}

// --- manifest ---

func TestBuildManifestShape(t *testing.T) {
	root, expected := tempLibrary(t)
	s, _ := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	defer s.Close()
	sc := NewScanner([]string{root}, s, "")
	sc.Scan(context.Background())

	mf, err := BuildManifest(context.Background(), s, []string{root}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if mf.Version != 1 {
		t.Errorf("version = %d", mf.Version)
	}
	if len(mf.Tracks) != expected {
		t.Errorf("tracks = %d, want %d", len(mf.Tracks), expected)
	}
	if len(mf.LibraryRoots) != 1 || mf.LibraryRoots[0] != filepath.Base(root) {
		t.Errorf("libraryRoots = %v", mf.LibraryRoots)
	}
	if mf.GeneratedAt.IsZero() {
		t.Error("generatedAt unset")
	}
	if len(mf.Folders) == 0 {
		t.Error("no folders recorded")
	}
}

// TestWriteManifestParityWithBuildManifest pins the streaming legacy
// path's wire shape against the in-memory builder. Both must agree on
// version, track set, library roots, folders, and the enrichment
// progress block — the streaming path is a shape-preserving rewrite.
func TestWriteManifestParityWithBuildManifest(t *testing.T) {
	root, expected := tempLibrary(t)
	s, _ := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	defer s.Close()
	sc := NewScanner([]string{root}, s, "")
	sc.Scan(context.Background())

	want, err := BuildManifest(context.Background(), s, []string{root}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := WriteManifest(context.Background(), &buf, s, []string{root}, time.Time{}); err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}
	var got Manifest
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("decode streamed manifest: %v\nbody=%s", err, buf.String())
	}

	if got.Version != want.Version {
		t.Errorf("version = %d, want %d", got.Version, want.Version)
	}
	if len(got.Tracks) != expected {
		t.Errorf("streamed tracks = %d, want %d", len(got.Tracks), expected)
	}
	if len(got.LibraryRoots) != len(want.LibraryRoots) {
		t.Errorf("libraryRoots length: got %d, want %d",
			len(got.LibraryRoots), len(want.LibraryRoots))
	} else {
		for i := range want.LibraryRoots {
			if got.LibraryRoots[i] != want.LibraryRoots[i] {
				t.Errorf("libraryRoots[%d]: got %q, want %q",
					i, got.LibraryRoots[i], want.LibraryRoots[i])
			}
		}
	}
	if len(got.Folders) != len(want.Folders) {
		t.Errorf("folders: got %d, want %d", len(got.Folders), len(want.Folders))
	}
	// EnrichmentProgress is the field most likely to drift between the
	// builder and the streaming writer (separate query path, separate
	// envelope-emission code). CodeRabbit on PR #70 — the parity test
	// has to cover it explicitly or a totals regression slips through.
	if (got.EnrichmentProgress == nil) != (want.EnrichmentProgress == nil) {
		t.Errorf("enrichmentProgress presence: got %v, want %v",
			got.EnrichmentProgress, want.EnrichmentProgress)
	}
	if got.EnrichmentProgress != nil && want.EnrichmentProgress != nil {
		if got.EnrichmentProgress.TracksTotal != want.EnrichmentProgress.TracksTotal {
			t.Errorf("tracksTotal: got %d, want %d",
				got.EnrichmentProgress.TracksTotal, want.EnrichmentProgress.TracksTotal)
		}
		if got.EnrichmentProgress.TracksEnriched != want.EnrichmentProgress.TracksEnriched {
			t.Errorf("tracksEnriched: got %d, want %d",
				got.EnrichmentProgress.TracksEnriched, want.EnrichmentProgress.TracksEnriched)
		}
	}
}

// TestWriteManifestStreamsLargeLibraryWithoutOOM exercises the legacy
// path with enough synthetic tracks that the prior in-memory builder
// would noticeably allocate. Verifies the streamed JSON parses and
// contains every row — that's the whole behavioural contract; the
// memory bound is structural (the implementation never collects a
// []Track).
func TestWriteManifestStreamsLargeLibraryWithoutOOM(t *testing.T) {
	const n = 5000
	s, _ := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	defer s.Close()

	for i := 0; i < n; i++ {
		if err := s.UpsertTrack(context.Background(), &Track{
			Path:    fmt.Sprintf("Artist/Album/%05d.flac", i),
			Size:    1234,
			ModTime: time.Now(),
			Title:   fmt.Sprintf("Track %d", i),
		}); err != nil {
			t.Fatalf("upsert %d: %v", i, err)
		}
	}

	var buf bytes.Buffer
	if err := WriteManifest(context.Background(), &buf, s, []string{"/tmp/nope/Music"}, time.Time{}); err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}
	var mf Manifest
	if err := json.Unmarshal(buf.Bytes(), &mf); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(mf.Tracks) != n {
		t.Errorf("tracks streamed = %d, want %d", len(mf.Tracks), n)
	}
	if mf.EnrichmentProgress == nil {
		t.Error("enrichmentProgress missing")
	} else if mf.EnrichmentProgress.TracksTotal != n {
		t.Errorf("tracksTotal = %d, want %d", mf.EnrichmentProgress.TracksTotal, n)
	}
}

func TestBuildManifestSinceFilter(t *testing.T) {
	root, _ := tempLibrary(t)
	s, _ := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	defer s.Close()
	sc := NewScanner([]string{root}, s, "")
	sc.Scan(context.Background())

	future := time.Now().Add(time.Hour)
	mf, _ := BuildManifest(context.Background(), s, []string{root}, future)
	if len(mf.Tracks) != 0 {
		t.Errorf("since-future: got %d tracks, want 0", len(mf.Tracks))
	}
}

// --- hot-reloadable roots ---

func TestScannerSetRootsAppliesToNextScan(t *testing.T) {
	a, _ := tempLibrary(t)
	b := filepath.Join(t.TempDir(), "Extra")
	if err := os.MkdirAll(filepath.Join(b, "Artist C", "Album 3"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeMinimalFLAC(t,
		filepath.Join(b, "Artist C", "Album 3", "only.flac"),
		44100, 16,
		map[string]string{"TITLE": "Extra", "ARTIST": "Artist C", "ALBUM": "Album 3"},
	)

	s, _ := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	defer s.Close()

	// Start with only root A.
	sc := NewScanner([]string{a}, s, "")
	if _, err := sc.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	total, _ := s.CountTracks(context.Background())
	if total != 3 {
		t.Fatalf("root A count = %d, want 3", total)
	}

	// Transition to multi-root. A 1→N transition changes storage form
	// (tracks get a "<basename>/" prefix), so the admin flow wipes first.
	if err := s.WipeAllTracks(context.Background()); err != nil {
		t.Fatalf("Wipe: %v", err)
	}
	sc.SetRoots([]string{a, b})
	if _, err := sc.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	total, _ = s.CountTracks(context.Background())
	if total != 4 {
		t.Errorf("A+B count = %d, want 4", total)
	}

	// Per-root counts should match the multi-root storage form.
	nA, _ := s.CountTracksByPrefix(context.Background(), filepath.Base(a)+"/")
	nB, _ := s.CountTracksByPrefix(context.Background(), filepath.Base(b)+"/")
	if nA != 3 || nB != 1 {
		t.Errorf("per-root counts A=%d B=%d, want 3,1", nA, nB)
	}

	// Roots snapshot reflects the update.
	got := sc.Roots()
	if len(got) != 2 || got[0] != a || got[1] != b {
		t.Errorf("Roots() = %v, want [%q, %q]", got, a, b)
	}
}

func TestProviderWriteManifestReflectsSetRoots(t *testing.T) {
	a, _ := tempLibrary(t)
	b := filepath.Join(t.TempDir(), "Other")
	if err := os.MkdirAll(b, 0o755); err != nil {
		t.Fatal(err)
	}

	s, _ := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	defer s.Close()
	sc := NewScanner([]string{a}, s, "")
	p := NewProvider(s, sc)

	decode := func(buf *bytes.Buffer) *Manifest {
		t.Helper()
		var mf Manifest
		if err := json.Unmarshal(buf.Bytes(), &mf); err != nil {
			t.Fatalf("unmarshal manifest: %v\nbody=%s", err, buf.String())
		}
		return &mf
	}

	var pre bytes.Buffer
	if err := p.WriteManifest(context.Background(), &pre, time.Time{}); err != nil {
		t.Fatal(err)
	}
	mf := decode(&pre)
	if len(mf.LibraryRoots) != 1 || mf.LibraryRoots[0] != filepath.Base(a) {
		t.Errorf("pre-swap manifest roots = %v", mf.LibraryRoots)
	}

	sc.SetRoots([]string{a, b})
	var post bytes.Buffer
	if err := p.WriteManifest(context.Background(), &post, time.Time{}); err != nil {
		t.Fatal(err)
	}
	mf = decode(&post)
	if len(mf.LibraryRoots) != 2 {
		t.Errorf("post-swap manifest roots = %v, want 2 entries", mf.LibraryRoots)
	}
}

func TestStoreDeleteTracksByPrefix(t *testing.T) {
	s, _ := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	defer s.Close()
	for _, p := range []string{
		"A/1.flac", "A/2.flac", "B/1.flac",
		// Tracks whose path contains SQL LIKE wildcards — the ESCAPE clause
		// must treat these literally rather than as wildcards.
		"A%magic/1.flac", "A_magic/1.flac",
	} {
		if err := s.UpsertTrack(context.Background(), &Track{Path: p, Size: 1, ModTime: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	nA, _ := s.CountTracksByPrefix(context.Background(), "A/")
	if nA != 2 {
		t.Errorf("prefix A/ count = %d, want 2 (the %%/_ variants must not match)", nA)
	}
	removed, err := s.DeleteTracksByPrefix(context.Background(), "A/")
	if err != nil {
		t.Fatal(err)
	}
	if removed != 2 {
		t.Errorf("DeleteTracksByPrefix removed %d, want 2", removed)
	}
	total, _ := s.CountTracks(context.Background())
	if total != 3 {
		t.Errorf("remaining = %d, want 3 (B + two escaped)", total)
	}
}

// --- helpers ---

func mustPaths(t *testing.T, s *Store) []string {
	t.Helper()
	paths, err := s.TrackPaths(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return paths
}
