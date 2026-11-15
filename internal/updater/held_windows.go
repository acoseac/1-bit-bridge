//go:build windows

package updater

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// inUse reports whether err, a refusal of a rename or a probe of the bridge's
// binary, is another process's hold on the file, which ends once that process
// lets the file go (backlog B898): a handle that shares no deletion
// (ERROR_SHARING_VIOLATION, what a real-time scanner or the search indexer
// holds on an executable just written or run) or a lock on a range of it
// (ERROR_LOCK_VIOLATION). Never ERROR_ACCESS_DENIED, which a rename onto the
// mapped running image answers (backlog B202) and a permission does, neither
// of which a wait ends, and never any other failure.
func inUse(err error) bool {
	return errors.Is(err, windows.ERROR_SHARING_VIOLATION) || errors.Is(err, windows.ERROR_LOCK_VIOLATION)
}

// movable opens path for deletion, as a rename of it opens its source, sharing
// reading, writing and deletion with every other handle and not following a
// reparse point, and closes it at once: it answers ERROR_SHARING_VIOLATION
// exactly where another handle on the file shares no deletion, the hold that
// would refuse the rename, and nil where the rename's own open would pass. The
// image loader's handle on a running binary shares deletion, which is why the
// running image can be renamed, so the running binary is movable. A file that
// is not there answers ERROR_FILE_NOT_FOUND.
func movable(path string) error {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return &os.PathError{Op: "open", Path: path, Err: err}
	}
	h, err := windows.CreateFile(p, windows.DELETE|windows.SYNCHRONIZE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return &os.PathError{Op: "open", Path: path, Err: err}
	}
	_ = windows.CloseHandle(h)
	return nil
}
