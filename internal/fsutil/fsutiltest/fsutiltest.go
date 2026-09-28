//go:build unix

// Package fsutiltest makes the entries that are not files, which a test of
// fsutil.NotAFile's refusals needs on disk: a named pipe and a Unix socket.
// Like net/http/httptest it is imported only by tests, so none of it
// reaches the binary. Only a POSIX host can make them.
//
// It also bounds a request against a handler that may wait on a named pipe,
// the defect those tests look for: a handler that opens a FIFO with a plain
// open waits for a writer, forever, so a test that simply waited would hang
// the suite, and httptest.Server.Close waits for every request it serves.
package fsutiltest

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// MakeFIFO makes a named pipe at p.
func MakeFIFO(t testing.TB, p string) {
	t.Helper()
	if err := syscall.Mkfifo(p, 0o644); err != nil {
		t.Fatalf("mkfifo %s: %v", p, err)
	}
}

// BindSocket makes a Unix socket at p. A socket's address holds at most 104
// bytes on macOS, which a test's temp dir overruns, so it is bound by its
// name from inside its directory, which changes the test's working
// directory (t.Chdir).
func BindSocket(t *testing.T, p string) {
	t.Helper()
	t.Chdir(filepath.Dir(p))
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Base(p), Net: "unix"})
	if err != nil {
		t.Fatalf("listen on %s: %v", p, err)
	}
	// Closing a listener removes its socket unless told not to, and the
	// entry has to outlive it.
	l.SetUnlinkOnClose(false)
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
}

// PlantNotAFiles makes in dir one entry of each kind a route that serves a
// file's bytes must refuse, each named like a track: a named pipe, a link to
// it, a link to a character device (os.DevNull) and a Unix socket, last,
// since BindSocket changes the working directory. It returns their names
// with the kind fsutil.NotAFile gives each, and the named pipe's path,
// which an open that waits is waiting on.
func PlantNotAFiles(t *testing.T, dir string) (kinds map[string]string, pipe string) {
	t.Helper()
	pipe = filepath.Join(dir, "Pipe.flac")
	MakeFIFO(t, pipe)
	for name, target := range map[string]string{"Linked-Pipe.flac": pipe, "Null.flac": os.DevNull} {
		if err := os.Symlink(target, filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	BindSocket(t, filepath.Join(dir, "Socket.flac"))
	return map[string]string{
		"Pipe.flac":        "named pipe",
		"Linked-Pipe.flac": "named pipe",
		"Null.flac":        "character device",
		"Socket.flac":      "socket",
	}, pipe
}

// UnblockFIFOs plays the writer for every named pipe in fifos, once: it
// opens each for writing and closes it at once, which lets an open waiting
// on it for a writer return. The open is nonblocking, so with no reader
// waiting it fails at once (ENXIO) rather than waiting for one.
func UnblockFIFOs(fifos ...string) {
	for _, p := range fifos {
		if w, err := os.OpenFile(p, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			_ = w.Close()
		}
	}
}

// AwaitPastFIFOs waits for done, which closes when the work a test started
// has returned, and reports whether it closed within bound. Work still
// running after bound is the defect these tests look for, an open waiting
// on one of fifos for a writer: AwaitPastFIFOs reports it, naming the work
// by what, then plays the writer (UnblockFIFOs) until the work returns, so
// the failure
// neither hangs the suite nor leaves the work running under the test's
// cleanups, and gives up with Fatalf 30 s after the first write.
func AwaitPastFIFOs(t testing.TB, what string, bound time.Duration, done <-chan struct{}, fifos ...string) bool {
	t.Helper()
	select {
	case <-done:
		return true
	case <-time.After(bound):
	}
	t.Errorf("%s: still running after %v, waiting to open a named pipe", what, bound)
	giveUp := time.After(30 * time.Second)
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-done:
			return false
		case <-giveUp:
			t.Fatalf("%s: still running 30 s after its named pipes were first written to", what)
		case <-tick.C:
			UnblockFIFOs(fifos...)
		}
	}
}

// ServeBound is how long ServeWithin lets a request run before it calls the
// handler stuck: a refusal is a stat and a JSON body, well under a second,
// with room for a loaded race runner.
const ServeBound = 5 * time.Second

// ServeWithin serves req through h on a goroutine of its own and returns
// what it wrote, bounding the request by ServeBound (AwaitPastFIFOs).
func ServeWithin(t testing.TB, h http.Handler, req *http.Request, fifos ...string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.ServeHTTP(rec, req)
	}()
	AwaitPastFIFOs(t, req.Method+" "+req.URL.String(), ServeBound, done, fifos...)
	return rec
}
