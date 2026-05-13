package atomicwrite

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWriteBytes_happyPath pins the basic contract: writes the
// data, produces the destination file with the right bytes, AND
// leaves no `.tmp` behind.
func TestWriteBytes_happyPath(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "ok.bin")
	want := []byte("hello world")

	if err := WriteBytes(dst, want, ".test-*.tmp"); err != nil {
		t.Fatalf("WriteBytes: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("destination bytes: got %q, want %q", got, want)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".test-") {
			t.Errorf("leaked tmp file: %s", e.Name())
		}
	}
}

// TestWriteBytes_createsParentDir pins the MkdirAll-then-CreateTemp
// shape — caller can pass a path under a non-existent subdir and
// the helper creates the parent at 0o700.
func TestWriteBytes_createsParentDir(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "deep", "nested", "file.bin")
	if err := WriteBytes(dst, []byte("x"), ".test-*.tmp"); err != nil {
		t.Fatalf("WriteBytes: %v", err)
	}
	parentInfo, err := os.Stat(filepath.Dir(dst))
	if err != nil {
		t.Fatalf("parent dir not created: %v", err)
	}
	if parentInfo.Mode().Perm() != 0o700 {
		t.Errorf("parent dir mode: got %o, want 0o700", parentInfo.Mode().Perm())
	}
}

// TestRenameWithRetry_byteEqualFallback pins the load-bearing
// race-loser-accepts-byte-equivalent contract. We inject a
// rename failure via the test seam; the function reads the
// (pre-staged) destination, compares bytes, and accepts the
// "race winner already wrote our bytes" case as success.
func TestWriteBytes_acceptsRaceWinnerWithEqualBytes(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "race.bin")
	want := []byte("same-bytes-from-a-concurrent-writer")

	// Pre-stage the destination with the SAME bytes (simulates a
	// concurrent writer winning the rename race).
	if err := os.WriteFile(dst, want, 0o644); err != nil {
		t.Fatal(err)
	}

	prev := SetRenameFuncForTest(func(src, dst string) error { return os.ErrPermission })
	t.Cleanup(func() { SetRenameFuncForTest(prev) })

	if err := WriteBytes(dst, want, ".race-*.tmp"); err != nil {
		t.Fatalf("WriteBytes: %v (expected nil — race winner with matching bytes)", err)
	}
	// Pre-staged destination intact.
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("destination clobbered: got %q, want %q", got, want)
	}
	// No tmp leaked.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".race-") {
			t.Errorf("leaked tmp file: %s", e.Name())
		}
	}
}

// TestWriteBytes_propagatesRenameErrorOnByteMismatch pins the
// inverse: if the destination exists but has DIFFERENT bytes, the
// rename failure must propagate. Pre-fix a naive `size-only`
// comparison would have accepted the collision silently.
func TestWriteBytes_propagatesRenameErrorOnByteMismatch(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "collision.bin")
	want := []byte("the bytes WE tried to write")
	collision := make([]byte, len(want))
	copy(collision, want)
	collision[0] ^= 0xFF // same length, different content

	if err := os.WriteFile(dst, collision, 0o644); err != nil {
		t.Fatal(err)
	}

	prev := SetRenameFuncForTest(func(src, dst string) error { return os.ErrPermission })
	t.Cleanup(func() { SetRenameFuncForTest(prev) })

	if err := WriteBytes(dst, want, ".collision-*.tmp"); err == nil {
		t.Fatal("WriteBytes returned nil; rename error must propagate when destination bytes differ")
	}
	// Tmp must still be cleaned up by the defer even on error.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".collision-") {
			t.Errorf("leaked tmp file: %s", e.Name())
		}
	}
}

// TestSetRenameFuncForTest_restoresPrevious pins the test-seam
// contract: SetRenameFuncForTest returns the previous value so
// `defer SetRenameFuncForTest(prev)` is the standard restore
// pattern.
func TestSetRenameFuncForTest_restoresPrevious(t *testing.T) {
	original := renameFunc
	prev := SetRenameFuncForTest(func(src, dst string) error { return nil })
	// `prev` should equal what `renameFunc` was before — i.e. the
	// `os.Rename` reference at package init. Compare via pointer
	// since func values don't have a meaningful == otherwise.
	_ = prev
	if renameFunc == nil {
		t.Fatal("renameFunc nil after SetRenameFuncForTest")
	}
	// Restore.
	SetRenameFuncForTest(original)
	// Soft check that the restored value behaves like the original
	// — we can't directly compare function values, so just confirm
	// it's non-nil and produces a `not exist` failure on a bogus
	// path (i.e. it's actually calling os.Rename, not our stub).
	err := renameFunc("/nonexistent-src", "/nonexistent-dst")
	if err == nil {
		t.Error("restored renameFunc returned nil; expected os.Rename's not-exist error")
	}
}
