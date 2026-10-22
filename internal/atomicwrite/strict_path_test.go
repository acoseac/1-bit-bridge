package atomicwrite

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

// realTempDir is t.TempDir() with its links resolved. The strict writer
// resolves its root once and spells every path below it from there, so the
// directories it fsyncs are named from the resolved root, and the system's
// temporary directory is a link on macOS.
func realTempDir(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// symlink makes link point at target, or skips the test where links cannot
// be made.
func symlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("no symbolic link here: %v", err)
	}
}

// mkdirs makes each directory and its parents.
func mkdirs(t *testing.T, dirs ...string) {
	t.Helper()
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
}

// refusedPlainly requires err to be ErrNotUnderRoot, nothing to have been
// fsynced, and each of untouched to be missing still.
func refusedPlainly(t *testing.T, what string, err error, synced *[]string, untouched ...string) {
	t.Helper()
	if !errors.Is(err, ErrNotUnderRoot) {
		t.Errorf("%s = %v, want ErrNotUnderRoot", what, err)
	}
	if len(*synced) != 0 {
		t.Errorf("%s fsynced %v before refusing", what, *synced)
	}
	for _, p := range untouched {
		if _, err := os.Lstat(p); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s left %s (%v)", what, p, err)
		}
	}
}

// TestWriteBytesDurable_RefusesADotDotElement pins a path or a root with a
// ".." element, refused before anything is created, written or synced.
// filepath.Abs cleans ".." lexically, and a reader resolves it after the
// link before it: with root/link a link to root/real/sub, round 6 wrote and
// fsynced root/ids for root/link/../ids, which a reader opens as
// root/real/ids, so a restart read no reservation and handed out its IDs
// and tokens again, with no crash at all (Codex on round 6). Refused with no
// link too, and in the root, and by the probe.
func TestWriteBytesDurable_RefusesADotDotElement(t *testing.T) {
	root := realTempDir(t)
	mkdirs(t, filepath.Join(root, "real", "sub"), filepath.Join(root, "a"))
	symlink(t, filepath.Join(root, "real", "sub"), filepath.Join(root, "link"))
	synced := stubSyncDir(t, nil)
	for _, tc := range []struct{ name, root, path string }{
		{"a path through a link and back", root, root + "/link/../ids"},
		{"a path with no link", root, root + "/a/../ids"},
		{"a root with one", root + "/real/..", root + "/ids"},
	} {
		*synced = nil
		err := WriteBytesDurable(tc.root, tc.path, []byte("reserved"), ".durable-*.tmp")
		refusedPlainly(t, tc.name+": WriteBytesDurable", err, synced,
			filepath.Join(root, "ids"), filepath.Join(root, "real", "ids"), filepath.Join(root, "real", "sub", "ids"))
		*synced = nil
		refusedPlainly(t, tc.name+": ProbeDurable", ProbeDurable(tc.root, tc.path), synced)
	}
}

// TestWriteBytesDurable_RefusesAPathThatNamesADirectory pins a path whose
// last element names a directory, a trailing separator or a ".": refused,
// an *fs.PathError wrapping syscall.EISDIR, before anything is created,
// written or synced, by the write and by the probe. filepath.Abs cleans
// root/ids/ and root/ids/. to root/ids, so round 7 wrote the file there,
// while a reader opens the path as it was given, which a regular file
// answers ENOTDIR (Codex on round 7: the engine's restart found its
// reservation stuck). The root may keep a trailing separator: it names a
// directory, which a root is.
func TestWriteBytesDurable_RefusesAPathThatNamesADirectory(t *testing.T) {
	root := realTempDir(t)
	synced := stubSyncDir(t, nil)
	sep := string(filepath.Separator)
	for _, path := range []string{root + sep + "ids" + sep, root + sep + "ids" + sep + ".", root + sep + "sub" + sep + "ids" + sep} {
		*synced = nil
		err := WriteBytesDurable(root, path, []byte("reserved"), ".durable-*.tmp")
		if !errors.Is(err, syscall.EISDIR) || !strings.Contains(err.Error(), path) {
			t.Errorf("WriteBytesDurable(%q) = %v, want EISDIR naming the path", path, err)
		}
		if err := ProbeDurable(root, path); !errors.Is(err, syscall.EISDIR) {
			t.Errorf("ProbeDurable(%q) = %v, want EISDIR", path, err)
		}
		if len(*synced) != 0 {
			t.Errorf("the refusals of %q fsynced %v", path, *synced)
		}
		if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
			t.Errorf("the refusals of %q left %v (%v) in the root", path, entries, err)
		}
	}
	*synced = nil
	if err := WriteBytesDurable(root+sep, filepath.Join(root, "ids"), []byte("reserved"), ".durable-*.tmp"); err != nil {
		t.Fatalf("WriteBytesDurable below a root with a trailing separator: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(root, "ids")); err != nil || string(got) != "reserved" {
		t.Fatalf("the file below a root with a trailing separator: %q, %v", got, err)
	}
}

