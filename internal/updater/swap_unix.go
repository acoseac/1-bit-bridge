//go:build !windows

package updater

import (
	"fmt"
	"os"
	"path/filepath"
)

// swapBinary atomically replaces the running binary on darwin/linux.
//
// macOS/Linux semantics: os.Rename can replace a running executable
// because the kernel pins the old inode for the still-running process
// — the rename only updates the directory entry. The next exec
// (after restart) will load the new bytes. fsync the parent
// directory after the rename so a crash before the buffer flushes
// doesn't end up with an inconsistent on-disk state (rename done in
// dentry but not in journal).
//
// Cross-device caveat: os.Rename fails with EXDEV if newBinary lives
// on a different filesystem from dst. We avoid this by extracting
// the new binary into <DataDir>/updates/, which is on the same
// filesystem as the bridge install (both under the operator's data
// dir) in every supported deployment. If a future install layout
// puts dataDir on a separate volume from the binary, the right fix
// is a copy+remove fallback for EXDEV at this seam — not adding an
// in-process executable copier upstream.
//
// Layout after success:
//
//	<dir>/bridge       -> new binary
//	<dir>/bridge.bak   -> previous binary (kept for rollback)
//
// If a stale .bak exists from a previous install cycle, it's
// overwritten — only ever one .bak, never an accumulation.
//
// dst is the path of the currently-running binary (typically from
// os.Executable()). newBinary is the path to the freshly-extracted
// binary in a temp location. backupExt is ".bak" in production.
func swapBinary(dst, newBinary, backupExt string) error {
	bak := dst + backupExt

	// Move dst → dst.bak. If a stale .bak exists, overwrite it.
	// os.Rename overwrites on POSIX so this is one syscall.
	if err := os.Rename(dst, bak); err != nil {
		return fmt.Errorf("rename %s -> %s: %w", dst, bak, err)
	}

	// Move new binary into place. If this fails, restore .bak so
	// we don't leave the operator with no executable at all.
	if err := os.Rename(newBinary, dst); err != nil {
		if rerr := os.Rename(bak, dst); rerr != nil {
			return fmt.Errorf("install %s -> %s failed (%v); rollback also failed (%v); manual recovery needed",
				newBinary, dst, err, rerr)
		}
		return fmt.Errorf("install %s -> %s: %w (rolled back)", newBinary, dst, err)
	}

	// fsync the parent directory so the rename is durable. A crash
	// between the rename's dentry update and the journal flush
	// could otherwise leave an inconsistent state (which the
	// rollback marker would notice on next boot, but we'd rather
	// avoid the rollback dance entirely).
	parent := filepath.Dir(dst)
	if d, err := os.Open(parent); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}

	return nil
}

// RollbackBinary restores dst.bak → dst, overwriting whatever is
// currently at dst. Called by startup housekeeping when the previous
// install attempt's targetVersion didn't come up. Returns an error
// if .bak doesn't exist (we have nothing to roll back to).
func RollbackBinary(dst, backupExt string) error {
	bak := dst + backupExt
	if _, err := os.Stat(bak); err != nil {
		return fmt.Errorf("backup %s missing: %w", bak, err)
	}
	if err := os.Rename(bak, dst); err != nil {
		return fmt.Errorf("rollback rename %s -> %s: %w", bak, dst, err)
	}
	if d, err := os.Open(filepath.Dir(dst)); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// RemoveBackup deletes dst.bak. Called by startup housekeeping the
// boot AFTER a successful install to free disk. No-op when .bak
// doesn't exist (some prior cleanup already removed it).
func RemoveBackup(dst, backupExt string) error {
	bak := dst + backupExt
	err := os.Remove(bak)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
