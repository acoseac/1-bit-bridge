package fsutil

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSamePathResolvesEachSpellingThroughItsLinks: a path spelled through a
// link to a directory and through the directory itself is one path, whether
// or not anything below the link exists yet, either way round; two names in
// one directory are two paths.
func TestSamePathResolvesEachSpellingThroughItsLinks(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unsupported on this platform/host: %v", err)
	}
	for _, rel := range []string{".", "bridge", filepath.Join("usb", "bridge")} {
		a, b := filepath.Join(link, rel), filepath.Join(real, rel)
		if !SamePath(a, b) || !SamePath(b, a) {
			t.Errorf("SamePath(%q, %q) = false: one path, spelled through a link and not", a, b)
		}
	}
	if SamePath(filepath.Join(real, "a"), filepath.Join(link, "b")) {
		t.Error("two names in one directory read as one path")
	}
}

// TestSamePathFoldsCaseWhereTheVolumeDoes: two spellings that differ in case
// alone are one path on a volume that folds case and two on one that does
// not, as the volume itself answers (caseInsensitiveFS), not the platform.
func TestSamePathFoldsCaseWhereTheVolumeDoes(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "Data")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	a, b := filepath.Join(base, "Data", "usb", "bridge"), filepath.Join(base, "data", "USB", "bridge")
	if got, want := SamePath(a, b), caseInsensitiveFS(dir); got != want {
		t.Errorf("SamePath(%q, %q) = %v on a volume that folds case: %v", a, b, got, want)
	}
}
