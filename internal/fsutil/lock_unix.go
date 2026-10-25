//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package fsutil

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func tryLock(f *os.File) error {
	return controlFD(f, "flock", func(fd uintptr) error {
		return lockedErr(flockRetryingEINTR(fd, unix.LOCK_EX|unix.LOCK_NB))
	})
}

// lockedErr is TryLock's reading of what flock(2) answered: EWOULDBLOCK, what
// it answers for a held lock, is ErrLocked, and anything else is itself. On
// every GOOS this file builds for EWOULDBLOCK is the same Errno as EAGAIN
// (Linux's pair is arch-independent, MIPS included; the BSDs and macOS give
// both 35), so this matches an NFS server's EAGAIN as well. EINTR never comes
// here (flockRetryingEINTR asks again), and ENOLCK and EOPNOTSUPP, a
// filesystem that keeps no locks, stay themselves.
func lockedErr(err error) error {
	if errors.Is(err, unix.EWOULDBLOCK) {
		return ErrLocked
	}
	return err
}

// keepsNoLocks is KeepsNoLocks on these platforms: flock's own answers for a
// filesystem that keeps no locks (an NFS mount without a lock manager
// answers ENOLCK; one that does not do flock, and some FUSE and SMB mounts,
// EOPNOTSUPP or ENOTSUP, which are one Errno on Linux and two on macOS and
// the BSDs), and errors.ErrUnsupported, which a kernel without flock
// (ENOSYS) matches too.
func keepsNoLocks(err error) bool {
	return errors.Is(err, unix.ENOLCK) || errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.ENOTSUP) ||
		errors.Is(err, errors.ErrUnsupported)
}

func unlock(f *os.File) error {
	return controlFD(f, "flock", func(fd uintptr) error {
		return flockRetryingEINTR(fd, unix.LOCK_UN)
	})
}

// flockRetryingEINTR is flock(2), asked again when a signal interrupted
// it: Go's runtime signals its own threads (preemption), and an
// interrupted call has neither taken nor refused the lock.
func flockRetryingEINTR(fd uintptr, how int) error {
	for {
		err := unix.Flock(int(fd), how)
		if !errors.Is(err, unix.EINTR) {
			return err
		}
	}
}