// TestWriteBytesDurable_RefusesALinkBelowTheRoot pins a path reached through
// a symbolic link strictly below the root: refused before anything is
// created, written or synced, by the write and by the probe, for a link
// whose target is inside the root too. Its target's entry is in the
// target's parent, and the link's own in the link's, which no walk up the
// path reaches whole: round 6 walked the path as written and as it
// resolved, and still missed a link reached through another link (root/a/l1
// to root/b/l2 to root/c/d: root/b, which holds l2's entry, is on neither
// walk) and synced outside the root for a link that leads out and back
// (root/l to out/m, whose y leads back to root/c), the write answering nil
// each time (Codex, the Claude API and Argon on round 6).
func TestWriteBytesDurable_RefusesALinkBelowTheRoot(t *testing.T) {
	base := realTempDir(t)
	root, out := filepath.Join(base, "root"), filepath.Join(base, "out")
	mkdirs(t, filepath.Join(root, "a"), filepath.Join(root, "b"), filepath.Join(root, "c", "d"), filepath.Join(root, "real"), filepath.Join(out, "m"))
	symlink(t, filepath.Join(root, "real"), filepath.Join(root, "link"))
	symlink(t, filepath.Join(root, "c", "d"), filepath.Join(root, "b", "l2"))
	symlink(t, filepath.Join(root, "b", "l2"), filepath.Join(root, "a", "l1"))
	symlink(t, filepath.Join(out, "m"), filepath.Join(root, "l"))
	symlink(t, filepath.Join(root, "c"), filepath.Join(out, "m", "y"))
	synced := stubSyncDir(t, nil)
	for _, tc := range []struct {
		name, path string
		untouched  []string
	}{
		{"a link to a directory below the root", filepath.Join(root, "link", "ids"), []string{filepath.Join(root, "real", "ids")}},
		{"a link to a directory below the root, a directory to make below it", filepath.Join(root, "link", "new", "ids"), []string{filepath.Join(root, "real", "new")}},
		{"a link to a link", filepath.Join(root, "a", "l1", "ids"), []string{filepath.Join(root, "c", "d", "ids")}},
		{"a link out and back", filepath.Join(root, "l", "y", "ids"), []string{filepath.Join(root, "c", "ids")}},
	} {
		*synced = nil
		err := WriteBytesDurable(root, tc.path, []byte("reserved"), ".durable-*.tmp")
		refusedPlainly(t, tc.name+": WriteBytesDurable", err, synced, tc.untouched...)
		*synced = nil
		refusedPlainly(t, tc.name+": ProbeDurable", ProbeDurable(root, tc.path), synced)
	}
}

