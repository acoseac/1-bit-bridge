//go:build linux

package atomicwrite

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestMediumMagicsAreLinuxs holds the medium check's f_type numbers to the
// kernel's, as x/sys/unix names them.
func TestMediumMagicsAreLinuxs(t *testing.T) {
	for _, m := range []struct {
		name      string
		got, want uint32
	}{
		{"tmpfs", tmpfsMagic, uint32(unix.TMPFS_MAGIC)},
		{"ramfs", ramfsMagic, uint32(unix.RAMFS_MAGIC)},
		{"overlayfs", overlayfsMagic, uint32(unix.OVERLAYFS_SUPER_MAGIC)},
		{"fuse", fuseMagic, uint32(unix.FUSE_SUPER_MAGIC)},
	} {
		if m.got != m.want {
			t.Errorf("%s's magic is %#x here, and %#x in x/sys/unix", m.name, m.got, m.want)
		}
	}
}

// statfsType is the f_type statfs gives dir.
func statfsType(t *testing.T, dir string) uint32 {
	t.Helper()
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		t.Fatalf("statfs %s: %v", dir, err)
	}
	return uint32(st.Type)
}

// TestPlatformMediumReadsTheKernels holds the medium check on Linux to what
// the kernel says of real directories: /dev/shm, a tmpfs wherever it is
// mounted, is refused (ErrVolatile, naming tmpfs), and the tests' temporary
// directory is refused when statfs says it is tmpfs or ramfs, and taken when
// it says a filesystem on a disk.
func TestPlatformMediumReadsTheKernels(t *testing.T) {
	medium := func(dir string) error {
		id, ok, err := platformMountID(dir)
		if err != nil || !ok {
			t.Fatalf("platformMountID(%s) = %d, %v, %v", dir, id, ok, err)
		}
		return platformMedium(dir, id)
	}
	if info, err := os.Stat("/dev/shm"); err == nil && info.IsDir() && statfsType(t, "/dev/shm") == tmpfsMagic {
		if err := medium("/dev/shm"); !errors.Is(err, ErrVolatile) || !strings.Contains(err.Error(), "tmpfs") {
			t.Errorf("the medium of /dev/shm, a tmpfs = %v, want ErrVolatile naming tmpfs", err)
		}
	} else {
		t.Log("no tmpfs at /dev/shm here")
	}
	dir := realTempDir(t)
	switch typ, err := statfsType(t, dir), medium(dir); typ {
	case tmpfsMagic, ramfsMagic:
		if !errors.Is(err, ErrVolatile) {
			t.Errorf("the medium of %s, on f_type %#x = %v, want ErrVolatile", dir, typ, err)
		}
	case overlayfsMagic:
		t.Logf("the temporary directory %s is on an overlay here: %v", dir, err)
	default:
		if err != nil {
			t.Errorf("the medium of %s, on f_type %#x = %v, want it taken", dir, typ, err)
		}
	}
}

// inOverlayNamespace runs scenario in a child process of this test binary
// that is root in a user namespace of its own, with a mount namespace of its
// own, so that it can mount a tmpfs and an overlay as Raspberry Pi OS's
// "Overlay File System" does, and nothing it mounts is seen outside it. It
// skips where the kernel or a policy gives an unprivileged process no such
// namespace, or no overlay in one (Linux 5.11), and fails, with the child's
// output, where the scenario fails.
func inOverlayNamespace(t *testing.T, scenario, base string) {
	t.Helper()
	t.Cleanup(func() {
		// An overlay's work directory leaves a directory of mode 0 behind,
		// which the temporary directory's removal could not read.
		_ = filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
			if d != nil && d.IsDir() {
				_ = os.Chmod(p, 0o700)
			}
			return nil
		})
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestOverlayNamespaceHelper$", "-test.v", "-test.count=1")
	cmd.Env = append(os.Environ(), overlayScenarioEnv+"="+scenario, overlayBaseEnv+"="+base)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:  syscall.CLONE_NEWUSER | syscall.CLONE_NEWNS,
		UidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}},
		GidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}},
	}
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	switch {
	case err != nil && !errors.As(err, &exit):
		t.Skipf("no user and mount namespaces of its own for a child here: %v", err)
	case strings.Contains(string(out), "--- SKIP: TestOverlayNamespaceHelper"):
		t.Skipf("the child could not set the scenario up:\n%s", out)
	case err != nil:
		t.Fatalf("the scenario %s failed in its namespace: %v\n%s", scenario, err, out)
	case !strings.Contains(string(out), "--- PASS: TestOverlayNamespaceHelper"):
		t.Fatalf("the scenario %s did not run:\n%s", scenario, out)
	}
}

