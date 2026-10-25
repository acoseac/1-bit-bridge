//go:build windows

package fsutil

import (
	"errors"
	"io/fs"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// resolveParent is ResolveParent on Windows, before its last element is
// followed: filepath.Abs, the normalisation Win32 makes of a path before the
// filesystem sees it, `..` taken by the name, then the directory it names as
// the OS opens it (physicalDir).
func resolveParent(path string) (dir, name string, err error) {
	last := path[len(filepath.VolumeName(path)):]
	last = last[strings.LastIndexAny(last, `\/`)+1:]
	if last == "" || last == "." || last == ".." {
		return "", "", &fs.PathError{Op: "resolve", Path: path, Err: syscall.EISDIR}
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", "", err
	}
	dir, err = physicalDir(filepath.Dir(abs))
	if err != nil {
		return "", "", err
	}
	return dir, filepath.Base(abs), nil
}

// physicalDir is dir as the OS opens it, named by the handle it opens to
// (ResolveLinks), through every junction, mounted folder and symbolic link on
// its way; a directory that does not exist yet (a config dir bridge init is
// about to make) is resolved through its nearest ancestor that does, with the
// directories after that ancestor as written. One that is not a directory,
// one ResolveLinks's fallback leaves a link (no handle could name it), and one
// whose resolution, asked again, answers another directory are refused
// (checkResolvedDir): answered as itself, a junction keeps its spelling when
// it is repointed. A reparse point that is no link (a OneDrive folder's
// cloud tag, a container layer's) is a directory like any other.
func physicalDir(dir string) (string, error) {
	missing := ""
	for {
		resolved, err := ResolveLinks(dir)
		if err == nil {
			if err := checkResolvedDir(dir, resolved, dirEntryOps{entry: entryOf, resolve: ResolveLinks}); err != nil {
				return "", err
			}
			if missing == "" {
				return resolved, nil
			}
			return filepath.Join(resolved, missing), nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", err
		}
		missing = filepath.Join(filepath.Base(dir), missing)
		dir = parent
	}
}

// fileAttributeTagInfo is FILE_ATTRIBUTE_TAG_INFO, which x/sys/windows does
// not define: what GetFileInformationByHandleEx answers for
// FileAttributeTagInfo.
type fileAttributeTagInfo struct {
	FileAttributes uint32
	ReparseTag     uint32
}

// entryOf is the reparse tag of p's own entry, 0 when it is no reparse
// point, and whether it is a directory, read as os.Lstat reads them: the
// attributes (GetFileAttributesEx), and for a reparse point the tag, from a
// handle opened on the entry itself (FILE_FLAG_OPEN_REPARSE_POINT) with no
// access asked for, by GetFileInformationByHandleEx's FileAttributeTagInfo.
func entryOf(p string) (tag uint32, dir bool, err error) {
	name, err := windows.UTF16PtrFromString(p)
	if err != nil {
		return 0, false, &fs.PathError{Op: "stat", Path: p, Err: err}
	}
	var data windows.Win32FileAttributeData
	if err := windows.GetFileAttributesEx(name, windows.GetFileExInfoStandard, (*byte)(unsafe.Pointer(&data))); err != nil {
		return 0, false, &fs.PathError{Op: "stat", Path: p, Err: err}
	}
	if data.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT == 0 {
		return 0, data.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0, nil
	}
	h, err := windows.CreateFile(name, 0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return 0, false, &fs.PathError{Op: "open", Path: p, Err: err}
	}
	defer windows.CloseHandle(h)
	var info fileAttributeTagInfo
	if err := windows.GetFileInformationByHandleEx(h, windows.FileAttributeTagInfo,
		(*byte)(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		return 0, false, &fs.PathError{Op: "stat", Path: p, Err: err}
	}
	dir = info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT == 0 {
		return 0, dir, nil
	}
	return info.ReparseTag, dir, nil
}

// joinAsWritten is filepath.Join on Windows, whose cleaning is Win32's own.
func joinAsWritten(dir, name string) string {
	return filepath.Join(dir, name)
}

// absAsWritten is AbsAsWritten on Windows: filepath.Abs.
func absAsWritten(path string) (string, error) {
	return filepath.Abs(path)
}
