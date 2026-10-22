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

// stubSyncDir installs a recording stub over the strict barrier, the fsync
// of a directory the strict writer or the probe opened (syncDir), and
// restores it on cleanup. It returns the names of the directories fsynced,
// in call order, each answered with ret. Callers MUST NOT call
// `t.Parallel()`, as with stubSyncParentDir.
func stubSyncDir(t *testing.T, ret error) *[]string {
	t.Helper()
	var seen []string
	prev := syncDir
	syncDir = func(d *os.File) error {
		seen = append(seen, d.Name())
		return ret
	}
	t.Cleanup(func() { syncDir = prev })
	return &seen
}

// noDirectoryBarrier sets both barrier seams to a platform with no
// directory barrier (Windows, where fsutil.SyncsParentDir is false) and
// restores them on cleanup. Callers MUST NOT call `t.Parallel()`, as with
// stubSyncParentDir.
func noDirectoryBarrier(t *testing.T) {
	t.Helper()
	prevParent, prevDir := syncParentDir, syncDir
	syncParentDir, syncDir = nil, nil
	t.Cleanup(func() { syncParentDir, syncDir = prevParent, prevDir })
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
	err := WriteBytesDurable(root, dst, []byte("reserved"), ".durable-*.tmp")
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
	root := t.TempDir()
	dst := filepath.Join(root, "reserved.json")
	err := WriteBytesDurable(root, dst, []byte("reserved"), ".durable-*.tmp")
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
	synced := stubSyncDir(t, nil)
	root := realTempDir(t)
	dst := filepath.Join(root, "a", "b", "reserved.json")
	if err := WriteBytesDurable(root, dst, []byte("reserved"), ".durable-*.tmp"); err != nil {
		t.Fatalf("WriteBytesDurable: %v", err)
	}
	if want := []string{filepath.Join(root, "a"), root, filepath.Join(root, "a", "b")}; !slices.Equal(*synced, want) {
		t.Fatalf("directories synced %v, want %v: b's entry in a, then a's in the root, then the file's in b", *synced, want)
	}

	syncErr := errors.New("fsync: input/output error")
	stubSyncDir(t, syncErr)
	other := filepath.Join(root, "c", "reserved.json")
	if err := WriteBytesDurable(root, other, []byte("reserved"), ".durable-*.tmp"); !errors.Is(err, syncErr) {
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
// directory there and taking it for one that was always durable, a write
// after one whose barrier succeeded included: the writer keeps no memory of
// what it synced, and syncs every entry between the file and the root each
// time. While the first barrier fails another writer puts a file in the
// directory, so no removal could have made the next write create it again.
func TestWriteBytesDurable_RetriesTheBarrierOfADirectoryItCreated(t *testing.T) {
	root := realTempDir(t)
	dir := filepath.Join(root, "a")
	dst := filepath.Join(dir, "reserved.json")
	theirs := filepath.Join(dir, "theirs")
	syncErr := errors.New("fsync: input/output error")
	// seen is the directories fsynced. The root's, which makes a's entry
	// durable, fails twice, and the first time another writer puts a file in
	// a before it answers.
	var seen []string
	fails := 0
	prev := syncDir
	t.Cleanup(func() { syncDir = prev })
	syncDir = func(d *os.File) error {
		seen = append(seen, d.Name())
		if d.Name() != root || fails == 2 {
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
		if err := WriteBytesDurable(root, dst, []byte("reserved"), ".durable-*.tmp"); !errors.Is(err, syncErr) {
			t.Fatalf("write %d = %v with the barrier for a's entry failing, want its failure; it ran the barrier for %v", write, err, seen)
		}
	}
	if got, err := os.ReadFile(theirs); err != nil || string(got) != "theirs" {
		t.Fatalf("the other writer's file in a: %q, %v", got, err)
	}
	seen = nil
	if err := WriteBytesDurable(root, dst, []byte("reserved"), ".durable-*.tmp"); err != nil {
		t.Fatalf("the write once a's barrier answers: %v", err)
	}
	if !slices.Contains(seen, root) {
		t.Fatalf("the write after the failures fsynced %v: never the root, for a's entry, whose barrier had failed", seen)
	}
	seen = nil
	if err := WriteBytesDurable(root, dst, []byte("again"), ".durable-*.tmp"); err != nil {
		t.Fatalf("the write after a's barrier succeeded: %v", err)
	}
	if want := []string{root, dir}; !slices.Equal(seen, want) {
		t.Fatalf("the write after a's barrier succeeded fsynced %v, want %v: a's entry again, then the file's", seen, want)
	}
	if got, err := os.ReadFile(dst); err != nil || string(got) != "again" {
		t.Fatalf("destination %q, %v", got, err)
	}
}

// TestWriteBytesDurable_RemovesNothingAnotherWriterMade pins a strict write
// that fails while another writer works in the directories it creates: it
// removes nothing. Through the beforeMkdirAll seam the other writer acts
// between the strict write's checks of the path and its creation: a
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
		stubSyncDir(t, syncErr)
		root := t.TempDir()
		dir := filepath.Join(root, "a")
		inTheWindow(t, func() {
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
		})
		if err := WriteBytesDurable(root, filepath.Join(dir, "reserved.json"), []byte("reserved"), ".durable-*.tmp"); !errors.Is(err, syncErr) {
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
		stubSyncDir(t, nil)
		root := t.TempDir()
		path := filepath.Join(root, "a")
		inTheWindow(t, func() {
			if err := os.WriteFile(path, []byte("theirs"), 0o600); err != nil {
				t.Fatal(err)
			}
		})
		if err := WriteBytesDurable(root, filepath.Join(path, "reserved.json"), []byte("reserved"), ".durable-*.tmp"); err == nil {
			t.Fatal("WriteBytesDurable below a regular file answered nil")
		}
		if got, err := os.ReadFile(path); err != nil || string(got) != "theirs" {
			t.Fatalf("the other writer's file at the path: %q, %v, want it untouched", got, err)
		}
	})
}

// TestWriteBytesDurable_AFreshWriterSyncsAnEntryAnEarlierRunLeft pins the
// restart round 5's process-local record missed: directories an earlier run
// created, whose entries it never made durable before it exited (its barrier
// failed, then it was killed or stopped), are found by the next run's first
// write, which must sync their entries before it counts the file durable.
// Nothing records them, as nothing survives a restart: the writer syncs
// every entry between the file and the root on every write.
func TestWriteBytesDurable_AFreshWriterSyncsAnEntryAnEarlierRunLeft(t *testing.T) {
	root := realTempDir(t)
	dir := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(dir, 0o700); err != nil { // the earlier run's
		t.Fatal(err)
	}
	synced := stubSyncDir(t, nil)
	if err := WriteBytesDurable(root, filepath.Join(dir, "reserved.json"), []byte("reserved"), ".durable-*.tmp"); err != nil {
		t.Fatalf("WriteBytesDurable: %v", err)
	}
	if want := []string{filepath.Join(root, "a"), root, dir}; !slices.Equal(*synced, want) {
		t.Fatalf("a fresh writer fsynced %v, want %v: b's entry in a and a's in the root, which an earlier run left, then the file's", *synced, want)
	}
}

// TestWriteBytesDurable_RetriesThroughASymlinkedAlias pins a retry that
// names the directory through a symbolic link: a write to root/b/real fails
// at the barrier for real's entry in b, and the retry through root/x/alias,
// a link to root/b/real, is refused (ErrNotUnderRoot) with nothing fsynced.
// real's entry is in b and the alias's in x, and round 6, which walked the
// path as written and as it resolved to reach both, still missed a link
// reached through another link, so a link strictly below the root is
// refused, never walked (TestWriteBytesDurable_RefusesALinkBelowTheRoot).
// The retry by real's own path syncs real's entry in b again, then b's in
// the root, then the file's.
func TestWriteBytesDurable_RetriesThroughASymlinkedAlias(t *testing.T) {
	root := realTempDir(t)
	b, x := filepath.Join(root, "b"), filepath.Join(root, "x")
	real, alias := filepath.Join(b, "real"), filepath.Join(x, "alias")
	if err := os.Mkdir(x, 0o700); err != nil {
		t.Fatal(err)
	}
	syncErr := errors.New("fsync: input/output error")
	var synced []string
	failB := true
	prev := syncDir
	t.Cleanup(func() { syncDir = prev })
	syncDir = func(d *os.File) error {
		synced = append(synced, d.Name())
		if failB && d.Name() == b {
			return syncErr
		}
		return nil
	}
	if err := WriteBytesDurable(root, filepath.Join(real, "reserved.json"), []byte("reserved"), ".durable-*.tmp"); !errors.Is(err, syncErr) {
		t.Fatalf("the write whose barrier for real's entry in b failed = %v, want its failure", err)
	}
	symlink(t, real, alias)
	failB, synced = false, nil
	if err := WriteBytesDurable(root, filepath.Join(alias, "reserved.json"), []byte("reserved"), ".durable-*.tmp"); !errors.Is(err, ErrNotUnderRoot) || len(synced) != 0 {
		t.Fatalf("the retry through the alias = %v after fsyncing %v, want ErrNotUnderRoot and nothing fsynced", err, synced)
	}
	if err := WriteBytesDurable(root, filepath.Join(real, "reserved.json"), []byte("reserved"), ".durable-*.tmp"); err != nil {
		t.Fatalf("the retry by real's own path: %v", err)
	}
	if want := []string{b, root, real}; !slices.Equal(synced, want) {
		t.Fatalf("the retry by real's own path fsynced %v, want %v: real's entry in b, b's in the root, then the file's", synced, want)
	}
}

// TestWriteBytesDurable_NeverSyncsAboveTheRoot pins the root's bound: a
// write fsyncs the root, for the entry of the directory below it, and never
// the root's parent, whose entry for the root is the caller's to vouch for.
// And it refuses, with nothing created, written or fsynced: a root that does
// not exist, which the writer never creates; a path not below the root as
// written, the path or the root spelled through a link to the other
// included; and a path through a symbolic link below the root, here one
// that leads out of it.
func TestWriteBytesDurable_NeverSyncsAboveTheRoot(t *testing.T) {
	base := realTempDir(t)
	root := filepath.Join(base, "root")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	synced := stubSyncDir(t, nil)
	write := func(root, path string) error {
		return WriteBytesDurable(root, path, []byte("reserved"), ".durable-*.tmp")
	}
	if err := write(root, filepath.Join(root, "a", "b", "reserved.json")); err != nil {
		t.Fatalf("a write below the root: %v", err)
	}
	if want := []string{filepath.Join(root, "a"), root, filepath.Join(root, "a", "b")}; !slices.Equal(*synced, want) {
		t.Fatalf("a write below the root fsynced %v, want %v: up to the root, and never its parent %s", *synced, want, base)
	}
	type refusal struct {
		name, root, path, untouched string
		want                        error
	}
	refusals := []refusal{
		{"a root that does not exist", filepath.Join(base, "missing"), filepath.Join(base, "missing", "reserved.json"), filepath.Join(base, "missing"), fs.ErrNotExist},
		{"a path not below the root", root, filepath.Join(base, "elsewhere", "reserved.json"), filepath.Join(base, "elsewhere"), ErrNotUnderRoot},
	}
	out := filepath.Join(base, "out")
	if err := os.Mkdir(out, 0o700); err != nil {
		t.Fatal(err)
	}
	if link := filepath.Join(root, "link"); os.Symlink(out, link) == nil {
		refusals = append(refusals, refusal{"a path a link below the root leads out of", root, filepath.Join(link, "new", "reserved.json"), filepath.Join(out, "new"), ErrNotUnderRoot})
	} else {
		t.Log("no symbolic link here: the cases of links are not run")
	}
	// The root and the path spelled one through a link to the other: below
	// the root as they resolve, not as written, so the walk up from the path
	// as written would never meet the root. Refused before anything is made.
	if alias := filepath.Join(base, "root-alias"); os.Symlink(root, alias) == nil {
		refusals = append(refusals,
			refusal{"a path spelled through a link to the root", root, filepath.Join(alias, "new", "reserved.json"), filepath.Join(root, "new"), ErrNotUnderRoot},
			refusal{"a root spelled through a link to it", alias, filepath.Join(root, "new", "reserved.json"), filepath.Join(root, "new"), ErrNotUnderRoot})
	}
	for _, tc := range refusals {
		*synced = nil
		if err := write(tc.root, tc.path); !errors.Is(err, tc.want) {
			t.Errorf("%s: WriteBytesDurable = %v, want %v", tc.name, err, tc.want)
		}
		if _, err := os.Lstat(tc.untouched); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s: the refused write left %s (%v)", tc.name, tc.untouched, err)
		}
		if len(*synced) != 0 {
			t.Errorf("%s: the refused write fsynced %v", tc.name, *synced)
		}
	}
}

