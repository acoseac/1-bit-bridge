//go:build windows

package config

import (
	"errors"

	"golang.org/x/sys/windows"
)

// noLockAnswers are what LockFileEx answers where no lock can be taken at
// all: a filesystem or redirector that does not do byte-range locks
// (ERROR_NOT_SUPPORTED, ERROR_INVALID_FUNCTION), or a platform that has
// none.
func noLockAnswers() []error {
	return []error{windows.ERROR_NOT_SUPPORTED, windows.ERROR_INVALID_FUNCTION, errors.ErrUnsupported}
}

// otherLockFailures are failures to take the lock that say nothing about the
// filesystem keeping no locks.
func otherLockFailures() []error {
	return []error{windows.ERROR_INVALID_HANDLE, windows.ERROR_ACCESS_DENIED, windows.ERROR_LOCK_FAILED,
		errors.New("some other failure")}
}
