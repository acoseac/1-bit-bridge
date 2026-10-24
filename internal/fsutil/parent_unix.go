//go:build !windows

package fsutil

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// resolveParent is ResolveParent on unix, before its last element is
// followed: the directory part as written, made absolute by the working
// directory without cleaning (AbsAsWritten), then resolved through its
// nearest existing ancestor (evalExisting), which follows each link before it
// takes the `..` after it.
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
	if dir, err = AbsAsWritten(dir); err != nil {
		return "", "", err
	}
	if dir, err = evalExisting(dir); err != nil {
		return "", "", err
	}
	return dir, name, nil
}

// evalExisting resolves dir as the kernel walks it (filepath.EvalSymlinks),
// through its nearest ancestor that exists when dir does not exist yet: that
// ancestor resolved, and the directories after it, which do not exist, as
// written. Only a directory that is not there is walked past; any other
// failure (one that is not a directory, one this user cannot search) is
// returned.
func evalExisting(dir string) (string, error) {
	missing := ""
	for {
		resolved, err := filepath.EvalSymlinks(dir)
		if err == nil {
			if missing == "" {
				return resolved, nil
			}
			return joinAsWritten(resolved, missing), nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		i := strings.LastIndexByte(dir, '/')
		if i < 0 || dir == "/" {
			return "", err
		}
		last := dir[i+1:]
		dir = dir[:i]
		if dir == "" {
			dir = "/"
		}
		if last == "" {
			continue
		}
		if missing == "" {
			missing = last
		} else {
			missing = last + "/" + missing
		}
	}
}

// joinAsWritten is dir and name joined as the kernel reads them, nothing
// cleaned: filepath.Join would take a `..` away with the element before it,
// which may be a link the kernel follows first.
func joinAsWritten(dir, name string) string {
	if strings.HasSuffix(dir, "/") {
		return dir + name
	}
	return dir + "/" + name
}

// absAsWritten is AbsAsWritten on unix: the working directory joined to a
// relative path by hand, and every empty element and "." dropped, which no
// link can change the meaning of; a ".." is kept where it is, since the
// kernel takes it from where the link before it leads.
func absAsWritten(path string) (string, error) {
	if !filepath.IsAbs(path) {
		wd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		path = wd + "/" + path
	}
	parts := strings.Split(path, "/")
	kept := parts[:1]
	for _, p := range parts[1:] {
		if p != "" && p != "." {
			kept = append(kept, p)
		}
	}
	if len(kept) == 1 {
		return "/", nil
	}
	return strings.Join(kept, "/"), nil
}
