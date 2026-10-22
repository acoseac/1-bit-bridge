package atomicwrite

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// TestMain takes every root for one on storage that outlives a reboot, for
// the whole run, where the system's temporary directory, in which the tests
// make their roots, is on storage that does not (a tmpfs /tmp, as Debian 13
// and Fedora mount it): the strict writer refuses such a root (ErrVolatile),
// and every test about something else would refuse with it. Elsewhere the
// real check runs under every test. The medium's own tests stub mediumOf, or
// call platformMedium.
func TestMain(m *testing.M) {
	if mountIDsExpected {
		if dir, err := filepath.EvalSymlinks(os.TempDir()); err == nil {
			if id, ok, err := platformMountID(dir); err == nil && ok {
				if err := platformMedium(dir, id); err != nil {
					fmt.Fprintf(os.Stderr, "atomicwrite tests: %v; every root is taken for one on storage that outlives a reboot\n", err)
					mediumOf = func(string, uint64) error { return nil }
				}
			}
		}
	}
	os.Exit(m.Run())
}

// stubMedium replaces the medium check with medium's answers, and restores
// it on cleanup. Callers MUST NOT call `t.Parallel()`, as with
// stubSyncParentDir.
func stubMedium(t *testing.T, medium func(root string, mountID uint64) error) {
	t.Helper()
	prev := mediumOf
	mediumOf = medium
	t.Cleanup(func() { mediumOf = prev })
}

// piMountInfo is a mount table in the shape Raspberry Pi OS's "Overlay File
// System" option leaves (overlayroot, overlayroot=tmpfs): the root an overlay
// whose lower layer is the card's ext4 root, read-only, and whose upper layer
// is a tmpfs, both mounted below /media. It is written for the tests, not
// captured. Mount 40 is a decoy whose mount point spells the data directory
// the tests take on mount 22, an overlay over a disk: a check that picked the
// line by its spelling would read it.
const piMountInfo = `22 1 0:22 / / rw,noatime shared:1 - overlay overlayroot rw,lowerdir=/media/root-ro,upperdir=/media/root-rw/overlay,workdir=/media/root-rw/overlay-workdir/_
23 22 179:2 / /media/root-ro ro,noatime shared:2 - ext4 /dev/mmcblk0p2 ro
24 22 0:23 / /media/root-rw rw,relatime shared:3 - tmpfs tmpfs-root rw,size=1948672k,mode=755
25 22 0:5 / /proc rw,nosuid,nodev,noexec,relatime shared:12 - proc proc rw
40 22 0:41 / /home/pi/.local/share/1-bit-bridge rw,relatime shared:20 - overlay overlay rw,lowerdir=/srv/lower,upperdir=/srv/disk/upper,workdir=/srv/disk/work
`

// TestOverlayLayerIsTheLineOfTheRootsMountID pins the mount table's reading
// for an overlay's upper layer: the line whose first field is the mount ID
// the writer already holds for the root, never one picked by its mount
// point's spelling; its type after the separator, which must be overlay; its
// upperdir, unescaped as the kernel escapes a space, a comma or a backslash;
// and volatile, as an option of its own or as fsync=volatile. A read-only
// overlay names no upper layer. A mount that is not an overlay, one the table
// does not list, and a line that does not read as one are errors.
func TestOverlayLayerIsTheLineOfTheRootsMountID(t *testing.T) {
	const table = piMountInfo + `50 22 0:50 / /srv/a\040b rw - overlay overlay rw,lowerdir=/l,upperdir=/u\040p\054q\134r,workdir=/w,volatile
51 22 0:51 / /srv/c rw shared:9 master:3 - overlay overlay rw,lowerdir=/l,upperdir=/u2,workdir=/w2,fsync=volatile
52 22 0:52 / /srv/ro ro - overlay overlay ro,lowerdir=/l1:/l2
53 22 0:53 / /srv/cut rw shared:9
`
	for _, tc := range []struct {
		id       uint64
		upper    string
		volatile bool
	}{
		{22, "/media/root-rw/overlay", false},
		{40, "/srv/disk/upper", false},
		{50, `/u p,q\r`, true},
		{51, "/u2", true},
		{52, "", false},
	} {
		m, err := overlayLayer([]byte(table), tc.id)
		if err != nil || m.upper != tc.upper || m.volatile != tc.volatile {
			t.Errorf("overlayLayer(%d) = %+v, %v, want upper %q, volatile %v", tc.id, m, err, tc.upper, tc.volatile)
		}
	}
	for _, id := range []uint64{23, 99, 53} {
		if m, err := overlayLayer([]byte(table), id); err == nil {
			t.Errorf("overlayLayer(%d) = %+v, want an error: not an overlay, not listed, or a line cut short", id, m)
		}
	}
}

