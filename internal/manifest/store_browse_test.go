package manifest

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
)

// seedBrowseFixture installs a small library across two roots so
// the browse tests exercise multi-root, nested folders, variants,
// and the rollup math.
//
// Layout:
//
//	MusicA/
//	  Album1/
//	    01.flac  (has variant)
//	    02.flac
//	  Album2/
//	    01.flac  (has variant)
//	MusicB/
//	  Album3/
//	    01.flac
//
// Folder rows: MusicA, MusicA/Album1, MusicA/Album2, MusicB, MusicB/Album3
// Track rows: MusicA/Album1/01.flac (200), MusicA/Album1/02.flac (300),
//
//	MusicA/Album2/01.flac (500), MusicB/Album3/01.flac (700)
//
// Variants: MusicA/Album1/01.flac → 1000 bytes
//
//	MusicA/Album2/01.flac → 1500 bytes
func seedBrowseFixture(t *testing.T, s *Store) {
	t.Helper()
	folders := []string{
		"MusicA",
		"MusicA/Album1",
		"MusicA/Album2",
		"MusicB",
		"MusicB/Album3",
	}
	for _, p := range folders {
		if err := s.UpsertFolder(context.Background(), &Folder{Path: p}); err != nil {
			t.Fatalf("UpsertFolder %q: %v", p, err)
		}
	}
	// Seed tags so browse SQL's json_extract returns numbers.
	tags := func(rate float64, bits int) []byte {
		raw := map[string]any{
			"sampleRate":    rate,
			"bitsPerSample": bits,
			"isDSD":         false,
			"codec":         "FLAC",
		}
		b, _ := json.Marshal(raw)
		return b
	}
	tracks := []struct {
		path string
		size int64
		rate float64
		bits int
	}{
		{"MusicA/Album1/01.flac", 200, 44100, 16},
		{"MusicA/Album1/02.flac", 300, 44100, 16},
		{"MusicA/Album2/01.flac", 500, 96000, 24},
		{"MusicB/Album3/01.flac", 700, 48000, 24},
	}
	for _, tr := range tracks {
		_, err := s.db.Exec(`
			INSERT INTO tracks(path, size, mtime_ns, tags_json, indexed_at)
			VALUES (?,?,?,?,?)
		`, tr.path, tr.size, int64(1), tags(tr.rate, tr.bits), int64(1))
		if err != nil {
			t.Fatalf("insert track %q: %v", tr.path, err)
		}
	}
	// Two variants — one per "covered" track.
	variants := []VariantRow{
		{
			SourcePath: "MusicA/Album1/01.flac", VariantID: "upscaled-v2-192000-24",
			SidecarPath: "/tmp/a.flac", Format: "flac",
			SampleRate: 192000, BitsPerSample: 24, SizeBytes: 1000,
			SourceMTimeNS: 1, SourceSize: 200, SoxSettings: "{}", CreatedAt: 1,
		},
		{
			SourcePath: "MusicA/Album2/01.flac", VariantID: "upscaled-v2-192000-24",
			SidecarPath: "/tmp/b.flac", Format: "flac",
			SampleRate: 192000, BitsPerSample: 24, SizeBytes: 1500,
			SourceMTimeNS: 1, SourceSize: 500, SoxSettings: "{}", CreatedAt: 1,
		},
	}
	for _, v := range variants {
		if err := s.UpsertVariant(context.Background(), v); err != nil {
			t.Fatalf("UpsertVariant %q: %v", v.SourcePath, err)
		}
	}
}

