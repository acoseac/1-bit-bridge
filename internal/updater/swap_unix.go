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
// linkFunc is the hardlink seam. renameFunc lives in swap_copy.go
// because staging is shared and Windows needs the same seam.
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
//
// markSwapStarted (nil in tests) is invoked immediately before the first
// filesystem mutation and aborts the swap on error; see State.SwapStarted.
// The window it closes is a Windows one — this path reaches its first
// mutation in microseconds — but the hook fires on both platforms so the
// marker's meaning is identical everywhere.
func swapBinary(dst, newBinary, backupExt string, markSwapStarted func() error) error {
	bak := dst + backupExt
	if err := armSwap(markSwapStarted); err != nil {
		return err
	}
	// Stage BEFORE anything is vacated. On this path it changes nothing
	// about the window — the hardlink keeps dst resolving through its own
	// dentry for the whole operation, so dst was never absent here even
	// when placeNewBinary fell back to a cross-volume copy. It matters
	// for the FALLBACK below, which had no such protection, and doing it
	// once for both keeps a single definition of "the new bytes are on
	// dst's volume, durable, with the right mode".
	staged, err := stageIntoDir(newBinary, dst, installedMode(dst))
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(staged)
		}
	}()

	// Try the link first, and clear a stale bak only when EEXIST says
	// that is the obstacle (R5). Removing bak unconditionally gave up the
	// atomic overwrite that left the previous .bak intact on failure: if
	// linkFunc then failed (link-less FS, fs.protected_hardlinks) AND the
	// fallback's first rename also failed, the install aborted with the
	// operator's rollback target already destroyed, and RollbackBinary
	// hard-fails on a missing bak.
	linkErr := linkFunc(dst, bak)
	if linkErr != nil && errors.Is(linkErr, fs.ErrExist) {
		if rmErr := os.Remove(bak); rmErr != nil && !os.IsNotExist(rmErr) {
			return fmt.Errorf("remove stale backup %s: %w", bak, rmErr)
		}
		linkErr = linkFunc(dst, bak)
	}
	if linkErr != nil {
		// Link-less / cross-device filesystem — fall back to the
		// two-rename swap. Its window is now two adjacent renames on one
		// volume, because the bytes are already staged beside dst.
		if err := commitViaRename(dst, staged, bak); err != nil {
			return err
		}
		committed = true
		return nil
	}

	// dst and bak now hardlink the same (old) inode. Atomically point dst
	// at the new binary; bak keeps the old inode alive for rollback. On
	// POSIX os.Rename over an existing dst is atomic, so a crash here
	// leaves dst as either the old or the new binary — never absent.
	//
	// Plain os.Rename, not renameFunc: staged sits in dst's OWN
	// directory, so this rename cannot be cross-device, and the test seam
	// forces EXDEV on every call it sees.
	if err := os.Rename(staged, dst); err != nil {
		// The install didn't happen, so dst still resolves to the old
		// binary via its own directory entry (the surviving hardlink) —
		// the bridge stays bootable. Drop the bak link we just made so a
		// stale .bak (identical to the live binary, with no install
		// marker committed) doesn't linger.
		_ = os.Remove(bak)
		return fmt.Errorf("install %s -> %s: %w", staged, dst, err)
	}
	committed = true
	fsyncDir(filepath.Dir(dst))
	return nil
}

// installedMode is the permission bits a swap should leave on dst: the
// ones dst ALREADY has.
//
// Not a hardcoded 0o755. The two swap paths used to disagree — the
// rename path inherited the extractor's `O_CREATE 0o755`, which IS
// umask-masked, while the copy path chmod'd an unmasked 0o755, under a
// comment asserting the two matched. Under `UMask=0027`, which this
// repo's own deployment runbook prescribes, that meant an update
// silently took the binary from 0755 to 0750: the service user still
// execs it, so the bridge runs, and every other account on the host gets
// EACCES on a binary that worked yesterday, with no log line.
//
// Preserving dst's mode fixes the divergence in both directions and is
// the least surprising rule — an update is not the place to change a
// binary's permissions, whichever way the operator set them. 0o755 is
// the fallback for a dst we cannot stat, which is the shape a first
// install has.
func installedMode(dst string) os.FileMode {
	if fi, err := os.Stat(dst); err == nil {
		if perm := fi.Mode().Perm(); perm != 0 {
			return perm
		}
	}
	return 0o755
}

// isCrossDeviceErr reports whether a rename failed because the two paths
// live on different filesystems — the one condition that makes staging
// fall back to a copy.
func isCrossDeviceErr(err error) bool { return errors.Is(err, syscall.EXDEV) }

// commitViaRename is the two-rename commit used when the filesystem
// cannot hardlink (EXDEV / no-hardlink-support), and the shape Windows
// uses unconditionally.
//
// `staged` is already beside dst, on dst's volume, with its mode set —
// so the no-file window really is the gap between two renames in one
// directory, which is what this path's comments have always claimed. It
// used to enclose placeNewBinary's cross-volume copy plus an fsync.
//
// A failure of the second rename restores bak, so the operator is never
// left without an executable by a swap that merely failed. The hazard
// this reordering removes is the one no in-process restore can cover: a
// power loss while dst is absent.
func commitViaRename(dst, staged, bak string) error {
	if err := renameFunc(dst, bak); err != nil {
		return fmt.Errorf("rename %s -> %s: %w", dst, bak, err)
	}
	// Plain os.Rename: same directory, so never cross-device.
	if err := os.Rename(staged, dst); err != nil {
		if rerr := os.Rename(bak, dst); rerr != nil {
			return fmt.Errorf("install %s -> %s failed (%v); rollback also failed (%v); manual recovery needed",
				staged, dst, err, rerr)
		}
		return fmt.Errorf("install %s -> %s: %w (rolled back)", staged, dst, err)
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
