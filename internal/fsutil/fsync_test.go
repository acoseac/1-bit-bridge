package fsutil

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSyncParentDir_FlushesRealDirectory pins the exported barrier
// against a real filesystem: a POSIX directory fsync must succeed, and
// on Windows the compile-time no-op must equally report success rather
// than the ERROR_INVALID_HANDLE a real FlushFileBuffers on a directory
// handle would produce.
func TestSyncParentDir_FlushesRealDirectory(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "committed.bin")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SyncParentDir(file); err != nil {
		t.Errorf("SyncParentDir(%q) = %v, want nil", file, err)
	}
}

// TestSyncParentDir_BareFilenameUsesCWD pins the documented
// filepath.Dir behaviour the implementation relies on: a bare filename
// yields ".", which os.Open handles natively, so no empty-string
// fallback is needed.
func TestSyncParentDir_BareFilenameUsesCWD(t *testing.T) {
	if err := SyncParentDir("bare-name-no-dir.bin"); err != nil {
		t.Errorf(`SyncParentDir("bare-name-no-dir.bin") = %v, want nil (should sync ".")`, err)
	}
}

// TestFsyncFileAndParent_StillDelegatesToSyncParentDir keeps the
// package's own consumer of the (now exported) barrier honest — the
// sidecar durability contract described in FsyncFileAndParent's
// docblock is unchanged by the rename.
func TestFsyncFileAndParent_StillDelegatesToSyncParentDir(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "sidecar.flac")
	if err := os.WriteFile(file, []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := FsyncFileAndParent(file); err != nil {
		t.Errorf("FsyncFileAndParent(%q) = %v, want nil", file, err)
	}
}
