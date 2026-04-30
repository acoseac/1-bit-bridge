package manifest

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestVariantCRUDRoundTrip pins the basic insert/get/list/delete
// shape on the new track_variants table. Underpins every higher-
// level test below.
func TestVariantCRUDRoundTrip(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })

	// First need a parent track row — track_variants has a FK on it.
	upsertParent(t, s, "Music/Album/01.flac")

	row := VariantRow{
		SourcePath:    "Music/Album/01.flac",
		VariantID:     "upscaled-v1-176400-24",
		SidecarPath:   "/tmp/x/abc-upscaled-v1-176400-24.flac",
		Format:        "flac",
		SampleRate:    176400,
		BitsPerSample: 24,
		SizeBytes:     12345678,
		SourceMTimeNS: time.Now().UnixNano(),
		SourceSize:    7654321,
		SoxSettings:   `{"resampler":"sox"}`,
		CreatedAt:     time.Now().UnixNano(),
	}
	if err := s.UpsertVariant(row); err != nil {
		t.Fatalf("UpsertVariant: %v", err)
	}

	got, err := s.GetVariant(row.SourcePath, row.VariantID)
	if err != nil {
		t.Fatalf("GetVariant: %v", err)
	}
	if got == nil {
		t.Fatal("GetVariant: row not found")
	}
	if got.SidecarPath != row.SidecarPath {
		t.Errorf("SidecarPath: got %q want %q", got.SidecarPath, row.SidecarPath)
	}
	if got.SampleRate != row.SampleRate {
		t.Errorf("SampleRate: got %d want %d", got.SampleRate, row.SampleRate)
	}

	all, err := s.AllVariants()
	if err != nil {
		t.Fatalf("AllVariants: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("AllVariants: got %d rows, want 1", len(all))
	}

	if err := s.DeleteVariant(row.SourcePath, row.VariantID); err != nil {
		t.Fatalf("DeleteVariant: %v", err)
	}
	got2, _ := s.GetVariant(row.SourcePath, row.VariantID)
	if got2 != nil {
		t.Fatal("DeleteVariant: row still present after delete")
	}
}

// TestUpsertVariantReplacesExisting proves the ON CONFLICT clause
// of UpsertVariant updates instead of inserting a duplicate. Test
// for `bridge upscale --force` re-converting an existing variant.
func TestUpsertVariantReplacesExisting(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })

	upsertParent(t, s, "Music/A/1.flac")

	first := VariantRow{
		SourcePath: "Music/A/1.flac", VariantID: "upscaled-v1-176400-24",
		SidecarPath: "/tmp/old.flac", Format: "flac",
		SampleRate: 176400, BitsPerSample: 24,
		SizeBytes: 100, SourceMTimeNS: 1, SourceSize: 1,
		SoxSettings: "{}", CreatedAt: 1,
	}
	if err := s.UpsertVariant(first); err != nil {
		t.Fatal(err)
	}

	second := first
	second.SidecarPath = "/tmp/new.flac"
	second.SizeBytes = 200
	second.CreatedAt = 99
	if err := s.UpsertVariant(second); err != nil {
		t.Fatal(err)
	}

	got, err := s.GetVariant(first.SourcePath, first.VariantID)
	if err != nil {
		t.Fatal(err)
	}
	if got.SidecarPath != "/tmp/new.flac" || got.SizeBytes != 200 || got.CreatedAt != 99 {
		t.Errorf("UpsertVariant did not replace: got %+v", got)
	}

	all, _ := s.AllVariants()
	if len(all) != 1 {
		t.Errorf("AllVariants: expected 1 row after replace, got %d", len(all))
	}
}

// TestListTracksSplicesVariants is the load-bearing integration:
// ListTracks must return Track.Variants populated from the
// `json_group_array` LEFT JOIN. Without this, the manifest serves
// the bare track and iOS never sees the variant.
func TestListTracksSplicesVariants(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })

	upsertParent(t, s, "Music/A/1.flac")
	upsertParent(t, s, "Music/A/2.flac")

	// Two variants on track 1, none on track 2 — covers the
	// non-empty + empty branches of the aggregation.
	for _, vid := range []string{"upscaled-v1-176400-24", "upscaled-v1-352800-32"} {
		if err := s.UpsertVariant(VariantRow{
			SourcePath: "Music/A/1.flac", VariantID: vid,
			SidecarPath: "/tmp/" + vid + ".flac", Format: "flac",
			SampleRate: 176400, BitsPerSample: 24, SizeBytes: 10,
			SourceMTimeNS: 1, SourceSize: 1, SoxSettings: "{}", CreatedAt: 1,
		}); err != nil {
			t.Fatal(err)
		}
	}

	tracks, err := s.ListTracks(nil)
	if err != nil {
		t.Fatalf("ListTracks: %v", err)
	}
	if len(tracks) != 2 {
		t.Fatalf("ListTracks: got %d tracks, want 2", len(tracks))
	}

	byPath := make(map[string]Track, len(tracks))
	for _, tr := range tracks {
		byPath[tr.Path] = tr
	}

	t1 := byPath["Music/A/1.flac"]
	if len(t1.Variants) != 2 {
		t.Errorf("Music/A/1.flac variants: got %d, want 2", len(t1.Variants))
	}
	for _, v := range t1.Variants {
		if v.Format != "flac" {
			t.Errorf("variant.Format = %q, want flac", v.Format)
		}
		if v.Label == "" {
			t.Errorf("variant.Label empty (humanLabelForVariant should have populated it)")
		}
	}

	t2 := byPath["Music/A/2.flac"]
	if len(t2.Variants) != 0 {
		t.Errorf("Music/A/2.flac (no variants) got %d entries (want 0)", len(t2.Variants))
	}
}

