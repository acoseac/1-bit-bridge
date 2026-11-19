package fsutil

import "io/fs"

// EntryIdentity reads the entry at path as it stands now, not followed, with
// its identity (the volume serial and file index, or the device and inode)
// read at the call, so that a later os.SameFile of two identities read so
// reads nothing and tells whether two observations saw one file, whatever the
// path names when it compares (backlog B898FU's round 4: the crash rollback's
// observations of its backup).
//
// Not os.Lstat on Windows: that reads the entry's attributes
// (GetFileAttributesEx) and leaves its identity to be read when os.SameFile
// first asks, by opening the path then (os/types_windows.go's loadFileId), so
// the identity is of whatever the path names at the comparison, and a read
// that fails there compares as another file. Here the entry itself is opened,
// a reparse point not followed (FILE_FLAG_OPEN_REPARSE_POINT), a directory
// too (FILE_FLAG_BACKUP_SEMANTICS), with no access asked for and every kind
// of sharing given, as entryOf opens one (parent_windows.go): an open that
// asks no read, write or delete access is checked against no other handle's
// sharing and counted in none, so it refuses no rename or removal of the file
// while it is open. Its type and identity come from the handle (GetFileType,
// GetFileInformationByHandle), and it is closed before EntryIdentity returns.
// A link, a junction or any other reparse point reads as what it is, never as
// a regular file, as a pipe or a device does; the open waits on nothing. On
// POSIX it is os.Lstat, which reads the device and the inode at the call and
// opens nothing.
//
// Nothing at path answers an error matching fs.ErrNotExist; any other failure,
// the read of the identity among them, is the read's error.
func EntryIdentity(path string) (fs.FileInfo, error) {
	return entryIdentity(path)
}
