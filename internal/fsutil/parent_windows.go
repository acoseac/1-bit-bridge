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
// and one ResolveLinks's fallback leaves a link (no handle could name it), are
// refused: answered as itself, a junction keeps its spelling when it is
// repointed.
func physicalDir(dir string) (string, error) {
	missing := ""
	for {
		resolved, err := ResolveLinks(dir)
		if err == nil {
			info, err := os.Lstat(resolved)
			switch {
			case err != nil:
				return "", err
			case info.Mode()&(fs.ModeSymlink|fs.ModeIrregular) != 0:
				return "", &fs.PathError{Op: "resolve", Path: dir, Err: errLinkNotResolved}
			case !info.IsDir():
				return "", &fs.PathError{Op: "resolve", Path: dir, Err: syscall.ENOTDIR}
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

// joinAsWritten is filepath.Join on Windows, whose cleaning is Win32's own.
func joinAsWritten(dir, name string) string {
	return filepath.Join(dir, name)
}

// absAsWritten is AbsAsWritten on Windows: filepath.Abs.
func absAsWritten(path string) (string, error) {
	return filepath.Abs(path)
}
