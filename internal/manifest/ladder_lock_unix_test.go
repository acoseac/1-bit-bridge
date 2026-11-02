//go:build unix

package manifest

import "golang.org/x/sys/unix"

// platformLadderLockKindCases are flock's answers as fsutil.KeepsNoLocks
// reads them on these platforms (B537 item 1): ENOLCK, EOPNOTSUPP, ENOTSUP
// and ENOSYS say no lock can be taken there, ENOLCK only that none was
// given; a descriptor the call refuses, an operation it calls invalid and a
// permission error are the third kind.
var platformLadderLockKindCases = []ladderLockKindCase{
	{"ENOLCK", unix.ENOLCK, true},
	{"EOPNOTSUPP", unix.EOPNOTSUPP, true},
	{"ENOTSUP", unix.ENOTSUP, true},
	{"ENOSYS", unix.ENOSYS, true},
	{"EBADF", unix.EBADF, false},
	{"EINVAL", unix.EINVAL, false},
	{"EACCES", unix.EACCES, false},
}
