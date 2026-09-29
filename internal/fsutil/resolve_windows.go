//go:build windows

package fsutil

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// GetFinalPathNameByHandle's volume-name flags (FILE_NAME_NORMALIZED beside
// them is 0). x/sys/windows does not export them.
const (
	// volumeNameDOS asks for a path under a drive letter or a UNC share.
	volumeNameDOS = 0x0
	// volumeNameGUID asks for a path under the volume's GUID name,
	// `\\?\Volume{…}\`.
	volumeNameGUID = 0x1
)

// errLinkNotResolved is the cause, inside an *fs.PathError, of ResolveLinks's
// answer for a path that is there, leads to a directory, and still ends at a
// link after every way of resolving it: answering that link as resolved is
// what made a walk of it see one entry.
var errLinkNotResolved = errors.New("links to a directory that could not be resolved to a path")

// resolveLinks is ResolveLinks on Windows: an absolute path is named by the
// handle it opens to, so every symbolic link, junction and mounted folder in
// it is followed; what cannot be named that way gets EvalSymlinks's answer
// when that answer is not itself a link to a directory.
func resolveLinks(p string) (string, error) {
	if !filepath.IsAbs(p) {
		return filepath.EvalSymlinks(p)
	}
	return resolveWith(p, resolveOps{
		finalPath:    finalPathName,
		stat:         os.Stat,
		lstat:        os.Lstat,
		evalSymlinks: filepath.EvalSymlinks,
	})
}

// resolveOps are what resolveWith asks of the filesystem, passed in so the
// shapes a test host cannot make (a volume mounted in a folder with no drive
// letter, a filesystem that refuses GetFinalPathNameByHandle) can drive it.
type resolveOps struct {
	finalPath    func(string) (string, error)
	stat, lstat  func(string) (fs.FileInfo, error)
	evalSymlinks func(string) (string, error)
}

// resolveWith is resolveLinks's decision for an absolute p. The final path
// the handle names wins. A p that is not there is a not-exist error, so a
// caller resolves an ancestor instead. A p that is there and cannot be named
// by its handle gets EvalSymlinks's answer (two junctions pointing at each
// other: the open fails with ERROR_CANT_RESOLVE_FILENAME), except an answer
// that still ends at a link to a directory, which is refused: a walk started
// there sees one entry, and a comparison made with it compares the link's own
// spelling, which is the defect ResolveLinks exists for.
func resolveWith(p string, ops resolveOps) (string, error) {
	final, err := ops.finalPath(p)
	if err == nil {
		return final, nil
	}
	if _, statErr := ops.stat(p); errors.Is(statErr, fs.ErrNotExist) {
		return "", &fs.PathError{Op: "resolve", Path: p, Err: statErr}
	}
	resolved, err := ops.evalSymlinks(p)
	if err != nil {
		return "", err
	}
	if endsAtALinkToADirectory(resolved, ops) {
		return "", &fs.PathError{Op: "resolve", Path: p, Err: errLinkNotResolved}
	}
	return resolved, nil
}

// endsAtALinkToADirectory reports whether p is not a directory itself but
// leads to one: a junction, a mounted folder or a symbolic link, left
// unresolved.
func endsAtALinkToADirectory(p string, ops resolveOps) bool {
	own, err := ops.lstat(p)
	if err != nil || own.IsDir() {
		return false
	}
	through, err := ops.stat(p)
	return err == nil && through.IsDir()
}

// finalPathName names what p opens to (openFollowing, nameFromHandle).
func finalPathName(p string) (string, error) {
	h, err := openFollowing(p)
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(h)
	return nameFromHandle(func(flags uint32) (string, error) { return finalPathNameByHandle(h, flags) })
}

// openFollowing opens p following every reparse point on the way (no
// FILE_FLAG_OPEN_REPARSE_POINT, and FILE_FLAG_BACKUP_SEMANTICS so a
// directory opens at all), with no access asked for, which is what os.Stat
// opens with.
func openFollowing(p string) (windows.Handle, error) {
	name, err := windows.UTF16PtrFromString(p)
	if err != nil {
		return windows.InvalidHandle, err
	}
	return windows.CreateFile(name, 0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
}

// finalPathNameByHandle is GetFinalPathNameByHandle with the buffer grown to
// fit.
func finalPathNameByHandle(h windows.Handle, flags uint32) (string, error) {
	buf := make([]uint16, windows.MAX_PATH)
	for {
		n, err := windows.GetFinalPathNameByHandle(h, &buf[0], uint32(len(buf)), flags)
		if err != nil {
			return "", err
		}
		if n < uint32(len(buf)) {
			return windows.UTF16ToString(buf[:n]), nil
		}
		// n is the size needed, terminating NUL included.
		buf = make([]uint16, n)
	}
}

// nameFromHandle names an opened file by its drive-letter or UNC path, and
// when it has none by its volume GUID path. A volume mounted only in a folder
// has no drive letter, and VOLUME_NAME_DOS can fail for it (CodeRabbit on
// #1090); `\\?\Volume{…}\…` reaches the files below the mount point, which
// Go's os and filepath functions take as they are, where the mount point's
// own spelling, left unresolved, is one entry to a walk. Both sides of a
// comparison resolve alike, so a root and a path on that volume agree.
func nameFromHandle(get func(flags uint32) (string, error)) (string, error) {
	dos, err := get(volumeNameDOS)
	if err == nil {
		return stripVerbatimPrefix(dos)
	}
	guid, guidErr := get(volumeNameGUID)
	if guidErr != nil || !strings.HasPrefix(guid, `\\?\Volume{`) {
		return "", err
	}
	return guid, nil
}

// stripVerbatimPrefix turns GetFinalPathNameByHandle's `\\?\C:\x` into
// `C:\x` and `\\?\UNC\server\share\x` into `\\server\share\x`.
func stripVerbatimPrefix(s string) (string, error) {
	switch {
	case strings.HasPrefix(s, `\\?\UNC\`):
		return `\\` + s[len(`\\?\UNC\`):], nil
	case strings.HasPrefix(s, `\\?\`) && len(s) > len(`\\?\`)+1 && s[len(`\\?\`)+1] == ':':
		return s[len(`\\?\`):], nil
	}
	return "", errors.New("GetFinalPathNameByHandle returned an unexpected path: " + s)
}
