//go:build windows

package fsutil

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// resolveParent is ResolveParent on Windows: filepath.Abs, the normalisation
// Win32 makes of a path before the filesystem sees it, `..` taken by the
// name, then the directory it names, which must exist.
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
	if err != nil {
		return "", "", err
	}
	if !info.IsDir() {
		return "", "", &fs.PathError{Op: "resolve", Path: dir, Err: syscall.ENOTDIR}
	}
	return dir, name, nil
}