// TestMediumRefusesWhatDoesNotOutliveAReboot pins the medium check, the
// orchestrator's decision on the Claude API's round 9 finding: a root on
// storage that does not keep what is written to it through a reboot is
// refused (ErrVolatile), the reboot taking the reservation and leaving the
// player's identity, so its IDs and tokens would be handed out again with no
// crash at all; and a root whose storage cannot be told is refused too
// (ErrMediumUnknown), the check failing closed. tmpfs and ramfs by their
// f_type; an overlay by its upper layer, the one the mount table names on
// the line of the root's own mount ID, which must be the filesystem the
// overlay reports (its totals), whatever the path that names it reaches
// here; Raspberry Pi OS's "Overlay File System" among them. A disk, an
// overlay over one, and a read-only overlay pass. The statfs answers, the
// table and the mount IDs are the test's, so it runs everywhere; Linux's own
// are TestPlatformMediumReadsTheKernels and the namespace tests.
func TestMediumRefusesWhatDoesNotOutliveAReboot(t *testing.T) {
	ext4 := fsStat{Type: 0xef53, Bsize: 4096, Blocks: 7577600, Files: 1900544}
	tmpfs := fsStat{Type: tmpfsMagic, Bsize: 4096, Blocks: 487168, Files: 241014}
	ramfs := fsStat{Type: ramfsMagic, Bsize: 4096}
	overlay := func(over fsStat) fsStat { over.Type = overlayfsMagic; return over }
	const data = "/home/pi/.local/share/1-bit-bridge"
	for _, tc := range []struct {
		name   string
		id     uint64
		stats  map[string]fsStat
		ids    map[string]uint64
		table  string
		tblErr error
		want   error  // nil: the root is taken
		says   string // what the refusal names
	}{
		{name: "tmpfs", stats: map[string]fsStat{data: tmpfs}, want: ErrVolatile, says: data + " is on tmpfs"},
		{name: "ramfs", stats: map[string]fsStat{data: ramfs}, want: ErrVolatile, says: data + " is on ramfs"},
		{name: "a disk", stats: map[string]fsStat{data: ext4}},
		{name: "Raspberry Pi OS's Overlay File System", id: 22, table: piMountInfo,
			stats: map[string]fsStat{data: overlay(tmpfs), "/media/root-rw/overlay": tmpfs},
			want:  ErrVolatile, says: data + " is on an overlay whose upper layer /media/root-rw/overlay is on tmpfs"},
		{name: "an overlay over a disk", id: 40, table: piMountInfo,
			stats: map[string]fsStat{data: overlay(ext4), "/srv/disk/upper": ext4}},
		{name: "an overlay mounted volatile", id: 7, table: "7 1 0:7 / /d rw - overlay overlay rw,lowerdir=/l,upperdir=/u,workdir=/w,volatile\n",
			stats: map[string]fsStat{data: overlay(ext4), "/u": ext4}, want: ErrVolatile, says: "volatile"},
		{name: "a read-only overlay", id: 8, table: "8 1 0:8 / /d ro - overlay overlay ro,lowerdir=/l1:/l2\n",
			stats: map[string]fsStat{data: overlay(ext4)}},
		{name: "an overlay whose upper layer cannot be read here", id: 22, table: piMountInfo,
			stats: map[string]fsStat{data: overlay(tmpfs)}, want: ErrMediumUnknown, says: "/media/root-rw/overlay"},
		{name: "an overlay whose upper layer names another filesystem here", id: 22, table: piMountInfo,
			stats: map[string]fsStat{data: overlay(tmpfs), "/media/root-rw/overlay": ext4}, want: ErrMediumUnknown, says: "/media/root-rw/overlay"},
		{name: "an overlay naming its upper layer by a relative path", id: 9, table: "9 1 0:9 / /d rw - overlay overlay rw,lowerdir=l,upperdir=u,workdir=w\n",
			stats: map[string]fsStat{data: overlay(ext4), "u": ext4}, want: ErrMediumUnknown, says: "relative"},
		{name: "an overlay the table does not list", id: 99, table: piMountInfo,
			stats: map[string]fsStat{data: overlay(tmpfs)}, want: ErrMediumUnknown, says: "99"},
		{name: "a table that cannot be read", id: 22, tblErr: &fs.PathError{Op: "open", Path: "/proc/self/mountinfo", Err: syscall.ENOENT},
			stats: map[string]fsStat{data: overlay(tmpfs)}, want: ErrMediumUnknown, says: "/proc/self/mountinfo"},
		{name: "an overlay over an overlay over tmpfs", id: 60,
			table: "60 1 0:60 / /d rw - overlay overlay rw,lowerdir=/l,upperdir=/outer/up,workdir=/outer/w\n61 1 0:61 / /outer rw - overlay overlay rw,lowerdir=/l,upperdir=/inner/up,workdir=/inner/w\n",
			stats: map[string]fsStat{data: overlay(tmpfs), "/outer/up": overlay(tmpfs), "/inner/up": tmpfs},
			ids:   map[string]uint64{"/outer/up": 61},
			want:  ErrVolatile, says: "an overlay whose upper layer /outer/up is on an overlay whose upper layer /inner/up is on tmpfs"},
		{name: "overlays nested past the bound", id: 70,
			table: "70 1 0:70 / /d rw - overlay overlay rw,lowerdir=/l,upperdir=/loop/up,workdir=/loop/w\n",
			stats: map[string]fsStat{data: overlay(ext4), "/loop/up": overlay(ext4)},
			ids:   map[string]uint64{"/loop/up": 70},
			want:  ErrMediumUnknown, says: "deep"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			statfs := func(p string) (fsStat, error) {
				st, ok := tc.stats[p]
				if !ok {
					return fsStat{}, &fs.PathError{Op: "statfs", Path: p, Err: syscall.ENOENT}
				}
				return st, nil
			}
			table := func() ([]byte, error) { return []byte(tc.table), tc.tblErr }
			idOf := func(p string) (uint64, bool, error) {
				id, ok := tc.ids[p]
				return id, ok, nil
			}
			err := mediumFrom(data, tc.id, statfs, table, idOf)
			switch {
			case tc.want == nil && err != nil:
				t.Fatalf("mediumFrom = %v, want the root taken", err)
			case tc.want != nil && !errors.Is(err, tc.want):
				t.Fatalf("mediumFrom = %v, want %v", err, tc.want)
			case tc.want != nil && !strings.Contains(err.Error(), tc.says):
				t.Fatalf("mediumFrom = %q, want it to say %q", err, tc.says)
			}
		})
	}
}

