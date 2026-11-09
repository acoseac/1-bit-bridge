//go:build windows

package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestAReadOnlyMarkerIsNeverTakenUpOnWindows pins what round 3 leaves as it
// is on Windows: the marker's sync in place is FlushFileBuffers, which needs a
// handle opened for writing, so a marker serve may not write there (its
// read-only attribute, here, or an ACL) is never taken up into the record,
// which a start writes without it, the data dir protected by its database and
// its lock file alone, and the marker left as it was. Linux and macOS take
// such a marker up (TestAMarkerThisUserMayNotWriteIsTakenUp).
func TestAReadOnlyMarkerIsNeverTakenUpOnWindows(t *testing.T) {
	const id = "0123456789abcdef0123456789abcdef"
	base := t.TempDir()
	lib := filepath.Join(base, "Music")
	dataDir := filepath.Join(base, "data")
	mkdirs(t, lib, dataDir)
	touch(t, filepath.Join(dataDir, "bridge.db"), filepath.Join(dataDir, serveLockFileName))
	writeIdentity(t, dataDir, id)
	marker := dataDirMarkerFile(dataDir)
	if err := os.Chmod(marker, 0o444); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(marker, 0o644) })
	if f, err := os.OpenFile(marker, os.O_RDWR, 0); err == nil {
		_ = f.Close()
		t.Fatal("a marker with the read-only attribute opened for writing: the case tests nothing")
	}
	cfg := writeIdentityConfig(t, filepath.Join(base, "bridge.yaml"), lib, dataDir)
	stopServed(t, bootServe(t, "--config", cfg))
	if got := recordedIdentity(t, cfg); got != "" {
		t.Errorf("the record holds the identity %q from a marker that could not be synced where it is, want none", got)
	}
	if got := identityOf(t, dataDir); got != id {
		t.Errorf("the marker holds %q, want %q, as it was", got, id)
	}
}
