//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package fsutil

import (
	"errors"
	"testing"

	"golang.org/x/sys/unix"
)

// TestLockedErrMapsOnlyAHeldLock pins which flock(2) answers TryLock reads as
// another open holding the lock: EWOULDBLOCK and EAGAIN, the same Errno on
// every GOOS this builds for. An interrupted call, a filesystem that keeps no
// locks and an I/O error stay themselves, so a caller never reads them as
// busy.
func TestLockedErrMapsOnlyAHeldLock(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		locked bool
	}{
		{"EWOULDBLOCK", unix.EWOULDBLOCK, true},
		{"EAGAIN", unix.EAGAIN, true},
		{"EINTR", unix.EINTR, false},
		{"ENOLCK", unix.ENOLCK, false},
		{"EOPNOTSUPP", unix.EOPNOTSUPP, false},
		{"EIO", unix.EIO, false},
		{"EBADF", unix.EBADF, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := lockedErr(tc.err)
			if errors.Is(got, ErrLocked) != tc.locked {
				t.Fatalf("lockedErr(%v) = %v, want locked %v", tc.err, got, tc.locked)
			}
			if !tc.locked && !errors.Is(got, tc.err) {
				t.Fatalf("lockedErr(%v) = %v, want the error itself", tc.err, got)
			}
		})
	}
	if lockedErr(nil) != nil {
		t.Fatal("lockedErr(nil) is not nil")
	}
}
