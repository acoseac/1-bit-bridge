package fsutil

import (
	"os"
	"path/filepath"
	"testing"
)

// TestIsUnderAny is the canonical containment truth table, shared by the CLI
// (bridge variants move), the admin variants-dir handler, and
// config.validateVariantsDir. "rel == '..'" / "rel starts with '..'+sep" means
// the candidate is OUTSIDE the root; anything else — including "." (equal) and
// dot-prefixed subpaths like ".cache/x" — means AT-OR-UNDER.
func TestIsUnderAny(t *testing.T) {
	roots := []string{"/library/music", "/library/audio"}
	cases := []struct {
		name string
		to   string
		want string // "" means safe; otherwise the matching root.
	}{
		{"outside every root — safe", "/data/transcoded", ""},
		{"outside every root — sibling of root", "/library/transcoded", ""},
		{"equal to root — REJECT", "/library/music", "/library/music"},
		{"direct child — REJECT", "/library/music/transcoded", "/library/music"},
		{"deep child — REJECT", "/library/music/Artist/Album/cache", "/library/music"},
		{"dot-prefixed child — REJECT", "/library/music/.cache", "/library/music"},
		{"dot-prefixed deep child — REJECT", "/library/music/.cache/variants", "/library/music"},
		{"matches second root", "/library/audio/variants", "/library/audio"},
		{"empty root slot is ignored", "/data/transcoded", ""},
		{"trailing slash on root — same shape after clean", "/library/music/transcoded", "/library/music"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			testRoots := roots
			switch c.name {
			case "empty root slot is ignored":
				testRoots = []string{"", "/library/music", "/library/audio"}
			case "trailing slash on root — same shape after clean":
				testRoots = []string{"/library/music/", "/library/audio/"}
			}
			got := IsUnderAny(c.to, testRoots)
			// Compare via filepath.Clean so the test isn't fragile against
			// trailing-slash variations in `want`.
			if filepath.Clean(got) != filepath.Clean(c.want) {
				t.Errorf("got %q, want %q (to=%q)", got, c.want, c.to)
			}
		})
	}
}

// TestIsUnderAny_ResolvesSymlinkedParent pins the symlink guard: a candidate
// whose PARENT symlinks into a root must be caught even though the candidate
// itself doesn't exist yet. A lexical-only Clean would let os.MkdirAll write
// through the symlink into the read-only library (PR #75 invariant).
func TestIsUnderAny_ResolvesSymlinkedParent(t *testing.T) {
	realRoot := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(realRoot, link); err != nil {
		t.Skipf("symlinks unsupported on this platform/host: %v", err)
	}
	// link -> realRoot; "variants" under it doesn't exist yet.
	candidate := filepath.Join(link, "variants")
	if got := IsUnderAny(candidate, []string{realRoot}); got == "" {
		t.Fatalf("symlinked-parent candidate %q not detected as under root %q", candidate, realRoot)
	}
}
