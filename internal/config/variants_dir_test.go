package config

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestEffectiveVariantsDirDefault: empty VariantsDir falls through
// to the historical `<dataDir>/transcoded` location so existing
// installs aren't affected.
func TestEffectiveVariantsDirDefault(t *testing.T) {
	u := UpscaleConfig{}
	got := u.EffectiveVariantsDir("/data/bridge")
	want := filepath.Join("/data/bridge", "transcoded")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestEffectiveVariantsDirExplicit: explicit VariantsDir wins
// regardless of the dataDir argument.
func TestEffectiveVariantsDirExplicit(t *testing.T) {
	u := UpscaleConfig{VariantsDir: "/mnt/external/variants"}
	got := u.EffectiveVariantsDir("/data/bridge")
	if got != "/mnt/external/variants" {
		t.Errorf("explicit VariantsDir should win, got %q", got)
	}
}

// TestValidateVariantsDirAcceptsEmpty: empty is the documented
// default — always valid.
func TestValidateVariantsDirAcceptsEmpty(t *testing.T) {
	if err := validateVariantsDir("", []string{"/some/lib"}); err != nil {
		t.Errorf("empty variantsDir should be valid: %v", err)
	}
}

// TestValidateVariantsDirRequiresAbsolute: relative paths get
// resolved against the process cwd which differs between `bridge
// serve` and `bridge upscale` invocations — variants would be
// unfindable across contexts.
func TestValidateVariantsDirRequiresAbsolute(t *testing.T) {
	err := validateVariantsDir("relative/path", []string{"/lib"})
	if err == nil {
		t.Fatalf("expected validation error for relative path")
	}
	if !strings.Contains(err.Error(), "absolute") {
		t.Errorf("error should mention 'absolute', got %q", err)
	}
}

// TestValidateVariantsDirRejectsUnderLibraryRoot: variants under
// the read-only library would break PR #75's invariant AND surface
// to scanner re-scans as mystery audio files.
func TestValidateVariantsDirRejectsUnderLibraryRoot(t *testing.T) {
	cases := []struct {
		name        string
		variantsDir string
		roots       []string
	}{
		{"direct child", "/lib/transcoded", []string{"/lib"}},
		{"deep child", "/lib/Music/transcoded", []string{"/lib"}},
		{"equal to root", "/lib", []string{"/lib"}},
		{"second root", "/libB/transcoded", []string{"/libA", "/libB"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateVariantsDir(c.variantsDir, c.roots)
			if err == nil {
				t.Fatalf("expected rejection for variantsDir under library root")
			}
			if !strings.Contains(err.Error(), "library root") {
				t.Errorf("error should mention 'library root', got %q", err)
			}
		})
	}
}

// TestValidateVariantsDirAcceptsSiblingPath: a variantsDir that's
// outside every library root is valid even if it shares a parent
// directory with one (e.g. /data/lib + /data/variants).
func TestValidateVariantsDirAcceptsSiblingPath(t *testing.T) {
	if err := validateVariantsDir("/data/variants", []string{"/data/lib"}); err != nil {
		t.Errorf("sibling path should be valid: %v", err)
	}
}

// TestValidateVariantsDirHandlesTrailingSlashes: filepath.Clean
// strips trailing slashes deterministically; the check works on
// both shapes.
func TestValidateVariantsDirHandlesTrailingSlashes(t *testing.T) {
	if err := validateVariantsDir("/lib/transcoded/", []string{"/lib/"}); err == nil {
		t.Errorf("trailing-slash variant of 'under library root' should still reject")
	}
}