// TestWriteBytesDurable_RefusesAnOverlayWhoseUpperLayerIsTmpfs pins the
// Raspberry Pi OS "Overlay File System" option against the real kernel, the
// Claude API's round 9 finding: an overlay whose lower layer is a directory
// of the test's and whose upper layer is a tmpfs, mounted in a namespace of
// the test's own, is refused by the probe and by the write (ErrVolatile,
// naming tmpfs), and nothing is copied up into the tmpfs.
func TestWriteBytesDurable_RefusesAnOverlayWhoseUpperLayerIsTmpfs(t *testing.T) {
	inOverlayNamespace(t, "tmpfs-upper", realTempDir(t))
}

// TestWriteBytesDurable_SyncsWhatACopyUpCreated pins Codex's round 9 finding
// against the real kernel: below an overlay whose upper layer is on the
// test's own filesystem, the data directory and the directory below it exist
// in the lower layer alone, the write's staging copies both up, and the
// syncs after the rename are the file's directory and the root, which holds
// the copied-up directory's entry (TestWriteBytesDurable_SyncsTheChainAgainOnceTheWriteLands
// is the order's own test, everywhere). It needs the test's temporary
// directory on storage that outlives a reboot, as the writer refuses an
// overlay over anything else.
func TestWriteBytesDurable_SyncsWhatACopyUpCreated(t *testing.T) {
	base := realTempDir(t)
	id, ok, err := platformMountID(base)
	if err != nil || !ok {
		t.Fatalf("platformMountID(%s) = %d, %v, %v", base, id, ok, err)
	}
	if typ := statfsType(t, base); typ == overlayfsMagic {
		t.Skipf("the temporary directory %s is on an overlay here, whose own upper layer the namespace may not reach", base)
	}
	if err := platformMedium(base, id); err != nil {
		t.Skipf("the temporary directory %s is not on storage that outlives a reboot: %v", base, err)
	}
	inOverlayNamespace(t, "copy-up", base)
}