// TestWriteBytesDurable_RefusesARootThatDoesNotOutliveAReboot pins the medium
// check in the write and the probe: asked of the root, as it resolves, and of
// its mount's ID, on every call, and its refusal, of storage that does not
// outlive a reboot (ErrVolatile) or that cannot be told to
// (ErrMediumUnknown), answered before anything is created, written or
// synced. Storage that outlives one lets the write go ahead.
func TestWriteBytesDurable_RefusesARootThatDoesNotOutliveAReboot(t *testing.T) {
	root := realTempDir(t)
	sub := filepath.Join(root, "sub")
	mkdirs(t, sub)
	synced := stubSyncDir(t, nil)
	stubMountIDs(t, func(string) (uint64, bool, error) { return 7, true, nil })
	path := filepath.Join(sub, "new", "ids")
	for _, refusal := range []error{
		fmt.Errorf("%w: %s is on tmpfs", ErrVolatile, root),
		fmt.Errorf("%w: %s is on an overlay whose upper layer cannot be read here", ErrMediumUnknown, root),
	} {
		var asked []string
		stubMedium(t, func(r string, id uint64) error {
			asked = append(asked, fmt.Sprintf("%s %d", r, id))
			return refusal
		})
		*synced = nil
		if err := WriteBytesDurable(root, path, []byte("reserved"), ".durable-*.tmp"); !errors.Is(err, refusal) {
			t.Errorf("WriteBytesDurable with the medium refused = %v, want %v", err, refusal)
		}
		if err := ProbeDurable(root, path); !errors.Is(err, refusal) {
			t.Errorf("ProbeDurable with the medium refused = %v, want %v", err, refusal)
		}
		if want := []string{root + " 7", root + " 7"}; strings.Join(asked, "|") != strings.Join(want, "|") {
			t.Errorf("the medium was asked %v, want %v: the root and its mount's ID, by the write and by the probe", asked, want)
		}
		if len(*synced) != 0 {
			t.Errorf("with the medium refused the write and the probe synced %v", *synced)
		}
		if _, err := os.Lstat(filepath.Join(sub, "new")); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("the refused write created %s (%v)", filepath.Join(sub, "new"), err)
		}
	}
	stubMedium(t, func(string, uint64) error { return nil })
	if err := WriteBytesDurable(root, path, []byte("reserved"), ".durable-*.tmp"); err != nil {
		t.Fatalf("WriteBytesDurable on storage that outlives a reboot: %v", err)
	}
}
