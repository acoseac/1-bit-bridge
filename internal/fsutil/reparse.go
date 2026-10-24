package fsutil

import (
	"errors"
	"io/fs"
	"syscall"
)

// The Windows reparse tags this package decides by (winnt.h), named here so
// that the decision below, and its tests, build on every platform.
const (
	// reparseTagNameSurrogate is the bit IsReparseTagNameSurrogate tests: a
	// reparse point that stands for another named entry, as a symbolic
	// link, a junction and a mounted folder do.
	reparseTagNameSurrogate = 0x20000000
	// reparseTagMountPoint is IO_REPARSE_TAG_MOUNT_POINT: a junction, or a
	// volume mounted in a folder.
	reparseTagMountPoint = 0xA0000003
	// reparseTagSymlink is IO_REPARSE_TAG_SYMLINK.
	reparseTagSymlink = 0xA000000C
	// reparseTagDedup is IO_REPARSE_TAG_DEDUP: a file the Data
	// Deduplication service optimised, which os.Lstat calls regular.
	reparseTagDedup = 0x80000013
)

// errLinkNotResolved is the cause, inside an *fs.PathError, of ResolveLinks's
// answer for a path that is there, leads to a directory, and still ends at a
// link after every way of resolving it: answering that link as resolved is
// what made a walk of it see one entry.
var errLinkNotResolved = errors.New("links to a directory that could not be resolved to a path")

// dirEntryOps are what checkResolvedDir asks of the OS about the directory
// ResolveLinks answered for a config's directory: entry, the reparse tag of
// that directory's own entry (0 when it is no reparse point) and whether it
// is a directory, as an open of the entry itself reads them; and resolve,
// ResolveLinks. Passed in, so the reparse points a test host cannot make (a
// OneDrive folder's cloud tag, a container's layers) can drive it.
type dirEntryOps struct {
	entry   func(path string) (tag uint32, dir bool, err error)
	resolve func(path string) (string, error)
}

// checkResolvedDir answers nil when resolved, the directory ResolveLinks
// answered for dir, is a directory and no link, and otherwise an
// *fs.PathError naming dir: errLinkNotResolved for one that is still a link,
// its own entry a name surrogate (IsReparseTagNameSurrogate: a symbolic link,
// a junction, a mounted folder), or one whose resolution, asked again,
// answers another directory (still a link, or one repointed meanwhile);
// syscall.ENOTDIR for one that is no directory; and the entry's or the
// resolution's own error when either cannot be had. Every other reparse
// point is a directory like any other: a OneDrive folder's cloud tag
// (IO_REPARSE_TAG_CLOUD_*), a container layer's (IO_REPARSE_TAG_WCI*), a
// deduplicated folder's. Go 1.23's os.Lstat calls each of those
// ModeIrregular, and refusing that mode refused every writer of a config
// under OneDrive (B506's fifth round).
func checkResolvedDir(dir, resolved string, ops dirEntryOps) error {
	tag, isDir, err := ops.entry(resolved)
	switch {
	case err != nil:
		return err
	case tag&reparseTagNameSurrogate != 0:
		return &fs.PathError{Op: "resolve", Path: dir, Err: errLinkNotResolved}
	case !isDir:
		return &fs.PathError{Op: "resolve", Path: dir, Err: syscall.ENOTDIR}
	}
	again, err := ops.resolve(resolved)
	switch {
	case err != nil:
		return err
	case again != resolved:
		return &fs.PathError{Op: "resolve", Path: dir, Err: errLinkNotResolved}
	}
	return nil
}