// TestListChildFolders_EmptyParentReturnsTopLevel covers the
// multi-root case at the root browse: two basename folders
// (MusicA, MusicB) returned, each carrying its subtree rollup.
func TestListChildFolders_EmptyParentReturnsTopLevel(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })
	seedBrowseFixture(t, s)

	rows, err := s.ListChildFolders(context.Background(), "")
	if err != nil {
		t.Fatalf("ListChildFolders(\"\"): %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d top-level folders, want 2", len(rows))
	}
	if rows[0].Path != "MusicA" || rows[1].Path != "MusicB" {
		t.Errorf("paths: got %v", []string{rows[0].Path, rows[1].Path})
	}
	// MusicA subtree: 3 tracks (200+300+500=1000 bytes), 2 upscaled
	// (1000+1500=2500 bytes).
	if rows[0].TrackCount != 3 {
		t.Errorf("MusicA TrackCount = %d, want 3", rows[0].TrackCount)
	}
	if rows[0].UpscaledTrackCount != 2 {
		t.Errorf("MusicA UpscaledTrackCount = %d, want 2", rows[0].UpscaledTrackCount)
	}
	if rows[0].TotalSizeBytes != 1000 {
		t.Errorf("MusicA TotalSizeBytes = %d, want 1000", rows[0].TotalSizeBytes)
	}
	if rows[0].UpscaledSizeBytes != 2500 {
		t.Errorf("MusicA UpscaledSizeBytes = %d, want 2500", rows[0].UpscaledSizeBytes)
	}
	// MusicB subtree: 1 track (700), 0 upscaled.
	if rows[1].TrackCount != 1 || rows[1].UpscaledTrackCount != 0 ||
		rows[1].TotalSizeBytes != 700 || rows[1].UpscaledSizeBytes != 0 {
		t.Errorf("MusicB rollup wrong: %+v", rows[1])
	}
}

// TestListChildFolders_NestedParentReturnsOneLevelOnly verifies
// the SQL's substr-instr filter exclusively returns direct
// children (no transitive descendants).
func TestListChildFolders_NestedParentReturnsOneLevelOnly(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })
	seedBrowseFixture(t, s)

	rows, err := s.ListChildFolders(context.Background(), "MusicA")
	if err != nil {
		t.Fatalf("ListChildFolders(MusicA): %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d, want 2 (Album1, Album2)", len(rows))
	}
	if rows[0].Path != "MusicA/Album1" || rows[1].Path != "MusicA/Album2" {
		t.Errorf("paths: got %v", []string{rows[0].Path, rows[1].Path})
	}
	// Album1 subtree: 2 tracks, 1 upscaled.
	if rows[0].TrackCount != 2 || rows[0].UpscaledTrackCount != 1 {
		t.Errorf("Album1 rollup: %+v", rows[0])
	}
	// Album2 subtree: 1 track, 1 upscaled.
	if rows[1].TrackCount != 1 || rows[1].UpscaledTrackCount != 1 {
		t.Errorf("Album2 rollup: %+v", rows[1])
	}
}

// TestListChildFolders_DeepestLevelReturnsEmpty asserts a leaf
// folder (no sub-folders) returns an empty slice. Important: the
// caller's UI should still try ListChildTracks at that level.
func TestListChildFolders_DeepestLevelReturnsEmpty(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })
	seedBrowseFixture(t, s)

	rows, err := s.ListChildFolders(context.Background(), "MusicA/Album1")
	if err != nil {
		t.Fatalf("ListChildFolders(MusicA/Album1): %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("got %d, want 0 (no nested sub-folders)", len(rows))
	}
}

// TestListChildTracks_PicksUpJSONFields verifies sample rate /
// bits / codec / isDSD survive the json_extract → Go pointer
// round-trip, and is_upscaled flips on a variant existence.
func TestListChildTracks_PicksUpJSONFields(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })
	seedBrowseFixture(t, s)

	rows, err := s.ListChildTracks(context.Background(), "MusicA/Album1")
	if err != nil {
		t.Fatalf("ListChildTracks: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d, want 2", len(rows))
	}
	// First track: 01.flac, has variant.
	if rows[0].Path != "MusicA/Album1/01.flac" {
		t.Errorf("rows[0].Path = %q", rows[0].Path)
	}
	if rows[0].SampleRate == nil || *rows[0].SampleRate != 44100 {
		t.Errorf("rows[0].SampleRate: %v", rows[0].SampleRate)
	}
	if rows[0].BitsPerSample == nil || *rows[0].BitsPerSample != 16 {
		t.Errorf("rows[0].BitsPerSample: %v", rows[0].BitsPerSample)
	}
	if rows[0].Codec != "FLAC" {
		t.Errorf("rows[0].Codec = %q", rows[0].Codec)
	}
	if rows[0].IsDSD == nil || *rows[0].IsDSD {
		t.Errorf("rows[0].IsDSD: %v", rows[0].IsDSD)
	}
	if !rows[0].IsUpscaled {
		t.Errorf("rows[0].IsUpscaled: want true")
	}
	// Second track: 02.flac, no variant.
	if rows[1].IsUpscaled {
		t.Errorf("rows[1].IsUpscaled: want false")
	}
}

