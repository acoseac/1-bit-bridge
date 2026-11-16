//go:build windows

package fsutil

import (
	"errors"

	"golang.org/x/sys/windows"
)

// platformRenameNoReplace is MoveFileEx with no flags: without
// MOVEFILE_REPLACE_EXISTING an entry at to fails the move
// (ERROR_ALREADY_EXISTS), and without MOVEFILE_COPY_ALLOWED a move to
// another volume fails (ERROR_NOT_SAME_DEVICE) where it would otherwise copy
// the file and delete it. The paths are passed as given, as os.Rename passes
// them wherever Go's runtime has made the process long-path aware (Windows
// 10 1703 and later); before that os.Rename adds the \\?\ prefix to a path
// of 248 bytes or more, and this does not.
func platformRenameNoReplace(from, to string) error {
	from16, err := windows.UTF16PtrFromString(from)
	if err != nil {
		return err
	}
	to16, err := windows.UTF16PtrFromString(to)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(from16, to16, 0)
}

// noReplaceUnsupported reads which of MoveFileEx's answers say no rename
// that never replaces can be made here: ERROR_NOT_SUPPORTED;
// ERROR_INVALID_FUNCTION, a file system driver without the request; and
// errors.ErrUnsupported, which ERROR_NOT_SUPPORTED and
// ERROR_CALL_NOT_IMPLEMENTED match.
func noReplaceUnsupported(err error) bool {
	return errors.Is(err, windows.ERROR_NOT_SUPPORTED) || errors.Is(err, windows.ERROR_INVALID_FUNCTION) ||
		errors.Is(err, errors.ErrUnsupported)
}

// noReplaceTransient reads which of MoveFileEx's answers are tried again:
// ERROR_SHARING_VIOLATION, a handle on from that shares no delete, as an
// antivirus or the search indexer keeps on a file just written, and
// ERROR_ACCESS_DENIED, which the same scans can give; atomicwrite's
// RenameWithRetry tries the two again for that reason.
func noReplaceTransient(err error) bool {
	return errors.Is(err, windows.ERROR_SHARING_VIOLATION) || errors.Is(err, windows.ERROR_ACCESS_DENIED)
}
