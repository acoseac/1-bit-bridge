package fsutil

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// ErrRootNotEntered is the cause, inside an *fs.PathError, of WalkableRoot's
// answer when a root stats through to a directory and neither the
// trailing-slash Lstat nor resolving the link chain names a directory.
// A separator that does not lead in fails the walk rather than reading as
// an empty one. A link chain has answered that Lstat "not a directory"
// while Stat of the same path answered a directory (backlog B257). When
// those two disagree, the chain is resolved and a resolved directory is
// walked under the configured spelling. The error remains when that
// resolution does not name a directory.
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
// compares against the returned path, not against root. WalkConfigured is
// that walk.
//
// filepath.EvalSymlinks is not the path the walk is given: since Go 1.23 it
// resolves no Windows junction and no mounted folder, which are not
// ModeSymlink, so on Windows the "resolved" root is the junction again and
// the walk still ends at its first entry. Nor is resolving the root through
// them (ResolveLinks, which the sidecar walks use through
// integrity.resolveSidecarRoot, because they unlink and must do it in the
// tree they walked) and walking that spelling: that rewrites every path the
// walk reports, which each caller would then have to map back.
//
// ResolveLinks decides walkability only when the two answers disagree. The
// trailing-slash Lstat has answered "not a directory", or returned an error,
// while Stat of the same root answered a directory (backlog B257, a link to
// a link on CI). The chain is resolved, and when that names a directory the
// walk is still given root with a separator appended. When the two answers
// agree, the chain is not resolved. WalkConfigured then reads that resolved
// directory and reports every path under the separator path. filepath.WalkDir
// of the separator path is a second Lstat, and that Lstat has answered "not
// a directory" after WalkableRoot had already returned the separator path,
// so the callback saw the root and nothing below it (backlog B358).
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

// walkableRootSlashLstat, when set, is the trailing-slash Lstat WalkableRoot
// uses. It is nil in production. A test sets it to the answer a link chain
// gave on CI: not a directory, or an error, while Stat of the same root is
// a directory.
var walkableRootSlashLstat func(string) (fs.FileInfo, error)

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
	entered, err := lstatWalkPath(walkPath, lstat)
	if err == nil && entered != nil && entered.IsDir() {
		return walkPath, nil
	}
	// Stat already said this root is a directory. The trailing-slash Lstat
	// disagreed (not a directory, or an error). A link chain can do that
	// while the target is a directory. Resolve the chain and decide from
	// the resolved directory. The string returned is still walkPath, the
	// configured spelling. os.Stat of the resolved path is the real stat:
	// the stat passed in stands for the root, and a test stub that answers
	// a directory for every path must not turn a failed resolution into a walk.
	if chainResolvesToDir(root) {
		return walkPath, nil
	}
	if err != nil {
		return "", err
	}
	// A PathError, like every other error here, so a caller that
	// strips the path before an error leaves the host does so here too.
	return "", &fs.PathError{Op: "walk", Path: root, Err: ErrRootNotEntered}
}

// chainResolvesToDir reports whether root, followed through every link,
// names a directory. The path it resolves is never what the walk is given.
func chainResolvesToDir(root string) bool {
	_, ok := resolvedDirectory(root)
	return ok
}

// resolvedDirectory is the directory ResolveLinks names for root, when it
// names one. The stat is os.Stat of that path, never the root's stat.
func resolvedDirectory(root string) (string, bool) {
	resolved, err := ResolveLinks(root)
	if err != nil {
		return "", false
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.IsDir() {
		return "", false
	}
	return resolved, true
}

// WalkConfigured walks walkPath, the string WalkableRoot returned.
// A directory is filepath.WalkDir of that string. A path that ends in a
// separator, whose chain names a directory, is read at that directory, and
// every path the callback sees is spelled under walkPath: the root entry is
// walkPath itself, and each path below it is filepath.Join of walkPath and
// the path relative to the resolved directory. filepath.WalkDir of the
// separator path is not used for that case. Its own Lstat of the path has
// answered "not a directory" for a chain WalkableRoot had accepted, and the
// callback then saw the root alone. Anything else is one callback for the
// Lstat of walkPath, which is what filepath.WalkDir does for a file and for
// an Lstat that fails.
func WalkConfigured(walkPath string, fn fs.WalkDirFunc) error {
	if walked, err := walkResolvedChain(walkPath, fn); walked {
		return err
	}
	info, err := lstatWalkPath(walkPath, os.Lstat)
	if err == nil && info != nil && info.IsDir() {
		return filepath.WalkDir(walkPath, fn)
	}
	return walkLstatAnswer(walkPath, info, err, fn)
}

// walkResolvedChain walks the directory a separator path's chain names.
// walked is false when walkPath does not end in a separator, when trimming
// the separator leaves nothing or only a volume name, or when the chain
// does not name a directory: the caller then walks walkPath itself.
func walkResolvedChain(walkPath string, fn fs.WalkDirFunc) (bool, error) {
	root := strings.TrimRight(walkPath, string(filepath.Separator))
	// "/" trims to nothing, and a volume root ("D:\") trims to the volume
	// name. ResolveLinks of either is not that directory.
	if root == walkPath || root == "" || root == filepath.VolumeName(walkPath) {
		return false, nil
	}
	resolved, ok := resolvedDirectory(root)
	if !ok {
		return false, nil
	}
	resolved = strings.TrimRight(resolved, string(filepath.Separator))
	err := filepath.WalkDir(resolved, func(path string, d fs.DirEntry, err error) error {
		reported, repErr := pathUnder(walkPath, resolved, path)
		if repErr != nil {
			return repErr
		}
		return fn(reported, d, err)
	})
	return true, err
}

// pathUnder is path spelled under walkPath. The root entry is walkPath
// itself, so a caller that compares the root callback with the string
// WalkableRoot returned still matches.
func pathUnder(walkPath, resolved, path string) (string, error) {
	if path == resolved {
		return walkPath, nil
	}
	rel, err := filepath.Rel(resolved, path)
	if err != nil {
		return "", err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", &fs.PathError{Op: "walk", Path: path, Err: fs.ErrInvalid}
	}
	return filepath.Join(walkPath, rel), nil
}

// walkLstatAnswer invokes fn once for the Lstat of walkPath and drops
// SkipDir and SkipAll, as filepath.WalkDir does when the root is not a
// directory or the Lstat fails.
func walkLstatAnswer(walkPath string, info fs.FileInfo, err error, fn fs.WalkDirFunc) error {
	if err != nil || info == nil {
		if err == nil {
			err = &fs.PathError{Op: "lstat", Path: walkPath, Err: fs.ErrInvalid}
		}
		err = fn(walkPath, nil, err)
	} else {
		err = fn(walkPath, fs.FileInfoToDirEntry(info), nil)
	}
	if err == fs.SkipDir || err == fs.SkipAll {
		return nil
	}
	return err
}

// lstatWalkPath is the trailing-slash Lstat. The package seam replaces it
// only while a test holds the disagreement a real Lstat showed.
func lstatWalkPath(walkPath string, lstat func(string) (fs.FileInfo, error)) (fs.FileInfo, error) {
	if walkableRootSlashLstat != nil {
		return walkableRootSlashLstat(walkPath)
	}
	return lstat(walkPath)
}
