//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package config

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

// TestATurnRefusesANamedPipeAtOnce: a named pipe where the config file goes
// is refused at once, never waited on and never asked again: the turn reads
// the file as a file (fsutil.ReadAsFile), whatever its read asks again on.
// A plain read of a pipe waits for a writer, holding the lock and every other
// writer of the file with it.
func TestATurnRefusesANamedPipeAtOnce(t *testing.T) {
	dir := realTempDir(t)
	path := filepath.Join(dir, "bridge.yaml")
	if err := unix.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(atomicwrite.SetReadRetryableForTest(func(err error) bool { return errors.Is(err, errBusyForTest) }))
	done := make(chan error, 1)
	go func() {
		l, err := LockFile(path)
		if err == nil {
			l.Unlock()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || fsutil.NotAFileKind(err) != "named pipe" {
			t.Errorf("LockFile over a named pipe = %v, want it refused as a named pipe", err)
		}
	case <-time.After(10 * time.Second):
		// Let the read the test is about go: a writer's open releases it.
		if w, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			_ = w.Close()
		}
		t.Fatal("LockFile waited on a named pipe where the config file goes")
	}
}
