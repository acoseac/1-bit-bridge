//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package adminauth

import "golang.org/x/sys/unix"

// noLocksErrno is what flock answers where no lock can be had, ENOLCK (an
// NFS mount with no lock manager), which fsutil.KeepsNoLocks reads.
var noLocksErrno error = unix.ENOLCK
