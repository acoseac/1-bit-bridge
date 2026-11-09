package fsutil

import (
	"fmt"
	"os"
	"path/filepath"
)

// The helpers here are the owner-keeping and durability helpers through an
// os.Root: for a writer that opened the directory it writes into once and
// does every operation through that handle, so that whatever comes to be at
// the directory's path later (a volume unmounted under its mountpoint, the
// local directory there again), nothing is made, removed or synced anywhere
// but the directory it opened. The analysis pool's waveform-only repair is
// the one (backlog B652, round 2). Every name is relative to the root.

// MkdirIn is Mkdir through r: name, relative to r, is made in its directory
// in r, which is reached through r's own descriptors, and, run as root, the
// new directory takes the owner of the directory it is made in, read through
// r as Mkdir's directory takes its parent's. Its error is r.Mkdir's, so a
// caller can still tell os.ErrExist apart. On Windows, where a directory
// takes its parent's ACL, it is r.Mkdir.
func MkdirIn(r *os.Root, name string, perm os.FileMode) error {
	return mkdirIn(r, name, perm)
}

// KeepOwnerIn is KeepOwner for a file staged through r: dst, relative to r,
// is the entry the staged file will be renamed onto, and the owner it keeps
// is that entry's own (r.Lstat, so a symlink's and never its target's), or,
// for a file that does not exist yet, the owner of the directory in r it is
// created in. A process that is not root changes nothing. On Windows it does
// nothing.
func KeepOwnerIn(r *os.Root, staged *os.File, dst string) error {
	return keepOwnerIn(r, staged, dst)
}

// FsyncInRoot is FsyncFileAndParent through r: name, relative to r, opened
// through r and flushed, and on POSIX its directory in r too, so the barrier
// reaches the file r holds, whatever the path names now.
func FsyncInRoot(r *os.Root, name string) error {
	// O_RDWR for the reason FsyncFileAndParent gives: FlushFileBuffers on
	// Windows needs GENERIC_WRITE.
	f, err := r.OpenFile(name, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open for fsync: %w", err)
	}
	if syncErr := f.Sync(); syncErr != nil {
		_ = f.Close()
		return fmt.Errorf("fsync file: %w", syncErr)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close after fsync: %w", err)
	}
	if err := syncDirIn(r, filepath.Dir(name)); err != nil {
		return fmt.Errorf("fsync parent dir: %w", err)
	}
	return nil
}
