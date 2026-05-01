package manifest

import (
	"path/filepath"
	"testing"
	"time"
)

// Pins the iOS↔bridge path-shape contract that
// `Store.LookupTrack` and `Store.LookupVariant` honour:
//
//   - iOS sends the path through `share.normalize(path:)` before
//     storing it in SwiftData and re-emitting on every endpoint
//     that takes a path. The normalisation lowercases the string
//     AND prepends "/" for bridge-source tracks.
//   - The bridge manifest stores the original FS-canonical case
//     (no leading slash, mixed case) as the PRIMARY KEY.
//
// Pre-fix the upscale eligibility gate called `GetTrack` directly
// and exact byte-compare missed every iOS-shaped request — POST
// /v1/upscale returned `enqueued=0` because `track == nil` made
// the eligibility gate short-circuit to `ErrUpscaleIneligible`.
//
// Note: `GetTrack` and `GetVariant` deliberately stay
// case-SENSITIVE so the scanner's unchanged-file fast-path can't
// alias two distinct case-colliding files on case-sensitive
// filesystems — see the function docs and Qodo's bug 1 on PR #126.
// External callers handing in iOS-shaped paths must use the
// Lookup* variants tested here.
func TestLookupTrack_caseInsensitiveAndLeadingSlash(t *testing.T) {
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
			tr, err := s.LookupTrack(tc.path)
			if err != nil {
				t.Fatalf("LookupTrack(%q): %v", tc.path, err)
			}
			if tr == nil {
				t.Fatalf("LookupTrack(%q) = nil; expected the track to be found", tc.path)
			}
			if tr.Path != canonical {
				t.Errorf("LookupTrack(%q).Path = %q, want canonical %q", tc.path, tr.Path, canonical)
			}
		})
	}
}

// A genuinely missing track must still return (nil, nil) — the
// case-insensitive fallback must not loosen the not-found contract.
func TestLookupTrack_stillReturnsNilOnMiss(t *testing.T) {
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

	tr, err := s.LookupTrack("/some/other/track.flac")
	if err != nil {
		t.Fatalf("LookupTrack: %v", err)
	}
	if tr != nil {
		t.Errorf("LookupTrack on a missing path returned %v; want nil", tr)
	}
}

// Pins the case-sensitive contract that internal callers (the
// scanner's unchanged-file fast-path in particular) rely on:
// `GetTrack` does an EXACT match. Two distinct case-colliding
// rows must each look up to themselves, never to each other.
// (Qodo bug 1 on PR #126 — case-folding the lookup made the
// scanner's optimization skip a real file that happened to have
// the same size/mtime as a case-colliding sibling.)
func TestGetTrack_remainsExactMatch(t *testing.T) {
	s, err := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	defer s.Close()

	const canonical = "Artist/Album/Track.flac"
	if err := s.UpsertTrack(&Track{
		Path: canonical, Size: 1, ModTime: time.Now(),
	}); err != nil {
		t.Fatalf("UpsertTrack: %v", err)
	}

	// Exact case → hit.
	if tr, err := s.GetTrack(canonical); err != nil || tr == nil {
		t.Fatalf("GetTrack(canonical) = (%v, %v); want non-nil", tr, err)
	}
	// Lowercase variant → MISS. The scanner relies on this.
	if tr, err := s.GetTrack("artist/album/track.flac"); err != nil {
		t.Fatalf("GetTrack(lowercase): %v", err)
	} else if tr != nil {
		t.Errorf("GetTrack(lowercase) = %v; want nil — exact-match contract broken", tr)
	}
	// Leading slash → MISS. Manifest never stores leading slash.
	if tr, err := s.GetTrack("/" + canonical); err != nil {
		t.Fatalf("GetTrack(leading-slash): %v", err)
	} else if tr != nil {
		t.Errorf("GetTrack(leading-slash) = %v; want nil — exact-match contract broken", tr)
	}
}

// Mirror of TestLookupTrack_caseInsensitiveAndLeadingSlash for the
// variant lookup that the upscale eligibility gate makes immediately
// after LookupTrack — same iOS-shaped path, same case-fold contract.
// Without this, the freshness check ("variant exists, refuse to
// re-convert") missed on case mismatch and the bridge would silently
// re-enqueue jobs whose output already lived on disk.
func TestLookupVariant_caseInsensitiveAndLeadingSlash(t *testing.T) {
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
			v, err := s.LookupVariant(tc.path, variantID)
			if err != nil {
				t.Fatalf("LookupVariant: %v", err)
			}
			if v == nil {
				t.Fatalf("LookupVariant returned nil for %q + %q", tc.path, variantID)
			}
			if v.SourcePath != canonical {
				t.Errorf("v.SourcePath = %q, want %q", v.SourcePath, canonical)
			}
		})
	}
}
