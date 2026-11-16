//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package manifest

import (
	"errors"
	"io"
	"os"

	"github.com/acoseac/1-bit-bridge/internal/fsutil"
	"golang.org/x/sys/unix"
)

// recordLocksAvailable says whether useRecordLocks can stand in here.
const recordLocksAvailable = true

// useRecordLocks makes the lifetime lock's tries POSIX record locks
// (fcntl F_SETLK), which is how a Linux NFS mount carries flock out: every
// open of the file in one process shares one lock, and closing any of them
// releases it (fsutil.TryLock's doc). It returns what puts the platform's
// own tries back.
func useRecordLocks() (restore func()) {
	storeLockSeamMu.Lock()
	prevTry, prevUnlock := storeLockTry, storeLockUnlock
	storeLockTry = func(f *os.File, exclusive bool) error {
		typ := int16(unix.F_RDLCK)
		if exclusive {
			typ = unix.F_WRLCK
		}
		lk := unix.Flock_t{Type: typ, Whence: io.SeekStart}
		err := unix.FcntlFlock(f.Fd(), unix.F_SETLK, &lk)
		if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EACCES) {
			return fsutil.ErrLocked
		}
		return err
	}
	storeLockUnlock = func(f *os.File) error {
		lk := unix.Flock_t{Type: unix.F_UNLCK, Whence: io.SeekStart}
		return unix.FcntlFlock(f.Fd(), unix.F_SETLK, &lk)
	}
	storeLockSeamMu.Unlock()
	return func() {
		storeLockSeamMu.Lock()
		storeLockTry, storeLockUnlock = prevTry, prevUnlock
		storeLockSeamMu.Unlock()
	}
}

// noLocksErrno is a definitive no-locks answer of flock's, and noLockToGive
// its ENOLCK, ioErr an I/O error.
var (
	noLocksErrno = error(unix.EOPNOTSUPP)
	enolck       = error(unix.ENOLCK)
	ioErr        = error(unix.EIO)
)