// TestProbeDurable pins the probe a caller runs before it counts on
// WriteBytesDurable: the barrier on every directory a write would fsync that
// exists now, from the destination's directory, or, while that does not
// exist, the nearest one above it that does, up to the root, creating
// nothing; the barrier's failure, naming the directory; and
// ErrNoDirectoryBarrier on a platform with none.
func TestProbeDurable(t *testing.T) {
	root := realTempDir(t)
	seen := stubSyncDir(t, nil)
	synced := func() []string {
		out := slices.Clone(*seen)
		*seen = nil
		return out
	}
	if err := ProbeDurable(root, filepath.Join(root, "reserved.json")); err != nil {
		t.Fatalf("ProbeDurable of a directory that syncs: %v", err)
	}
	if got := synced(); !slices.Equal(got, []string{root}) {
		t.Fatalf("synced %v, want the destination's directory %s", got, root)
	}
	if err := ProbeDurable(root, filepath.Join(root, "a", "b", "reserved.json")); err != nil {
		t.Fatalf("ProbeDurable below a missing directory: %v", err)
	}
	if got := synced(); !slices.Equal(got, []string{root}) {
		t.Fatalf("below a missing directory synced %v, want the nearest that exists, %s", got, root)
	}
	if _, err := os.Lstat(filepath.Join(root, "a")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the probe created %s (%v)", filepath.Join(root, "a"), err)
	}
	if err := os.MkdirAll(filepath.Join(root, "a", "b"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := ProbeDurable(root, filepath.Join(root, "a", "b", "reserved.json")); err != nil {
		t.Fatalf("ProbeDurable below existing directories: %v", err)
	}
	if got, want := synced(), []string{filepath.Join(root, "a", "b"), filepath.Join(root, "a"), root}; !slices.Equal(got, want) {
		t.Fatalf("below existing directories synced %v, want %v: each a write fsyncs, up to the root", got, want)
	}

	// A regular file where the directory should be is no directory, though
	// the barrier, which opens and fsyncs whatever is there, answers nil for
	// it; and below it too, where Stat's own error stands.
	file := filepath.Join(root, "existing-file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{filepath.Join(file, "reserved.json"), filepath.Join(file, "sub", "reserved.json")} {
		if err := ProbeDurable(root, p); !errors.Is(err, syscall.ENOTDIR) || !strings.Contains(err.Error(), file) {
			t.Fatalf("ProbeDurable(%s) = %v, want ENOTDIR naming %s", p, err, file)
		}
	}
	synced()

	syncErr := errors.New("invalid argument")
	stubSyncDir(t, syncErr)
	if err := ProbeDurable(root, filepath.Join(root, "reserved.json")); !errors.Is(err, syncErr) || !strings.Contains(err.Error(), root) {
		t.Fatalf("ProbeDurable with a failing barrier = %v, want its failure, naming %s", err, root)
	}
	noDirectoryBarrier(t)
	if err := ProbeDurable(root, filepath.Join(root, "reserved.json")); !errors.Is(err, ErrNoDirectoryBarrier) {
		t.Fatalf("ProbeDurable with no directory barrier = %v, want ErrNoDirectoryBarrier", err)
	}
}

// TestBarrierFailuresAreMarked pins ErrBarrierFailed: in what ProbeDurable
// and WriteBytesDurable return it marks a directory fsync that failed, for
// the file's entry and for a directory the write created, beside the
// fsync's own error, as an *fs.PathError naming the directory that would not
// sync, though the barrier here answers a bare error; it marks nothing else,
// neither a regular file where the directory should be nor a platform with
// no barrier (nor an open that failed:
// TestProbeDurable_SyncsTheDirectoryItOpened and the two
// unreadable-directory tests).
func TestBarrierFailuresAreMarked(t *testing.T) {
	root := realTempDir(t)
	syncErr := errors.New("invalid argument")
	stubSyncDir(t, syncErr)
	for name, err := range map[string]error{
		"the probe":                     ProbeDurable(root, filepath.Join(root, "reserved.json")),
		"the write's entry":             WriteBytesDurable(root, filepath.Join(root, "reserved.json"), []byte("reserved"), ".durable-*.tmp"),
		"a directory the write created": WriteBytesDurable(root, filepath.Join(root, "a", "reserved.json"), []byte("reserved"), ".durable-*.tmp"),
	} {
		if !errors.Is(err, ErrBarrierFailed) || !errors.Is(err, syncErr) {
			t.Errorf("%s with the barrier failing = %v, want its failure marked ErrBarrierFailed", name, err)
		}
		if pe := (*fs.PathError)(nil); !errors.As(err, &pe) || pe.Path != root {
			t.Errorf("%s with the barrier failing = %v, want an *fs.PathError naming %s, the directory that would not sync", name, err, root)
		}
	}
	file := filepath.Join(root, "existing-file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ProbeDurable(root, filepath.Join(file, "reserved.json")); !errors.Is(err, syscall.ENOTDIR) || errors.Is(err, ErrBarrierFailed) {
		t.Errorf("ProbeDurable below a regular file = %v, want ENOTDIR, unmarked", err)
	}
	noDirectoryBarrier(t)
	for name, err := range map[string]error{
		"the probe": ProbeDurable(root, filepath.Join(root, "reserved.json")),
		"the write": WriteBytesDurable(root, filepath.Join(root, "reserved.json"), []byte("reserved"), ".durable-*.tmp"),
	} {
		if !errors.Is(err, ErrNoDirectoryBarrier) || errors.Is(err, ErrBarrierFailed) {
			t.Errorf("%s with no directory barrier = %v, want ErrNoDirectoryBarrier, unmarked", name, err)
		}
	}
}

// TestProbeDurable_AnUnreadableDirectoryIsNoBarrierFailure pins the probe
// on a directory this process cannot read: the error is the one reading it
// gave, a permission error, never marked ErrBarrierFailed, whose caller
// would advise moving to a filesystem that can sync a directory. A
// directory it can stat and not open (searchable, not readable) is refused
// by the probe's own open, before any fsync, which here would answer nil;
// one below a directory it cannot search is refused by Stat. POSIX
// permissions as a user other than root; Windows and root skip.
func TestProbeDurable_AnUnreadableDirectoryIsNoBarrierFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits")
	}
	if os.Geteuid() == 0 {
		t.Skip("root reads and searches every directory")
	}
	stubSyncDir(t, nil)
	root := t.TempDir()
	closed := filepath.Join(root, "closed")
	locked := filepath.Join(root, "locked")
	for _, d := range []string{closed, filepath.Join(locked, "dir")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for d, mode := range map[string]os.FileMode{closed: 0o300, locked: 0} {
		if err := os.Chmod(d, mode); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(d, 0o700) })
	}
	for name, path := range map[string]string{
		"a directory it can stat and not open": filepath.Join(closed, "reserved.json"),
		"a directory it cannot stat":           filepath.Join(locked, "dir", "reserved.json"),
	} {
		if err := ProbeDurable(root, path); !errors.Is(err, fs.ErrPermission) || errors.Is(err, ErrBarrierFailed) {
			t.Errorf("ProbeDurable of %s = %v, want its permission error, unmarked", name, err)
		}
	}
}