// TestRollupByPrefix_EmptyMatchesAll asserts the empty-prefix
// case rolls up across the entire library.
func TestRollupByPrefix_EmptyMatchesAll(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })
	seedBrowseFixture(t, s)

	r, err := s.RollupByPrefix(context.Background(), "")
	if err != nil {
		t.Fatalf("RollupByPrefix(\"\"): %v", err)
	}
	if r.TrackCount != 4 {
		t.Errorf("TrackCount = %d, want 4", r.TrackCount)
	}
	if r.UpscaledTrackCount != 2 {
		t.Errorf("UpscaledTrackCount = %d, want 2", r.UpscaledTrackCount)
	}
	if r.TotalSizeBytes != 1700 {
		t.Errorf("TotalSizeBytes = %d, want 1700", r.TotalSizeBytes)
	}
	if r.UpscaledSizeBytes != 2500 {
		t.Errorf("UpscaledSizeBytes = %d, want 2500", r.UpscaledSizeBytes)
	}
}

// TestRollupByPrefix_LikeEscapeProtectsAgainstWildcard plants a
// folder containing `%` in its name and verifies the rollup for a
// SISTER folder doesn't include it. Without likeEscape on the
// prefix, the `%` in the sister query would match the bogus
// folder's contents.
func TestRollupByPrefix_LikeEscapeProtectsAgainstWildcard(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })

	// Folder shaped "Test_A" and a separate "Test%A" — without
	// LIKE-escape the underscore in "Test_A" would match any
	// single character including the % in "Test%A".
	folders := []string{"Test_A", "Test_A/album", "TestQA", "TestQA/album"}
	for _, p := range folders {
		if err := s.UpsertFolder(context.Background(), &Folder{Path: p}); err != nil {
			t.Fatal(err)
		}
	}
	tags, _ := json.Marshal(map[string]any{"sampleRate": 44100.0, "bitsPerSample": 16})
	for _, p := range []string{"Test_A/album/01.flac", "TestQA/album/01.flac"} {
		_, err := s.db.Exec(
			`INSERT INTO tracks(path, size, mtime_ns, tags_json, indexed_at) VALUES (?,100,1,?,1)`,
			p, tags,
		)
		if err != nil {
			t.Fatal(err)
		}
	}
	r, err := s.RollupByPrefix(context.Background(), "Test_A")
	if err != nil {
		t.Fatalf("RollupByPrefix: %v", err)
	}
	if r.TrackCount != 1 {
		t.Errorf("TrackCount = %d, want 1 (the byte-range scan matches Test_A literally, isolating it from TestQA)", r.TrackCount)
	}
}

