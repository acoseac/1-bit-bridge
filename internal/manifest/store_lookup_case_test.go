package manifest

import (
	"path/filepath"
	"testing"
	"time"
)

// Pins the iOS↔bridge path-shape contract that `Store.GetTrack`
// and `Store.GetVariant` both have to honour:
//
//   - iOS sends the path through `share.normalize(path:)` before
//     storing it in SwiftData and re-emitting on every endpoint
//     that takes a path. The normalisation lowercases the string
//     AND prepends "/" for bridge-source tracks.
//   - The bridge manifest stores the original FS-canonical case
//     (no leading slash, mixed case) as the PRIMARY KEY.
//
// Pre-fix the lookup did exact byte-compare and missed every
// iOS-shaped request — POST /v1/upscale handler returned
// `enqueued=0` because `track == nil` made the eligibility gate
// short-circuit to `ErrUpscaleIneligible`. These tests lock the
// case-insensitive + leading-slash-tolerant lookup so a future
// "back to exact compare" refactor breaks the build instead of
// the upscale pipeline.
func TestGetTrack_caseInsensitiveAndLeadingSlash(t *testing.T) {
	s, err := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer s.Close()

	const canonical = "Abdullah Ibrahim/The Balance/09 - Devotion.flac"
	if err := s.UpsertTrack(&Track{
		Path: canonical, Size: 1, ModTime: time.Now(),
		Artist: "Abdullah Ibrahim", Album: "The Balance",
	}); err != nil {
		t.Fatalf("UpsertTrack: %v", err)
	}

	cases := []struct {
		name string
		path string
	}{
		{"exact match (back-compat)", canonical},
		{"leading slash only", "/" + canonical},
		{"lowercase only (the iOS shape on a case-sensitive FS)",
			"abdullah ibrahim/the balance/09 - devotion.flac"},
		{"leading slash + lowercase (real iOS shape)",
			"/abdullah ibrahim/the balance/09 - devotion.flac"},
		{"uppercase",
			"ABDULLAH IBRAHIM/THE BALANCE/09 - DEVOTION.FLAC"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr, err := s.GetTrack(tc.path)
			if err != nil {
				t.Fatalf("GetTrack(%q): %v", tc.path, err)
			}
			if tr == nil {
				t.Fatalf("GetTrack(%q) = nil; expected the track to be found", tc.path)
			}
			if tr.Path != canonical {
				t.Errorf("GetTrack(%q).Path = %q, want canonical %q", tc.path, tr.Path, canonical)
			}
		})
	}
}

// A genuinely missing track must still return (nil, nil) — the
// case-insensitive layer must not loosen the not-found contract.
func TestGetTrack_caseInsensitiveStillReturnsNilOnMiss(t *testing.T) {
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

	tr, err := s.GetTrack("/some/other/track.flac")
	if err != nil {
		t.Fatalf("GetTrack: %v", err)
	}
	if tr != nil {
		t.Errorf("GetTrack on a missing path returned %v; want nil", tr)
	}
}

// Mirror of TestGetTrack_caseInsensitiveAndLeadingSlash for the
// variant lookup that the upscale eligibility gate makes immediately
// after GetTrack — same iOS-shaped path, same case-fold contract.
// Without this, the freshness check ("variant exists, refuse to
// re-convert") missed on case mismatch and the bridge would silently
// re-enqueue jobs whose output already lived on disk.
func TestGetVariant_caseInsensitiveAndLeadingSlash(t *testing.T) {
	s, err := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer s.Close()

	const canonical = "Abdullah Ibrahim/The Balance/09 - Devotion.flac"
	const variantID = "v176400-24"
	if err := s.UpsertTrack(&Track{
		Path: canonical, Size: 1, ModTime: time.Now(),
	}); err != nil {
		t.Fatalf("UpsertTrack: %v", err)
	}
	if err := s.UpsertVariant(VariantRow{
		SourcePath:    canonical,
		VariantID:     variantID,
		SidecarPath:   "/cache/variant.flac",
		Format:        "flac",
		SampleRate:    176_400,
		BitsPerSample: 24,
		SizeBytes:     1024,
		SourceMTimeNS: 0,
		SourceSize:    1,
		SoxSettings:   "{}",
		CreatedAt:     time.Now().Unix(),
	}); err != nil {
		t.Fatalf("UpsertVariant: %v", err)
	}

	cases := []struct {
		name string
		path string
	}{
		{"exact", canonical},
		{"leading slash + lowercase", "/abdullah ibrahim/the balance/09 - devotion.flac"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v, err := s.GetVariant(tc.path, variantID)
			if err != nil {
				t.Fatalf("GetVariant: %v", err)
			}
			if v == nil {
				t.Fatalf("GetVariant returned nil for %q + %q", tc.path, variantID)
			}
			if v.SourcePath != canonical {
				t.Errorf("v.SourcePath = %q, want %q", v.SourcePath, canonical)
			}
		})
	}
}
