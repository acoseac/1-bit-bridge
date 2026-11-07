//go:build windows

package doctor

import "golang.org/x/sys/windows"

// bindPermissionErrno is what Windows answers a bind it refuses by a
// permission: WSAEACCES, for a port another process holds exclusively or one
// in an excluded range.
var bindPermissionErrno = windows.WSAEACCES

// addrNotAvailErrno is what Windows answers a bind of an address no
// interface holds: WSAEADDRNOTAVAIL.
var addrNotAvailErrno = windows.WSAEADDRNOTAVAIL
