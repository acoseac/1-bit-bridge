package config

import (
	"os"
	"path/filepath"
	"testing"
)

// writeEnrichTestConfig writes a minimal valid bridge.yaml (with an
// existing library root, mirroring TestEnvOverrides's setup so Load's
// root validation passes) plus the given trailing YAML, and returns the
// config path.
func writeEnrichTestConfig(t *testing.T, extraYAML string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	yaml := "libraryRoots:\n  - " + filepath.Join(dir, "lib") + "\n" +
		"dataDir: \"" + filepath.Join(dir, "data") + "\"\n" + extraYAML
	cfgPath := filepath.Join(dir, "bridge.yaml")
	if err := os.WriteFile(cfgPath, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	return cfgPath
}

// TestEnrichBaseURLs_DefaultEmpty: with no enrich block, both base URLs
// stay empty so the enrich clients fall back to their public defaults
// (https://musicbrainz.org/ws/2, https://coverartarchive.org).
func TestEnrichBaseURLs_DefaultEmpty(t *testing.T) {
	cfg, err := Load(writeEnrichTestConfig(t, ""))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Enrich.MusicBrainzBaseURL != "" || cfg.Enrich.CoverArtBaseURL != "" {
		t.Errorf("expected empty enrich bases by default, got mb=%q caa=%q",
			cfg.Enrich.MusicBrainzBaseURL, cfg.Enrich.CoverArtBaseURL)
	}
}

// TestEnrichBaseURLs_FromYAML: the enrich block is parsed from YAML.
func TestEnrichBaseURLs_FromYAML(t *testing.T) {
	cfg, err := Load(writeEnrichTestConfig(t,
		"enrich:\n"+
			"  musicbrainzBaseURL: \"https://atlas.example/ws/2\"\n"+
			"  coverArtBaseURL: \"https://atlas.example\"\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Enrich.MusicBrainzBaseURL != "https://atlas.example/ws/2" {
		t.Errorf("MusicBrainzBaseURL = %q, want https://atlas.example/ws/2", cfg.Enrich.MusicBrainzBaseURL)
	}
	if cfg.Enrich.CoverArtBaseURL != "https://atlas.example" {
		t.Errorf("CoverArtBaseURL = %q, want https://atlas.example", cfg.Enrich.CoverArtBaseURL)
	}
}

// TestEnrichBaseURLs_EnvOverrides: BRIDGE_MUSICBRAINZ_BASE_URL /
// BRIDGE_COVERART_BASE_URL win over YAML — the knob for pointing
// enrichment at a self-hosted 1-bit-atlas mirror in container deployments.
func TestEnrichBaseURLs_EnvOverrides(t *testing.T) {
	cfgPath := writeEnrichTestConfig(t,
		"enrich:\n"+
			"  musicbrainzBaseURL: \"https://yaml.example/ws/2\"\n"+
			"  coverArtBaseURL: \"https://yaml.example\"\n")

	t.Setenv("BRIDGE_MUSICBRAINZ_BASE_URL", "https://atlas.ars.md/ws/2")
	t.Setenv("BRIDGE_COVERART_BASE_URL", "https://atlas.ars.md")

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Enrich.MusicBrainzBaseURL != "https://atlas.ars.md/ws/2" {
		t.Errorf("MusicBrainzBaseURL = %q, want https://atlas.ars.md/ws/2", cfg.Enrich.MusicBrainzBaseURL)
	}
	if cfg.Enrich.CoverArtBaseURL != "https://atlas.ars.md" {
		t.Errorf("CoverArtBaseURL = %q, want https://atlas.ars.md", cfg.Enrich.CoverArtBaseURL)
	}
}
