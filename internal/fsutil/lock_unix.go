//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package fsutil

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func tryLock(f *os.File) error {
	return controlFD(f, "flock", func(fd uintptr) error {
		err := flockRetryingEINTR(fd, unix.LOCK_EX|unix.LOCK_NB)
		// EWOULDBLOCK is what flock(2) answers for a held lock, and on
		// every GOOS this file builds for it is the same Errno as EAGAIN
		// (Linux's pair is arch-independent, MIPS included; the BSDs and
		// macOS give both 35), so this matches an NFS server's EAGAIN as
		// well.
		if errors.Is(err, unix.EWOULDBLOCK) {
			return ErrLocked
		}
		return err
	})
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
