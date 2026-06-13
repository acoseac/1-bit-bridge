//go:build !windows

package fsutil

import (
	"fmt"
	"os"
	"path/filepath"
)

// syncDir opens the parent directory of `filePath` and Syncs the
// handle. On ext4 / XFS / APFS a fresh rename(2) can leave the
// directory entry in the journal but not yet on disk; syncing the
// directory handle commits the entry update so a power-loss between
// the file fsync and the entry flush can't resurrect the file at its
// old path (or lose it). A missing parent is treated as success — the
// caller's file fsync would already have failed if the path didn't
// exist.
func syncDir(filePath string) (err error) {
	dir := filepath.Dir(filePath)
	if dir == "" {
		dir = "."
	}
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
