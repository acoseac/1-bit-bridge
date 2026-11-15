//go:build windows

package manifest

import (
	"golang.org/x/sys/windows"
)

// recordLocksAvailable says whether useRecordLocks can stand in here:
// Windows' locks belong to a handle, and it has no record locks to share.
const recordLocksAvailable = false

// useRecordLocks has nothing to stand in with here.
func useRecordLocks() (restore func()) { return func() {} }

// noLocksErrno is a definitive no-locks answer of LockFileEx's; Windows has
// no ENOLCK, and its stand-in is one an opener must refuse all the same.
var (
	noLocksErrno = error(windows.ERROR_NOT_SUPPORTED)
	enolck       = error(windows.ERROR_NOT_ENOUGH_MEMORY)
	ioErr        = error(windows.ERROR_IO_DEVICE)
)
