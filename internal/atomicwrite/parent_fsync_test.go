package atomicwrite

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
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

// noDirectoryBarrier sets the platform seam to a platform with no
// directory barrier (Windows, where fsutil.SyncsParentDir is false) and
// restores it on cleanup. Callers MUST NOT call `t.Parallel()`, as with
// stubSyncParentDir.
func noDirectoryBarrier(t *testing.T) {
	t.Helper()
	prev := syncParentDir
	syncParentDir = nil
	t.Cleanup(func() { syncParentDir = prev })
}

// TestWriteBytesDurable_RefusesAPlatformWithNoDirectoryBarrier pins the
// strict writer on a platform with no directory barrier, through the
// platform seam: a rename there is atomic and never made durable, so
// WriteBytesDurable refuses with ErrNoDirectoryBarrier, an
// errors.ErrUnsupported, before it writes anything, the directory it would
// create included. WriteBytes, best effort, writes as it always has.
func TestWriteBytesDurable_RefusesAPlatformWithNoDirectoryBarrier(t *testing.T) {
	noDirectoryBarrier(t)
	root := t.TempDir()
	dst := filepath.Join(root, "missing", "reserved.json")
	err := WriteBytesDurable(dst, []byte("reserved"), ".durable-*.tmp")
	if !errors.Is(err, ErrNoDirectoryBarrier) || !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("WriteBytesDurable with no directory barrier = %v, want ErrNoDirectoryBarrier, an errors.ErrUnsupported", err)
	}
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
		t.Fatalf("the refused write left %v (%v) behind, want nothing", entries, err)
	}
	cover := filepath.Join(root, "cover.jpg")
	if err := WriteBytes(cover, []byte("cover"), ".caa-*.jpg.tmp"); err != nil {
		t.Fatalf("WriteBytes with no directory barrier = %v, want nil: best effort is unchanged", err)
	}
	if got, err := os.ReadFile(cover); err != nil || string(got) != "cover" {
		t.Fatalf("WriteBytes wrote %q, %v", got, err)
	}
}

// TestWriteBytesDurable_OnThisPlatform pins the strict writer on the
// platform the test runs on, with no seam replaced: on Windows, whose
// barrier is a no-op, it refuses; everywhere else it writes, through a real
// directory fsync.
func TestWriteBytesDurable_OnThisPlatform(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "reserved.json")
	err := WriteBytesDurable(dst, []byte("reserved"), ".durable-*.tmp")
	if runtime.GOOS == "windows" {
		if !errors.Is(err, ErrNoDirectoryBarrier) {
			t.Fatalf("WriteBytesDurable on Windows = %v, want ErrNoDirectoryBarrier", err)
		}
		return
	}
	if err != nil {
		t.Fatalf("WriteBytesDurable: %v", err)
	}
	if got, err := os.ReadFile(dst); err != nil || string(got) != "reserved" {
		t.Fatalf("destination %q, %v", got, err)
	}
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

// TestWriteBytesDurable_DirSyncFailureFailsTheWrite pins the strict
// writer's half of the contract, the inverse of the tolerance above: a
// caller that hands out nothing the file covers until the write is durable
// gets the barrier's failure as the write's, through the same one barrier
// per write. The bytes may be in place; the write is not counted as made.
func TestWriteBytesDurable_DirSyncFailureFailsTheWrite(t *testing.T) {
	syncErr := errors.New("fsync: input/output error")
	seen := stubSyncParentDir(t, syncErr)

	dst := filepath.Join(t.TempDir(), "reserved.json")
	err := WriteBytesDurable(dst, []byte("reserved"), ".durable-*.tmp")
	if !errors.Is(err, syncErr) {
		t.Fatalf("WriteBytesDurable = %v, want the barrier's failure", err)
	}
	if len(*seen) != 1 || (*seen)[0] != dst {
		t.Fatalf("parent-dir sync calls = %v, want exactly one, for %q", *seen, dst)
	}
}

// TestWriteBytesDurable_SyncsParentDirectoryOnce pins the strict writer's
// success: nil only after the barrier ran, once, on the destination.
func TestWriteBytesDurable_SyncsParentDirectoryOnce(t *testing.T) {
	seen := stubSyncParentDir(t, nil)

	dst := filepath.Join(t.TempDir(), "reserved.json")
	if err := WriteBytesDurable(dst, []byte("reserved"), ".durable-*.tmp"); err != nil {
		t.Fatalf("WriteBytesDurable: %v", err)
	}
	if len(*seen) != 1 || (*seen)[0] != dst {
		t.Fatalf("parent-dir sync calls = %v, want exactly one, for %q", *seen, dst)
	}
	if got, err := os.ReadFile(dst); err != nil || string(got) != "reserved" {
		t.Fatalf("destination %q, %v", got, err)
	}
}

// TestWriteBytesDurable_RaceWinnerStillNeedsTheBarrier pins the fallback a
// failed rename takes when the destination already holds the same bytes:
// WriteBytes accepts it as the winner's write, while the strict writer
// accepts it only once the directory entry is durable, since the winner's
// barrier may have been tolerated.
func TestWriteBytesDurable_RaceWinnerStillNeedsTheBarrier(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "reserved.json")
	want := []byte("same bytes")
	if err := os.WriteFile(dst, want, 0o600); err != nil {
		t.Fatal(err)
	}
	prev := SetRenameFuncForTest(func(src, dst string) error { return os.ErrPermission })
	t.Cleanup(func() { SetRenameFuncForTest(prev) })

	syncErr := errors.New("fsync: input/output error")
	stubSyncParentDir(t, syncErr)
	if err := WriteBytesDurable(dst, want, ".durable-*.tmp"); !errors.Is(err, syncErr) {
		t.Fatalf("WriteBytesDurable over the winner's bytes with a failing barrier = %v, want the barrier's failure", err)
	}
	seen := stubSyncParentDir(t, nil)
	if err := WriteBytesDurable(dst, want, ".durable-*.tmp"); err != nil || len(*seen) != 1 {
		t.Fatalf("WriteBytesDurable over the winner's bytes = %v with %d barriers, want nil after one", err, len(*seen))
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
