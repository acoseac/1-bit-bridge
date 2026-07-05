package manifest

import (
	"context"
	"strings"
	"testing"
)

// seedVariantKindFixture extends seedBrowseFixture's variant set
// with one optimize variant + one orphan-kind variant for the
// kind-split tests below. The base fixture seeds two upscale
// variants on MusicA/Album1/01 and MusicA/Album2/01; we add an
// optimize variant on MusicA/Album1/02 (a track the base fixture
// has but didn't variant) and an optimize variant on
// MusicA/Album1/01 (so the SAME source has both kinds — the
// senior-review cross-contamination case).
func seedVariantKindFixture(t *testing.T, s *Store) {
	t.Helper()
	seedBrowseFixture(t, s)

	more := []VariantRow{
		{
			SourcePath: "MusicA/Album1/01.flac", VariantID: "optimized-v2-44100-16",
			SidecarPath: "/tmp/a-opt.flac", Format: "flac",
			SampleRate: 44100, BitsPerSample: 16, SizeBytes: 250,
			SourceMTimeNS: 1, SourceSize: 200, SoxSettings: "{}", CreatedAt: 1,
		},
		{
			SourcePath: "MusicA/Album1/02.flac", VariantID: "optimized-v2-44100-16",
			SidecarPath: "/tmp/b-opt.flac", Format: "flac",
			SampleRate: 44100, BitsPerSample: 16, SizeBytes: 400,
			SourceMTimeNS: 1, SourceSize: 300, SoxSettings: "{}", CreatedAt: 1,
		},
	}
	for _, v := range more {
		if err := s.UpsertVariant(context.Background(), v); err != nil {
			t.Fatalf("UpsertVariant %q: %v", v.VariantID, err)
		}
	}
}

// TestListChildFolders_SplitsVariantCountsByKind asserts the
// per-kind rollup at the top-level browse: MusicA carries two
// upscale variants (Album1/01, Album2/01) AND two optimize
// variants (Album1/01, Album1/02). Same source can carry both
// kinds — the COUNT(DISTINCT source_path) is per-kind, so the
// upscale count and optimize count are computed independently.
func TestListChildFolders_SplitsVariantCountsByKind(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })
	seedVariantKindFixture(t, s)

	rows, err := s.ListChildFolders(context.Background(), "")
	if err != nil {
		t.Fatalf("ListChildFolders(\"\"): %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d top-level folders, want 2", len(rows))
	}
	musicA := rows[0]
	if musicA.UpscaledTrackCount != 2 {
		t.Errorf("MusicA UpscaledTrackCount = %d, want 2", musicA.UpscaledTrackCount)
	}
	if musicA.OptimizedTrackCount != 2 {
		t.Errorf("MusicA OptimizedTrackCount = %d, want 2", musicA.OptimizedTrackCount)
	}
	if musicA.UpscaledSizeBytes != 2500 {
		t.Errorf("MusicA UpscaledSizeBytes = %d, want 2500", musicA.UpscaledSizeBytes)
	}
	if musicA.OptimizedSizeBytes != 650 {
		t.Errorf("MusicA OptimizedSizeBytes = %d, want 650 (250+400)", musicA.OptimizedSizeBytes)
	}
	// MusicB carries no variants of either kind.
	musicB := rows[1]
	if musicB.UpscaledTrackCount != 0 || musicB.OptimizedTrackCount != 0 {
		t.Errorf("MusicB rollup expected zero per-kind, got %+v", musicB)
	}
}

// TestListChildTracks_SplitsIsUpscaledIsOptimizedByKind: a single
// track can have both an upscale and an optimize variant; the row
// shape carries both flags independently. Without per-kind EXISTS
// the prior shape would have a single IsUpscaled flag that's true
// for ANY variant kind — losing the discrimination.
func TestListChildTracks_SplitsIsUpscaledIsOptimizedByKind(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })
	seedVariantKindFixture(t, s)

	rows, err := s.ListChildTracks(context.Background(), "MusicA/Album1")
	if err != nil {
		t.Fatalf("ListChildTracks: %v", err)
	}
	// Album1 has two tracks (01, 02).
	// Track 01 has both upscale + optimize variants.
	// Track 02 has only the optimize variant (no upscale variant in seed).
	if len(rows) != 2 {
		t.Fatalf("got %d tracks, want 2", len(rows))
	}
	byPath := make(map[string]ChildTrack, len(rows))
	for _, r := range rows {
		byPath[r.Path] = r
	}
	t01, ok := byPath["MusicA/Album1/01.flac"]
	if !ok {
		t.Fatalf("01.flac missing from rows: %+v", rows)
	}
	if !t01.IsUpscaled || !t01.IsOptimized {
		t.Errorf("01.flac flags: IsUpscaled=%v IsOptimized=%v; want both true",
			t01.IsUpscaled, t01.IsOptimized)
	}
	t02, ok := byPath["MusicA/Album1/02.flac"]
	if !ok {
		t.Fatalf("02.flac missing from rows: %+v", rows)
	}
	if t02.IsUpscaled {
		t.Errorf("02.flac IsUpscaled = true, want false (no upscale variant seeded)")
	}
	if !t02.IsOptimized {
		t.Errorf("02.flac IsOptimized = false, want true (optimize variant seeded)")
	}
}

