package atomicwrite

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
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

// TestWriteBytesDurable_MakesTheDirectoriesItCreatesDurable pins the
// directories the strict writer creates for a destination whose parents
// are missing: each is made durable in its parent, the deepest first,
// before the file is written, so a crash cannot take a durable file's
// directory with it; and a barrier that fails for one of them fails the
// write before the file is written.
func TestWriteBytesDurable_MakesTheDirectoriesItCreatesDurable(t *testing.T) {
	seen := stubSyncParentDir(t, nil)
	root := t.TempDir()
	dst := filepath.Join(root, "a", "b", "reserved.json")
	if err := WriteBytesDurable(dst, []byte("reserved"), ".durable-*.tmp"); err != nil {
		t.Fatalf("WriteBytesDurable: %v", err)
	}
	var synced []string
	for _, p := range *seen {
		synced = append(synced, filepath.Dir(p))
	}
	if want := []string{filepath.Join(root, "a"), root, filepath.Join(root, "a", "b")}; !slices.Equal(synced, want) {
		t.Fatalf("directories synced %v, want %v: b's entry in a, then a's in the root, then the file's in b", synced, want)
	}

	syncErr := errors.New("fsync: input/output error")
	stubSyncParentDir(t, syncErr)
	other := filepath.Join(root, "c", "reserved.json")
	if err := WriteBytesDurable(other, []byte("reserved"), ".durable-*.tmp"); !errors.Is(err, syncErr) {
		t.Fatalf("WriteBytesDurable with the barrier failing for the directory it created = %v, want the barrier's failure", err)
	}
	if _, err := os.Lstat(other); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the write whose directory is not durable wrote %q (%v), want nothing", other, err)
	}
}

