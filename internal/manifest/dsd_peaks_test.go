package manifest

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"
)

func fpk(v float64) *float64 { return &v }

func countDSDPeaks(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM dsd_peaks`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func mustUpsertVariant(t *testing.T, s *Store, v VariantRow) {
	t.Helper()
	if v.Format == "" {
		v.Format = "flac"
	}
	if v.SidecarPath == "" {
		v.SidecarPath = "/variants/" + v.SourcePath + "." + v.VariantID + ".flac"
	}
	if v.CreatedAt == 0 {
		v.CreatedAt = time.Now().UnixNano()
	}
	if err := s.UpsertVariant(context.Background(), v); err != nil {
		t.Fatalf("UpsertVariant(%s, %s): %v", v.SourcePath, v.VariantID, err)
	}
}

// TestSeedDSDPeaksProfileSpelling: v47's seed turns each existing DSD
// rendition row into a peak on the profile a render will ask for. The
// literal "a1|compact|44100|-v" is transcode.DSDPeakProfile's, pinned there
// by TestDSDPeakProfile — this package cannot import transcode, so both
// sides pinning one string is what keeps the SQL spelling in step. A silent
// row is recorded as a silent measurement, a PCM variant never, and the
// seed is idempotent (the ladder may re-run it).
func TestSeedDSDPeaksProfileSpelling(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	seedOptimizeTrack(t, s, "DSD/a.dsf", 2822400, 1, "DSF", true)
	seedOptimizeTrack(t, s, "DSD/b.dsf", 2822400, 1, "DSF", true)
	seedOptimizeTrack(t, s, "PCM/c.flac", 96000, 24, "FLAC", false)
	mA, sA := trackRowMTimeAndSize(t, s, "DSD/a.dsf")
	mB, sB := trackRowMTimeAndSize(t, s, "DSD/b.dsf")
	mC, sC := trackRowMTimeAndSize(t, s, "PCM/c.flac")
	// Rows as a pre-v47 bridge wrote them: no PeakProfile, so no peak yet.
	mustUpsertVariant(t, s, VariantRow{SourcePath: "DSD/a.dsf", VariantID: "optimized-dsd-v1-44100-16", SampleRate: 44100, BitsPerSample: 16,
		SourceMTimeNS: mA, SourceSize: sA, AppliedGainDB: fpk(3.0), TruePeakDBTP: fpk(-4.0), SoxSettings: `{"rateFlag":"-v","schemaVersion":"v1"}`})
	mustUpsertVariant(t, s, VariantRow{SourcePath: "DSD/a.dsf", VariantID: "pcm-v1-176400-24", SampleRate: 176400, BitsPerSample: 24,
		SourceMTimeNS: mA, SourceSize: sA, AppliedGainDB: fpk(2.8), TruePeakDBTP: fpk(-3.8), SoxSettings: `{"rateFlag":"-v"}`})
	mustUpsertVariant(t, s, VariantRow{SourcePath: "DSD/b.dsf", VariantID: "optimized-dsd-v1-44100-16", SampleRate: 44100, BitsPerSample: 16,
		SourceMTimeNS: mB, SourceSize: sB, AppliedGainDB: fpk(6.0), SoxSettings: `{"rateFlag":"-v"}`})
	mustUpsertVariant(t, s, VariantRow{SourcePath: "PCM/c.flac", VariantID: "optimized-v2-48000-16", SampleRate: 48000, BitsPerSample: 16,
		SourceMTimeNS: mC, SourceSize: sC, SoxSettings: `{"rateFlag":"-v"}`})
	if n := countDSDPeaks(t, s); n != 0 {
		t.Fatalf("premise: %d peaks before the seed", n)
	}
	for range 2 {
		if err := seedDSDPeaksFromVariants(s.db); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	compact, err := s.FreshDSDPeaks(ctx, "a1|compact|44100|-v", []string{"DSD/a.dsf", "DSD/b.dsf", "PCM/c.flac"})
	if err != nil {
		t.Fatal(err)
	}
	if p, ok := compact["DSD/a.dsf"]; !ok || p.TruePeakDBTP == nil || *p.TruePeakDBTP != -4.0 {
		t.Errorf("a's compact peak %+v, want −4.0", p)
	}
	if p, ok := compact["DSD/b.dsf"]; !ok || p.TruePeakDBTP != nil {
		t.Errorf("b's compact peak %+v, want a recorded SILENT measurement (nil peak)", p)
	}
	if _, ok := compact["PCM/c.flac"]; ok {
		t.Error("a PCM variant must never become a DSD peak")
	}
	faithful, _ := s.FreshDSDPeaks(ctx, "a1|faithful|176400|-v", []string{"DSD/a.dsf"})
	if p := faithful["DSD/a.dsf"]; p.TruePeakDBTP == nil || *p.TruePeakDBTP != -3.8 {
		t.Errorf("a's faithful peak %+v, want −3.8", p)
	}
	if n := countDSDPeaks(t, s); n != 3 {
		t.Errorf("%d peaks after seeding twice, want 3", n)
	}
}

// TestUpsertVariantRecordsTheRendersPeak: a DSD rendition's Stage B peak is
// recorded in the same transaction as the variant row. A row with a
// profile but no applied gain is not a DSD rendition and records nothing.
func TestUpsertVariantRecordsTheRendersPeak(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	seedOptimizeTrack(t, s, "DSD/a.dsf", 2822400, 1, "DSF", true)
	seedOptimizeTrack(t, s, "PCM/c.flac", 96000, 24, "FLAC", false)
	m, sz := trackRowMTimeAndSize(t, s, "DSD/a.dsf")
	mustUpsertVariant(t, s, VariantRow{SourcePath: "DSD/a.dsf", VariantID: "optimized-dsd-v1-44100-16", SampleRate: 44100, BitsPerSample: 16,
		SourceMTimeNS: m, SourceSize: sz, AppliedGainDB: fpk(4.5), TruePeakDBTP: fpk(-5.5), PeakProfile: "a1|compact|44100|-v", SoxSettings: "{}"})
	got, err := s.FreshDSDPeaks(ctx, "a1|compact|44100|-v", []string{"DSD/a.dsf"})
	if err != nil {
		t.Fatal(err)
	}
	if p := got["DSD/a.dsf"]; p.TruePeakDBTP == nil || *p.TruePeakDBTP != -5.5 || p.SourceMTimeNS != m || p.SourceSize != sz {
		t.Errorf("recorded %+v, want −5.5 with the variant's source facts", p)
	}
	mc, sc := trackRowMTimeAndSize(t, s, "PCM/c.flac")
	mustUpsertVariant(t, s, VariantRow{SourcePath: "PCM/c.flac", VariantID: "optimized-v2-48000-16", SampleRate: 48000, BitsPerSample: 16,
		SourceMTimeNS: mc, SourceSize: sc, PeakProfile: "a1|compact|48000|-v", SoxSettings: "{}"})
	if n := countDSDPeaks(t, s); n != 1 {
		t.Errorf("%d peaks, want 1 — a PCM row carries no applied gain and records no peak", n)
	}
}

// TestFreshDSDPeaksIgnoresAStaleSource: a peak is trusted only while the
// track row's source facts match it, and only on its own profile. The IN
// list is chunked, so a list longer than one chunk still resolves.
func TestFreshDSDPeaksIgnoresAStaleSource(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	seedOptimizeTrack(t, s, "DSD/a.dsf", 2822400, 1, "DSF", true)
	m, sz := trackRowMTimeAndSize(t, s, "DSD/a.dsf")
	if err := s.UpsertDSDPeak(ctx, DSDPeak{SourcePath: "DSD/a.dsf", Profile: "a1|compact|44100|-v", TruePeakDBTP: fpk(-3), SourceMTimeNS: m, SourceSize: sz, MeasuredAt: 1}); err != nil {
		t.Fatal(err)
	}
	paths := []string{"DSD/a.dsf"}
	for i := range freshDSDPeaksChunk + 20 {
		paths = append(paths, fmt.Sprintf("missing/%d.dsf", i))
	}
	got, err := s.FreshDSDPeaks(ctx, "a1|compact|44100|-v", paths)
	if err != nil || len(got) != 1 {
		t.Fatalf("got %v err=%v, want the one fresh peak across chunks", got, err)
	}
	if other, _ := s.FreshDSDPeaks(ctx, "a1|faithful|176400|-v", paths[:1]); len(other) != 0 {
		t.Errorf("a compact peak must not answer for the faithful profile: %v", other)
	}
	if _, err := s.db.Exec(`UPDATE tracks SET mtime_ns = mtime_ns + 1 WHERE path = ?`, "DSD/a.dsf"); err != nil {
		t.Fatal(err)
	}
	if stale, _ := s.FreshDSDPeaks(ctx, "a1|compact|44100|-v", paths[:1]); len(stale) != 0 {
		t.Errorf("a peak of a re-encoded source must not be trusted: %v", stale)
	}
}

func TestUpsertDSDPeakRefusesAMissingTrackOrBlankKey(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	if err := s.UpsertDSDPeak(ctx, DSDPeak{SourcePath: "gone.dsf", Profile: "a1|compact|44100|-v"}); err == nil {
		t.Error("a peak for a track that is not in the library must fail (FK)")
	}
	if err := s.UpsertDSDPeak(ctx, DSDPeak{SourcePath: "x.dsf"}); err == nil {
		t.Error("a blank profile must be refused")
	}
}

// TestDSDPeaksGoWithTheirTrack: CASCADE on the tracks PK, like
// track_variants — a removed or renamed track takes its peaks with it.
func TestDSDPeaksGoWithTheirTrack(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })
	seedOptimizeTrack(t, s, "DSD/a.dsf", 2822400, 1, "DSF", true)
	m, sz := trackRowMTimeAndSize(t, s, "DSD/a.dsf")
	if err := s.UpsertDSDPeak(context.Background(), DSDPeak{SourcePath: "DSD/a.dsf", Profile: "p", SourceMTimeNS: m, SourceSize: sz}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DELETE FROM tracks WHERE path = ?`, "DSD/a.dsf"); err != nil {
		t.Fatal(err)
	}
	if n := countDSDPeaks(t, s); n != 0 {
		t.Errorf("%d peaks outlived their track", n)
	}
}