// TestRollupAndCountByPrefix_ByteRangeMatchesLike pins that the
// index-range prefix scans (RollupByPrefix / CountTracksByPrefix, which
// dropped `LIKE 'prefix/%'` for `path >= prefix||'/' AND path < prefix||'0'`)
// return counts numerically identical to the LIKE form — including the
// trailing-boundary case where a sibling folder ("Album 2", "AlbumX")
// must NOT be counted under "Album". The expected counts are derived from
// the pre-fix LIKE query run against the same DB, so a divergence between
// the two forms fails the test.
func TestRollupAndCountByPrefix_ByteRangeMatchesLike(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()

	tagsJSON, _ := json.Marshal(map[string]any{"sampleRate": 44100.0, "bitsPerSample": 16, "codec": "FLAC"})
	// "Album 2" (space 0x20 < '/' 0x2F) sorts BEFORE "Album/"; "AlbumX"
	// ('X' 0x58 > '0' 0x30) sorts AFTER "Album0". They bracket the "Album"
	// prefix on both sides of the byte range — neither may be counted
	// under "Album".
	tracks := []struct {
		path string
		size int64
	}{
		{"Album/01.flac", 100},
		{"Album/02.flac", 200},
		{"Album/Disc 2/03.flac", 300}, // nested, still under Album
		{"Album 2/01.flac", 400},      // trailing-boundary sibling before "Album/"
		{"AlbumX/01.flac", 500},       // sibling after "Album0"
	}
	for _, tr := range tracks {
		if _, err := s.db.Exec(
			`INSERT INTO tracks(path, size, mtime_ns, tags_json, indexed_at) VALUES (?,?,1,?,1)`,
			tr.path, tr.size, tagsJSON,
		); err != nil {
			t.Fatalf("insert %q: %v", tr.path, err)
		}
	}
	// One upscaled + one optimized variant under Album, plus a variant
	// under the "Album 2" sibling that must NOT roll up into Album.
	variants := []VariantRow{
		{SourcePath: "Album/01.flac", VariantID: "upscaled-v2-192000-24", SidecarPath: "/tmp/u.flac", Format: "flac", SampleRate: 192000, BitsPerSample: 24, SizeBytes: 1000, SourceMTimeNS: 1, SourceSize: 100, SoxSettings: "{}", CreatedAt: 1},
		{SourcePath: "Album/02.flac", VariantID: "optimized-v2-48000-16", SidecarPath: "/tmp/o.flac", Format: "flac", SampleRate: 48000, BitsPerSample: 16, SizeBytes: 60, SourceMTimeNS: 1, SourceSize: 200, SoxSettings: "{}", CreatedAt: 1},
		{SourcePath: "Album 2/01.flac", VariantID: "upscaled-v2-192000-24", SidecarPath: "/tmp/s.flac", Format: "flac", SampleRate: 192000, BitsPerSample: 24, SizeBytes: 9999, SourceMTimeNS: 1, SourceSize: 400, SoxSettings: "{}", CreatedAt: 1},
	}
	for _, v := range variants {
		if err := s.UpsertVariant(ctx, v); err != nil {
			t.Fatalf("UpsertVariant %q: %v", v.SourcePath, err)
		}
	}

	for _, prefix := range []string{"Album", "Album 2", "AlbumX"} {
		// Reference: the pre-fix LIKE form, run against the same DB.
		var wantTracks int
		var wantSize int64
		if err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(*), COALESCE(SUM(size),0) FROM tracks WHERE path LIKE ? ESCAPE '\'`,
			likeEscape(prefix)+`/%`,
		).Scan(&wantTracks, &wantSize); err != nil {
			t.Fatalf("reference track count %q: %v", prefix, err)
		}
		var wantUp, wantOpt int
		if err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(DISTINCT source_path) FROM track_variants WHERE source_path LIKE ? ESCAPE '\' AND variant_id LIKE 'upscaled-%'`,
			likeEscape(prefix)+`/%`,
		).Scan(&wantUp); err != nil {
			t.Fatalf("reference upscaled count %q: %v", prefix, err)
		}
		if err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(DISTINCT source_path) FROM track_variants WHERE source_path LIKE ? ESCAPE '\' AND variant_id LIKE 'optimized-%'`,
			likeEscape(prefix)+`/%`,
		).Scan(&wantOpt); err != nil {
			t.Fatalf("reference optimized count %q: %v", prefix, err)
		}

		r, err := s.RollupByPrefix(ctx, prefix)
		if err != nil {
			t.Fatalf("RollupByPrefix(%q): %v", prefix, err)
		}
		if r.TrackCount != wantTracks {
			t.Errorf("RollupByPrefix(%q).TrackCount = %d, want %d (LIKE form)", prefix, r.TrackCount, wantTracks)
		}
		if r.TotalSizeBytes != wantSize {
			t.Errorf("RollupByPrefix(%q).TotalSizeBytes = %d, want %d", prefix, r.TotalSizeBytes, wantSize)
		}
		if r.UpscaledTrackCount != wantUp {
			t.Errorf("RollupByPrefix(%q).UpscaledTrackCount = %d, want %d (LIKE form)", prefix, r.UpscaledTrackCount, wantUp)
		}
		if r.OptimizedTrackCount != wantOpt {
			t.Errorf("RollupByPrefix(%q).OptimizedTrackCount = %d, want %d (LIKE form)", prefix, r.OptimizedTrackCount, wantOpt)
		}

		// CountTracksByPrefix takes a slash-terminated prefix.
		gotN, err := s.CountTracksByPrefix(ctx, prefix+"/")
		if err != nil {
			t.Fatalf("CountTracksByPrefix(%q): %v", prefix+"/", err)
		}
		if gotN != wantTracks {
			t.Errorf("CountTracksByPrefix(%q) = %d, want %d (LIKE form)", prefix+"/", gotN, wantTracks)
		}
	}

	// Belt-and-braces boundary assertions independent of the reference
	// math: "Album" owns exactly its own 3 tracks + 1/1 variants — never
	// the bracketing "Album 2" / "AlbumX" siblings.
	r, err := s.RollupByPrefix(ctx, "Album")
	if err != nil {
		t.Fatalf("RollupByPrefix(Album): %v", err)
	}
	if r.TrackCount != 3 {
		t.Errorf("RollupByPrefix(\"Album\").TrackCount = %d, want 3 (excludes 'Album 2' + 'AlbumX')", r.TrackCount)
	}
	if r.UpscaledTrackCount != 1 || r.OptimizedTrackCount != 1 {
		t.Errorf("RollupByPrefix(\"Album\") variants = up %d/opt %d, want 1/1 (excludes the 'Album 2' variant)", r.UpscaledTrackCount, r.OptimizedTrackCount)
	}
}

