package updater

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// copyIntoDirAndRename copies src into a temp file inside dst's OWN
// directory, fsyncs it, optionally chmods it, then atomically renames it
// over dst. On success src is removed (it's the now-consumed scratch copy).
//
// Shared by both platforms' cross-device fallbacks — Unix's EXDEV branch
// and Windows' ERROR_NOT_SAME_DEVICE branch — which differ only in whether
// the executable bit is meaningful. Keeping one implementation means the
// commit ordering (copy -> sync -> close -> chmod -> rename -> suppress
// cleanup) can't drift between them; that ordering is the load-bearing
// part, since the rename is what makes the swap atomic on dst's
// filesystem and is why we copy into dst's directory rather than renaming
// across the device boundary.
//
// mode 0 skips the chmod (NTFS has no executable bit to set).
//
// Plain os.Rename, NOT renameFunc: the test seam forces a cross-device
// error on the newBinary->dst rename, but this same-directory rename must
// run for real or the fallback could never commit.
func copyIntoDirAndRename(src, dst string, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".bridge-swap-*")
	if err != nil {
		return fmt.Errorf("create temp in dst dir: %w", err)
	}
	tmpName := tmp.Name()
	// LIFO: Close (registered second) runs BEFORE Remove — Windows refuses
	// to unlink a file that still has an open handle.
	defer func() {
		if tmpName != "" {
			_ = os.Remove(tmpName)
		}
	}()
	defer func() { _ = tmp.Close() }()

	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open source binary: %w", err)
	}
	defer in.Close()

	if _, err := io.Copy(tmp, in); err != nil {
		return fmt.Errorf("copy binary across devices: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync copied binary: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close copied binary: %w", err)
	}
	if mode != 0 {
		// CreateTemp makes the file 0o600; the installed binary must be
		// executable, matching the extractor's O_CREATE 0o755.
		if err := os.Chmod(tmpName, mode); err != nil {
			return fmt.Errorf("chmod copied binary: %w", err)
		}
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return fmt.Errorf("rename copied binary into place: %w", err)
	}
	tmpName = "" // committed; don't remove
	_ = os.Remove(src)
	return nil
}
