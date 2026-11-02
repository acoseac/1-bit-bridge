//go:build unix

package fsutil_test

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/fsutil"
	"github.com/acoseac/1-bit-bridge/internal/fsutil/fsutiltest"
)

// TestTheWalkUpAPathSaysWhatItCannotRead: an Lstat on the way that fails for
// another reason than an entry's absence, and an entry there whose links
// cannot be followed, are the walk's error, never a blocker nor nothing in the
// way, so bridge doctor's data-dir line says "not checked" of them: a link
// cycle above the path or at it (ELOOP), a link through a regular file, whose
// os.Stat answers ENOTDIR on unix, and a directory this user cannot search
// (EACCES, not as root). A named pipe at the path is in the way, named by its
// kind, and read without opening it.
func TestTheWalkUpAPathSaysWhatItCannotRead(t *testing.T) {
	base := t.TempDir()
	loop1, loop2 := filepath.Join(base, "loop1"), filepath.Join(base, "loop2")
	file := filepath.Join(base, "file")
	through := filepath.Join(base, "through")
	for _, link := range [][2]string{{loop2, loop1}, {loop1, loop2}, {filepath.Join(file, "x"), through}} {
		if err := os.Symlink(link[0], link[1]); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(file, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, path string
		want       error
	}{
		{"a link cycle above the path", filepath.Join(loop1, "data"), syscall.ELOOP},
		{"a link cycle at the path", loop1, syscall.ELOOP},
		{"a link through a regular file", through, syscall.ENOTDIR},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if nearest, blocker, err := fsutil.BlockingEntry(tc.path); nearest != "" || blocker != nil || !errors.Is(err, tc.want) {
				t.Errorf("BlockingEntry(%s) = %q, %+v, %v; want no answer and %v", tc.path, nearest, blocker, err, tc.want)
			}
		})
	}
	t.Run("below a directory this user cannot search", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root searches a directory of mode 0")
		}
		locked := filepath.Join(base, "locked")
		if err := os.Mkdir(locked, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(locked, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
		path := filepath.Join(locked, "data")
		if nearest, blocker, err := fsutil.BlockingEntry(path); nearest != "" || blocker != nil || !errors.Is(err, syscall.EACCES) {
			t.Errorf("BlockingEntry(%s) = %q, %+v, %v; want no answer and EACCES", path, nearest, blocker, err)
		}
	})
	t.Run("a named pipe at the path", func(t *testing.T) {
		pipe := filepath.Join(base, "pipe")
		fsutiltest.MakeFIFO(t, pipe)
		nearest, blocker, err := fsutil.BlockingEntry(filepath.Join(pipe, "data"))
		if want := (fsutil.EntryInTheWay{Entry: pipe, Kind: "a named pipe"}); err != nil || nearest != pipe || blocker == nil || *blocker != want {
			t.Errorf("BlockingEntry(below a named pipe) = %q, %+v, %v; want %s and %+v", nearest, blocker, err, pipe, want)
		}
	})
}