// TestStreamDSDCatalogRefsIsTheCatalogsDSDRows: the album index's stream is
// the full catalog stream narrowed to DSD — the same served-rows rule and
// the same row mapping — so a DSD track groups into the album the catalog
// shows.
func TestStreamDSDCatalogRefsIsTheCatalogsDSDRows(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	seedOptimizeTrack(t, s, "DSD/a.dsf", 2822400, 1, "DSF", true)
	seedOptimizeTrack(t, s, "DSD/b.dff", 5644800, 1, "DFF", true)
	seedOptimizeTrack(t, s, "PCM/c.flac", 96000, 24, "FLAC", false)
	seedOptimizeTrack(t, s, "DSD/suppressed.dsf", 2822400, 1, "DSF", true)
	if _, err := s.db.Exec(`UPDATE tracks SET dupe_suppressed = 1 WHERE path = ?`, "DSD/suppressed.dsf"); err != nil {
		t.Fatal(err)
	}
	full := map[string]CatalogRef{}
	if err := s.StreamCatalogRefs(ctx, func(r CatalogRef) error { full[r.Path] = r; return nil }); err != nil {
		t.Fatal(err)
	}
	var dsd []CatalogRef
	if err := s.StreamDSDCatalogRefs(ctx, func(r CatalogRef) error { dsd = append(dsd, r); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(dsd) != 2 {
		t.Fatalf("streamed %d rows, want the 2 served DSD tracks: %+v", len(dsd), dsd)
	}
	for _, r := range dsd {
		if !r.IsDSD {
			t.Errorf("%s streamed as PCM", r.Path)
		}
		if !reflect.DeepEqual(r, full[r.Path]) {
			t.Errorf("%s: DSD stream %+v differs from the catalog's %+v", r.Path, r, full[r.Path])
		}
	}
}