// TestProbeDurable_SyncsTheDirectoryItOpened pins the probe's one open: it
// fsyncs the descriptor it opened, through the strict barrier (syncDir), and
// opens nothing a second time to sync it, so no failure of such an open (an
// EMFILE, an EACCES, an EIO) can be taken for the filesystem refusing to
// sync. Here the best-effort barrier, which opens the directory again, fails
// as a second open would; the probe never asks it. Only the fsync's own
// failure is marked ErrBarrierFailed.
func TestProbeDurable_SyncsTheDirectoryItOpened(t *testing.T) {
	root := realTempDir(t)
	secondOpen := stubSyncParentDir(t, &fs.PathError{Op: "open", Path: root, Err: syscall.EMFILE})
	var synced []string
	var syncErr error
	prev := syncDir
	t.Cleanup(func() { syncDir = prev })
	syncDir = func(d *os.File) error {
		synced = append(synced, d.Name())
		return syncErr
	}
	if err := ProbeDurable(root, filepath.Join(root, "reserved.json")); err != nil {
		t.Fatalf("ProbeDurable = %v, want nil: it fsyncs the descriptor it opened", err)
	}
	if !slices.Equal(synced, []string{root}) || len(*secondOpen) != 0 {
		t.Fatalf("the probe fsynced %v through its own descriptor and asked a second open for %v, want %s and nothing", synced, *secondOpen, root)
	}
	syncErr = errors.New("invalid argument")
	if err := ProbeDurable(root, filepath.Join(root, "reserved.json")); !errors.Is(err, ErrBarrierFailed) || !errors.Is(err, syncErr) {
		t.Fatalf("ProbeDurable with the fsync failing = %v, want the fsync's failure marked ErrBarrierFailed", err)
	}
}

