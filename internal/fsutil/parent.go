package fsutil

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// ErrDanglingLink is what ResolveParent's refusal of a path whose last
// element is a symbolic link that leads to nothing matches: a write there
// would either make the file the link names or replace the link with a file,
// and neither is the writer's to choose.
var ErrDanglingLink = errors.New("a symbolic link that leads to no file")

// ResolveParent splits path into the directory a create or a rename of the
// file path names lands in, and that file's name there, as the OS resolves
// path. Two spellings of one file answer one directory and one name, so a
// lock taken beside a file, and a file staged to be renamed onto it, go where
// the file is whichever spelling a writer was given.
//
// On unix a link in the directory part is followed where it stands and a
// `..` after it climbs from where the link leads, as the kernel walks a
// path: with cfg/link leading to real/sub, cfg/link/../bridge.yaml is in
// real. filepath.Dir cleans that name to cfg, a directory the file is not
// in, so the directory part is never cleaned before it is resolved; a
// relative one is resolved from the working directory, by the directory's
// own links. On Windows the OS takes a `..` by the name alone, before any
// link is followed (Win32 normalises a path before the filesystem sees it),
// so the directory is filepath.Abs's, which is that same normalisation; a
// link in it is left as it is, since opening through it reaches the same
// directory.
//
// A last element that is a symbolic link is followed to the file it leads
// to (filepath.EvalSymlinks of the whole path), and that file's directory
// and own name are answered: a lock beside the answer sits beside the target
// and is named after it, and a file renamed onto the answer replaces the
// target and leaves the link. One that leads to no file is refused, an
// *fs.PathError naming path and wrapping ErrDanglingLink.
//
// Directories that do not exist yet (a config dir bridge init is about to
// make) are answered through their nearest ancestor that does, resolved as
// above, with the missing ones after it as written; a file that does not
// exist is answered by its name. A path whose last element names a
// directory (none at all, ".", "..") is refused, an *fs.PathError wrapping
// syscall.EISDIR.
func ResolveParent(path string) (dir, name string, err error) {
	dir, name, err = resolveParent(path)
	if err != nil {
		return "", "", err
	}
	return followLast(path, dir, name)
}

// AbsAsWritten is path made absolute as the OS will resolve it, for an entry
// that keeps a config's name to hand on (serve, init): on unix the working
// directory joined to a relative path by hand, with nothing cleaned that a
// link can change the meaning of (an empty element and "." are dropped; a
// ".." stays, since the kernel climbs from where the link before it leads,
// which filepath.Abs would take away with that link); on Windows
// filepath.Abs, the normalisation Win32 makes of a path before the
// filesystem sees it.
func AbsAsWritten(path string) (string, error) {
	return absAsWritten(path)
}

// followLast answers the file dir/name resolves to when name is a symbolic
// link there (ResolveParent's last element), and dir and name as they are
// otherwise.
func followLast(path, dir, name string) (string, string, error) {
	full := joinAsWritten(dir, name)
	info, err := os.Lstat(full)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return dir, name, nil
	case err != nil:
		return "", "", err
	case info.Mode()&fs.ModeSymlink == 0:
		return dir, name, nil
	}
	target, err := filepath.EvalSymlinks(full)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "", "", &fs.PathError{Op: "resolve", Path: path, Err: ErrDanglingLink}
	case err != nil:
		return "", "", err
	}
	return filepath.Dir(target), filepath.Base(target), nil
}
