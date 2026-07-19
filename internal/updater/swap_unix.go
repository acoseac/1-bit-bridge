//go:build !windows

package updater

import (
	"fmt"
	"os"
	"path/filepath"
)

// linkFunc indirects os.Link so tests can force the hardlink step in
// swapBinary to fail and exercise the two-rename fallback. Test-only
// seam — production code MUST NOT mutate it (same convention as
// renameFunc / removeFunc elsewhere in the package).
var linkFunc = os.Link

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
//
// Crash-safety: the naive "rename dst→bak, then rename newBinary→dst"
// leaves a window between the two syscalls in which NO file exists at
// dst. A power loss there permanently loses the binary, and the
// boot-time rollback can't recover — the missing file IS the bridge, so
// the service manager has nothing to launch. Instead we hardlink dst→bak
// FIRST (both directory entries now point at the old inode, so dst stays
// present the whole time AND bak holds the old binary for rollback), then
// atomically rename newBinary over dst. dst is never absent.
//
// os.Link fails on filesystems without hardlink support (some FUSE / FAT
// / network mounts) and across filesystems (EXDEV). There we fall back to
// the original two-rename swap (swapBinaryViaRename) — it reintroduces the
// tiny no-file window but is the best a link-less filesystem allows.
func swapBinary(dst, newBinary, backupExt string) error {
	bak := dst + backupExt

	// os.Link refuses to create bak if it already exists (EEXIST), so
	// clear any stale .bak from a previous cycle first. A missing .bak
	// is the normal case, not an error.
	if err := os.Remove(bak); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove stale backup %s: %w", bak, err)
	}
	if err := linkFunc(dst, bak); err != nil {
		// Link-less / cross-device filesystem — fall back to the
		// two-rename swap (which overwrites any bak itself).
		return swapBinaryViaRename(dst, newBinary, bak)
	}

	// dst and bak now hardlink the same (old) inode. Atomically point dst
	// at the new binary; bak keeps the old inode alive for rollback. On
	// POSIX os.Rename over an existing dst is atomic, so a crash here
	// leaves dst as either the old or the new binary — never absent.
	if err := os.Rename(newBinary, dst); err != nil {
		// The rename didn't happen, so dst still resolves to the old
		// binary via its own directory entry (the surviving hardlink) —
		// the bridge stays bootable. Drop the bak link we just made so a
		// stale .bak (identical to the live binary, with no install
		// marker committed) doesn't linger.
		_ = os.Remove(bak)
		return fmt.Errorf("install %s -> %s: %w", newBinary, dst, err)
	}

	fsyncDir(filepath.Dir(dst))
	return nil
}

// swapBinaryViaRename is the fallback two-rename swap used when the
// filesystem can't hardlink (EXDEV / no-hardlink-support). It carries the
// original no-file window between the two renames; the hardlink path in
// swapBinary is preferred precisely to avoid that window on filesystems
// that support it. bak is dst+backupExt and has already been cleared by
// the caller.
func swapBinaryViaRename(dst, newBinary, bak string) error {
	// Move dst → dst.bak. os.Rename overwrites on POSIX so a residual
	// .bak (there shouldn't be one — the caller removed it) is fine.
	if err := os.Rename(dst, bak); err != nil {
		return fmt.Errorf("rename %s -> %s: %w", dst, bak, err)
	}
	// Move new binary into place. If this fails, restore .bak so we
	// don't leave the operator with no executable at all.
	if err := os.Rename(newBinary, dst); err != nil {
		if rerr := os.Rename(bak, dst); rerr != nil {
			return fmt.Errorf("install %s -> %s failed (%v); rollback also failed (%v); manual recovery needed",
				newBinary, dst, err, rerr)
		}
		return fmt.Errorf("install %s -> %s: %w (rolled back)", newBinary, dst, err)
	}
	fsyncDir(filepath.Dir(dst))
	return nil
}

// fsyncDir fsyncs a directory so a rename inside it is durable. A crash
// between the rename's dentry update and the journal flush could
// otherwise leave an inconsistent state (which the rollback marker would
// notice on next boot, but we'd rather avoid the rollback dance entirely).
// Best-effort: a directory that can't be opened/synced isn't fatal.
func fsyncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
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
	fsyncDir(filepath.Dir(dst))
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
