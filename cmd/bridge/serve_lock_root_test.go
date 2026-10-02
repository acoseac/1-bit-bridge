//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// TestServeLockFileKeepsTheInstallOwnerAsRoot runs lockServeDataDir as root
// over a data dir another uid owns, and requires the lock file it creates
// to be that uid's. Left root's and 0600, the service user could not open
// it again, and every serve after a `sudo bridge serve` would run without
// the check. Root only: CI skips it, and dido's container runs it.
func TestServeLockFileKeepsTheInstallOwnerAsRoot(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root: it hands the lock file to another uid")
	}
	dataDir := filepath.Join(t.TempDir(), "data")
	if err := os.Mkdir(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(dataDir, 4242, 4243); err != nil {
		t.Fatal(err)
	}
	release, err := lockServeDataDir(dataDir)
	if err != nil {
		t.Fatalf("lockServeDataDir: %v", err)
	}
	defer release()
	info, err := os.Stat(filepath.Join(dataDir, serveLockFileName))
	if err != nil {
		t.Fatal(err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("no owner in %T", info.Sys())
	}
	if st.Uid != 4242 || st.Gid != 4243 {
		t.Errorf("the lock file is %d:%d, want the data dir's 4242:4243", st.Uid, st.Gid)
	}
}
