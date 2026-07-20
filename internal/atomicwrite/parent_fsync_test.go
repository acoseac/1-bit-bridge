package atomicwrite

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// stubSyncParentDir installs a recording stub over the parent-dir
// barrier seam and restores the production implementation on cleanup.
// Returns a pointer to the slice of paths the barrier was asked to
// flush, in call order.
//
// **Callers MUST NOT call `t.Parallel()`.** `syncParentDir` is a
// process-global seam (see its docblock): parallel tests installing
// different stubs would clobber each other's expectations, and the
// recorded-path slice would interleave calls from unrelated tests.
// Guarding the assignment with a mutex would only quiet the race
// detector while leaving that logical interference intact — serial
// execution plus the `t.Cleanup` restore below is the actual contract,
// and it's the convention every test seam in this repo follows.
func stubSyncParentDir(t *testing.T, ret error) *[]string {
	t.Helper()
	var seen []string
	prev := syncParentDir
	syncParentDir = func(path string) error {
		seen = append(seen, path)
		return ret
	}
	t.Cleanup(func() { syncParentDir = prev })
	return &seen
}

// TestWriteBytes_SyncsParentDirectory pins the durability barrier on
// the buffered write path: after the tmp-file rename commits, the
// destination's parent directory entry MUST be flushed. Without it the
// write is atomic but not crash-durable — the bytes are fsynced while
// the directory entry publishing them can still be unflushed in the
// filesystem journal.
func TestWriteBytes_SyncsParentDirectory(t *testing.T) {
	seen := stubSyncParentDir(t, nil)

	dir := t.TempDir()
	dst := filepath.Join(dir, "durable.bin")
	if err := WriteBytes(dst, []byte("payload"), ".sync-*.tmp"); err != nil {
		t.Fatalf("WriteBytes: %v", err)
	}

	if len(*seen) != 1 {
		t.Fatalf("parent-dir sync calls = %d (%v), want exactly 1", len(*seen), *seen)
	}
	// The barrier is handed the DESTINATION path (it derives the parent
	// itself) — handing it the tmp path would flush the same directory
	// today but breaks the moment a caller stages a tmp elsewhere.
	if (*seen)[0] != dst {
		t.Errorf("synced path = %q, want the destination %q", (*seen)[0], dst)
	}
}

// TestRenameWithRetry_SyncsParentDirectoryOnce pins that the barrier
// runs exactly once per successful rename — on the attempt that
// actually commits, not on each retry.
func TestRenameWithRetry_SyncsParentDirectoryOnce(t *testing.T) {
	seen := stubSyncParentDir(t, nil)

	calls := 0
	prev := SetRenameFuncForTest(func(src, dst string) error {
		calls++
		if calls == 1 {
			return os.ErrPermission // transient — forces one retry
		}
		return nil
	})
	t.Cleanup(func() { SetRenameFuncForTest(prev) })

	if err := RenameWithRetry("src", "dst"); err != nil {
		t.Fatalf("RenameWithRetry: %v", err)
	}
	if calls < 2 {
		t.Fatalf("rename attempts = %d, want >= 2 (retry didn't fire)", calls)
	}
	if len(*seen) != 1 {
		t.Errorf("parent-dir sync calls = %d (%v), want exactly 1 (only the committing attempt)", len(*seen), *seen)
	}
}

// TestRenameWithRetry_NoSyncWhenRenameFails pins the inverse: nothing
// was published, so there is no directory entry to flush. A barrier
// call here would fsync a directory for a write that never landed.
func TestRenameWithRetry_NoSyncWhenRenameFails(t *testing.T) {
	seen := stubSyncParentDir(t, nil)

	prev := SetRenameFuncForTest(func(src, dst string) error { return os.ErrPermission })
	t.Cleanup(func() { SetRenameFuncForTest(prev) })

	if err := RenameWithRetry("src", "dst"); err == nil {
		t.Fatal("RenameWithRetry returned nil; the injected failure must propagate")
	}
	if len(*seen) != 0 {
		t.Errorf("parent-dir sync calls = %d (%v), want 0 on a failed rename", len(*seen), *seen)
	}
}

// TestWriteBytes_DirSyncFailureDoesNotFailWrite is the load-bearing
// tolerance contract. By the time the barrier runs the rename has
// already committed: the new content is live and visible to every
// reader. Some network / FUSE mounts answer EINVAL / ENOTSUP on a
// directory fsync — surfacing that as an error would make callers take
// remediation paths (retry, abort a scan, surface a failure to the
// operator) for an operation that actually succeeded.
func TestWriteBytes_DirSyncFailureDoesNotFailWrite(t *testing.T) {
	seen := stubSyncParentDir(t, errors.New("fsync: operation not supported"))

	dir := t.TempDir()
	dst := filepath.Join(dir, "tolerated.bin")
	want := []byte("still committed")

	if err := WriteBytes(dst, want, ".tolerate-*.tmp"); err != nil {
		t.Fatalf("WriteBytes = %v, want nil (a dir-sync failure must not fail a completed write)", err)
	}
	if len(*seen) != 1 {
		t.Fatalf("parent-dir sync calls = %d, want 1 (the failing branch must still have been exercised)", len(*seen))
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("destination bytes = %q, want %q", got, want)
	}
}

// TestRenameWithRetryCtx_DirSyncFailureDoesNotFailRename mirrors the
// tolerance contract on the cancellation-aware variant used by the
// transcode / analyze job workers.
func TestRenameWithRetryCtx_DirSyncFailureDoesNotFailRename(t *testing.T) {
	stubSyncParentDir(t, errors.New("fsync: invalid argument"))

	prev := SetRenameFuncForTest(func(src, dst string) error { return nil })
	t.Cleanup(func() { SetRenameFuncForTest(prev) })

	if err := RenameWithRetryCtx(context.Background(), "src", "dst"); err != nil {
		t.Fatalf("RenameWithRetryCtx = %v, want nil (dir-sync failure is tolerated)", err)
	}
}

// TestWriteBytes_RealParentFsyncSucceeds exercises the PRODUCTION
// barrier (no stub) end-to-end against a real temp directory, so the
// wiring is proven against the actual syscall — on POSIX a genuine
// directory fsync, on Windows the compile-time no-op.
func TestWriteBytes_RealParentFsyncSucceeds(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "real.bin")
	want := []byte("real fsync path")

	if err := WriteBytes(dst, want, ".real-*.tmp"); err != nil {
		t.Fatalf("WriteBytes: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("destination bytes = %q, want %q", got, want)
	}
}
