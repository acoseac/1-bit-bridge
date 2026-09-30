package fsutil

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"
)

// TestOpenDirOpensADirectoryAndRefusesAFile pins OpenDir on every platform:
// a directory opens and lists as os.Open's handle did, a file is refused
// with an *fs.PathError holding ENOTDIR (O_DIRECTORY's answer on unix, the
// opened file's stat's elsewhere), and a path that is not there comes back
// as os.OpenFile gave it.
func TestOpenDirOpensADirectoryAndRefusesAFile(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "01 Track.flac")
	if err := os.WriteFile(file, []byte("fLaC"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "Disc 1"), 0o755); err != nil {
		t.Fatal(err)
	}

	f, err := OpenDir(dir)
	if err != nil {
		t.Fatalf("OpenDir(a directory): %v", err)
	}
	names, err := f.Readdirnames(-1)
	_ = f.Close()
	sort.Strings(names)
	if err != nil || strings.Join(names, "|") != "01 Track.flac|Disc 1" {
		t.Errorf("listed %q, %v; want the directory's two entries", names, err)
	}

	f, err = OpenDir(file)
	var pe *fs.PathError
	switch {
	case f != nil:
		_ = f.Close()
		t.Errorf("OpenDir(a file) returned a handle")
	case !errors.Is(err, syscall.ENOTDIR):
		t.Errorf("OpenDir(a file): %v; want ENOTDIR", err)
	case !errors.As(err, &pe) || pe.Path != file:
		t.Errorf("OpenDir(a file): %v; want an *fs.PathError naming %s", err, file)
	}

	if _, err := OpenDir(filepath.Join(dir, "missing")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("OpenDir(a missing path): %v; want os.OpenFile's not-exist error", err)
	}
}
