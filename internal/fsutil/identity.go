package fsutil

import (
	"errors"
	"os"
)

// DirIdentity returns the directory at dir as it is NOW, for an os.SameFile
// comparison made later: does the path still name that directory? A volume
// unmounted under a mountpoint leaves the path naming the local directory
// beneath it, which is another directory, and that comparison is how a sweep
// that deletes rows whose files it cannot find tells an unmount from a
// deletion (backlog B203: the variant watcher asks it, and its directory
// probe, which the variant delete handler reads too, takes the same stat
// from the handle it reads the directory with).
//
// Not os.Stat. On Windows, os.Stat of a path that is no reparse point reads
// its attributes with GetFileAttributesEx and leaves the file's identity
// (volume serial and file index) to be read when os.SameFile first asks,
// from whatever the path names THEN. So an os.Stat taken before a directory
// was replaced and one taken after compare as the same file: both
// identities are read at the comparison, from the new directory (measured on
// Windows 11 with go1.26.6: a directory renamed away and another made at its
// path, SameFile true; TestOSStatLeavesTheWindowsIdentityToTheComparison
// pins it). The stat of an open handle reads the identity from the handle
// (GetFileInformationByHandle), at the call. On POSIX both read the device
// and inode at the call.
//
// Opened with OpenDir, so a path that is no longer a directory is refused
// with ENOTDIR rather than opened (a named pipe there would wait for a
// writer).
func DirIdentity(dir string) (os.FileInfo, error) {
	f, err := OpenDir(dir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return f.Stat()
}

// ErrDirReplaced is DirStillIs's answer when dir names a directory that is
// not the one its start was taken from.
var ErrDirReplaced = errors.New("the directory at the path is another one")

// DirStillIs reports whether dir still names the directory start was taken
// from (DirIdentity), compared with os.SameFile: nil while it does,
// DirIdentity's own error when the path cannot be opened as a directory now
// (one wrapping fs.ErrNotExist when nothing is there), and ErrDirReplaced
// when it names another directory. A nil start is never the same. The one
// comparison the integrity sweeps' identity checks make (integrity's
// VariantsDirChanged) and the analysis pool's waveform-only repair makes
// before it writes and before it publishes (backlog B652), so the two
// cannot disagree about what a volume that went looks like.
func DirStillIs(dir string, start os.FileInfo) error {
	now, err := DirIdentity(dir)
	if err != nil {
		return err
	}
	if start == nil || !os.SameFile(start, now) {
		return ErrDirReplaced
	}
	return nil
}
