//go:build !unix

package backup_test

import "errors"

// noLockAnswer is a try's answer that grants no lock (fsutil.KeepsNoLocks).
type noLockAnswer struct {
	name string
	err  error
}

// noLockAnswers are the answers a try gives where no lock is granted: the
// platform's own word for a filesystem that keeps none.
func noLockAnswers() []noLockAnswer {
	return []noLockAnswer{{"unsupported", errors.ErrUnsupported}}
}
