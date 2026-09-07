package config

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestDSDRenderDefaultsOff pins the opt-in: a config that never mentions
// dsdRender has it OFF. Flipping it on starts a sweep that reads the
// whole DSD library and adds an ffmpeg requirement, so it must stay an
// explicit operator act — a zero-value default of true would enable
// that on every upgrade.
func TestDSDRenderDefaultsOff(t *testing.T) {
	if (UpscaleConfig{}).DSDRender.Enabled {
		t.Fatal("zero UpscaleConfig has DSDRender.Enabled = true; the renditions must be opt-in")
	}
	if (UpscaleConfig{}).TempDir != "" {
		t.Fatal("zero UpscaleConfig has a TempDir; empty (= the OS temp dir) is the documented default")
	}
}

// TestValidateRenderTempDir mirrors validateVariantsDir's contract on the
// scratch directory: empty is fine, absolute-outside-the-library is fine,
// relative and under-a-library-root are refused.
func TestValidateRenderTempDir(t *testing.T) {
	// absTestPath, not "/srv/music": these must be absolute on the HOST.
	// On Windows "/srv/music" has no volume, so the absolute-path check
	// (correctly) refuses it before the under-a-root check this test is
	// about — the same lesson TestValidateVariantsDirRejectsUnderLibraryRoot
	// records, re-learned here on the windows-latest leg of PR #863.
	roots := []string{absTestPath("srv", "music"), absTestPath("mnt", "nas", "lib")}
	cases := []struct {
		name    string
		dir     string
		wantErr string // substring; "" = must pass
	}{
		{"empty is the OS temp dir", "", ""},
		{"absolute outside the roots", absTestPath("var", "tmp", "render"), ""},
		{"relative", filepath.Join("scratch", "render"), "absolute"},
		{"under the first root", absTestPath("srv", "music", ".scratch"), "library root"},
		{"under the second root", absTestPath("mnt", "nas", "lib", "tmp", "render"), "library root"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateRenderTempDir(tc.dir, roots)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("validateRenderTempDir(%q) = %v, want nil", tc.dir, err)
			case tc.wantErr != "" && err == nil:
				t.Errorf("validateRenderTempDir(%q) = nil, want an error mentioning %q", tc.dir, tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Errorf("validateRenderTempDir(%q) = %v, want it to mention %q", tc.dir, err, tc.wantErr)
			}
		})
	}
}

// TestValidateWiresRenderTempDir pins that Validate() actually consults
// the helper — a helper nobody calls is the class this repo keeps
// finding (a contract with no observer).
func TestValidateWiresRenderTempDir(t *testing.T) {
	cfg := minimalValidAutoOptimizeConfig()
	// Host-absolute roots (see TestValidateRenderTempDir): the under-root
	// arm below must reach the containment check on Windows too.
	cfg.LibraryRoots = []string{absTestPath("lib")}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("premise: the minimal config must validate: %v", err)
	}
	cfg.Upscale.TempDir = filepath.Join("relative", "scratch")
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "upscale.tempDir") {
		t.Fatalf("Validate() = %v, want an upscale.tempDir error for a relative scratch dir", err)
	}
	cfg.Upscale.TempDir = absTestPath("lib", "scratch")
	err = cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "library root") {
		t.Fatalf("Validate() = %v, want a library-root refusal for a scratch dir under a root", err)
	}
	cfg.Upscale.TempDir = absTestPath("var", "tmp", "render")
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil for an absolute scratch dir outside the roots", err)
	}
}

// TestResolvePathsResolvesRenderTempDir: a relative tempDir in the yaml
// resolves against the config's own directory, exactly like variantsDir,
// and empty stays empty (the OS temp dir) rather than becoming the
// config dir.
func TestResolvePathsResolvesRenderTempDir(t *testing.T) {
	cfg := minimalValidAutoOptimizeConfig()
	cfg.Upscale.TempDir = "scratch"
	cfgDir := absTestPath("etc", "bridge")
	cfg.resolvePaths(cfgDir)
	if want := filepath.Join(cfgDir, "scratch"); cfg.Upscale.TempDir != want {
		t.Errorf("TempDir = %q, want %q (resolved against the config dir)", cfg.Upscale.TempDir, want)
	}
	cfg2 := minimalValidAutoOptimizeConfig()
	cfg2.resolvePaths(cfgDir)
	if cfg2.Upscale.TempDir != "" {
		t.Errorf("empty TempDir resolved to %q, want it to stay empty (= the OS temp dir)", cfg2.Upscale.TempDir)
	}
}
