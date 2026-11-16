//go:build !(darwin || dragonfly || freebsd || linux || netbsd || openbsd || windows)

package fsutil

import (
	"os"
)

// openLockFileNoFollow is OpenLockFileNoFollow's open where this package has
// no lock (TryLock answers errors.ErrUnsupported): an entry that is no
// regular file is refused by Lstat before the open, which a link put in place
// between the two would pass. No lock is taken on the file this opens.
func openLockFileNoFollow(path string, perm os.FileMode, write bool) (*os.File, error) {
	if fi, err := os.Lstat(path); err == nil && !fi.Mode().IsRegular() {
		return nil, &os.PathError{Op: "open", Path: path, Err: ErrLockFileNotAFile}
	}
	flags := os.O_RDONLY
	if write {
		flags = os.O_RDWR | os.O_CREATE
	}
	return os.OpenFile(path, flags, perm)
}

// openRefusedForWriting is OpenRefusedForWriting's platform half: nothing
// more than fs.ErrPermission here.
func openRefusedForWriting(error) bool { return false }
