package transcode

import (
	"fmt"
	"os"
)

// fsyncFileAndParent durably commits `path` (and on POSIX systems
// the parent directory entry created by the upstream rename) to
// stable storage.
//
// Why: `UpsertVariant` writes the SQLite row that points at this
// path. If the bridge crashes between SoX's atomic rename and the
// DB commit, recovery is clean (the next manifest scan re-runs the
// transcode). But if the crash lands between DB commit and the
// OS flushing the rename to disk, iOS clients can request a row
// whose file isn't durably on disk yet — at best a 404, at worst
// a partially-readable file. Fsync upgrades that to a strict "file
// then DB" ordering: post-fsync, the file is durable, and the DB
// commit publishes a pointer at something the kernel won't lose.
//
// The function is the single chokepoint for variant durability —
// tests inject a stub via `Pool.fsyncFn` to drive the error branch.
// Production wiring goes through `pool.NewPool` defaulting
// `p.fsyncFn = fsyncFileAndParent`.
func fsyncFileAndParent(path string) error {
	// File fsync — cross-platform. **O_RDWR**, not O_RDONLY, because
	// Go's `File.Sync()` maps to `FlushFileBuffers` on Windows, and
	// that WinAPI explicitly **requires the handle be opened with
	// GENERIC_WRITE access**:
	//   https://learn.microsoft.com/en-us/windows/win32/api/fileapi/nf-fileapi-flushfilebuffers
	//   "The handle for hFile must be opened with the GENERIC_WRITE
	//    access right."
	// Opening RDONLY surfaces as ERROR_ACCESS_DENIED at sync time on
	// Windows — every Windows operator running optimize/upscale would
	// see "fsync sidecar: Access is denied" on every job (verified
	// 2026-05-19 on home-pc with SoX 14.4.2). POSIX side keeps
	// working because `fsync(2)` doesn't care about access mode —
	// any open fd is fine. RDWR is the cross-platform answer; we
	// don't actually write anything new through this handle, so
	// there's no behavioral cost on the POSIX side.
	//
	// The earlier `RDONLY because Sync only needs to flush pending
	// writes` rationale was a POSIX-centric micro-optimization that
	// quietly broke Windows when the optimize/upscale feature was
	// later exercised against a real Windows install.
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open for fsync: %w", err)
	}
	if syncErr := f.Sync(); syncErr != nil {
		_ = f.Close()
		return fmt.Errorf("fsync file: %w", syncErr)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close after fsync: %w", err)
	}
	// Parent-directory fsync (POSIX only — see syncDir's per-platform
	// implementations). Required on ext4/XFS/APFS so the directory-entry
	// update from SoX's rename is flushed alongside the file data.
	if err := syncDir(path); err != nil {
		return fmt.Errorf("fsync parent dir: %w", err)
	}
	return nil
}
