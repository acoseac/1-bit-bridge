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

// volumeNameDOS is GetFinalPathNameByHandle's VOLUME_NAME_DOS flag (0, and
// FILE_NAME_NORMALIZED beside it is 0 too): a path under a drive letter or a
// UNC share, never a volume GUID. x/sys/windows does not export it.
const volumeNameDOS = 0x0

// resolveLinks is ResolveLinks on Windows: an absolute path is named by the
// handle it opens to, so every symbolic link, junction and mounted folder in
// it is followed; what cannot be named that way gets EvalSymlinks's answer.
func resolveLinks(p string) (string, error) {
	if !filepath.IsAbs(p) {
		return filepath.EvalSymlinks(p)
	}
	final, err := finalPathName(p)
	if err == nil {
		return final, nil
	}
	if _, statErr := os.Stat(p); errors.Is(statErr, fs.ErrNotExist) {
		// Not there, through whatever links it names: say so, as
		// EvalSymlinks does, so a caller can resolve an ancestor instead.
		return "", &fs.PathError{Op: "resolve", Path: p, Err: statErr}
	}
	return filepath.EvalSymlinks(p)
}

// finalPathName opens p following every reparse point on the way (no
// FILE_FLAG_OPEN_REPARSE_POINT, and FILE_FLAG_BACKUP_SEMANTICS so a
// directory opens at all), with no access asked for, which is what os.Stat
// opens with, and returns the path the handle names without the `\\?\`
// prefix.
func finalPathName(p string) (string, error) {
	name, err := windows.UTF16PtrFromString(p)
	if err != nil {
		return "", err
	}
	h, err := windows.CreateFile(name, 0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(h)
	buf := make([]uint16, windows.MAX_PATH)
	for {
		n, err := windows.GetFinalPathNameByHandle(h, &buf[0], uint32(len(buf)), volumeNameDOS)
		if err != nil {
			return "", err
		}
		if n < uint32(len(buf)) {
			buf = buf[:n]
			break
		}
		// n is the size needed, terminating NUL included.
		buf = make([]uint16, n)
	}
	return stripVerbatimPrefix(windows.UTF16ToString(buf))
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
