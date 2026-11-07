//go:build !windows

package doctor

import "syscall"

// bindPermissionErrno is what this platform answers a bind it refuses by a
// permission: EACCES, for a port below 1024 without the capability.
var bindPermissionErrno = syscall.EACCES

// addrNotAvailErrno is what this platform answers a bind of an address the
// host does not have.
var addrNotAvailErrno = syscall.EADDRNOTAVAIL
