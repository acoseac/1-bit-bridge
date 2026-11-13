//go:build !windows

package updater

import (
	"errors"
	"syscall"
	"testing"
)

// errFileInUse stands in for Windows' ERROR_SHARING_VIOLATION, which no
// rename here answers.
var errFileInUse = errors.New("The process cannot access the file because it is being used by another process.")

// inUseFailure is what a rename answers while another process holds its
// file, as the rollback reads it on this platform: errFileInUse, with
// renameInUse made to take it as Windows' inUseByAnotherProcess takes the
// real one, until the test ends.
func inUseFailure(t *testing.T) error {
	t.Helper()
	prev := renameInUse
	t.Cleanup(func() { renameInUse = prev })
	renameInUse = func(err error) bool { return errors.Is(err, errFileInUse) }
	return errFileInUse
}

// TestNoRenameFailureIsAFileInUseOffWindows: off Windows a rename never
// waits on another process's open file, so the platform's answer takes no
// failure for one, Windows' numbers for a sharing and a lock violation
// included (here a broken pipe and a domain error).
func TestNoRenameFailureIsAFileInUseOffWindows(t *testing.T) {
	for _, err := range []error{errFileInUse, syscall.Errno(32), syscall.Errno(33), syscall.EACCES, syscall.EBUSY} {
		if inUseByAnotherProcess(err) {
			t.Errorf("inUseByAnotherProcess(%v) = true off Windows", err)
		}
	}
}
