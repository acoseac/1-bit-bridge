package fsutil

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// ErrRootNotEntered is the cause, inside an *fs.PathError, of WalkableRoot's
// answer for a root that stats through to a directory which a separator
// still does not lead into. No platform Go supports produces it; it exists
// so that a platform that did would make the walk fail, not read as an
// empty one.
var ErrRootNotEntered = errors.New("links to a directory the walk cannot enter")

// WalkableRoot returns the path to hand filepath.WalkDir so that the walk
// descends into the directory root names, also when root is itself a link to
// that directory.
//
// filepath.WalkDir Lstats its root and follows no link. So a library root
// that is a symbolic link to a directory (`/music -> /mnt/nas/music`, an
// ordinary way to point at a mount), and on Windows a directory junction or a
// volume mounted in a folder (both of which os.Lstat reports ModeIrregular,
// without ModeDir, since Go 1.23), reaches the callback as ONE entry that is
// not a directory, and the walk ends there. The scanner then indexed nothing
// under such a root, and an install whose root became a link after it was
// indexed was told, every scan, that its mount looked empty. Following the
// hint that line gives (the `.bridge-allow-empty` sentinel, which is found
// THROUGH the link) deleted every row.
//
// For such a root the answer is root with a separator appended. A trailing
// separator makes os.Lstat resolve the last component: POSIX resolves a
// pathname that ends in a slash through a symlink, chains included, and Go's
// os.Lstat on Windows follows a name surrogate (a symlink or a junction) when
// the path ends in a separator. Every path WalkDir hands its callback below
// the root is filepath.Join of that path and a name, so it keeps root's own
// spelling: a caller that relates those paths to root with filepath.Rel,
// stores them, or names them in a log line does all of that under the
// configured spelling, and a multi-root basename stays the configured root's,
// never its target's. The callback is handed the returned string itself for
// the root entry, so a caller that tells the root apart by string identity
// compares against the returned path, not against root.
//
// filepath.EvalSymlinks is not the answer, though the sidecar walks use it
// (integrity.resolveSidecarRoot): since Go 1.23 it resolves no Windows
// junction and no mounted folder, which are not ModeSymlink, so on Windows
// the "resolved" root is the junction again and the walk still ends at its
// first entry. It also rewrites the spelling of every path the walk reports,
// which each caller would then have to map back.
//
// An ordinary directory, and anything that is not a directory even through a
// link, is returned unchanged, so those walks are exactly what they were.
// Only the root is followed: a link BELOW it is the walk's business, and the
// scanner indexes nothing behind a link to a directory (loops).
//
// A root that cannot be stat'ed through (missing, a dangling link, a link
// into a mount that went away, a permission wall on the way) returns the
// error and "": a walk that could not see the directory must say so, never
// read as an empty one, and walking root unresolved instead IS the defect.
// So does a root that stats to a directory the separator does not lead into
// (ErrRootNotEntered).
func WalkableRoot(root string) (string, error) {
	return walkableRoot(root, os.Lstat, os.Stat)
}

// walkableRoot is WalkableRoot with the two stats passed in, so a shape the
// host cannot make (a Windows junction anywhere but Windows) can drive it.
func walkableRoot(root string, lstat, stat func(string) (fs.FileInfo, error)) (string, error) {
	own, err := lstat(root)
	if err != nil {
		return "", err
	}
	if own.IsDir() {
		// An ordinary directory, or a root already spelled with a trailing
		// separator (which os.Lstat resolved through).
		return root, nil
	}
	through, err := stat(root)
	if err != nil {
		return "", err
	}
	if !through.IsDir() {
		return root, nil
	}
	walkPath := root + string(filepath.Separator)
	entered, err := lstat(walkPath)
	if err != nil {
		return "", err
	}
	if !entered.IsDir() {
		// A PathError, like every other error here, so a caller that
		// strips the path before an error leaves the host does so here too.
		return "", &fs.PathError{Op: "walk", Path: root, Err: ErrRootNotEntered}
	}
	return walkPath, nil
}
