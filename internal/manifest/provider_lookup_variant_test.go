package manifest

import (
	"path/filepath"
	"testing"
	"time"
)

// Pins the Provider.LookupVariant path-shape contract — same iOS↔bridge
// convention `Store.LookupVariant` honours, exposed through the
// `api.VariantStore` interface that `/v1/download?variant=…` hits.
//
// Pre-fix `Provider.LookupVariant` called `Store.GetVariant` (exact
// byte-compare), so every iOS-shaped lowercase path produced 404 even
// when the case-folded SQL row was sitting right there in the table.
// PR #126 split the case-insensitive `Store.LookupVariant` out of
// `GetVariant` for the upscale-enqueue path, but missed wiring the
// `/v1/download` provider wrapper through to the new method.
//
// Symptom (from the field, 2026-05-02): user enables Prefer-upscaled,
// taps play, gets "This track is no longer on the share" — iOS sees
// `BridgeError.http(404)` from the variant download, classifies it as
// missing-track via `PlayerService.isMissingTrackError`, surfaces the
// rescan affordance. Even though the `track_variants` row exists.
func TestProviderLookupVariant_caseInsensitive(t *testing.T) {
	s, err := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer s.Close()

	const canonical = "Abdullah Ibrahim/The Balance/09 - Devotion.flac"
	if err := s.UpsertTrack(&Track{
		Path: canonical, Size: 1, ModTime: time.Now(),
	}); err != nil {
		t.Fatalf("UpsertTrack: %v", err)
	}
	if err := s.UpsertVariant(VariantRow{
		SourcePath:    canonical,
		VariantID:     "upscaled-v2-176400-24",
		SidecarPath:   "/tmp/x/abc-upscaled-v2-176400-24.flac",
		Format:        "flac",
		SampleRate:    176400,
		BitsPerSample: 24,
		SizeBytes:     12345678,
		SourceMTimeNS: time.Now().UnixNano(),
		SourceSize:    1,
		SoxSettings:   `{"resampler":"sox"}`,
		CreatedAt:     time.Now().UnixNano(),
	}); err != nil {
		t.Fatalf("UpsertVariant: %v", err)
	}

	p := NewProvider(s, NewScanner(nil, s, ""))

	cases := []struct {
		name string
		path string
	}{
		{"exact match (back-compat)", canonical},
		{"leading slash only", "/" + canonical},
		{"lowercase only (case-sensitive FS shape)",
			"abdullah ibrahim/the balance/09 - devotion.flac"},
		{"leading slash + lowercase (real iOS shape)",
			"/abdullah ibrahim/the balance/09 - devotion.flac"},
		{"uppercase",
			"ABDULLAH IBRAHIM/THE BALANCE/09 - DEVOTION.FLAC"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := p.LookupVariant(tc.path, "upscaled-v2-176400-24")
			if err != nil {
				t.Fatalf("LookupVariant(%q): %v", tc.path, err)
			}
			if got == nil {
				t.Fatalf("LookupVariant(%q) = nil; expected the variant to be found", tc.path)
			}
			if got.SidecarPath != "/tmp/x/abc-upscaled-v2-176400-24.flac" {
				t.Errorf("SidecarPath: got %q", got.SidecarPath)
			}
		})
	}
}

// A genuinely missing variant must still return (nil, nil) — the
// case-insensitive fallback must not loosen the not-found contract,
// otherwise the api's 404 → "no such variant" path can't distinguish
// "row exists but stale" (410) from "row doesn't exist".
func TestProviderLookupVariant_stillReturnsNilOnMiss(t *testing.T) {
	s, err := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer s.Close()

	if err := s.UpsertTrack(&Track{
		Path: "Real/Track.flac", Size: 1, ModTime: time.Now(),
	}); err != nil {
		t.Fatalf("UpsertTrack: %v", err)
	}

	p := NewProvider(s, NewScanner(nil, s, ""))
	got, err := p.LookupVariant("/some/other/track.flac", "upscaled-v2-176400-24")
	if err != nil {
		t.Fatalf("LookupVariant: %v", err)
	}
	if got != nil {
		t.Errorf("LookupVariant on a missing path returned %+v; want nil", got)
	}
}
