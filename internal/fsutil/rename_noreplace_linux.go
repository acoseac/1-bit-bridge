//go:build linux

package fsutil

import (
	"errors"

	"golang.org/x/sys/unix"
)

// platformRenameNoReplace is renameat2(2) with RENAME_NOREPLACE, both paths
// as given (AT_FDCWD: a relative one is the working directory's, as for
// os.Rename). The kernel looks for an entry at to in the call that renames,
// and answers EEXIST where one is there. Linux 3.15 brought the call. An
// EINTR is asked again, as os.Rename asks it.
func platformRenameNoReplace(from, to string) error {
	for {
		err := unix.Renameat2(unix.AT_FDCWD, from, unix.AT_FDCWD, to, unix.RENAME_NOREPLACE)
		if !errors.Is(err, unix.EINTR) {
			return err
		}
	}
}

// noReplaceUnsupported reads which of renameat2's answers say no rename that
// never replaces can be made here: EINVAL, a filesystem that does not take
// RENAME_NOREPLACE; ENOSYS, a kernel without the call, or a sandbox's filter
// that refuses it so; ENOTSUP and EOPNOTSUPP, one errno on Linux; and
// errors.ErrUnsupported, which ENOSYS and EOPNOTSUPP match too.
func noReplaceUnsupported(err error) bool {
	return errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.ENOTSUP) ||
		errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, errors.ErrUnsupported)
}

// noReplaceTransient is false on Linux: no answer of renameat2's is tried
// again.
func noReplaceTransient(error) bool { return false }
