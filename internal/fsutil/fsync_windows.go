//go:build windows

package fsutil

// syncDir is a no-op on Windows. NTFS journals metadata updates
// implicitly once the file's data is durable (post the file-handle
// Sync() in FsyncFileAndParent), so there's no separate directory-entry
// flush analogous to POSIX. Worse, os.File.Sync() on a directory handle
// returns ERROR_INVALID_HANDLE and would turn every sidecar commit into
// a hard failure on Windows.
func syncDir(filePath string) error {
	_ = filePath
	return nil
}
