//go:build windows

package fsutil

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// openLockFileNoFollow is OpenLockFileNoFollow's open on Windows: CreateFile
// with FILE_FLAG_OPEN_REPARSE_POINT, which opens a link (a symbolic link, a
// junction) as itself rather than what it names, so a link to nothing makes
// nothing; then the handle's attributes are asked, and a reparse point or a
// directory is refused. Without FILE_FLAG_BACKUP_SEMANTICS a directory does
// not open at all. OPEN_ALWAYS for writing creates the file; OPEN_EXISTING
// for reading never does. Every sharing mode is granted, as Go's own opens
// grant read and write: the lock, not the handle, is what excludes.
func openLockFileNoFollow(path string, _ os.FileMode, write bool) (*os.File, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	access, disposition := uint32(windows.GENERIC_READ), uint32(windows.OPEN_EXISTING)
	if write {
		access, disposition = windows.GENERIC_READ|windows.GENERIC_WRITE, windows.OPEN_ALWAYS
	}
	h, err := windows.CreateFile(name, access,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		disposition, windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		_ = windows.CloseHandle(h)
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	if info.FileAttributes&(windows.FILE_ATTRIBUTE_REPARSE_POINT|windows.FILE_ATTRIBUTE_DIRECTORY) != 0 {
		_ = windows.CloseHandle(h)
		return nil, &os.PathError{Op: "open", Path: path, Err: ErrLockFileNotAFile}
	}
	return os.NewFile(uintptr(h), path), nil
}

// openRefusedForWriting is OpenRefusedForWriting's platform half: a medium
// that is write-protected (ERROR_WRITE_PROTECT). ERROR_ACCESS_DENIED is
// fs.ErrPermission already.
func openRefusedForWriting(err error) bool {
	return errors.Is(err, windows.ERROR_WRITE_PROTECT)
}