// TestWriteBytesDurable_RetriesTheBarrierOfADirectoryItCreated pins a
// directory whose barrier failed: the write that created it fails and
// leaves it where it is, and every later write below it runs that barrier
// again before it counts the file durable, rather than finding the
// directory there and taking it for one that was always durable, until the
// barrier succeeds; from then on the directory is one that was there. While
// the first barrier fails another writer puts a file in the directory, so
// no removal could have made the next write create it again.
func TestWriteBytesDurable_RetriesTheBarrierOfADirectoryItCreated(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "a")
	dst := filepath.Join(dir, "reserved.json")
	theirs := filepath.Join(dir, "theirs")
	syncErr := errors.New("fsync: input/output error")
	// seen is the paths the barrier is asked for. The one for a's entry in
	// the root fails twice, and the first time another writer puts a file
	// in a before it answers.
	var seen []string
	fails := 0
	prev := syncParentDir
	t.Cleanup(func() { syncParentDir = prev })
	syncParentDir = func(path string) error {
		seen = append(seen, path)
		if path != dir || fails == 2 {
			return nil
		}
		if fails++; fails == 1 {
			if err := os.WriteFile(theirs, []byte("theirs"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return syncErr
	}
	for write := 1; write <= 2; write++ {
		seen = nil
		if err := WriteBytesDurable(dst, []byte("reserved"), ".durable-*.tmp"); !errors.Is(err, syncErr) {
			t.Fatalf("write %d = %v with the barrier for a's entry failing, want its failure; it ran the barrier for %v", write, err, seen)
		}
	}
	if got, err := os.ReadFile(theirs); err != nil || string(got) != "theirs" {
		t.Fatalf("the other writer's file in a: %q, %v", got, err)
	}
	seen = nil
	if err := WriteBytesDurable(dst, []byte("reserved"), ".durable-*.tmp"); err != nil {
		t.Fatalf("the write once a's barrier answers: %v", err)
	}
	if !slices.Contains(seen, dir) {
		t.Fatalf("the write after the failures ran the barrier for %v: never for a's entry in the root, whose barrier had failed", seen)
	}
	seen = nil
	if err := WriteBytesDurable(dst, []byte("again"), ".durable-*.tmp"); err != nil {
		t.Fatalf("the write after a's barrier succeeded: %v", err)
	}
	if !slices.Equal(seen, []string{dst}) {
		t.Fatalf("the write after a's barrier succeeded ran the barrier for %v, want the file's entry alone", seen)
	}
	if got, err := os.ReadFile(dst); err != nil || string(got) != "again" {
		t.Fatalf("destination %q, %v", got, err)
	}
}

// TestWriteBytesDurable_RemovesNothingAnotherWriterMade pins a strict write
// that fails while another writer works in the directories it creates: it
// removes nothing. Through the beforeMkdirAll seam the other writer acts
// between the strict write's look for what is missing and its creation: a
// directory it makes there survives the strict write's failed barrier, and
// it goes on writing into it; a regular file it makes at the path is never
// touched, though the strict write then fails.
func TestWriteBytesDurable_RemovesNothingAnotherWriterMade(t *testing.T) {
	inTheWindow := func(t *testing.T, act func()) {
		prev := beforeMkdirAll
		t.Cleanup(func() { beforeMkdirAll = prev })
		beforeMkdirAll = func(string) { act() }
	}
	t.Run("a directory", func(t *testing.T) {
		syncErr := errors.New("fsync: input/output error")
		stubSyncParentDir(t, syncErr)
		dir := filepath.Join(t.TempDir(), "a")
		inTheWindow(t, func() {
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
		})
		if err := WriteBytesDurable(filepath.Join(dir, "reserved.json"), []byte("reserved"), ".durable-*.tmp"); !errors.Is(err, syncErr) {
			t.Fatalf("WriteBytesDurable with the barrier failing = %v, want its failure", err)
		}
		if info, err := os.Lstat(dir); err != nil || !info.IsDir() {
			t.Fatalf("the directory the other writer made is gone after the strict write failed (%v)", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "theirs"), []byte("theirs"), 0o600); err != nil {
			t.Fatalf("the other writer's write into its directory: %v", err)
		}
	})
	t.Run("a file at the path", func(t *testing.T) {
		stubSyncParentDir(t, nil)
		path := filepath.Join(t.TempDir(), "a")
		inTheWindow(t, func() {
			if err := os.WriteFile(path, []byte("theirs"), 0o600); err != nil {
				t.Fatal(err)
			}
		})
		if err := WriteBytesDurable(filepath.Join(path, "reserved.json"), []byte("reserved"), ".durable-*.tmp"); err == nil {
			t.Fatal("WriteBytesDurable below a regular file answered nil")
		}
		if got, err := os.ReadFile(path); err != nil || string(got) != "theirs" {
			t.Fatalf("the other writer's file at the path: %q, %v, want it untouched", got, err)
		}
	})
}

// TestProbeDurable pins the probe a caller runs before it counts on
// WriteBytesDurable: the barrier on the destination's directory, or, while
// that directory does not exist, on the nearest one above it that does,
// creating nothing; the barrier's failure, naming the directory; and
// ErrNoDirectoryBarrier on a platform with none.
func TestProbeDurable(t *testing.T) {
	root := t.TempDir()
	seen := stubSyncParentDir(t, nil)
	synced := func() []string {
		var out []string
		for _, p := range *seen {
			out = append(out, filepath.Dir(p))
		}
		*seen = nil
		return out
	}
	if err := ProbeDurable(filepath.Join(root, "reserved.json")); err != nil {
		t.Fatalf("ProbeDurable of a directory that syncs: %v", err)
	}
	if got := synced(); !slices.Equal(got, []string{root}) {
		t.Fatalf("synced %v, want the destination's directory %s", got, root)
	}
	if err := ProbeDurable(filepath.Join(root, "a", "b", "reserved.json")); err != nil {
		t.Fatalf("ProbeDurable below a missing directory: %v", err)
	}
	if got := synced(); !slices.Equal(got, []string{root}) {
		t.Fatalf("below a missing directory synced %v, want the nearest that exists, %s", got, root)
	}
	if _, err := os.Lstat(filepath.Join(root, "a")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the probe created %s (%v)", filepath.Join(root, "a"), err)
	}

	// A regular file where the directory should be is no directory, though
	// the barrier, which opens and fsyncs whatever is there, answers nil for
	// it; and below it too, where Stat's own error stands.
	file := filepath.Join(root, "existing-file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{filepath.Join(file, "reserved.json"), filepath.Join(file, "sub", "reserved.json")} {
		if err := ProbeDurable(p); !errors.Is(err, syscall.ENOTDIR) || !strings.Contains(err.Error(), file) {
			t.Fatalf("ProbeDurable(%s) = %v, want ENOTDIR naming %s", p, err, file)
		}
	}
	synced()

	syncErr := errors.New("sync dir: invalid argument")
	stubSyncParentDir(t, syncErr)
	if err := ProbeDurable(filepath.Join(root, "reserved.json")); !errors.Is(err, syncErr) || !strings.Contains(err.Error(), root) {
		t.Fatalf("ProbeDurable with a failing barrier = %v, want its failure, naming %s", err, root)
	}
	noDirectoryBarrier(t)
	if err := ProbeDurable(filepath.Join(root, "reserved.json")); !errors.Is(err, ErrNoDirectoryBarrier) {
		t.Fatalf("ProbeDurable with no directory barrier = %v, want ErrNoDirectoryBarrier", err)
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
// accepts it only once it has fsynced the destination, through the open it
// read the bytes from, and then the directory entry naming it: matching
// bytes prove the winner's write visible, not durable, and the winner's own
// barrier may have been tolerated. Other bytes are the rename's failure,
// and nothing is synced for them.
func TestWriteBytesDurable_RaceWinnerStillNeedsTheBarrier(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "reserved.json")
	want := []byte("same bytes")
	if err := os.WriteFile(dst, want, 0o600); err != nil {
		t.Fatal(err)
	}
	prev := SetRenameFuncForTest(func(src, dst string) error { return os.ErrPermission })
	t.Cleanup(func() { SetRenameFuncForTest(prev) })

	// syncs is every file and directory sync after the staged file's, in
	// order; fileErr fails the destination's, dirErr the barrier.
	var syncs []string
	var fileErr, dirErr error
	prevFile, prevDir := syncFile, syncParentDir
	t.Cleanup(func() { syncFile, syncParentDir = prevFile, prevDir })
	syncFile = func(f *os.File) error {
		if f.Name() != dst {
			return f.Sync() // the staged file
		}
		syncs = append(syncs, "file "+f.Name())
		return fileErr
	}
	syncParentDir = func(path string) error {
		syncs = append(syncs, "dir "+path)
		return dirErr
	}
	syncErr := errors.New("fsync: input/output error")
	for _, tc := range []struct {
		name            string
		data            []byte
		fileErr, dirErr error
		want            error
		syncs           []string
	}{
		{"the destination, then its entry", want, nil, nil, nil, []string{"file " + dst, "dir " + dst}},
		{"a destination that cannot be synced", want, syncErr, nil, syncErr, []string{"file " + dst}},
		{"an entry that cannot be synced", want, nil, syncErr, syncErr, []string{"file " + dst, "dir " + dst}},
		{"other bytes", []byte("other bytes"), nil, nil, os.ErrPermission, nil},
	} {
		syncs, fileErr, dirErr = nil, tc.fileErr, tc.dirErr
		err := WriteBytesDurable(dst, tc.data, ".durable-*.tmp")
		if (tc.want == nil && err != nil) || (tc.want != nil && !errors.Is(err, tc.want)) {
			t.Errorf("%s: WriteBytesDurable over the winner's bytes = %v, want %v", tc.name, err, tc.want)
		}
		if !slices.Equal(syncs, tc.syncs) {
			t.Errorf("%s: synced %v, want %v", tc.name, syncs, tc.syncs)
		}
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
