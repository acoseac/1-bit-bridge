package manifest

import (
	"context"
	"testing"
	"time"
)

func catTrack(path, title, album, albumArtist string) *Track {
	return &Track{Path: path, Title: title, Album: album, AlbumArtist: albumArtist,
		Size: 1000, ModTime: time.Unix(1, 0)}
}

// TestStreamCatalogRefsExcludesSuppressed pins the served-set rule one
// level out from /v1: the player is a listener surface, so an operator
// must not be able to queue a track the phone's manifest doesn't
// contain, and album counts must agree with iOS.
func TestStreamCatalogRefsExcludesSuppressed(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	for _, p := range []string{"A/keep.flac", "A/hide.flac"} {
		if err := s.UpsertTrack(ctx, catTrack(p, "T", "Al", "AA")); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE tracks SET dupe_suppressed = 1 WHERE path = ?`, "A/hide.flac"); err != nil {
		t.Fatal(err)
	}
	var got []string
	if err := s.StreamCatalogRefs(ctx, func(r CatalogRef) error {
		got = append(got, r.Path)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "A/keep.flac" {
		t.Errorf("streamed %v, want only A/keep.flac", got)
	}
}

// TestStreamCatalogRefsIncludesRoutedWithoutFanout — routed rows are
// the overwhelming majority on a hybrid library, so they must be
// present; and the LEFT JOIN must yield exactly one row per track even
// when a routing row exists, or every routed album's track count is
// silently inflated.
func TestStreamCatalogRefsIncludesRoutedWithoutFanout(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if err := s.UpsertTrack(ctx, catTrack("2go/A/x.flac", "T", "Al", "AA")); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertTrack(ctx, catTrack("local/A/y.flac", "T", "Al", "AA")); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertUPnPRouting(ctx, &UPnPRouting{
		SourcePath: "2go/A/x.flac", ServerUDN: "uuid:srv", ObjectID: "1",
		ResURL: "http://host/1.flac", LastSeenAt: time.Unix(1, 0),
	}); err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	udns := map[string]string{}
	if err := s.StreamCatalogRefs(ctx, func(r CatalogRef) error {
		counts[r.Path]++
		udns[r.Path] = r.RoutedUDN
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if counts["2go/A/x.flac"] != 1 {
		t.Errorf("routed track yielded %d rows, want exactly 1 — a fan-out would "+
			"inflate every routed album's track count", counts["2go/A/x.flac"])
	}
	if udns["2go/A/x.flac"] != "uuid:srv" {
		t.Errorf("routed UDN = %q, want uuid:srv", udns["2go/A/x.flac"])
	}
	// Assert PRESENCE explicitly. Reading udns["local/A/y.flac"] and
	// comparing to "" passes vacuously when the row is missing
	// altogether — which is exactly what an INNER JOIN would do, and
	// exactly how the first version of this test passed under that
	// control.
	if counts["local/A/y.flac"] != 1 {
		t.Errorf("filesystem track yielded %d rows, want 1 — the join must be a LEFT "+
			"JOIN or every non-routed track vanishes from the catalog",
			counts["local/A/y.flac"])
	}
	if udns["local/A/y.flac"] != "" {
		t.Errorf("filesystem track UDN = %q, want empty", udns["local/A/y.flac"])
	}
}

// TestStreamCatalogRefsDistinguishesAbsentFromZero — an explicit disc
// or track 0 is a VALUE; only absence licenses the folder/filename
// inference. Collapsing the two would change album ordering and group
// membership, which is why the query reads them without COALESCE.
func TestStreamCatalogRefsDistinguishesAbsentFromZero(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	zero := 0
	withZero := catTrack("A/zero.flac", "T", "Al", "AA")
	withZero.DiscNumber = &zero
	withZero.TrackNumber = &zero
	if err := s.UpsertTrack(ctx, withZero); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertTrack(ctx, catTrack("A/absent.flac", "T", "Al", "AA")); err != nil {
		t.Fatal(err)
	}
	got := map[string]CatalogRef{}
	if err := s.StreamCatalogRefs(ctx, func(r CatalogRef) error {
		got[r.Path] = r
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if z := got["A/zero.flac"]; !z.DiscTagged || z.Disc != 0 || !z.TrackTagged || z.Track != 0 {
		t.Errorf("explicit zero must read as TAGGED zero, got disc=%d/%v track=%d/%v",
			z.Disc, z.DiscTagged, z.Track, z.TrackTagged)
	}
	if a := got["A/absent.flac"]; a.DiscTagged || a.TrackTagged {
		t.Errorf("absent tags must read as untagged, got disc=%v track=%v",
			a.DiscTagged, a.TrackTagged)
	}
}

// TestStreamCatalogRefsReusesItsStruct pins the callback contract the
// whole streaming family shares — a caller that retains the value sees
// it mutate, which is why librarycat copies.
func TestStreamCatalogRefsReusesItsStruct(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	for _, p := range []string{"A/1.flac", "A/2.flac"} {
		if err := s.UpsertTrack(ctx, catTrack(p, "T"+p, "Al", "AA")); err != nil {
			t.Fatal(err)
		}
	}
	var seen []*CatalogRef
	if err := s.StreamCatalogRefs(ctx, func(r CatalogRef) error {
		seen = append(seen, &r) //nolint:exportloopref // deliberately retaining, see below
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// Go copies the struct into the callback parameter, so retaining
	// &r is safe HERE; the contract exists because the scan target
	// upstream is reused. Assert the values that reached the callback
	// were distinct — i.e. the reset between rows actually happens.
	if len(seen) != 2 || seen[0].Path == seen[1].Path {
		t.Errorf("expected two distinct rows, got %d with paths %v", len(seen), seen)
	}
}

func TestCatalogTrackRowsForPaths(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	tr := catTrack("A/x.flac", "Title", "Album", "AlbumArtist")
	dur := 123.5
	rate := 96000.0
	bits := 24
	tr.Duration = &dur
	tr.SampleRate = &rate
	tr.BitsPerSample = &bits
	tr.Codec = "FLAC"
	if err := s.UpsertTrack(ctx, tr); err != nil {
		t.Fatal(err)
	}
	got, err := s.CatalogTrackRowsForPaths(ctx, []string{"A/x.flac", "A/missing.flac"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d rows, want 1 — a missing path must be absent, not an error", len(got))
	}
	r := got[0]
	if r.Title != "Title" || r.Codec != "FLAC" || r.SampleRate != 96000 ||
		r.BitsPerSample != 24 || r.Duration != 123.5 {
		t.Errorf("hydrated row wrong: %+v", r)
	}
	if empty, err := s.CatalogTrackRowsForPaths(ctx, nil); err != nil || empty != nil {
		t.Errorf("empty input: got %v, %v; want nil, nil", empty, err)
	}
}

// TestCatalogTrackRowsForPathsChunks exercises the >400 path chunk
// boundary — the one place an off-by-one silently drops a page of an
// album.
func TestCatalogTrackRowsForPathsChunks(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	var paths []string
	for i := 0; i < 905; i++ {
		p := "A/" + string(rune('a'+i%26)) + "/" + itoa(i) + ".flac"
		paths = append(paths, p)
		if err := s.UpsertTrack(ctx, catTrack(p, "T", "Al", "AA")); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.CatalogTrackRowsForPaths(ctx, paths)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(paths) {
		t.Errorf("got %d rows across the chunk boundary, want %d", len(got), len(paths))
	}
}

func TestVariantsForPaths(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if err := s.UpsertTrack(ctx, catTrack("A/x.flac", "T", "Al", "AA")); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertVariant(ctx, VariantRow{
		SourcePath: "A/x.flac", VariantID: "optimized-v2-44100-16",
		SidecarPath: "/v/x.flac", Format: "flac", SampleRate: 44100,
		BitsPerSample: 16, SizeBytes: 10, SourceMTimeNS: 42, SourceSize: 1000,
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.VariantsForPaths(ctx, []string{"A/x.flac", "A/none.flac"})
	if err != nil {
		t.Fatal(err)
	}
	rows := got["A/x.flac"]
	if len(rows) != 1 || rows[0].VariantID != "optimized-v2-44100-16" {
		t.Fatalf("variants = %+v", rows)
	}
	// The freshness inputs must survive the round-trip; without them the
	// caller cannot tell a playable sidecar from a stale one.
	if rows[0].SourceMTimeNS != 42 || rows[0].SourceSize != 1000 {
		t.Errorf("freshness fields lost: mtime=%d size=%d",
			rows[0].SourceMTimeNS, rows[0].SourceSize)
	}
	if _, ok := got["A/none.flac"]; ok {
		t.Error("a path with no variants must be absent from the map")
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
