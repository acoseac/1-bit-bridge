//go:build windows

package fsutil

// SyncParentDir is a no-op on Windows. NTFS journals metadata updates
// implicitly once the file's data is durable (post the file-handle
// Sync() in FsyncFileAndParent), so there's no separate directory-entry
// flush analogous to POSIX. Worse, os.File.Sync() on a directory handle
// maps to FlushFileBuffers, which rejects directory handles
// (ERROR_INVALID_HANDLE / ERROR_ACCESS_DENIED) — implementing this for
// real would turn every sidecar and atomic-write commit into a hard
// failure on Windows.
//
// Keeping the no-op here (rather than at each call site) means callers
// need no `runtime.GOOS` branch: the platform difference is resolved at
// compile time and both `FsyncFileAndParent` and
// `internal/atomicwrite`'s post-rename barrier build and behave sanely
// on windows/amd64 + windows/arm64.
func SyncParentDir(filePath string) error {
	_ = filePath
	return nil
}