// TestWriteBytesDurable_RefusesALinkPutInPlaceWhileItWrites pins the check
// the write makes again once it has created the directories it needs, before
// it syncs anything: another writer that puts a symbolic link where a
// directory was missing, between the write's first check and its creation of
// that directory (the beforeMkdirAll seam), has the write refused
// (ErrNotUnderRoot) with nothing fsynced and no file written where the link
// leads, though the directory the write wanted now exists, through the link.
func TestWriteBytesDurable_RefusesALinkPutInPlaceWhileItWrites(t *testing.T) {
	root := realTempDir(t)
	real := filepath.Join(root, "real")
	mkdirs(t, real)
	prev := beforeMkdirAll
	t.Cleanup(func() { beforeMkdirAll = prev })
	beforeMkdirAll = func(string) { symlink(t, real, filepath.Join(root, "a")) }
	synced := stubSyncDir(t, nil)
	err := WriteBytesDurable(root, filepath.Join(root, "a", "ids"), []byte("reserved"), ".durable-*.tmp")
	refusedPlainly(t, "WriteBytesDurable with a link put where a directory was missing", err, synced, filepath.Join(real, "ids"))
}

// TestWriteBytesDurable_TakesALinkAtOrAboveTheRoot pins the layouts that stay
// allowed: a root that is itself a link (some installs make the data
// directory one), and a link above the root (macOS's /var). The root is
// resolved once, and every directory the write fsyncs is spelled from where
// it resolved: the file's directory, then the root, for the entry of the
// directory below it, and never the link's parent, whose entries are the
// caller's. The file reads back through the link.
func TestWriteBytesDurable_TakesALinkAtOrAboveTheRoot(t *testing.T) {
	base := realTempDir(t)
	real, alias := filepath.Join(base, "real"), filepath.Join(base, "alias")
	mkdirs(t, real)
	symlink(t, real, alias)
	synced := stubSyncDir(t, nil)
	path := filepath.Join(alias, "sub", "ids")
	if err := ProbeDurable(alias, path); err != nil {
		t.Fatalf("ProbeDurable below a root that is a link: %v", err)
	}
	if want := []string{real}; !slices.Equal(*synced, want) {
		t.Fatalf("the probe fsynced %v, want %v: the root as it resolves", *synced, want)
	}
	*synced = nil
	if err := WriteBytesDurable(alias, path, []byte("reserved"), ".durable-*.tmp"); err != nil {
		t.Fatalf("WriteBytesDurable below a root that is a link: %v", err)
	}
	if want := []string{real, filepath.Join(real, "sub")}; !slices.Equal(*synced, want) {
		t.Fatalf("the write fsynced %v, want %v: sub's entry in the root, then the file's in sub, spelled from where the root resolves", *synced, want)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "reserved" {
		t.Fatalf("the file through the link: %q, %v", got, err)
	}
	// A link above the root: the root is spelled through it, and resolved.
	above := filepath.Join(base, "above")
	symlink(t, base, above)
	*synced = nil
	if err := WriteBytesDurable(filepath.Join(above, "real"), filepath.Join(above, "real", "ids"), []byte("again"), ".durable-*.tmp"); err != nil {
		t.Fatalf("WriteBytesDurable below a link above the root: %v", err)
	}
	if want := []string{real}; !slices.Equal(*synced, want) {
		t.Fatalf("the write below a link above the root fsynced %v, want %v", *synced, want)
	}
}

// TestWriteBytesDurable_RefusesAMountBelowTheRoot pins a path through a
// mount of another filesystem strictly below the root, a real one: /dev,
// below the root /, where this host mounts another filesystem there. The
// mounted filesystem's own root holds the entries below it, and its
// mount point's entry is the mounted filesystem's to keep, outside the walk
// up to the root (Codex on round 6), so the write and the probe refuse it by
// the identity the platform gives its mount (the kernel's mount ID on Linux,
// the volume's filesystem ID on macOS), before anything is created, written
// or synced. The barrier is the test's: nothing is fsynced on the host's
// directories.
func TestWriteBytesDurable_RefusesAMountBelowTheRoot(t *testing.T) {
	if !mountIDsExpected {
		t.Skip("no directory barrier: the strict writer refuses before it looks at a path")
	}
	if info, err := os.Lstat("/dev"); err != nil || !info.IsDir() {
		t.Skipf("no /dev directory here: %v", err)
	}
	top, okTop, errTop := mountIDOf("/")
	dev, okDev, errDev := mountIDOf("/dev")
	switch {
	case !okTop || !okDev || errTop != nil || errDev != nil:
		t.Skipf("no mount identity for / or /dev here: %v, %v", errTop, errDev)
	case top == dev:
		t.Skip("/dev is not a mount of its own here")
	}
	name := fmt.Sprintf("atomicwrite-test-%d-%d", os.Getpid(), time.Now().UnixNano())
	missing := filepath.Join("/dev", name)
	t.Cleanup(func() {
		// Nothing is made there unless the refusal is broken, and only a
		// process that may write in /dev could make it.
		_ = os.Remove(filepath.Join(missing, "ids"))
		_ = os.Remove(missing)
	})
	synced := stubSyncDir(t, nil)
	for _, path := range []string{filepath.Join(missing, "ids"), missing} {
		*synced = nil
		err := WriteBytesDurable("/", path, []byte("reserved"), ".durable-*.tmp")
		refusedPlainly(t, "WriteBytesDurable of "+path, err, synced, missing)
		*synced = nil
		refusedPlainly(t, "ProbeDurable of "+path, ProbeDurable("/", path), synced)
		if err != nil && !strings.Contains(err.Error(), "/dev") {
			t.Errorf("the refusal %q does not name /dev, the mount point", err)
		}
	}
}

// stubMountIDs replaces the kernel's mount IDs with id's answers, and
// restores them on cleanup. Callers MUST NOT call `t.Parallel()`, as with
// stubSyncParentDir.
func stubMountIDs(t *testing.T, id func(path string) (uint64, bool, error)) {
	t.Helper()
	prev := mountIDOf
	mountIDOf = id
	t.Cleanup(func() { mountIDOf = prev })
}

// noMountIDs is a platform that gives no mount identity: on Linux, a kernel
// that gives no mount ID through statx (older than 5.8, or a statx a policy
// refuses) with no /proc/self/fdinfo to read one from.
func noMountIDs(string) (uint64, bool, error) { return 0, false, nil }

// mountedAt is the mount IDs of a kernel with a mount of its own at point,
// as point is now: what a path reaches is on that mount when it is point, or
// lies below it, however the path spells it (os.SameFile, as the kernel
// finds a mount by the entry a path reaches, never by its name); everything
// else is on another.
func mountedAt(t *testing.T, point string) func(string) (uint64, bool, error) {
	t.Helper()
	at, err := os.Lstat(point)
	if err != nil {
		t.Fatal(err)
	}
	return func(path string) (uint64, bool, error) {
		for d := path; ; d = filepath.Dir(d) {
			fi, err := os.Lstat(d)
			if err != nil {
				return 0, false, err
			}
			if os.SameFile(fi, at) {
				return 2, true, nil
			}
			if filepath.Dir(d) == d {
				return 1, true, nil
			}
		}
	}
}

// TestWriteBytesDurable_RefusesABindMountByItsMountID pins the mount check
// the writer makes on every write, and the probe, by the mount identity the
// platform gives: a directory below the root on a mount of its own, a bind
// mount of the root's own filesystem, which shows the root's device, is
// refused (ErrNotUnderRoot) before anything is created, written or synced,
// and a directory on the root's mount passes. Round 7 looked for such a
// mount in the probe alone, by the mount table, so a bind mount made after
// the probe was never looked for.
func TestWriteBytesDurable_RefusesABindMountByItsMountID(t *testing.T) {
	root := realTempDir(t)
	sub := filepath.Join(root, "sub")
	mkdirs(t, sub)
	synced := stubSyncDir(t, nil)
	stubMountIDs(t, mountedAt(t, sub))
	for _, path := range []string{filepath.Join(sub, "ids"), filepath.Join(sub, "new", "ids")} {
		*synced = nil
		err := WriteBytesDurable(root, path, []byte("reserved"), ".durable-*.tmp")
		refusedPlainly(t, "WriteBytesDurable of "+path, err, synced, filepath.Join(sub, "ids"), filepath.Join(sub, "new"))
		*synced = nil
		refusedPlainly(t, "ProbeDurable of "+path, ProbeDurable(root, path), synced)
	}
	other := filepath.Join(root, "other")
	mkdirs(t, other)
	if err := ProbeDurable(root, filepath.Join(other, "ids")); err != nil {
		t.Errorf("ProbeDurable below a directory on the root's mount = %v", err)
	}
	// A platform that gives the root's mount ID and not that of a directory
	// below it rules nothing out there: ErrMountsUnknown.
	stubMountIDs(t, func(path string) (uint64, bool, error) { return 1, path == root, nil })
	if err := ProbeDurable(root, filepath.Join(other, "ids")); !errors.Is(err, ErrMountsUnknown) {
		t.Errorf("ProbeDurable with the mount ID of the root alone = %v, want ErrMountsUnknown", err)
	}
	if err := WriteBytesDurable(root, filepath.Join(other, "ids"), []byte("reserved"), ".durable-*.tmp"); !errors.Is(err, ErrMountsUnknown) {
		t.Errorf("WriteBytesDurable with the mount ID of the root alone = %v, want ErrMountsUnknown", err)
	}
}

// TestWriteBytesDurable_RefusesABindMountWhateverItsSpelling pins Codex's
// round 7 finding: on a filesystem that folds case, root/Player reaches a
// mount at root/player, which the kernel's mount table names by that
// spelling alone, so round 7's probe, matching the table by spelling, passed
// root/Player/ids, and the writer synced none of the mount source's
// entries. By mount identity the spelling does not matter. Where the
// platform gives no identity, the writer and the probe say so
// (ErrMountsUnknown) rather than pass the path, as round 8's fallback to the
// table, matched by spelling, did (Codex on round 8). The test runs where
// the filesystem folds case (APFS and NTFS by default), and skips where
// another spelling names another directory.
func TestWriteBytesDurable_RefusesABindMountWhateverItsSpelling(t *testing.T) {
	root := realTempDir(t)
	point := filepath.Join(root, "player")
	mkdirs(t, point)
	other := filepath.Join(root, "Player")
	if _, err := os.Lstat(other); err != nil {
		t.Skipf("this filesystem tells %s from %s: no other spelling reaches the mount (%v)", other, point, err)
	}
	synced := stubSyncDir(t, nil)
	stubMountIDs(t, mountedAt(t, point))
	path := filepath.Join(other, "ids")
	refusedPlainly(t, "ProbeDurable through the mount by another spelling", ProbeDurable(root, path), synced)
	*synced = nil
	err := WriteBytesDurable(root, path, []byte("reserved"), ".durable-*.tmp")
	refusedPlainly(t, "WriteBytesDurable through the mount by another spelling", err, synced, filepath.Join(point, "ids"))
	if !mountIDsExpected {
		return
	}
	stubMountIDs(t, noMountIDs)
	if err := ProbeDurable(root, path); !errors.Is(err, ErrMountsUnknown) {
		t.Errorf("ProbeDurable through another spelling with no mount identity = %v, want ErrMountsUnknown", err)
	}
	if err := WriteBytesDurable(root, path, []byte("reserved"), ".durable-*.tmp"); !errors.Is(err, ErrMountsUnknown) {
		t.Errorf("WriteBytesDurable through another spelling with no mount identity = %v, want ErrMountsUnknown", err)
	}
}

// TestWriteBytesDurable_RefusesAMountedDestination pins the destination's own
// mount check (Codex on round 7): a file mounted at the destination, by its
// mount identity, is refused by the probe and the write before anything is
// written or synced, with the bytes the write would write already there.
// Round 7 checked the directories alone, and a rename over a mounted file,
// which fails with EBUSY, took the fallback for matching bytes: it fsynced
// the mounted file and the root and answered nil, never the mount source's
// directory entry. Where the platform gives no identity, the probe and the
// write say so (ErrMountsUnknown).
func TestWriteBytesDurable_RefusesAMountedDestination(t *testing.T) {
	root := realTempDir(t)
	dst := filepath.Join(root, "ids")
	want := []byte("reserved")
	if err := os.WriteFile(dst, want, 0o600); err != nil {
		t.Fatal(err)
	}
	synced := stubSyncDir(t, nil)
	stubMountIDs(t, mountedAt(t, dst))
	refusedPlainly(t, "ProbeDurable of a destination on a mount of its own", ProbeDurable(root, dst), synced)
	*synced = nil
	err := WriteBytesDurable(root, dst, want, ".durable-*.tmp")
	refusedPlainly(t, "WriteBytesDurable over a destination on a mount of its own", err, synced)
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 1 {
		t.Errorf("the refused write left %v (%v), want the destination alone", entries, err)
	}
	// Where the platform gives no mount identity, whether a file is mounted
	// there cannot be told, and the probe and the write say so (Codex on
	// round 8), with nothing synced or written.
	if !mountIDsExpected {
		return
	}
	*synced = nil
	stubMountIDs(t, noMountIDs)
	if err := ProbeDurable(root, dst); !errors.Is(err, ErrMountsUnknown) {
		t.Errorf("ProbeDurable of a destination with no mount identity = %v, want ErrMountsUnknown", err)
	}
	if err := WriteBytesDurable(root, dst, []byte("other"), ".durable-*.tmp"); !errors.Is(err, ErrMountsUnknown) {
		t.Errorf("WriteBytesDurable over a destination with no mount identity = %v, want ErrMountsUnknown", err)
	}
	if got, err := os.ReadFile(dst); err != nil || string(got) != string(want) || len(*synced) != 0 {
		t.Errorf("with no mount identity the destination holds %q (%v) and %v were synced, want it untouched and nothing synced", got, err, *synced)
	}
}

// TestWriteBytesDurable_TakesTheFallbackOnlyFromAPlainFile pins the fallback
// a failed rename takes when the destination already holds the bytes: it
// takes them only from a regular file on its directory's own mount, asked
// again as it takes them, since what is there now may not be what the
// checks saw. A file mounted there once the checks have passed (its mount
// ID), the rename then failing with EBUSY, as a rename over a mount point
// does, and a symbolic link to a file with those bytes, the rename failing,
// each answer the rename's error, with neither the bytes nor the root
// synced: round 7 synced the mounted file, or the link's target, and the
// root, and answered nil, the entry naming those bytes never synced. A
// destination whose mount the platform gives no identity for once the checks
// have passed answers ErrMountsUnknown beside the rename's error, nothing
// synced: round 8 took two unknown identities for one, and the bytes with
// them (Codex on round 8).
func TestWriteBytesDurable_TakesTheFallbackOnlyFromAPlainFile(t *testing.T) {
	want := []byte("reserved")
	for _, tc := range []struct {
		name    string
		failErr error
		alsoIs  error
		set     func(t *testing.T, root, dst string) (mount func())
	}{
		{"a file whose mount cannot be told once the checks passed", syscall.EBUSY, ErrMountsUnknown, func(t *testing.T, root, dst string) func() {
			if !mountIDsExpected {
				t.Skip("this platform refuses before it looks at a path: no directory barrier")
			}
			if err := os.WriteFile(dst, want, 0o600); err != nil {
				t.Fatal(err)
			}
			gone := false
			stubMountIDs(t, func(string) (uint64, bool, error) { return 1, !gone, nil })
			return func() { gone = true }
		}},
		{"a file mounted once the checks passed", syscall.EBUSY, nil, func(t *testing.T, root, dst string) func() {
			if err := os.WriteFile(dst, want, 0o600); err != nil {
				t.Fatal(err)
			}
			at, err := os.Lstat(dst)
			if err != nil {
				t.Fatal(err)
			}
			mounted := false
			stubMountIDs(t, func(path string) (uint64, bool, error) {
				fi, err := os.Lstat(path)
				if err != nil {
					return 0, false, err
				}
				if mounted && os.SameFile(fi, at) {
					return 2, true, nil
				}
				return 1, true, nil
			})
			return func() { mounted = true }
		}},
		{"a symbolic link to a file with the bytes", os.ErrPermission, nil, func(t *testing.T, root, dst string) func() {
			target := filepath.Join(root, "elsewhere")
			if err := os.WriteFile(target, want, 0o600); err != nil {
				t.Fatal(err)
			}
			symlink(t, target, dst)
			return func() {}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := realTempDir(t)
			dst := filepath.Join(root, "ids")
			mount := tc.set(t, root, dst)
			prev := SetRenameFuncForTest(func(string, string) error {
				mount()
				return tc.failErr
			})
			t.Cleanup(func() { SetRenameFuncForTest(prev) })
			// syncs is every sync after the rename: of the destination's bytes
			// and of a directory.
			var syncs []string
			renamed := false
			prevFile, prevDir := syncFile, syncDir
			t.Cleanup(func() { syncFile, syncDir = prevFile, prevDir })
			syncFile = func(f *os.File) error {
				if f.Name() != dst {
					renamed = true // the staged file's sync comes just before the rename
					return f.Sync()
				}
				syncs = append(syncs, "file "+f.Name())
				return nil
			}
			syncDir = func(d *os.File) error {
				if renamed {
					syncs = append(syncs, "dir "+d.Name())
				}
				return nil
			}
			err := WriteBytesDurable(root, dst, want, ".durable-*.tmp")
			if !errors.Is(err, tc.failErr) {
				t.Errorf("WriteBytesDurable = %v, want the rename's %v: the bytes there are not the write's to vouch for", err, tc.failErr)
			}
			if tc.alsoIs != nil && !errors.Is(err, tc.alsoIs) {
				t.Errorf("WriteBytesDurable = %v, want %v beside the rename's error", err, tc.alsoIs)
			}
			if len(syncs) != 0 {
				t.Errorf("after the failed rename the write synced %v, want nothing", syncs)
			}
		})
	}
}

// TestPlatformMountIDReadsTheKernels pins the mount identity the writer reads
// on each platform: on Linux the kernel's mount ID, from statx or else
// /proc/self/fdinfo (TestPlatformMountTableReadsTheKernels holds both to the
// kernel's own table), / and /proc on mounts of their own; on macOS the
// mounted volume's filesystem ID, / and /dev on mounts of their own; and a
// directory on its parent's wherever an identity is given. Windows gives
// none, and no error: it refuses before it asks (mountIDsExpected).
func TestPlatformMountIDReadsTheKernels(t *testing.T) {
	top, ok, err := platformMountID("/")
	if !mountIDsExpected {
		if ok || err != nil {
			t.Fatalf("platformMountID on %s = %d, %v, %v, want no ID", runtime.GOOS, top, ok, err)
		}
		return
	}
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skipf("no mount identity on %s: the strict writer refuses there", runtime.GOOS)
	}
	if !ok || err != nil {
		t.Fatalf("platformMountID(/) = %d, %v, %v, want the platform's ID", top, ok, err)
	}
	other := "/proc"
	if runtime.GOOS == "darwin" {
		other = "/dev"
	}
	if id, ok, err := platformMountID(other); !ok || err != nil || id == top {
		t.Errorf("platformMountID(%s) = %d, %v, %v, want an ID of its own, not /'s %d", other, id, ok, err, top)
	}
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	mkdirs(t, sub)
	a, okA, errA := platformMountID(dir)
	b, okB, errB := platformMountID(sub)
	if !okA || !okB || errA != nil || errB != nil || a != b {
		t.Errorf("a directory and the one below it: %d (%v, %v) and %d (%v, %v), want one ID", a, okA, errA, b, okB, errB)
	}
}

