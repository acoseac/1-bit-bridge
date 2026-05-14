//go:build windows

package transcode

// syncDir is a no-op on Windows. NTFS journals metadata updates
// implicitly when the file's data is durable (post the file-handle
// `Sync()` in `fsyncFileAndParent`), so there's no separate
// directory-entry flush analogous to POSIX. Worse, `os.File.Sync()`
// on a directory handle returns `ERROR_INVALID_HANDLE` and would
// turn every variant commit into a hard failure on Windows
// installs.
//
// Keeping the empty body in a //go:build-tagged file (rather than a
// runtime `runtime.GOOS != "windows"` check inside `fsync.go`)
// surfaces the asymmetry at the right layer: anyone reading the
// Unix flavour sees the explicit directory fsync; anyone reading
// the Windows flavour sees the explicit decision NOT to.
func syncDir(filePath string) error {
	_ = filePath
	return nil
}
