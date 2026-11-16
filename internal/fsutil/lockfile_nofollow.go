package fsutil

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// ErrLockFileNotAFile is OpenLockFileNoFollow's answer for an entry at the
// lock file's name that is no regular file: a link (to anything, or to
// nothing), a directory, a named pipe, a device.
var ErrLockFileNotAFile = errors.New("the lock file is not a regular file")

// OpenLockFileNoFollow opens the lock file name in dir for TryLock or
// TryLockShared, as OpenLockFileLike does, but never follows a link at name:
// a link there, whether it leads to a file, to a directory or to nothing, is
// refused (ErrLockFileNotAFile, inside an *fs.PathError naming the path)
// before anything it names is made or opened, and so is any entry that is no
// regular file. OpenLockFileLike opens through dir's os.Root, which follows a
// link that stays inside dir, so a dangling `bridge.db-open.lock` link to
// `bridge.db` would make the database at the lock's first open, before the
// store's own link check ran (B653 PR 2a's brief review). The directories
// above name are followed as the path names them: a data dir that is a link
// is the operator's.
//
// write opens it for writing, creating it when it is missing: a file this
// process makes as root first takes the owner a file staged for like would
// (Precreate), with perm; an exclusive lock on a Linux NFS mount, carried out
// as a POSIX record lock, wants a descriptor open for writing. Otherwise it
// is opened for reading and never created: a shared flock, a POSIX read lock
// and a shared LockFileEx each take a descriptor open for reading, which is
// what a user who may read the lock file but not write it can hold
// (OpenRefusedForWriting says which answers to a write open that is).
//
// The caller closes the file, after Unlock.
func OpenLockFileNoFollow(dir, name, like string, perm os.FileMode, write bool) (*os.File, error) {
	path := filepath.Join(dir, name)
	if write {
		// O_EXCL, so a link at path is EEXIST here, never followed, and the
		// open below refuses it.
		if err := Precreate(path, perm, like); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, err
		}
	}
	for try := 1; ; try++ {
		f, err := openLockFileNoFollow(path, perm, write)
		// A create that loses a race with another opener's create answers
		// ENOENT on macOS 27 through openat(2) (OpenLockFile's measurement);
		// asked again as OpenLockFile asks.
		if err == nil || !write || !errors.Is(err, fs.ErrNotExist) || try == lockFileOpenTries {
			return f, err
		}
	}
}

// OpenRefusedForWriting reports whether err, what an open of a file for
// writing answered, says this process may not write the file or its
// filesystem, where an open for reading may still serve: EACCES and EPERM
// (fs.ErrPermission), and EROFS (Windows: ERROR_ACCESS_DENIED, which is
// fs.ErrPermission there, and ERROR_WRITE_PROTECT).
func OpenRefusedForWriting(err error) bool {
	return err != nil && (errors.Is(err, fs.ErrPermission) || openRefusedForWriting(err))
}