// TestWriteBytesDurable_AnUnreadableDirectoryIsNoBarrierFailure pins a
// strict write into a directory this process can write and search but not
// read: the rename lands, and the barrier on the file's entry cannot open
// the directory, so the write fails with that open's permission error,
// never marked ErrBarrierFailed, since no fsync ran. POSIX permissions as a
// user other than root; Windows and root skip.
func TestWriteBytesDurable_AnUnreadableDirectoryIsNoBarrierFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits")
	}
	if os.Geteuid() == 0 {
		t.Skip("root reads every directory")
	}
	root := t.TempDir()
	closed := filepath.Join(root, "closed")
	if err := os.Mkdir(closed, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(closed, 0o300); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(closed, 0o700) })
	err := WriteBytesDurable(root, filepath.Join(closed, "reserved.json"), []byte("reserved"), ".durable-*.tmp")
	if !errors.Is(err, fs.ErrPermission) || errors.Is(err, ErrBarrierFailed) {
		t.Fatalf("WriteBytesDurable into a directory it cannot read = %v, want the open's permission error, unmarked", err)
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
	seen := stubSyncDir(t, syncErr)

	root := realTempDir(t)
	dst := filepath.Join(root, "reserved.json")
	err := WriteBytesDurable(root, dst, []byte("reserved"), ".durable-*.tmp")
	if !errors.Is(err, syncErr) {
		t.Fatalf("WriteBytesDurable = %v, want the barrier's failure", err)
	}
	if len(*seen) != 1 || (*seen)[0] != filepath.Dir(dst) {
		t.Fatalf("directory fsyncs = %v, want exactly one, of %q", *seen, filepath.Dir(dst))
	}
}

