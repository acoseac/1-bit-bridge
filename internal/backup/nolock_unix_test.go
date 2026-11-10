//go:build unix

package backup_test

import (
	"errors"

	"golang.org/x/sys/unix"
)

// noLockAnswer is a try's answer that grants no lock (fsutil.KeepsNoLocks).
type noLockAnswer struct {
	name string
	err  error
}

// noLockAnswers are the answers a try gives where no lock is granted:
// ENOLCK (an NFS mount with no lock service, a full lock table), and the
// platform's own word for a filesystem that keeps none.
func noLockAnswers() []noLockAnswer {
	return []noLockAnswer{
		{"ENOLCK", unix.ENOLCK},
		{"EOPNOTSUPP", unix.EOPNOTSUPP},
		{"unsupported", errors.ErrUnsupported},
	}
}