// TestWriteBytesDurable_SaysSoWhereNoMountIDCanBeHad pins Codex's round 8
// finding: where the platform gives no mount identity, which on Linux takes a
// kernel that gives no mount ID through statx and no /proc/self/fdinfo to
// read one from, the writer cannot tell one mount from another, and says so
// (ErrMountsUnknown) rather than take them for one. Round 8 then compared no
// mount on any write, its probe matched the mount table by spelling, and its
// fallback took two unknown identities for one. The write and the probe
// refuse, with nothing created, written or synced: a file in the root, as the
// probe of a reservation not yet written is asked, since every write after
// the first needs the identity; one below a directory, there or to be made;
// and a destination already there. Windows gives none and needs none: it
// refuses before, for want of a directory barrier.
func TestWriteBytesDurable_SaysSoWhereNoMountIDCanBeHad(t *testing.T) {
	if !mountIDsExpected {
		t.Skip("this platform refuses before it looks at a path: no directory barrier")
	}
	root := realTempDir(t)
	sub := filepath.Join(root, "sub")
	mkdirs(t, sub)
	existing := filepath.Join(root, "existing")
	if err := os.WriteFile(existing, []byte("theirs"), 0o600); err != nil {
		t.Fatal(err)
	}
	synced := stubSyncDir(t, nil)
	stubMountIDs(t, noMountIDs)
	for _, path := range []string{filepath.Join(root, "ids"), filepath.Join(sub, "ids"), filepath.Join(sub, "new", "ids"), existing} {
		*synced = nil
		if err := WriteBytesDurable(root, path, []byte("reserved"), ".durable-*.tmp"); !errors.Is(err, ErrMountsUnknown) {
			t.Errorf("WriteBytesDurable(%s) with no mount identity = %v, want ErrMountsUnknown", path, err)
		}
		if err := ProbeDurable(root, path); !errors.Is(err, ErrMountsUnknown) {
			t.Errorf("ProbeDurable(%s) with no mount identity = %v, want ErrMountsUnknown", path, err)
		}
		if len(*synced) != 0 {
			t.Errorf("with no mount identity the write and the probe of %s synced %v", path, *synced)
		}
	}
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 2 {
		t.Errorf("the refusals left %v (%v) in the root, want sub and existing alone", entries, err)
	}
	if entries, err := os.ReadDir(sub); err != nil || len(entries) != 0 {
		t.Errorf("the refusals left %v (%v) in sub, want nothing", entries, err)
	}
	if got, err := os.ReadFile(existing); err != nil || string(got) != "theirs" {
		t.Errorf("the destination already there holds %q (%v), want it untouched", got, err)
	}
}

