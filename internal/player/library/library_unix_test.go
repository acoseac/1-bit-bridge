//go:build unix

package library

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// TestTheResolverRefusesANamedPipeWithoutWaiting: a named pipe at a row's
// path is not_a_file, refused at once: a plain open of one waits for a
// writer, which nothing could cancel, and the backend's opener would wait
// with it. Mkfifo comes from x/sys, which has it wherever the unix tag
// builds.
func TestTheResolverRefusesANamedPipeWithoutWaiting(t *testing.T) {
	f := newLibraryFixture(t)
	if err := unix.Mkfifo(filepath.Join(f.root, "pipe.flac"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := f.open(f.index("pipe.flac"))
	f.refusedWith("a named pipe", c, err, ReasonNotAFile)
	// A writer that opens without waiting finds no reader left on the pipe.
	if w, err := os.OpenFile(filepath.Join(f.root, "pipe.flac"), os.O_WRONLY|unix.O_NONBLOCK, 0); err == nil {
		_ = w.Close()
		t.Error("a reader still holds the pipe: the open was not closed")
	}
}
