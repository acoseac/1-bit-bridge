//go:build !windows

package transcode

import (
	"fmt"
	"os"
	"path/filepath"
)

// syncDir opens the parent directory of `filePath` read-only and
// calls Sync on the handle. On ext4 / XFS / APFS the directory's
// entries are stored as B-tree nodes that aren't flushed by a
// file fsync alone — a fresh rename(2) can leave the entry in
// the journal but not yet on disk, so a power-loss between the
// file fsync and the entry flush would resurrect the file at
// its OLD path (or not at all). Syncing the directory handle
// commits the entry update.
//
// Best-effort safety: a missing parent directory is treated as a
// success — the caller's file fsync would have already failed if
// the path didn't exist. Same for filesystems where dir-fsync is
// a documented no-op (procfs, sysfs); the call returns nil per
// POSIX semantics.
func syncDir(filePath string) (err error) {
	dir := filepath.Dir(filePath)
	if dir == "" {
		// `filepath.Dir("")` returns "." per the stdlib contract, so
		// this only fires on pathological input. Normalise to cwd
		// rather than no-opping — the durability guarantee is the
		// load-bearing reason this function exists, and a bare
		// filename in production (test fixture or otherwise) should
		// still get its parent directory synced. CodeRabbit minor on
		// PR #251.
		dir = "."
	}
	// Named return + deferred Close so a panic in d.Sync() (or any
	// future code added before the return) can't leak the fd. Explicit
	// `=` (NOT `:=`) so we assign the named `err`, not a shadow — the
	// defer must observe the same variable. Gemini r4.
	var d *os.File
	d, err = os.Open(dir)
	if err != nil {
		return fmt.Errorf("open dir %q: %w", dir, err)
	}
	defer func() {
		if cerr := d.Close(); err == nil && cerr != nil {
			err = fmt.Errorf("close dir %q: %w", dir, cerr)
		}
	}()
	if syncErr := d.Sync(); syncErr != nil {
		return fmt.Errorf("sync dir %q: %w", dir, syncErr)
	}
	return nil
}
