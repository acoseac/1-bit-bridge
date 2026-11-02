//go:build windows

package main

import "golang.org/x/sys/windows"

// platformLockKindCases are LockFileEx's answers as fsutil.KeepsNoLocks
// reads them on Windows (B537 item 1): a filesystem or redirector that does
// no byte-range locks answers ERROR_NOT_SUPPORTED, or ERROR_INVALID_FUNCTION
// from a driver without the request; a denied access and a handle the call
// refuses are the third kind.
var platformLockKindCases = []lockKindCase{
	{"ERROR_NOT_SUPPORTED", windows.ERROR_NOT_SUPPORTED, true},
	{"ERROR_INVALID_FUNCTION", windows.ERROR_INVALID_FUNCTION, true},
	{"ERROR_ACCESS_DENIED", windows.ERROR_ACCESS_DENIED, false},
	{"ERROR_INVALID_HANDLE", windows.ERROR_INVALID_HANDLE, false},
}
