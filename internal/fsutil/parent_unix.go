//go:build !windows

package fsutil

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// resolveParent is ResolveParent on unix: the directory part as written,
// made absolute by the working directory without cleaning, then
// filepath.EvalSymlinks, which follows each link before it takes the `..`
// after it.
func resolveParent(path string) (dir, name string, err error) {
	dir, name = ".", path
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		dir, name = path[:i], path[i+1:]
		if dir == "" {
			dir = "/"
		}
	}
	if name == "" || name == "." || name == ".." {
		return "", "", &fs.PathError{Op: "resolve", Path: path, Err: syscall.EISDIR}
	}
	if !filepath.IsAbs(dir) {
		wd, err := os.Getwd()
		if err != nil {
			return "", "", err
		}
		// Joined by hand: filepath.Join would clean a `..` in dir before
		// any link in it is followed.
		dir = wd + "/" + dir
	}
	dir, err = filepath.EvalSymlinks(dir)
	if err != nil {
		return "", "", err
	}
	return dir, name, nil
}
