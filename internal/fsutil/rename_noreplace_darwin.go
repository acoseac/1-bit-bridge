//go:build darwin

package fsutil

import (
	"errors"

	"golang.org/x/sys/unix"
)

// platformRenameNoReplace is renamex_np(2) with RENAME_EXCL: the call that
// renames answers EEXIST where an entry is at to. A volume that does not
// take the flag refuses it (Apple's rename(2) ties the flag to
// getattrlist(2)'s VOL_CAP_INT_RENAME_EXCL; not measured), an answer
// noReplaceUnsupported reads as no such rename. An EINTR is asked again, as
// os.Rename asks it.
func platformRenameNoReplace(from, to string) error {
	for {
		err := unix.RenamexNp(from, to, unix.RENAME_EXCL)
		if !errors.Is(err, unix.EINTR) {
			return err
		}
	}
}

// noReplaceUnsupported reads which of renamex_np's answers say no rename
// that never replaces can be made here: ENOTSUP and EOPNOTSUPP, two errnos
// on macOS, and EINVAL, a volume or a flag the call does not take; ENOSYS;
// and errors.ErrUnsupported, which ENOSYS, ENOTSUP and EOPNOTSUPP match too.
func noReplaceUnsupported(err error) bool {
	return errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.ENOTSUP) ||
		errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, errors.ErrUnsupported)
}

// noReplaceTransient is false on macOS: no answer of renamex_np's is tried
// again.
func noReplaceTransient(error) bool { return false }