// TestListTrackProjectionsUnderPrefix_KindScopedHasVariant is the
// load-bearing senior-review fix: a track with ONLY an upscale
// variant must show as `HasVariant=false` under kind=optimize, so
// the optimize projection correctly counts it as eligible (not
// "already covered"). Pre-fix the EXISTS subquery was kind-
// agnostic and the optimize projection mis-counted upscaled tracks
// as covered, silently zeroing the projected file count.
func TestListTrackProjectionsUnderPrefix_KindScopedHasVariant(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })
	seedBrowseFixture(t, s) // base seed: 2 upscale variants, no optimize

	// Upscale projection: tracks with upscale variants ARE covered.
	upscale, err := s.ListTrackProjectionsUnderPrefix(context.Background(), "MusicA", VariantKindPrefixUpscaled)
	if err != nil {
		t.Fatalf("upscale projection: %v", err)
	}
	upscaleCovered := 0
	for _, p := range upscale {
		if p.HasVariant {
			upscaleCovered++
		}
	}
	if upscaleCovered != 2 {
		t.Errorf("upscale HasVariant count: %d, want 2 (Album1/01 + Album2/01)", upscaleCovered)
	}

	// Optimize projection on the SAME data: same tracks must NOT
	// show as covered — they have no optimize variants.
	optimize, err := s.ListTrackProjectionsUnderPrefix(context.Background(), "MusicA", VariantKindPrefixOptimized)
	if err != nil {
		t.Fatalf("optimize projection: %v", err)
	}
	optimizeCovered := 0
	for _, p := range optimize {
		if p.HasVariant {
			optimizeCovered++
		}
	}
	if optimizeCovered != 0 {
		t.Errorf("optimize HasVariant count: %d, want 0 (no optimize variants seeded)", optimizeCovered)
	}
}

// TestListTrackProjectionsUnderPrefix_BindingOrder pins the
// senior-review note that the `variantPrefix` placeholder sits
// inside the SELECT-block EXISTS subquery (positionally BEFORE
// the WHERE clause's `pattern` placeholder). Swapping the
// bindings would have SQLite searching track paths for the
// variant-prefix string — silently zero rows.
//
// Test shape: seed tracks under "MusicA/" (matches `MusicA/%`),
// run a projection with variantPrefix="upscaled". A regression
// that swapped the bindings would search for `t.path LIKE
// 'upscaled-%'` and return zero rows. We assert non-zero rows.
func TestListTrackProjectionsUnderPrefix_BindingOrder(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })
	seedBrowseFixture(t, s)

	projs, err := s.ListTrackProjectionsUnderPrefix(context.Background(), "MusicA", VariantKindPrefixUpscaled)
	if err != nil {
		t.Fatalf("projection: %v", err)
	}
	if len(projs) == 0 {
		t.Fatalf("zero rows — binding order regressed (variantPrefix and pattern swapped?)")
	}
}

// TestCountVariantsByKind_classifiesPrefixesAndSumsSizes asserts
// the CASE aggregate sums per kind correctly. Seeds two upscale
// (1000+1500=2500 bytes) + two optimize (250+400=650 bytes)
// variants.
func TestCountVariantsByKind_classifiesPrefixesAndSumsSizes(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })
	seedVariantKindFixture(t, s)

	got, err := s.CountVariantsByKind(context.Background())
	if err != nil {
		t.Fatalf("CountVariantsByKind: %v", err)
	}
	if got["upscale"] != 2500 {
		t.Errorf("upscale sum = %d, want 2500", got["upscale"])
	}
	if got["optimize"] != 650 {
		t.Errorf("optimize sum = %d, want 650", got["optimize"])
	}
	// "unknown" key should be absent (defensive bucket only
	// surfaces when seeded variants would land there).
	if _, ok := got["unknown"]; ok {
		t.Errorf("unknown bucket present: %v", got)
	}
}

