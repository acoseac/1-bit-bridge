//go:build windows

package fsutil

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// lockAllBytes, as both halves of LockFileEx's length, locks every byte a
// file can have, from offset zero: Go's own cmd/go lockedfile does the
// same.
const lockAllBytes = ^uint32(0)

func tryLock(f *os.File) error {
	return controlFD(f, "LockFileEx", func(fd uintptr) error {
		// os.File's handle is synchronous, and LockFileEx still takes an
		// OVERLAPPED: its offset is where the range starts, zero.
		err := windows.LockFileEx(windows.Handle(fd),
			windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
			0, lockAllBytes, lockAllBytes, new(windows.Overlapped))
		if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return ErrLocked
		}
		return err
	})
}

func unlock(f *os.File) error {
	return controlFD(f, "UnlockFileEx", func(fd uintptr) error {
		return windows.UnlockFileEx(windows.Handle(fd), 0, lockAllBytes, lockAllBytes, new(windows.Overlapped))
	})
}
