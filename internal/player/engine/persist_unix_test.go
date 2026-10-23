//go:build unix

package engine

import (
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestANamedPipeAtTheEnginesFilesIsMovedAsideAtOnce pins the engine's own
// files, the queue file and the reservation file, opened without waiting and
// read only when they are files: a named pipe at either path is a bad file,
// moved aside at once as any bad file is, and the engine starts from what
// remains. A plain open of the pipe waited for a writer, which held New.
func TestANamedPipeAtTheEnginesFilesIsMovedAsideAtOnce(t *testing.T) {
	for _, tc := range []struct {
		name string
		path func(h *harness) string
	}{
		{"the queue file", (*harness).queuePath},
		{"the reservation file", (*harness).reservePath},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			path := tc.path(h)
			if err := syscall.Mkfifo(path, 0o600); err != nil {
				t.Fatal(err)
			}
			h.be = newFakeBackend(t)
			cfg := h.config()
			type built struct {
				e   *Engine
				err error
			}
			done := make(chan built, 1)
			go func() {
				e, err := New(cfg)
				done <- built{e, err}
			}()
			var b built
			select {
			case b = <-done:
			case <-time.After(*watchdog):
				// An open that waits for a writer is let go by one, so New
				// returns before the test's directory goes.
				go func() {
					if w, err := os.OpenFile(path, os.O_WRONLY, 0); err == nil {
						_ = w.Close()
					}
				}()
				<-done
				t.Fatalf("New did not return within %v with a named pipe at %s", *watchdog, filepath.Base(path))
			}
			if b.err != nil {
				t.Fatalf("New: %v", b.err)
			}
			h.eng = b.e
			t.Cleanup(func() { h.joined(b.e) })
			t.Cleanup(h.be.close)
			h.run()
			aside, _ := filepath.Glob(path + ".bad-*")
			if len(aside) != 1 {
				t.Fatalf("moved aside: %v, want the pipe", aside)
			}
			if info, err := os.Lstat(aside[0]); err != nil || info.Mode()&fs.ModeNamedPipe == 0 {
				t.Fatalf("what was moved aside is not the pipe: %v, %v", info, err)
			}
			if n := h.logs.count("named pipe is not a file"); n != 1 {
				t.Fatalf("%d lines say the pipe is not a file, want 1: %v", n, h.logs.all())
			}
			h.insert("a")
			if ed := h.snap().Edits(); ed.Refused != "" {
				t.Fatalf("edits %+v once the pipe was moved aside", ed)
			}
		})
	}
}
