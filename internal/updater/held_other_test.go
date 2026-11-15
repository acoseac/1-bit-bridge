//go:build !windows

package updater

import (
	"syscall"
	"testing"
)

// TestNoRenameFailureIsAFileInUseOffWindows: off Windows a rename never waits
// on another process's open file, so the platform's predicate takes no failure
// for a hold, Windows' numbers for a sharing and a lock violation included
// (here a broken pipe and a domain error), and its probe moves nothing.
func TestNoRenameFailureIsAFileInUseOffWindows(t *testing.T) {
	for _, err := range []error{errHeld, syscall.Errno(32), syscall.Errno(33), syscall.EACCES, syscall.EBUSY} {
		if inUse(err) {
			t.Errorf("inUse(%v) = true off Windows", err)
		}
	}
	if err := movable("/a path that is not there"); err != nil {
		t.Errorf("movable = %v off Windows, want nil", err)
	}
}
