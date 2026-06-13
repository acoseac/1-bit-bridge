// Package fsutil holds small cross-cutting filesystem helpers shared
// across the bridge — today the durability barrier (file + parent-dir
// fsync) that producers of on-disk sidecars run before committing the
// SQLite row that points at the freshly-written file.
//
// `internal/transcode` carries an equivalent unexported copy
// (`fsyncFileAndParent` / `syncDir`) predating this package; keep the
// two in lockstep until that one is migrated over.
package fsutil

import (
	"fmt"
	"os"
)

// FsyncFileAndParent flushes `path` (and, on POSIX, its parent
// directory entry) to stable storage. Producers call this on a
// freshly-renamed sidecar BEFORE committing the DB row that points at
// it, so a client reacting to the commit (e.g. an iOS delta-sync) can
// never race a non-durable file. The inverse ordering (commit then
// fsync) recovers cleanly — a crash in the gap just re-runs the
// producer — but a missing-file race after a published commit does not.
func FsyncFileAndParent(path string) error {
	// O_RDWR (not O_RDONLY) because Go's File.Sync() maps to
	// FlushFileBuffers on Windows, which requires GENERIC_WRITE —
	// O_RDONLY surfaces as ERROR_ACCESS_DENIED at sync time. POSIX
	// fsync(2) accepts any open fd, so this is the cross-platform
	// answer.
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
	if err := syncDir(path); err != nil {
		return fmt.Errorf("fsync parent dir: %w", err)
	}
	return nil
}
