package fsutil

import (
	"os"
	"path/filepath"
	"testing"
)

// On a case-insensitive filesystem (macOS/Windows) a variants dir whose only
// difference from a library root is letter case points at the SAME physical
// directory, so IsUnderAny must report it as nested — otherwise the containment
// guard lets sidecars be written inside the (possibly read-only) library root
// (the PR #475 phantom-rows class).
func TestIsUnderAny_CaseInsensitiveFSDetectsNested(t *testing.T) {
	base := t.TempDir()
	// Probe the actual temp filesystem rather than assuming from GOOS — the
	// case-only-nesting assertion below holds ONLY on a case-insensitive volume
	// (covers case-sensitive macOS / case-insensitive Linux mounts too).
	if !caseInsensitiveFS(base) {
		t.Skip("case-only nesting only applies on case-insensitive filesystems")
	}
	root := filepath.Join(base, "Music")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	// Same base, root component case-swapped, plus a not-yet-created variants dir.
	candidate := filepath.Join(base, "music", "variants")
	if got := IsUnderAny(candidate, []string{root}); got == "" {
		t.Errorf("IsUnderAny(%q, [%q]) = \"\" (not-under); want nested (case-only difference)", candidate, root)
	}
}

// A genuinely-sibling path (not merely case-different) must still be reported
// as NOT under the root on every platform — the case-fold must not over-match.
func TestIsUnderAny_CaseFoldDoesNotOverMatchSibling(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "Music")
	sibling := filepath.Join(base, "Movies", "x") // different dir, not nested
	if got := IsUnderAny(sibling, []string{root}); got != "" {
		t.Errorf("IsUnderAny(%q, [%q]) = %q; want \"\" (sibling, not nested)", sibling, root, got)
	}
}
