//go:build windows

package fsutil

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestWalkableRootDescendsARealJunction makes a directory junction, which
// needs no privilege (a symbolic link does), and walks it: the OS half of
// what TestWalkableRootTakesAWindowsJunction drives with stats. os.Lstat of
// a junction is ModeIrregular without ModeDir, so a walk of the junction
// itself visits it alone; through WalkableRoot it descends, under the
// junction's own spelling.
func TestWalkableRootDescendsARealJunction(t *testing.T) {
	target := libraryTree(t)
	junction := filepath.Join(t.TempDir(), "Music")
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", junction, target).CombinedOutput(); err != nil {
		t.Fatalf("mklink /J: %v: %s", err, out)
	}
	own, err := os.Lstat(junction)
	if err != nil {
		t.Fatal(err)
	}
	if own.IsDir() || own.Mode()&fs.ModeIrregular == 0 {
		t.Fatalf("premise: os.Lstat of a junction is %v, want ModeIrregular without ModeDir", own.Mode())
	}
	requireWalksLibraryTree(t, junction)
}