// TestCountVariantsByKind_unknownBucketSurfacesUnclassifiedRows
// covers the defensive "unknown" bucket: a variant_id that doesn't
// match either documented prefix lands in `unknown` so the helper
// surfaces drift instead of silently dropping the row.
func TestCountVariantsByKind_unknownBucketSurfacesUnclassifiedRows(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })
	if err := s.UpsertFolder(context.Background(), &Folder{Path: "X"}); err != nil {
		t.Fatal(err)
	}
	// Insert a track + variant whose variant_id matches NEITHER
	// `upscaled-%` nor `optimized-%` — simulates a future kind
	// landing in the table before the helper is updated.
	if _, err := s.db.Exec(`
		INSERT INTO tracks(path, size, mtime_ns, tags_json, indexed_at)
		VALUES (?,?,?,?,?)
	`, "X/track.flac", int64(100), int64(1), []byte(`{"sampleRate":44100,"bitsPerSample":16,"codec":"FLAC","isDSD":false}`), int64(1)); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertVariant(context.Background(), VariantRow{
		SourcePath: "X/track.flac", VariantID: "synthesized-v1-2822400-1",
		SidecarPath: "/tmp/x.dsf", Format: "dsf",
		SampleRate: 2822400, BitsPerSample: 1, SizeBytes: 999,
		SourceMTimeNS: 1, SourceSize: 100, SoxSettings: "{}", CreatedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.CountVariantsByKind(context.Background())
	if err != nil {
		t.Fatalf("CountVariantsByKind: %v", err)
	}
	if got["unknown"] != 999 {
		t.Errorf("unknown bucket = %d, want 999 (synthesized- prefix shouldn't match either kind)", got["unknown"])
	}
	if got["upscale"] != 0 || got["optimize"] != 0 {
		t.Errorf("known buckets bled into: %+v", got)
	}
}

// TestRollupByPrefix_SplitsVariantSizesByKind asserts the legacy
// recursive rollup helper also splits by kind, so any future
// caller gets honest per-kind values.
func TestRollupByPrefix_SplitsVariantSizesByKind(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })
	seedVariantKindFixture(t, s)

	r, err := s.RollupByPrefix(context.Background(), "MusicA")
	if err != nil {
		t.Fatalf("RollupByPrefix: %v", err)
	}
	if r.UpscaledTrackCount != 2 || r.UpscaledSizeBytes != 2500 {
		t.Errorf("upscale rollup: count=%d size=%d, want 2/2500", r.UpscaledTrackCount, r.UpscaledSizeBytes)
	}
	if r.OptimizedTrackCount != 2 || r.OptimizedSizeBytes != 650 {
		t.Errorf("optimize rollup: count=%d size=%d, want 2/650", r.OptimizedTrackCount, r.OptimizedSizeBytes)
	}
}

// TestHumanLabelForVariant pins the picker/admin label rendering across
// both kinds AND both the FLAC + non-FLAC format branches. The non-FLAC
// case is the regression guard for the fix: the `default` branch MUST keep
// the operator-facing kind prefix and upper-case the format (previously it
// dropped `kind` and emitted the raw lower-case format, e.g. "wav 24/192").
func TestHumanLabelForVariant(t *testing.T) {
	cases := []struct {
		name string
		v    Variant
		want string
	}{
		{"flac upscaled 44.1 family", Variant{ID: "upscaled-v2-176400-24", Format: "flac", SampleRate: 176400, BitsPerSample: 24}, "Upscaled FLAC 24/176.4"},
		{"flac optimized 44.1", Variant{ID: "optimized-v2-44100-16", Format: "flac", SampleRate: 44100, BitsPerSample: 16}, "Optimized FLAC 16/44.1"},
		{"non-flac upscaled keeps kind + upper format", Variant{ID: "upscaled-v2-192000-24", Format: "wav", SampleRate: 192000, BitsPerSample: 24}, "Upscaled WAV 24/192"},
		{"non-flac optimized keeps kind + upper format", Variant{ID: "optimized-v2-48000-16", Format: "alac", SampleRate: 48000, BitsPerSample: 16}, "Optimized ALAC 16/48"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := humanLabelForVariant(tc.v); got != tc.want {
				t.Errorf("humanLabelForVariant(%+v) = %q, want %q", tc.v, got, tc.want)
			}
		})
	}
}

// TestVariantKindPrefixConstants pins the exported constant strings
// so a future refactor that renames them doesn't silently produce
// SQL that filters by an empty / mis-typed prefix. The bridge-side
// `transcode.VariantPrefixUpscaled` / `VariantPrefixOptimized`
// MUST stay in lockstep — duplicated here because manifest can't
// import transcode (circular).
func TestVariantKindPrefixConstants(t *testing.T) {
	if VariantKindPrefixUpscaled != "upscaled" {
		t.Errorf("VariantKindPrefixUpscaled = %q, want \"upscaled\"", VariantKindPrefixUpscaled)
	}
	if VariantKindPrefixOptimized != "optimized" {
		t.Errorf("VariantKindPrefixOptimized = %q, want \"optimized\"", VariantKindPrefixOptimized)
	}
	// Sanity: neither carries trailing dash; LIKE pattern construction
	// appends `-%` at the call site.
	if strings.HasSuffix(VariantKindPrefixUpscaled, "-") ||
		strings.HasSuffix(VariantKindPrefixOptimized, "-") {
		t.Errorf("kind prefixes should not carry trailing dash")
	}
}
