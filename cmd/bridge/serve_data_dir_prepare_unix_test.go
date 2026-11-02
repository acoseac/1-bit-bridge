//go:build unix

package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

// platformLockKindCases are flock's answers as fsutil.KeepsNoLocks reads
// them on these platforms (B537 item 1): ENOLCK, EOPNOTSUPP, ENOTSUP and
// ENOSYS say no lock can be taken there, ENOLCK only that none was given;
// a descriptor the call refuses, an operation it calls invalid and a
// permission error are the third kind.
var platformLockKindCases = []lockKindCase{
	{"ENOLCK", unix.ENOLCK, true},
	{"EOPNOTSUPP", unix.EOPNOTSUPP, true},
	{"ENOTSUP", unix.ENOTSUP, true},
	{"ENOSYS", unix.ENOSYS, true},
	{"EBADF", unix.EBADF, false},
	{"EINVAL", unix.EINVAL, false},
	{"EACCES", unix.EACCES, false},
}

// TestServeSaysNoLockWasGivenWhereTheKernelHadNoneToGive drives serve's lock
// through ENOLCK, the answer of an NFS mount whose lock manager is not
// running, and of a kernel out of lock records on any filesystem, while
// another open may hold the lock: serve serves on, with the no-lock line,
// which does not say the filesystem keeps no locks and says another serve
// may run all the same.
func TestServeSaysNoLockWasGivenWhereTheKernelHadNoneToGive(t *testing.T) {
	base := t.TempDir()
	lib := filepath.Join(base, "Music")
	if err := os.Mkdir(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := writeServeConfig(t, filepath.Join(base, "bridge.yaml"), lib, filepath.Join(base, "data"), "127.0.0.1:0", "127.0.0.1:0")
	code, served, _, stderr := runServeWith(t, serveOpts{configPath: cfg, dataDirTryLock: lockAnswer(unix.ENOLCK)})
	if !served {
		t.Fatalf("serve did not serve where the kernel gave no lock (exit %d); stderr=%s", code, stderr)
	}
	for _, s := range []string{noLockLine, unix.ENOLCK.Error(), noLockLineReason} {
		if !strings.Contains(stderr, s) {
			t.Errorf("serve did not say %q; stderr=%s", s, stderr)
		}
	}
	if strings.Contains(stderr, couldNotLockLine) {
		t.Errorf("serve gave another kind's line; stderr=%s", stderr)
	}
}

// TestServeStartsWhereTheLockFileCannotBeOpened: a data directory that is
// there, with a lock file this user cannot open (a mode-0 file, as an older
// `sudo bridge serve` could leave one), is no refusal: serve says it could
// not lock the data directory, naming the permission error, and serves.
// Whether the directory can be written is the player's preflight's and the
// doctor's to say, never the preparation's.
func TestServeStartsWhereTheLockFileCannotBeOpened(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root opens a file of mode 0")
	}
	base := t.TempDir()
	lib := filepath.Join(base, "Music")
	dataDir := filepath.Join(base, "data")
	for _, d := range []string{lib, dataDir} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dataDir, serveLockFileName), nil, 0); err != nil {
		t.Fatal(err)
	}
	cfg := writeServeConfig(t, filepath.Join(base, "bridge.yaml"), lib, dataDir, "127.0.0.1:0", "127.0.0.1:0")
	served := bootServe(t, "--config", cfg)
	s := served.stderr.String()
	if !strings.Contains(s, couldNotLockLine+filepath.Join(dataDir, serveLockFileName)) || !strings.Contains(s, unix.EACCES.Error()) {
		t.Errorf("serve did not say it could not open its lock file; stderr=%s", s)
	}
	if strings.Contains(s, "cannot be used") {
		t.Errorf("serve refused a data directory that is there; stderr=%s", s)
	}
}

// TestPreparingTheDataDirectoryAsRootGivesEachDirectoryItMakesItsParentsOwner:
// run as root over an install another user owns (simulated), the
// preparation makes the data directory and the directory above it that is
// missing, each given the owner of the directory it is made in, so the
// service's user can write there; a data directory that is there is given
// nothing.
func TestPreparingTheDataDirectoryAsRootGivesEachDirectoryItMakesItsParentsOwner(t *testing.T) {
	base := t.TempDir()
	dataDir := filepath.Join(base, "config", "data")
	changes, restore := fsutil.SimulateRootForTest(4242, 4243)
	defer restore()
	if err := prepareServeDataDir(dataDir); err != nil {
		t.Fatalf("prepareServeDataDir as root: %v", err)
	}
	want := []fsutil.OwnerChange{
		{Dst: filepath.Join(base, "config"), UID: 4242, GID: 4243},
		{Dst: dataDir, UID: 4242, GID: 4243},
	}
	if got := changes(); !slices.Equal(got, want) {
		t.Errorf("the preparation as root gave %v, want %v", got, want)
	}
	if info, err := os.Stat(dataDir); err != nil || !info.IsDir() {
		t.Errorf("the preparation as root made no directory at %s (stat: %v)", dataDir, err)
	}
	if err := prepareServeDataDir(dataDir); err != nil {
		t.Fatalf("prepareServeDataDir of a data directory that is there, as root: %v", err)
	}
	if got := changes(); len(got) != len(want) {
		t.Errorf("preparing a data directory that is there gave %v", got[len(want):])
	}
}

// TestPreparingAnExistingDataDirectoryAsRootMakesAndGivesNothing: run as
// root (simulated) over a data directory that is there, a directory or a
// link to one, the preparation makes nothing and gives nothing an owner:
// fsutil.MkdirAll's walk as root returns as soon as os.Stat finds a
// directory, through a link too, so neither the directory nor the link is
// made, replaced or chowned, and nothing appears beside them. This covers
// the preparation alone, on a directory that can be statted: the lock, and
// serve's later writes there on a read-only filesystem, are not claimed.
func TestPreparingAnExistingDataDirectoryAsRootMakesAndGivesNothing(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "data")
	if err := os.Mkdir(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o750); err != nil { // whatever the umask
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadDir(base)
	if err != nil {
		t.Fatal(err)
	}
	changes, restore := fsutil.SimulateRootForTest(4242, 4243)
	defer restore()
	for _, dataDir := range []string{dir, link} {
		if err := prepareServeDataDir(dataDir); err != nil {
			t.Errorf("prepareServeDataDir(%s) as root = %v, want the directory taken as it is", dataDir, err)
		}
	}
	if got := changes(); len(got) != 0 {
		t.Errorf("preparing a data directory that is there as root gave %v, want nothing made and nothing chowned", got)
	}
	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the link at the data directory is no longer a link (lstat: %v)", err)
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() || info.Mode().Perm() != 0o750 {
		t.Errorf("the data directory changed: %v (stat: %v)", info, err)
	}
	after, err := os.ReadDir(base)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Errorf("the preparation made %d entries beside the data directory, want none", len(after)-len(before))
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Errorf("the preparation made %d entries in the data directory (err %v), want none", len(entries), err)
	}
}
