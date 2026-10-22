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
// its device (st_dev), before anything is created, written or synced. The
// barrier is the test's: nothing is fsynced on the host's directories.
func TestWriteBytesDurable_RefusesAMountBelowTheRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no directory barrier: the strict writer refuses before it looks at a path")
	}
	top, err := os.Lstat("/")
	if err != nil {
		t.Fatal(err)
	}
	devInfo, err := os.Lstat("/dev")
	if err != nil {
		t.Skipf("no /dev here: %v", err)
	}
	if a, ok := device(top); !ok || !devInfo.IsDir() {
		t.Skip("no device to compare here")
	} else if b, _ := device(devInfo); a == b {
		t.Skip("/dev is not a mount of another filesystem here")
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

// stubMountTable replaces the kernel's mount table with points and err, and
// counts the reads. Callers MUST NOT call `t.Parallel()`, as with
// stubSyncParentDir.
func stubMountTable(t *testing.T, points []string, err error) *int {
	t.Helper()
	reads := 0
	prev := mountTable
	mountTable = func() (map[string]bool, error) {
		reads++
		if err != nil {
			return nil, err
		}
		m := map[string]bool{}
		for _, p := range points {
			m[p] = true
		}
		return m, nil
	}
	t.Cleanup(func() { mountTable = prev })
	return &reads
}

// TestProbeDurable_RefusesAMountPointTheKernelLists pins the probe's read of
// the mount table, for a mount the device cannot show: a bind mount of the
// root's own filesystem onto a directory below the root, whose source's
// entries are outside the walk up to the root (Codex on round 6). The table
// is the test's, so it runs everywhere; Linux's own is
// TestPlatformMountTableReadsTheKernels. A mount point between the file and
// the root is refused (ErrNotUnderRoot) with nothing fsynced; a table that
// cannot be read is ErrMountsUnknown while a directory lies between them;
// a mount point off the path, the root's own, or one below the directories
// that exist yet refuses nothing; and with the file's directory the root,
// nothing lies between them and the table is not read.
func TestProbeDurable_RefusesAMountPointTheKernelLists(t *testing.T) {
	root := realTempDir(t)
	sub := filepath.Join(root, "sub")
	mkdirs(t, sub, filepath.Join(root, "other"))
	synced := stubSyncDir(t, nil)
	path := filepath.Join(sub, "missing", "ids")

	stubMountTable(t, []string{sub}, nil)
	refusedPlainly(t, "ProbeDurable through a mount point the kernel lists", ProbeDurable(root, path), synced)

	*synced = nil
	readErr := errors.New("permission denied")
	stubMountTable(t, nil, readErr)
	if err := ProbeDurable(root, path); !errors.Is(err, ErrMountsUnknown) || !errors.Is(err, readErr) {
		t.Errorf("ProbeDurable with the mount table unread = %v, want ErrMountsUnknown with the read's error", err)
	}
	if len(*synced) != 0 {
		t.Errorf("the probe with the mount table unread fsynced %v", *synced)
	}

	for _, points := range [][]string{{filepath.Join(root, "other")}, {root}, {filepath.Join(sub, "missing")}} {
		*synced = nil
		stubMountTable(t, points, nil)
		if err := ProbeDurable(root, path); err != nil {
			t.Errorf("ProbeDurable with mount points %v, none between the file and the root = %v, want nil", points, err)
		}
		if want := []string{sub, root}; !slices.Equal(*synced, want) {
			t.Errorf("with mount points %v the probe fsynced %v, want %v", points, *synced, want)
		}
	}

	reads := stubMountTable(t, nil, readErr)
	*synced = nil
	if err := ProbeDurable(root, filepath.Join(root, "ids")); err != nil || *reads != 0 {
		t.Errorf("ProbeDurable of a file in the root = %v after %d reads of the table, want nil and none", err, *reads)
	}
}

// TestParseMountInfo pins the mount table's reading: the fifth field of
// each line, a mount point, unescaped as the kernel escapes a space, a tab,
// a newline and a backslash; and a line that does not read as one, refused,
// since a table that does not read rules no mount out.
func TestParseMountInfo(t *testing.T) {
	const table = `22 28 0:21 / /proc rw,nosuid,nodev,noexec,relatime shared:12 - proc proc rw
28 1 259:2 / / rw,relatime shared:1 - ext4 /dev/root rw
61 28 259:2 /srv/real /data/with\040space rw,relatime shared:1 - ext4 /dev/root rw
62 28 259:2 /srv/back /data/back\134slash rw,relatime - ext4 /dev/root rw
`
	got, err := parseMountInfo([]byte(table))
	if err != nil {
		t.Fatalf("parseMountInfo: %v", err)
	}
	want := map[string]bool{"/proc": true, "/": true, "/data/with space": true, `/data/back\slash`: true}
	if len(got) != len(want) {
		t.Fatalf("mount points %v, want %v", got, want)
	}
	for p := range want {
		if !got[p] {
			t.Fatalf("mount points %v, want %v", got, want)
		}
	}
	for _, bad := range []string{"22 28 0:21 /\n", "22 28 0:21 / proc rw - proc proc rw\n"} {
		if _, err := parseMountInfo([]byte(bad)); err == nil {
			t.Errorf("parseMountInfo(%q) answered no error", bad)
		}
	}
}

// TestPlatformMountTableReadsTheKernels pins the table the probe reads: on
// Linux the kernel's, which lists / and /proc for every process; elsewhere
// none, and no error.
func TestPlatformMountTableReadsTheKernels(t *testing.T) {
	got, err := platformMountTable()
	if runtime.GOOS != "linux" {
		if got != nil || err != nil {
			t.Fatalf("platformMountTable on %s = %v, %v, want no table", runtime.GOOS, got, err)
		}
		return
	}
	if err != nil {
		t.Fatalf("platformMountTable: %v", err)
	}
	for _, p := range []string{"/", "/proc"} {
		if !got[p] {
			t.Errorf("the kernel's mount table has no %s: %v", p, got)
		}
	}
}
