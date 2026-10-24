//go:build windows

package fsutil

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// resolveParent is ResolveParent on Windows, before its last element is
// followed: filepath.Abs, the normalisation Win32 makes of a path before the
// filesystem sees it, `..` taken by the name, then the directory it names,
// which must be one where it exists; directories that do not exist yet are
// answered as written, as Win32 will make them.
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
	dir, name = filepath.Dir(abs), filepath.Base(abs)
	info, err := os.Stat(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return dir, name, nil
	case err != nil:
		return "", "", err
	case !info.IsDir():
		return "", "", &fs.PathError{Op: "resolve", Path: dir, Err: syscall.ENOTDIR}
	}
	return dir, name, nil
}

// joinAsWritten is filepath.Join on Windows, whose cleaning is Win32's own.
func joinAsWritten(dir, name string) string {
	return filepath.Join(dir, name)
}

// absAsWritten is AbsAsWritten on Windows: filepath.Abs.
func absAsWritten(path string) (string, error) {
	return filepath.Abs(path)
}
