package manifest

import (
	"context"
	"testing"
)

// seedKindVariants inserts one parent track + the given variants so
// VariantStatsByKind has rows to group. Source paths are distinct so
// the variant rows don't collide on the (source_path, variant_id) PK.
func seedKindVariants(t *testing.T, s *Store, variants []VariantRow) {
	t.Helper()
	seen := map[string]bool{}
	for _, v := range variants {
		if !seen[v.SourcePath] {
			if _, err := s.db.Exec(`
				INSERT INTO tracks(path, size, mtime_ns, tags_json, indexed_at)
				VALUES (?,?,?,?,?)
			`, v.SourcePath, int64(100), int64(1), []byte(`{"codec":"FLAC"}`), int64(1)); err != nil {
				t.Fatalf("insert track %q: %v", v.SourcePath, err)
			}
			seen[v.SourcePath] = true
		}
		if err := s.UpsertVariant(context.Background(), v); err != nil {
			t.Fatalf("UpsertVariant %q/%q: %v", v.SourcePath, v.VariantID, err)
		}
	}
}

// TestVariantStatsByKind_PreseedsEmptyTable asserts the method returns
// zero-valued upscale + optimize entries (never a nil/absent key) when
// track_variants is empty — the JSON-shape-stability contract the admin
// frontend relies on.
func TestVariantStatsByKind_PreseedsEmptyTable(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })

	got, err := s.VariantStatsByKind(context.Background())
	if err != nil {
		t.Fatalf("VariantStatsByKind: %v", err)
	}
	for _, kind := range []string{"upscale", "optimize"} {
		st, ok := got[kind]
		if !ok {
			t.Errorf("missing pre-seeded key %q", kind)
		}
		if st.Files != 0 || st.Bytes != 0 {
			t.Errorf("%q = %+v, want zero", kind, st)
		}
	}
	if _, ok := got["unknown"]; ok {
		t.Errorf("unexpected unknown bucket on empty table")
	}
}

// TestVariantStatsByKind_SplitsByPrefix verifies file counts + byte sums
// are bucketed by the upscaled-/optimized- variant_id prefix. A single
// source track carrying two upscaled targets must count as 2 files (file
// count, not DISTINCT-source).
func TestVariantStatsByKind_SplitsByPrefix(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })

	seedKindVariants(t, s, []VariantRow{
		{SourcePath: "A/01.flac", VariantID: "upscaled-v2-192000-24", SidecarPath: "/tmp/a1.flac", Format: "flac", SampleRate: 192000, BitsPerSample: 24, SizeBytes: 1000, SourceMTimeNS: 1, SourceSize: 100, SoxSettings: "{}", CreatedAt: 1},
		{SourcePath: "A/01.flac", VariantID: "upscaled-v2-176400-24", SidecarPath: "/tmp/a2.flac", Format: "flac", SampleRate: 176400, BitsPerSample: 24, SizeBytes: 1500, SourceMTimeNS: 1, SourceSize: 100, SoxSettings: "{}", CreatedAt: 1},
		{SourcePath: "B/01.flac", VariantID: "optimized-v2-44100-16", SidecarPath: "/tmp/b1.flac", Format: "flac", SampleRate: 44100, BitsPerSample: 16, SizeBytes: 400, SourceMTimeNS: 1, SourceSize: 100, SoxSettings: "{}", CreatedAt: 1},
	})

	got, err := s.VariantStatsByKind(context.Background())
	if err != nil {
		t.Fatalf("VariantStatsByKind: %v", err)
	}
	if up := got["upscale"]; up.Files != 2 || up.Bytes != 2500 {
		t.Errorf("upscale = %+v, want {Files:2 Bytes:2500}", up)
	}
	if opt := got["optimize"]; opt.Files != 1 || opt.Bytes != 400 {
		t.Errorf("optimize = %+v, want {Files:1 Bytes:400}", opt)
	}
}

// TestRollupByPrefix_GlobalFastPathMatchesPrefixForm pins that the
// empty-prefix fast path (no LIKE clause) returns the same per-kind
// DISTINCT-source counts + byte sums the general prefix form would for
// the whole library.
func TestRollupByPrefix_GlobalFastPathMatchesPrefixForm(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })

	seedKindVariants(t, s, []VariantRow{
		{SourcePath: "A/01.flac", VariantID: "upscaled-v2-192000-24", SidecarPath: "/tmp/a1.flac", Format: "flac", SampleRate: 192000, BitsPerSample: 24, SizeBytes: 1000, SourceMTimeNS: 1, SourceSize: 100, SoxSettings: "{}", CreatedAt: 1},
		// Second upscaled target on the SAME source — pins that the
		// per-kind track count uses COUNT(DISTINCT source_path) (still 1
		// track) while the byte sum adds up (1000+600). A regression to
		// COUNT(*) would surface as UpscaledTrackCount==2 here.
		{SourcePath: "A/01.flac", VariantID: "upscaled-v2-176400-24", SidecarPath: "/tmp/a1b.flac", Format: "flac", SampleRate: 176400, BitsPerSample: 24, SizeBytes: 600, SourceMTimeNS: 1, SourceSize: 100, SoxSettings: "{}", CreatedAt: 1},
		{SourcePath: "A/02.flac", VariantID: "optimized-v2-44100-16", SidecarPath: "/tmp/a2.flac", Format: "flac", SampleRate: 44100, BitsPerSample: 16, SizeBytes: 400, SourceMTimeNS: 1, SourceSize: 100, SoxSettings: "{}", CreatedAt: 1},
	})

	r, err := s.RollupByPrefix(context.Background(), "")
	if err != nil {
		t.Fatalf("RollupByPrefix(\"\"): %v", err)
	}
	if r.TrackCount != 2 {
		t.Errorf("TrackCount = %d, want 2", r.TrackCount)
	}
	if r.UpscaledTrackCount != 1 || r.UpscaledSizeBytes != 1600 {
		t.Errorf("upscaled = (%d,%d), want (1,1600)", r.UpscaledTrackCount, r.UpscaledSizeBytes)
	}
	if r.OptimizedTrackCount != 1 || r.OptimizedSizeBytes != 400 {
		t.Errorf("optimized = (%d,%d), want (1,400)", r.OptimizedTrackCount, r.OptimizedSizeBytes)
	}
}
