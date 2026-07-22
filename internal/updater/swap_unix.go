//go:build !windows

package updater

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// linkFunc / renameFunc indirect os.Link / os.Rename so tests can force
// the hardlink step to fail (exercising the two-rename fallback) or force
// the new-binary rename to return EXDEV (exercising the cross-device
// copy fallback). Test-only seams — production code MUST NOT mutate them
// (same convention as renameFunc / removeFunc elsewhere in the package).
var (
	linkFunc   = os.Link
	renameFunc = os.Rename
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
// Cross-device caveat: the new binary is extracted into a per-attempt
// scratch dir under DataDir, which is NOT guaranteed to share a
// filesystem with the install path — a
// common production layout puts dataDir under /home or /var and the binary
// under /usr (e.g. bridge.ars.md installs to /usr/local/bin/bridge). When
// they differ, os.Rename(newBinary, dst) fails with EXDEV. placeNewBinary
// handles that by copying the new binary into a temp file in dst's OWN
// directory and atomically renaming it there — so the swap stays atomic on
// dst's filesystem and the bak-holds-old-binary rollback contract is kept.
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

	// os.Link refuses to create bak if it already exists (EEXIST), so a
	// stale .bak from a previous cycle has to go — but ONLY once we know
	// that is actually why the link failed.
	//
	// Pre-fix this was an unconditional os.Remove(bak) BEFORE the link.
	// The shape it replaced opened with os.Rename(dst, bak) — an atomic
	// overwrite that left the previous .bak intact on failure — and the
	// remove-first version gave that up. If linkFunc then failed
	// (link-less FS, fs.protected_hardlinks) AND swapBinaryViaRename's
	// first rename also failed, the install aborted with the operator's
	// rollback target already destroyed, and RollbackBinary hard-fails on
	// a missing bak. Narrow (both must fail in a directory
	// preflightWritable just certified) but strictly worse than what it
	// replaced. So: try the link first, and clear a stale bak only when
	// EEXIST says that is the obstacle (R5).
	linkErr := linkFunc(dst, bak)
	if linkErr != nil && errors.Is(linkErr, fs.ErrExist) {
		if rmErr := os.Remove(bak); rmErr != nil && !os.IsNotExist(rmErr) {
			return fmt.Errorf("remove stale backup %s: %w", bak, rmErr)
		}
		linkErr = linkFunc(dst, bak)
	}
	if linkErr != nil {
		// Link-less / cross-device filesystem — fall back to the
		// two-rename swap (which overwrites any bak itself).
		return swapBinaryViaRename(dst, newBinary, bak)
	}

	// dst and bak now hardlink the same (old) inode. Atomically point dst
	// at the new binary; bak keeps the old inode alive for rollback. On
	// POSIX os.Rename over an existing dst is atomic, so a crash here
	// leaves dst as either the old or the new binary — never absent.
	// placeNewBinary falls back to a copy-into-dst-dir on EXDEV (cross-fs).
	if err := placeNewBinary(newBinary, dst); err != nil {
		// The install didn't happen, so dst still resolves to the old
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
	// Move dst → dst.bak. os.Rename overwrites on POSIX, so an existing
	// .bak from a previous cycle is consumed here — unavoidable on this
	// path, since bak IS the vacate target the two-rename swap needs.
	// Routed through renameFunc (not bare os.Rename) so tests can drive
	// the "this rename fails" branch; that is the case where the caller's
	// deferred-clear fix actually pays off, because a pre-existing .bak
	// survives untouched (R5).
	if err := renameFunc(dst, bak); err != nil {
		return fmt.Errorf("rename %s -> %s: %w", dst, bak, err)
	}
	// Move new binary into place (EXDEV → copy into dst's dir; see
	// placeNewBinary). If this fails, restore .bak so we don't leave the
	// operator with no executable at all.
	if err := placeNewBinary(newBinary, dst); err != nil {
		if rerr := os.Rename(bak, dst); rerr != nil {
			return fmt.Errorf("install %s -> %s failed (%v); rollback also failed (%v); manual recovery needed",
				newBinary, dst, err, rerr)
		}
		return fmt.Errorf("install %s -> %s: %w (rolled back)", newBinary, dst, err)
	}
	fsyncDir(filepath.Dir(dst))
	return nil
}

// placeNewBinary installs newBinary at dst. It first tries an atomic
// os.Rename (no extra copy) and falls back to copyAndRename ONLY on EXDEV
// — the cross-filesystem case where the scratch dir under DataDir and
// the install path live on different mounts (e.g. /var vs /usr on
// Linux). Both paths
// leave the OLD binary reachable via bak (the caller's rollback contract);
// copyAndRename's own rename happens WITHIN dst's directory, so it's atomic
// on that filesystem and dst is never absent. Any non-EXDEV rename error
// surfaces directly (a permission/IO fault is not a cross-device case).
func placeNewBinary(newBinary, dst string) error {
	err := renameFunc(newBinary, dst)
	if err != nil && errors.Is(err, syscall.EXDEV) {
		return copyAndRename(newBinary, dst)
	}
	return err
}

// copyAndRename copies src into a temp file in dst's OWN directory, fsyncs
// it, sets the executable bit, then atomically renames it over dst (a
// same-filesystem rename, so never EXDEV and never leaves dst absent).
// Used only as placeNewBinary's cross-device fallback. On success src is
// removed (it's the now-consumed scratch-dir copy). The tmp file is
// cleaned up on any failure via the deferred Remove (LIFO after Close, so
// Close runs first — Windows-safe ordering isn't needed here but mirrors
// the atomic-write idiom used elsewhere in the tree).
func copyAndRename(src, dst string) error {
	return copyIntoDirAndRename(src, dst, 0o755)
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
