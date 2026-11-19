//go:build windows

package fsutil

import (
	"io/fs"
	"os"

	"golang.org/x/sys/windows"
)

// openEntry opens the entry at path as EntryIdentity reads it: the entry
// itself, a reparse point not followed, a directory too, with no access asked
// for and every kind of sharing given. The caller closes it.
func openEntry(path string) (*os.File, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: path, Err: err}
	}
	h, err := windows.CreateFile(name, 0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(h), path), nil
}

// entryIdentity is EntryIdentity on Windows: the stat of openEntry's handle,
// which carries the file's identity as it is taken.
func entryIdentity(path string) (fs.FileInfo, error) {
	f, err := openEntry(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return f.Stat()
}
