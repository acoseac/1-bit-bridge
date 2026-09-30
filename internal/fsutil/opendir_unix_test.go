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

// TestOpenDirRefusesWhatIsNotADirectoryWithoutWaiting: a named pipe, a link
// to one, a link to a character device and a socket are each refused with
// ENOTDIR at once, and a link to a directory opens, as a linked album
// directory in a library does. A plain os.Open of the pipe waits for a
// writer, and nothing can cancel the wait: /v1/list opened the directory it
// lists that way until 2026-09-29.
func TestOpenDirRefusesWhatIsNotADirectoryWithoutWaiting(t *testing.T) {
	dir := t.TempDir()
	parked := t.TempDir()
	if err := os.Symlink(parked, filepath.Join(dir, "Linked Album")); err != nil {
		t.Fatal(err)
	}
	kinds, pipe := fsutiltest.PlantNotAFiles(t, dir)
	for name, kind := range kinds {
		t.Run(name, func(t *testing.T) {
			var (
				f   *os.File
				err error
			)
			done := make(chan struct{})
			go func() {
				defer close(done)
				f, err = fsutil.OpenDir(filepath.Join(dir, name))
			}()
			fsutiltest.AwaitPastFIFOs(t, "OpenDir("+name+")", fsutiltest.ServeBound, done, pipe)
			if f != nil {
				_ = f.Close()
				t.Fatalf("opened a %s as a directory", kind)
			}
			if !errors.Is(err, syscall.ENOTDIR) {
				t.Errorf("refused with %v, want ENOTDIR", err)
			}
		})
	}
	f, err := fsutil.OpenDir(filepath.Join(dir, "Linked Album"))
	if err != nil {
		t.Fatalf("OpenDir(a link to a directory): %v", err)
	}
	_ = f.Close()
}