// TestWriteBytesDurable_SyncsParentDirectoryOnce pins the strict writer's
// success: nil only after the barrier ran, once, on the destination's
// directory.
func TestWriteBytesDurable_SyncsParentDirectoryOnce(t *testing.T) {
	seen := stubSyncDir(t, nil)

	root := realTempDir(t)
	dst := filepath.Join(root, "reserved.json")
	if err := WriteBytesDurable(root, dst, []byte("reserved"), ".durable-*.tmp"); err != nil {
		t.Fatalf("WriteBytesDurable: %v", err)
	}
	if len(*seen) != 1 || (*seen)[0] != filepath.Dir(dst) {
		t.Fatalf("directory fsyncs = %v, want exactly one, of %q", *seen, filepath.Dir(dst))
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
	root := realTempDir(t)
	dst := filepath.Join(root, "reserved.json")
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
	prevFile, prevDir := syncFile, syncDir
	t.Cleanup(func() { syncFile, syncDir = prevFile, prevDir })
	syncFile = func(f *os.File) error {
		if f.Name() != dst {
			return f.Sync() // the staged file
		}
		syncs = append(syncs, "file "+f.Name())
		return fileErr
	}
	syncDir = func(d *os.File) error {
		syncs = append(syncs, "dir "+d.Name())
		return dirErr
	}
	dir := filepath.Dir(dst)
	syncErr := errors.New("fsync: input/output error")
	for _, tc := range []struct {
		name            string
		data            []byte
		fileErr, dirErr error
		want            error
		syncs           []string
	}{
		{"the destination, then its entry", want, nil, nil, nil, []string{"file " + dst, "dir " + dir}},
		{"a destination that cannot be synced", want, syncErr, nil, syncErr, []string{"file " + dst}},
		{"an entry that cannot be synced", want, nil, syncErr, syncErr, []string{"file " + dst, "dir " + dir}},
		{"other bytes", []byte("other bytes"), nil, nil, os.ErrPermission, nil},
	} {
		syncs, fileErr, dirErr = nil, tc.fileErr, tc.dirErr
		err := WriteBytesDurable(root, dst, tc.data, ".durable-*.tmp")
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
