package manifest

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestIsVariantSidecarName pins the predicate that keeps the scanner from
// indexing its own optimize/upscale transcode sidecars as library tracks.
// Sidecars are `<srcBase>.<variantID>.flac` (variantID `<kind>-<schema>-<rate>-<bits>`),
// so the basename always carries a `.upscaled-` / `.optimized-` infix. Version-agnostic
// by design — a future schema (v3) must still be excluded.
func TestIsVariantSidecarName(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		// Real sidecars (the field-observed shapes).
		{"01 Love Letters.flac.upscaled-v2-176400-24.flac", true},
		{"01 - Carol of the Bells.flac.optimized-v2-44100-16.flac", true},
		{"13 - String Quartet No. 3- Animato.flac.optimized-v2-48000-16.flac", true},
		// Version-agnostic: a pre-v2 or future-schema sidecar is still excluded.
		{"Track.flac.upscaled-v1-96000-24.flac", true},
		{"Track.flac.optimized-v3-44100-16.flac", true},
		// Real library files — must NOT be excluded.
		{"01 - Carol of the Bells.flac", false},
		{"Symphony No. 5.flac", false},
		{"My Song.mp3", false},
		{"08 - Blågutten.flac", false},
		// A top-level name that merely starts with the kind word is NOT a sidecar
		// (no `.<kind>-` infix): the srcBase-dot prefix is what makes it a variant.
		{"upscaled-mix.flac", false},
		{"optimized.flac", false},
		{"", false},
		// Anchoring (Gemini + CodeRabbit on PR #475): a real single-extension file that
		// merely CONTAINS a variant-shaped infix must NOT be excluded — the trailing
		// segment isn't a well-formed variant ID, and/or isn't preceded by an audio ext.
		{"01.upscaled-mix.flac", false},
		{"something.optimized-version.flac", false},
		{"Song (Album).optimized-Mix.flac", false},
		{"Track.flac.optimized-v2-44100.flac", false},   // variant ID missing the bits segment
		{"Track.txt.optimized-v2-44100-16.flac", false}, // source is not a supported audio ext
	}
	for _, tc := range cases {
		if got := isVariantSidecarName(tc.name); got != tc.want {
			t.Errorf("isVariantSidecarName(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestScannerSkipsVariantSidecars is the end-to-end guard: a real track is
// indexed, but optimize/upscale sidecars — whether under a `variants/` subtree
// (the B2-bucket field case) or sitting next to their source — are NOT indexed
// as standalone tracks. Without the exclusion these phantom rows doubled every
// affected album (~26% of a 24k-track library in the field report).
func TestScannerSkipsVariantSidecars(t *testing.T) {
	root := t.TempDir()

	realTrack := filepath.Join("Album", "01 - Song.flac")
	sidecars := []string{
		// The field case: sidecars synced into a `variants/` folder inside the root.
		filepath.Join("variants", "Album", "01 - Song.flac.optimized-v2-44100-16.flac"),
		filepath.Join("variants", "Album", "01 - Song.flac.upscaled-v2-176400-24.flac"),
		// Defense-in-depth: a sidecar sitting next to its source (variantsDir === source dir).
		filepath.Join("Album", "01 - Song.flac.optimized-v2-48000-16.flac"),
	}

	for _, rel := range append([]string{realTrack}, sidecars...) {
		abs := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, audioBytes, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	s, err := OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	sc := NewScanner([]string{root}, s, "")
	if _, err := sc.Scan(context.Background()); err != nil {
		t.Fatalf("scan: %v", err)
	}

	paths, err := s.TrackPaths(context.Background())
	if err != nil {
		t.Fatalf("TrackPaths: %v", err)
	}

	if !containsString(paths, realTrack) {
		t.Errorf("real track %q was not indexed; got %v", realTrack, paths)
	}
	for _, sc := range sidecars {
		if containsString(paths, sc) {
			t.Errorf("variant sidecar %q was indexed as a track; got %v", sc, paths)
		}
	}
	// Exactly one row: the real track, nothing else.
	if len(paths) != 1 {
		t.Errorf("expected exactly 1 indexed track (the real one), got %d: %v", len(paths), paths)
	}
}