// TestProbeDurable_RefusesTmpfsThroughThePackagesOwnMediumCheck pins the
// medium check's wiring (the Claude API on round 10): with mediumOf as the
// package initializes it, not as TestMain may have taken it or a test stubbed
// it, the probe of a directory on /dev/shm, a tmpfs, is refused
// (ErrVolatile). A no-op in its place passed every other test.
func TestProbeDurable_RefusesTmpfsThroughThePackagesOwnMediumCheck(t *testing.T) {
	if info, err := os.Stat("/dev/shm"); err != nil || !info.IsDir() || statfsType(t, "/dev/shm") != tmpfsMagic {
		t.Skip("no tmpfs at /dev/shm here")
	}
	prev := mediumOf
	mediumOf = initialMediumOf
	t.Cleanup(func() { mediumOf = prev })
	root, err := os.MkdirTemp("/dev/shm", "atomicwrite-test-")
	if err != nil {
		t.Skipf("no directory can be made in /dev/shm: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	synced := stubSyncDir(t, nil)
	if err := ProbeDurable(root, filepath.Join(root, "ids")); !errors.Is(err, ErrVolatile) || len(*synced) != 0 {
		t.Errorf("ProbeDurable on /dev/shm through the package's own medium check = %v after syncing %v, want ErrVolatile and nothing synced", err, *synced)
	}
}

// TestOverlayNamespaceHelper is the child inOverlayNamespace starts, root in
// a user namespace of its own; it does nothing unless that set its scenario.
func TestOverlayNamespaceHelper(t *testing.T) {
	scenario := os.Getenv(overlayScenarioEnv)
	if scenario == "" {
		return
	}
	base := os.Getenv(overlayBaseEnv)
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		t.Skipf("the namespace's mounts cannot be made private: %v", err)
	}
	lower := filepath.Join(base, "lower")
	mkdirs(t, filepath.Join(lower, "data", "player"))
	if err := os.WriteFile(filepath.Join(lower, "data", "player-uuid"), []byte("5f0c0f6e-2b1e-4c1a-9d43-6d1b6f1e2a77\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	layer := filepath.Join(base, "layer")
	mkdirs(t, layer)
	switch scenario {
	case "tmpfs-upper":
		if err := unix.Mount("tmpfs", layer, "tmpfs", 0, "mode=0700"); err != nil {
			t.Skipf("no tmpfs can be mounted in the namespace: %v", err)
		}
		root := filepath.Join(mountOverlay(t, base, lower, layer), "data")
		path := filepath.Join(root, "player", "ids")
		if err := ProbeDurable(root, path); !errors.Is(err, ErrVolatile) || !strings.Contains(err.Error(), "tmpfs") {
			t.Errorf("ProbeDurable below an overlay whose upper layer is tmpfs = %v, want ErrVolatile naming tmpfs", err)
		}
		if err := WriteBytesDurable(root, path, []byte("reserved"), ".durable-*.tmp"); !errors.Is(err, ErrVolatile) {
			t.Errorf("WriteBytesDurable below an overlay whose upper layer is tmpfs = %v, want ErrVolatile", err)
		}
		if _, err := os.Lstat(filepath.Join(layer, "upper", "data")); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("the refused write copied the data directory up into the tmpfs (%v)", err)
		}
	case "copy-up":
		root := filepath.Join(mountOverlay(t, base, lower, layer), "data")
		player := filepath.Join(root, "player")
		if _, err := os.Lstat(filepath.Join(layer, "upper", "data")); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("the data directory is in the upper layer before the write (%v)", err)
		}
		var log []string
		prevDir := syncDir
		t.Cleanup(func() { syncDir = prevDir })
		syncDir = func(d *os.File) error {
			log = append(log, d.Name())
			return d.Sync()
		}
		prev := SetRenameFuncForTest(func(src, dst string) error {
			log = append(log, "rename")
			return os.Rename(src, dst)
		})
		t.Cleanup(func() { SetRenameFuncForTest(prev) })
		if err := WriteBytesDurable(root, filepath.Join(player, "ids"), []byte("reserved"), ".durable-*.tmp"); err != nil {
			t.Fatalf("WriteBytesDurable below an overlay over the test's own filesystem: %v", err)
		}
		for _, d := range []string{"data", filepath.Join("data", "player"), filepath.Join("data", "player", "ids")} {
			if _, err := os.Lstat(filepath.Join(layer, "upper", d)); err != nil {
				t.Errorf("the write left %s out of the upper layer (%v)", d, err)
			}
		}
		merged := filepath.Dir(root)
		if want := []string{root, "rename", player, root, merged}; !slices.Equal(log, want) {
			t.Errorf("the write synced %v, want %v: the root while the player directory was in the lower layer alone, then once the write landed the player directory, the root, which holds its copied-up entry, and the overlay's own root, which holds the root's (Codex on round 10)", log, want)
		}
	default:
		t.Fatalf("no scenario %q", scenario)
	}
}

// mountOverlay mounts at base/merged an overlay of lower under an upper
// layer in layer, as Raspberry Pi OS's overlayroot mounts one over its root,
// with the overlay's xattrs in the user namespace where the kernel takes that
// (userxattr), and skips where it can mount none. It returns the overlay.
func mountOverlay(t *testing.T, base, lower, layer string) string {
	t.Helper()
	upper, work, merged := filepath.Join(layer, "upper"), filepath.Join(layer, "work"), filepath.Join(base, "merged")
	mkdirs(t, upper, work, merged)
	opts := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s", lower, upper, work)
	err := unix.Mount("overlay", merged, "overlay", 0, opts+",userxattr")
	if err != nil {
		err = unix.Mount("overlay", merged, "overlay", 0, opts)
	}
	if err != nil {
		t.Skipf("no overlay can be mounted in the namespace: %v", err)
	}
	return merged
}
