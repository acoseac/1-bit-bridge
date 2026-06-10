package admin

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestAssertNotUnderLibraryRoots pins the lockstep contract with
// config.validateVariantsDir: sibling paths are accepted, paths at or
// under a root are rejected. (The config-side twin has its own table
// in internal/config/variants_dir_test.go.)
func TestAssertNotUnderLibraryRoots(t *testing.T) {
	cases := []struct {
		name      string
		candidate string
		roots     []string
		wantErr   bool
	}{
		{"sibling path", "/data/variants", []string{"/data/lib"}, false},
		{"direct child", "/lib/transcoded", []string{"/lib"}, true},
		{"deep child", "/lib/Music/transcoded", []string{"/lib"}, true},
		{"equal to root", "/lib", []string{"/lib"}, true},
		{"second root", "/libB/transcoded", []string{"/libA", "/libB"}, true},
		{"empty root skipped", "/data/variants", []string{""}, false},
		// /a/transcoded2 is NOT under /a/transcoded — prefix-shaped
		// sibling names must not false-match.
		{"prefix-named sibling", "/a/transcoded2", []string{"/a/transcoded"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := assertNotUnderLibraryRoots(c.candidate, c.roots)
			if c.wantErr && err == nil {
				t.Errorf("assertNotUnderLibraryRoots(%q, %v) = nil, want rejection", c.candidate, c.roots)
			}
			if !c.wantErr && err != nil {
				t.Errorf("assertNotUnderLibraryRoots(%q, %v) = %v, want nil", c.candidate, c.roots, err)
			}
		})
	}
}

// TestAssertNotUnderLibraryRootsResolvesSymlinks pins the symlink
// resolution that keeps the admin check in lockstep with
// config.validateVariantsDir: a candidate that LEXICALLY sits outside
// every root but RESOLVES under one must be rejected — otherwise the
// admin accepts a value that config.Load's validation refuses on the
// next boot.
func TestAssertNotUnderLibraryRootsResolvesSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink test requires unix-style symlinks")
	}
	// Every path here is created on disk: evalSymlinksOrClean falls
	// back to a lexical Clean for non-existent paths (same documented
	// trade-off as the config twin), so the resolution under test only
	// engages on real directory trees. t.TempDir() itself may sit under
	// a symlink (/var → /private/var on macOS), which the resolution
	// handles for free.
	tmp := t.TempDir()
	root := filepath.Join(tmp, "lib")
	if err := os.MkdirAll(filepath.Join(root, "variants"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(tmp, "link")
	if err := os.Symlink(root, link); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}

	// <tmp>/link/variants is lexically a sibling of <tmp>/lib but
	// resolves to <tmp>/lib/variants — under the root.
	candidate := filepath.Join(link, "variants")
	if err := assertNotUnderLibraryRoots(candidate, []string{root}); err == nil {
		t.Errorf("symlinked path resolving under library root was accepted; lexical-only check regressed")
	}

	// Inverse direction: the ROOT is configured via a symlink and the
	// candidate names the resolved tree directly.
	candidate2 := filepath.Join(root, "variants")
	if err := assertNotUnderLibraryRoots(candidate2, []string{link}); err == nil {
		t.Errorf("path under symlinked library root was accepted; root-side resolution regressed")
	}

	// A genuine sibling stays accepted with symlinked roots in play.
	sibling := filepath.Join(tmp, "variants")
	if err := os.MkdirAll(sibling, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := assertNotUnderLibraryRoots(sibling, []string{link}); err != nil {
		t.Errorf("sibling path rejected: %v", err)
	}
}
