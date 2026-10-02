//go:build !(darwin || dragonfly || freebsd || linux || netbsd || openbsd || windows)

package fsutil

import (
	"errors"
	"os"
)

// tryLock has no lock to take on this platform, and says so: a caller
// treats that as a filesystem that keeps no locks.
func tryLock(f *os.File) error {
	return &os.PathError{Op: "lock", Path: f.Name(), Err: errors.ErrUnsupported}
}

func unlock(*os.File) error { return nil }
