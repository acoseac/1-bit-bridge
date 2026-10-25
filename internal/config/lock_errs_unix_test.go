//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package config

import (
	"errors"

	"golang.org/x/sys/unix"
)

// noLockAnswers are what flock answers where no lock can be taken at all: a
// filesystem that keeps none (EOPNOTSUPP and ENOTSUP, from some NFS, FUSE and
// SMB mounts), or a platform that has none. ENOLCK is not one of them: it is
// also a kernel out of lock records beside a writer that holds the lock
// (lock_nolock_unix_test.go).
func noLockAnswers() []error {
	return []error{unix.EOPNOTSUPP, unix.ENOTSUP, errors.ErrUnsupported}
}

// otherLockFailures are failures to take the lock that say nothing about the
// filesystem keeping no locks.
func otherLockFailures() []error {
	return []error{unix.EIO, unix.EBADF, unix.EINVAL, errors.New("some other failure")}
}
