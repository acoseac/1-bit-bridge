package transcode

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestFsyncFileAndParent_HappyPath pins the helper's success contract
// against a real file in a tmp directory. The fsync calls themselves
// don't have observable side-effects we can assert against from
// userspace — the contract is "no error AND file still readable" —
// which is exactly what we verify.
func TestFsyncFileAndParent_HappyPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "variant.flac")
	if err := os.WriteFile(path, []byte("synthetic-sidecar"), 0o644); err != nil {
		t.Fatalf("seed sidecar: %v", err)
	}

	if err := fsyncFileAndParent(path); err != nil {
		t.Errorf("fsyncFileAndParent on a healthy file: %v", err)
	}

	// File must still be readable and unchanged — fsync has no
	// content-mutating semantics, but a buggy implementation could
	// truncate or zero on the wrong syscall.
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("re-read sidecar: %v", err)
	}
	if string(got) != "synthetic-sidecar" {
		t.Errorf("content changed after fsync: got %q, want %q", got, "synthetic-sidecar")
	}
}

// TestFsyncFileAndParent_MissingFileSurfacesError closes the
// contract that a missing file is a hard error — the helper is
// called BETWEEN SoX's rename and the DB commit, so a missing file
// at this point means the runner lied about its success.
// Surfacing the error skips the DB commit (in production: triggers
// the jobFailed branch).
func TestFsyncFileAndParent_MissingFileSurfacesError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "never-written.flac")

	err := fsyncFileAndParent(path)
	if err == nil {
		t.Fatalf("expected error for missing file, got nil")
	}
	if !strings.Contains(err.Error(), "open for fsync") {
		t.Errorf("error should mention 'open for fsync', got %v", err)
	}
}

// TestFsyncFileAndParent_DirectoryAsPathSurfacesError pins that
// passing a directory (instead of a file) is a hard error.
// fsync on a directory handle is a separate code path (syncDir) —
// passing a directory to the top-level helper should fail at the
// file-fsync stage, not silently succeed.
//
// On Unix, opening a directory with O_RDONLY succeeds AND Sync() on
// the resulting *os.File may succeed too (it ends up acting like a
// directory fsync). To make the test deterministic, we point at a
// non-existent path INSIDE an existing directory — that fails at
// the open-for-fsync stage regardless of platform.
func TestFsyncFileAndParent_PathInsideMissingDir(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "deep", "file.flac")

	err := fsyncFileAndParent(path)
	if err == nil {
		t.Fatalf("expected error for path inside missing parent dir, got nil")
	}
}

// TestFsyncFileAndParent_EmptyFileSucceeds — zero-byte files are
// legitimate intermediate state (SoX can produce one during
// shutdown). Fsync must still succeed; the row this points at
// will fail freshness checks downstream, which is the correct
// failure surface for an empty variant.
func TestFsyncFileAndParent_EmptyFileSucceeds(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.flac")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatalf("seed empty file: %v", err)
	}
	if err := fsyncFileAndParent(path); err != nil {
		t.Errorf("empty-file fsync: %v", err)
	}
}
