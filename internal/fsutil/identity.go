package fsutil

import "os"

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
