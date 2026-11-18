//go:build !(darwin || dragonfly || freebsd || linux || netbsd || openbsd || windows)

package adminauth

import "errors"

// noLocksErrno is what a platform with no lock answers (fsutil's tryLock
// there), which fsutil.KeepsNoLocks reads.
var noLocksErrno error = errors.ErrUnsupported
