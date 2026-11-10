package backup

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

// ErrOutsideBackups is a verification's and a claim's answer for a folder
// that is not a snapshot in the install's backups folder, by where each
// resolves (resolveSnapshotIn): a copy elsewhere, the backups folder
// itself, a folder below a snapshot.
var ErrOutsideBackups = errors.New("the folder is not a snapshot in this install's backups folder")

// resolveDir is p as the lock names it: absolute, every link in it resolved
// (fsutil.ResolveLinks, junctions included on Windows).
func resolveDir(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	return resolveLinks(abs)
}

// resolveLinks is fsutil.ResolveLinks, which asks the filesystem of every
// element of a path, and a test's stand-in for a metadata path that does not
// answer (SetMetadataForTest).
var resolveLinks = fsutil.ResolveLinks

// resolveSnapshotIn is the snapshot at dir and the backups folder that holds
// it, each where it resolves (backlog B653's second round), so that `.`
// inside a snapshot, a link to one and a backups folder reached through a
// link all name the folder the pruner names, and the lock in it: the
// snapshot's resolved parent must be the directory backupsRoot resolves to
// (os.SameFile, whatever either spelling's case or links), else
// ErrOutsideBackups. An empty backupsRoot is the folder the snapshot is in.
func resolveSnapshotIn(dir, backupsRoot string) (snap, root string, err error) {
	snap, err = resolveDir(dir)
	if err != nil {
		return "", "", fmt.Errorf("find the snapshot: %w", err)
	}
	parent := filepath.Dir(snap)
	if backupsRoot == "" {
		return snap, parent, nil
	}
	root, err = resolveDir(backupsRoot)
	if err != nil {
		return "", "", fmt.Errorf("%w: find the backups folder: %w", ErrOutsideBackups, err)
	}
	pi, perr := os.Stat(parent)
	ri, rerr := os.Stat(root)
	if perr != nil || rerr != nil || !os.SameFile(pi, ri) {
		return "", "", ErrOutsideBackups
	}
	return snap, root, nil
}
