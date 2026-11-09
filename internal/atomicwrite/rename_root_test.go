package atomicwrite

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestRenameInRootRenamesInTheDirectoryItsRootHolds: the rename lands in the
// directory the root opened, with the path naming another directory, which
// is left as it was (backlog B652, round 2). It skips on Windows, where the
// root's handle keeps the directory it holds from being renamed while the
// root is open (backlog B652, round 4).
func TestRenameInRootRenamesInTheDirectoryItsRootHolds(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skipf("on Windows a root holds its directory: the root's handle shares no delete, " +
			"so the directory cannot be renamed while the root is open, which this case does; " +
			"backlog B819 holds the coverage this leaves there")
	}
	path := filepath.Join(t.TempDir(), "waveforms")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	r, err := os.OpenRoot(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err := os.WriteFile(filepath.Join(path, "curve.tmp"), []byte("curve"), 0o600); err != nil {
		t.Fatal(err)
	}
	aside := path + ".aside"
	if err := os.Rename(path, aside); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := RenameInRootCtx(context.Background(), r, "curve.tmp", "curve.bin"); err != nil {
		t.Fatalf("rename through the root: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(aside, "curve.bin")); err != nil || string(got) != "curve" {
		t.Errorf("the curve in the directory the root opened: %q (%v)", got, err)
	}
	if left, err := os.ReadDir(path); err != nil || len(left) != 0 {
		t.Errorf("the directory now at the path holds %v (%v), want nothing", left, err)
	}
}
