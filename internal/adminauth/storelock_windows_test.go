//go:build windows

package adminauth

import "golang.org/x/sys/windows"

// noLocksErrno is what LockFileEx answers where the file system does not do
// byte-range locks, ERROR_NOT_SUPPORTED, which fsutil.KeepsNoLocks reads.
var noLocksErrno error = windows.ERROR_NOT_SUPPORTED
