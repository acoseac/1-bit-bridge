//go:build !(darwin || dragonfly || freebsd || linux || netbsd || openbsd || windows)

package config

import "errors"

// noLockAnswers is what a lock attempt answers on a platform with no lock
// (fsutil's lock_other.go).
func noLockAnswers() []error { return []error{errors.ErrUnsupported} }

// otherLockFailures are failures to take the lock that say nothing about the
// filesystem keeping no locks.
func otherLockFailures() []error { return []error{errors.New("some other failure")} }