// TestDeleteTrackRemovesSidecarFiles is the regression test for
// the orphan-sidecar trap (CASCADE deletes the DB row but not the
// `.flac` on disk). Pre-fix: 100MB+ files would leak per deleted
// source, and `--gc` couldn't find them by querying the DB
// (because the row was already gone).
func TestDeleteTrackRemovesSidecarFiles(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })

	tempDir := t.TempDir()
	upsertParent(t, s, "Music/A/1.flac")

	// Create two real sidecar files — proves DeleteTrack handles
	// multi-variant tracks correctly.
	side1 := filepath.Join(tempDir, "abc-upscaled-v1-176400-24.flac")
	side2 := filepath.Join(tempDir, "abc-upscaled-v1-352800-32.flac")
	for _, p := range []string{side1, side2} {
		if err := os.WriteFile(p, []byte("not really a flac"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	for vid, path := range map[string]string{
		"upscaled-v1-176400-24": side1,
		"upscaled-v1-352800-32": side2,
	} {
		if err := s.UpsertVariant(VariantRow{
			SourcePath: "Music/A/1.flac", VariantID: vid,
			SidecarPath: path, Format: "flac",
			SampleRate: 176400, BitsPerSample: 24,
			SizeBytes: 17, SourceMTimeNS: 1, SourceSize: 1,
			SoxSettings: "{}", CreatedAt: 1,
		}); err != nil {
			t.Fatal(err)
		}
	}

	if err := s.DeleteTrack("Music/A/1.flac"); err != nil {
		t.Fatalf("DeleteTrack: %v", err)
	}

	for _, p := range []string{side1, side2} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("sidecar %q should have been removed by DeleteTrack (err: %v)", p, err)
		}
	}

	// CASCADE should have removed the variant rows too.
	all, _ := s.AllVariants()
	if len(all) != 0 {
		t.Errorf("variants should have been cascaded; got %d rows", len(all))
	}
}

// TestDeleteTrackToleratesMissingSidecar — best-effort cleanup
// must not block the parent DELETE when a sidecar is already
// gone (manual rm, OS-side cleanup, double-delete races). The
// scanner needs the row gone regardless of disk state.
func TestDeleteTrackToleratesMissingSidecar(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })

	upsertParent(t, s, "Music/A/1.flac")

	// Insert variant pointing at a path that doesn't exist on disk.
	if err := s.UpsertVariant(VariantRow{
		SourcePath: "Music/A/1.flac", VariantID: "upscaled-v1-176400-24",
		SidecarPath: "/does/not/exist/abc.flac", Format: "flac",
		SampleRate: 176400, BitsPerSample: 24, SizeBytes: 1,
		SourceMTimeNS: 1, SourceSize: 1, SoxSettings: "{}", CreatedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}

	if err := s.DeleteTrack("Music/A/1.flac"); err != nil {
		t.Fatalf("DeleteTrack should swallow missing-sidecar errors: %v", err)
	}

	got, _ := s.GetTrack("Music/A/1.flac")
	if got != nil {
		t.Error("DeleteTrack: row still present despite cleanup tolerance")
	}
}

// TestVariantsHumanLabel pins the iOS-facing label format — any
// drift here will look wrong in the picker UI.
func TestVariantsHumanLabel(t *testing.T) {
	cases := []struct {
		v    Variant
		want string
	}{
		{Variant{Format: "flac", BitsPerSample: 24, SampleRate: 176400}, "Upscaled FLAC 24/176.4"},
		{Variant{Format: "flac", BitsPerSample: 24, SampleRate: 192000}, "Upscaled FLAC 24/192"},
		{Variant{Format: "flac", BitsPerSample: 32, SampleRate: 352800}, "Upscaled FLAC 32/352.8"},
	}
	for _, c := range cases {
		got := humanLabelForVariant(c.v)
		if got != c.want {
			t.Errorf("humanLabelForVariant(%+v) = %q, want %q", c.v, got, c.want)
		}
	}
}

// --- helpers ---

func openTempStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := OpenStore(filepath.Join(dir, "bridge.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	return s
}

// upsertParent creates a minimum-viable Track row at the given path
// so the FK on track_variants is satisfied. We don't care about tag
// content for these tests — only that the row exists.
func upsertParent(t *testing.T, s *Store, path string) {
	t.Helper()
	if err := s.UpsertTrack(&Track{
		Path:    path,
		Size:    100,
		ModTime: time.Now(),
	}); err != nil {
		t.Fatalf("UpsertTrack(%q): %v", path, err)
	}
}