// TestListTrackProjectionsUnderPrefix_FiltersDescendants validates
// the projection-input list walks the full subtree (recursive) and
// surfaces HasVariant correctly.
func TestListTrackProjectionsUnderPrefix_FiltersDescendants(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })
	seedBrowseFixture(t, s)

	projs, err := s.ListTrackProjectionsUnderPrefix(context.Background(), "MusicA", VariantKindPrefixUpscaled)
	if err != nil {
		t.Fatalf("ListTrackProjectionsUnderPrefix: %v", err)
	}
	if len(projs) != 3 {
		t.Fatalf("got %d, want 3", len(projs))
	}
	covered := 0
	for _, p := range projs {
		if p.HasVariant {
			covered++
		}
		if p.Size <= 0 {
			t.Errorf("track %q has zero size", p.Path)
		}
		if p.SampleRate <= 0 {
			t.Errorf("track %q has zero sample rate", p.Path)
		}
	}
	if covered != 2 {
		t.Errorf("covered=%d, want 2", covered)
	}
}

// TestUpscaleBatchesTablePersistsUUID is an unrelated smoke that
// uses the seedBrowseFixture path to confirm migration v6 + v6's
// CRUD coexist with the seeded library data without FK constraint
// surprises. Keeps the suite from regressing if a future migration
// disturbs the table interaction.
func TestUpscaleBatchesTablePersistsUUID(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })
	seedBrowseFixture(t, s)

	id := uuid.Must(uuid.NewRandom())
	if err := s.InsertUpscaleBatch(context.Background(), UpscaleBatchRow{
		ID: id, Path: "MusicA", TargetRate: 192000, TargetBits: 24,
		Status: "pending", CreatedAt: 1, UpdatedAt: 1,
	}); err != nil {
		t.Fatalf("InsertUpscaleBatch: %v", err)
	}
}
