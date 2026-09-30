//go:build unix

package fsutil_test

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/acoseac/1-bit-bridge/internal/fsutil"
	"github.com/acoseac/1-bit-bridge/internal/fsutil/fsutiltest"
)

// openBounded runs fsutil.OpenAsFile(p) within fsutiltest.ServeBound,
// playing the writer on fifos if it is still waiting then, so a test of the
// defect neither hangs nor leaves the open behind.
func openBounded(t *testing.T, p string, fifos ...string) (*os.File, os.FileInfo, error) {
	t.Helper()
	var (
		f    *os.File
		info os.FileInfo
		err  error
	)
	done := make(chan struct{})
	go func() {
		defer close(done)
		f, info, err = fsutil.OpenAsFile(p)
	}()
	fsutiltest.AwaitPastFIFOs(t, "OpenAsFile("+filepath.Base(p)+")", fsutiltest.ServeBound, done, fifos...)
	return f, info, err
}

// TestOpenAsFileRefusesWhatIsNotAFileWithoutWaiting: a named pipe, a link to
// one and a link to a character device are refused at once, each named by
// its kind, and a socket, which no open reaches, fails with the kernel's own
// error (EOPNOTSUPP on macOS, ENXIO on Linux). A plain os.Open of the pipe
// waits for a writer, and nothing can cancel the wait; that is the open
// /v1/download, /v1/read, the web player's audio route and the DLNA file
// route made until 2026-09-28.
func TestOpenAsFileRefusesWhatIsNotAFileWithoutWaiting(t *testing.T) {
	dir := t.TempDir()
	kinds, pipe := fsutiltest.PlantNotAFiles(t, dir)
	for name, kind := range kinds {
		t.Run(name, func(t *testing.T) {
			f, info, err := openBounded(t, filepath.Join(dir, name), pipe)
			if f != nil || info != nil {
				_ = f.Close()
				t.Fatalf("opened a %s as a file", kind)
			}
			want := kind
			if kind == "socket" {
				// Refused by the kernel before there is a file to stat.
				want = ""
			}
			if got := fsutil.NotAFileKind(err); err == nil || got != want {
				t.Errorf("refused as %q (%v), want %q", got, err, want)
			}
		})
	}
}

// TestOpenAsFileLeavesTheFileItOpensBlocking: the O_NONBLOCK that lets a
// named pipe's open return is cleared once the file is known to be one, so
// its reads are a plain open's. A FUSE daemon is handed the file's flags
// with every read, and one that honours the flag can answer EAGAIN where a
// plain open's read would have waited for the data.
func TestOpenAsFileLeavesTheFileItOpensBlocking(t *testing.T) {
	p := filepath.Join(t.TempDir(), "01 Track.flac")
	if err := os.WriteFile(p, []byte("fLaC"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, _, err := fsutil.OpenAsFile(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	rc, err := f.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var flags int
	var flagsErr error
	if err := rc.Control(func(fd uintptr) { flags, flagsErr = unix.FcntlInt(fd, unix.F_GETFL, 0) }); err != nil {
		t.Fatal(err)
	}
	if flagsErr != nil {
		t.Fatal(flagsErr)
	}
	if flags&unix.O_NONBLOCK != 0 {
		t.Errorf("the opened file is still O_NONBLOCK (flags %#x)", flags)
	}
}

// TestReadAsFileRefusesWhatIsNotAFileWithoutWaiting: ReadAsFile refuses a
// named pipe, a link to one and a link to a character device at once, each
// named by its kind, as OpenAsFile refuses them. os.ReadFile of the pipe
// waited for a writer, and of a link to /dev/zero read until the process ran
// out of memory: what the scanner's folder-art and lyrics-sidecar reads did
// until 2026-09-29.
func TestReadAsFileRefusesWhatIsNotAFileWithoutWaiting(t *testing.T) {
	dir := t.TempDir()
	kinds, pipe := fsutiltest.PlantNotAFiles(t, dir)
	for name, kind := range kinds {
		if kind == "socket" {
			// Refused by the kernel before there is a file to stat
			// (TestOpenAsFileRefusesWhatIsNotAFileWithoutWaiting).
			continue
		}
		t.Run(name, func(t *testing.T) {
			var (
				body []byte
				err  error
			)
			done := make(chan struct{})
			go func() {
				defer close(done)
				body, err = fsutil.ReadAsFile(filepath.Join(dir, name))
			}()
			fsutiltest.AwaitPastFIFOs(t, "ReadAsFile("+name+")", fsutiltest.ServeBound, done, pipe)
			if body != nil || fsutil.NotAFileKind(err) != kind {
				t.Errorf("read %d bytes, %v; want the %s refused", len(body), err, kind)
			}
		})
	}
}
