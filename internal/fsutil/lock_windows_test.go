//go:build windows

package fsutil

import (
	"errors"
	"testing"

	"golang.org/x/sys/windows"
)

// TestLockedErrMapsOnlyAHeldLock pins which LockFileEx answers TryLock reads
// as another handle holding the lock: ERROR_LOCK_VIOLATION alone. A sharing
// violation, a refused access, a filesystem that keeps no locks and an I/O
// error stay themselves, so a caller never reads them as busy.
func TestLockedErrMapsOnlyAHeldLock(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		locked bool
	}{
		{"ERROR_LOCK_VIOLATION", windows.ERROR_LOCK_VIOLATION, true},
		{"ERROR_SHARING_VIOLATION", windows.ERROR_SHARING_VIOLATION, false},
		{"ERROR_ACCESS_DENIED", windows.ERROR_ACCESS_DENIED, false},
		{"ERROR_NOT_SUPPORTED", windows.ERROR_NOT_SUPPORTED, false},
		{"ERROR_IO_PENDING", windows.ERROR_IO_PENDING, false},
		{"ERROR_INVALID_HANDLE", windows.ERROR_INVALID_HANDLE, false},
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
