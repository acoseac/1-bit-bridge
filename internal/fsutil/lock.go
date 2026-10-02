package fsutil

import (
	"errors"
	"os"
)

// ErrLocked is TryLock's answer when another open of the file holds its
// lock: an open in another process, or another *os.File in this one.
var ErrLocked = errors.New("locked by another open of the file")

// TryLock takes an exclusive lock on f without waiting for it. It answers
// nil once f holds the lock, an error wrapping ErrLocked when another open
// of the file holds it, and any other error when no lock could be taken at
// all: a filesystem that keeps none (some NFS and SMB mounts answer ENOLCK
// or EOPNOTSUPP), or a platform this package has no lock for
// (errors.ErrUnsupported).
//
// The lock belongs to the OPEN FILE, not to the process: another *os.File
// of the same file, opened in this process, is refused like one opened in
// another. That is flock on unix and LockFileEx on Windows, never fcntl's
// record locks, which every open in one process shares and closing any of
// them releases. Unlock or Close releases it, and so does the process's
// exit however it exits, a SIGKILL included: the kernel drops the lock with
// the last descriptor, so it cannot go stale the way a lock FILE does (one
// whose presence is the lock, left behind by a crash). The file itself can
// stay; only the lock on it means anything.
//
// Advisory on unix (only another TryLock is refused); on Windows the
// locked range also refuses another handle's reads and writes, which is
// why the file it is taken on should be one nothing reads.
//
// A nil f is os.ErrInvalid, as os.File's own methods answer it.
func TryLock(f *os.File) error {
	if f == nil {
		return &os.PathError{Op: "lock", Err: os.ErrInvalid}
	}
	return tryLock(f)
}

// Unlock releases the lock TryLock took on f. Closing f releases it too,
// but on Windows the release on a close may lag ("depends upon available
// system resources", LockFileEx's documentation), so a holder that means to
// hand the lock on at once unlocks before it closes. A nil f is
// os.ErrInvalid.
func Unlock(f *os.File) error {
	if f == nil {
		return &os.PathError{Op: "unlock", Err: os.ErrInvalid}
	}
	return unlock(f)
}

// controlFD runs fn on f's descriptor and wraps what it answers in an
// *os.PathError naming op and f. Through SyscallConn rather than Fd, which
// on unix also puts the descriptor in blocking mode.
func controlFD(f *os.File, op string, fn func(fd uintptr) error) error {
	rc, err := f.SyscallConn()
	if err != nil {
		return &os.PathError{Op: op, Path: f.Name(), Err: err}
	}
	var opErr error
	if err := rc.Control(func(fd uintptr) { opErr = fn(fd) }); err != nil {
		return &os.PathError{Op: op, Path: f.Name(), Err: err}
	}
	if opErr != nil {
		return &os.PathError{Op: op, Path: f.Name(), Err: opErr}
	}
	return nil
}