// TestProbeDurable_RefusesAMountPointTheKernelLists pins the probe's mount
// check by the identity the platform lists for each entry, beside the write's
// (TestWriteBytesDurable_RefusesABindMountByItsMountID): a mount point between
// the file and the root is refused (ErrNotUnderRoot) with nothing fsynced; a
// mount point off the path, or the root's own, refuses nothing, the probe
// syncing each directory a write would; and the root's identity is asked for
// even where the file's directory is the root, a platform that gives none
// saying so (ErrMountsUnknown). Round 8's probe read the mount table where
// the kernel gave no mount ID and matched it by spelling; no table is read
// now (Codex on round 8).
func TestProbeDurable_RefusesAMountPointTheKernelLists(t *testing.T) {
	root := realTempDir(t)
	sub := filepath.Join(root, "sub")
	other := filepath.Join(root, "other")
	mkdirs(t, sub, other)
	synced := stubSyncDir(t, nil)
	path := filepath.Join(sub, "missing", "ids")

	stubMountIDs(t, mountedAt(t, sub))
	refusedPlainly(t, "ProbeDurable through a mount point the platform lists", ProbeDurable(root, path), synced)

	for _, point := range []string{other, root} {
		*synced = nil
		stubMountIDs(t, mountedAt(t, point))
		if err := ProbeDurable(root, path); err != nil {
			t.Errorf("ProbeDurable with a mount point at %s, none between the file and the root = %v, want nil", point, err)
		}
		if want := []string{sub, root}; !slices.Equal(*synced, want) {
			t.Errorf("with a mount point at %s the probe fsynced %v, want %v", point, *synced, want)
		}
	}

	if !mountIDsExpected {
		return
	}
	*synced = nil
	stubMountIDs(t, noMountIDs)
	if err := ProbeDurable(root, filepath.Join(root, "ids")); !errors.Is(err, ErrMountsUnknown) || len(*synced) != 0 {
		t.Errorf("ProbeDurable of a file in the root with no mount identity = %v after syncing %v, want ErrMountsUnknown and nothing synced", err, *synced)
	}
}
