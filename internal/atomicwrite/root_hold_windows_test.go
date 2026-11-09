package atomicwrite

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

// TestAnOpenRootHoldsItsDirectory pins what the Windows skips here and in
// internal/analyze rest on (backlog B652, round 5): os.OpenRoot opens the
// directory with syscall.Open, whose share mode has no FILE_SHARE_DELETE, so
// while the root is open Windows refuses to rename or remove the directory it
// holds, and once the root is closed the rename goes through.
func TestAnOpenRootHoldsItsDirectory(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "waveforms")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	r, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	open := true
	defer func() {
		if open {
			r.Close()
		}
	}()
	moved := filepath.Join(parent, "moved")
	if err := os.Rename(dir, moved); !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
		t.Fatalf("rename while the root is open = %v, want a sharing violation", err)
	}
	if err := os.Remove(dir); !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
		t.Fatalf("remove while the root is open = %v, want a sharing violation", err)
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		t.Fatalf("the directory after both refusals: %v", err)
	}
	open = false
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(dir, moved); err != nil {
		t.Fatalf("rename once the root is closed: %v", err)
	}
}
