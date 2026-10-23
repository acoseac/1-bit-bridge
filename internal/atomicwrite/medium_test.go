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
)

// The scenario TestOverlayNamespaceHelper runs, in a child process of the
// test binary, and the directory it works in.
const (
	overlayScenarioEnv = "ATOMICWRITE_OVERLAY_SCENARIO"
	overlayBaseEnv     = "ATOMICWRITE_OVERLAY_BASE"
)

// initialMediumOf is the medium check as the package initializes it, before
// TestMain can take every root for persistent storage:
// TestProbeDurable_RefusesTmpfsThroughThePackagesOwnMediumCheck holds that
// default to the kernel, which no other test asks (the Claude API on round
// 10: a no-op in its place passed the whole suite).
var initialMediumOf = mediumOf

// TestMain takes every root for one on storage that outlives a reboot, for
// the whole run, where the system's temporary directory, in which the tests
// make their roots, is on storage that does not (a tmpfs /tmp, as Debian 13
// and Fedora mount it): the strict writer refuses such a root (ErrVolatile),
// and every test about something else would refuse with it. Elsewhere the
// real check runs under every test. It takes nothing in a namespace
// scenario's child, whose writes go through the package's own check. The
// medium's own tests stub mediumOf, or call platformMedium.
func TestMain(m *testing.M) {
	if mountIDsExpected && os.Getenv(overlayScenarioEnv) == "" {
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
// and volatile, as an option of its own or as fsync=volatile. The line is
// split on the ASCII space the kernel writes between fields, and nowhere
// else: the kernel leaves Unicode white space in a path as it is, which
// strings.Fields split, so round 10 read `lowerdir=/lower` and nothing after
// it from a lowerdir holding U+00A0, and lost the upperdir and volatile that
// followed (Codex on round 10). A read-only overlay alone names no upper
// layer. A mount that is not an overlay, one the table does not list, a line
// cut short, one with a field too many or an empty one, and a writable
// overlay that names no upper layer are errors. The source alone may be
// empty, as the kernel writes it for an overlay mounted with none (the Claude
// API on round 11: round 11 refused that line as one with an empty field).
func TestOverlayLayerIsTheLineOfTheRootsMountID(t *testing.T) {
	const table = piMountInfo + `50 22 0:50 / /srv/a\040b rw - overlay overlay rw,lowerdir=/l,upperdir=/u\040p\054q\134r,workdir=/w,volatile
51 22 0:51 / /srv/c rw shared:9 master:3 - overlay overlay rw,lowerdir=/l,upperdir=/u2,workdir=/w2,fsync=volatile
52 22 0:52 / /srv/ro ro - overlay overlay ro,lowerdir=/l1:/l2
53 22 0:53 / /srv/cut rw shared:9
` + "60 22 0:60 / /srv/nb rw - overlay overlay rw,lowerdir=/lower\u00a0dir,upperdir=/ram/upper,workdir=/ram/work,volatile\n" +
		"61 22 0:61 / /srv/\u3000wide rw - overlay overlay rw,lowerdir=/l,upperdir=/u\u2028line,workdir=/w\n" +
		`62 22 0:62 / /srv/rw rw - overlay overlay rw,lowerdir=/l
63 22 0:63 / /srv/x rw - overlay overlay rw,lowerdir=/l,upperdir=/u,workdir=/w more
64 22 0:64 / /srv/y  rw - overlay overlay ro,lowerdir=/l
65 22 0:65 / /srv/e rw - overlay  rw,lowerdir=/l,upperdir=/u3,workdir=/w3
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
		{60, "/ram/upper", true},
		{61, "/u\u2028line", false},
		{65, "/u3", false},
	} {
		m, err := overlayLayer([]byte(table), tc.id)
		if err != nil || m.upper != tc.upper || m.volatile != tc.volatile {
			t.Errorf("overlayLayer(%d) = %+v, %v, want upper %q, volatile %v", tc.id, m, err, tc.upper, tc.volatile)
		}
	}
	for _, id := range []uint64{23, 99, 53, 62, 63, 64} {
		if m, err := overlayLayer([]byte(table), id); err == nil {
			t.Errorf("overlayLayer(%d) = %+v, want an error: not an overlay, not listed, a line cut short or with a field too many or an empty one, or a writable overlay with no upper layer", id, m)
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
		{name: "a bind of a read-only overlay", id: 8, table: "8 1 0:8 /sub /d ro - overlay overlay ro,lowerdir=/l1:/l2\n",
			stats: map[string]fsStat{data: overlay(ext4)}},
		{name: "a read-only overlay", id: 8, table: "8 1 0:8 / /d ro - overlay overlay ro,lowerdir=/l1:/l2\n",
			stats: map[string]fsStat{data: overlay(ext4)}},
		{name: "a writable bind whose upper path reaches a read-only overlay", id: 60,
			table: "60 1 0:60 /srv/data /d rw - overlay overlay rw,lowerdir=/l,upperdir=/outer/up,workdir=/outer/w\n61 1 0:61 / /outer/up ro - overlay overlay ro,lowerdir=/l1:/l2\n",
			stats: map[string]fsStat{data: overlay(ext4), "/outer/up": overlay(ext4)},
			ids:   map[string]uint64{"/outer/up": 61},
			want:  ErrMediumUnknown, says: "upper path reaches a read-only overlay, which cannot be a writable overlay's upper layer"},
		{name: "a writable overlay whose upper path reaches a read-only overlay", id: 60,
			table: "60 1 0:60 / /d rw - overlay overlay rw,lowerdir=/l,upperdir=/outer/up,workdir=/outer/w\n61 1 0:61 / /outer/up ro - overlay overlay ro,lowerdir=/l1:/l2\n",
			stats: map[string]fsStat{data: overlay(ext4), "/outer/up": overlay(ext4)},
			ids:   map[string]uint64{"/outer/up": 61},
			want:  ErrMediumUnknown, says: "upper path reaches a read-only overlay, which cannot be a writable overlay's upper layer"},
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
		{name: "an overlay over fuse-overlayfs uses the inner mount line", id: 60,
			table: "60 1 0:60 / /d rw - overlay overlay rw,lowerdir=/l,upperdir=/outer/up,workdir=/outer/w\n61 1 0:61 / /outer rw - fuse.fuse-overlayfs fuse-overlayfs rw\n",
			stats: map[string]fsStat{data: overlay(ext4), "/outer/up": {Type: fuseMagic, Bsize: ext4.Bsize, Blocks: ext4.Blocks, Files: ext4.Files}},
			ids:   map[string]uint64{"/outer/up": 61}, want: ErrMediumUnknown, says: "fuse-overlayfs"},
		{name: "fuse-overlayfs, whose upper layer the table does not show", id: 80,
			table: "80 1 0:80 / / rw - fuse.fuse-overlayfs fuse-overlayfs rw,user_id=1000,group_id=1000\n",
			stats: map[string]fsStat{data: {Type: fuseMagic, Bsize: 4096, Blocks: 7577600, Files: 1900544}},
			want:  ErrMediumUnknown, says: "fuse-overlayfs"},
		{name: "another FUSE filesystem", id: 81,
			table: "81 1 0:81 / /home/pi rw - fuse.sshfs host:/dir rw,user_id=1000,group_id=1000\n",
			stats: map[string]fsStat{data: {Type: fuseMagic, Bsize: 4096}}},
		{name: "a FUSE root whose table cannot be read", id: 81, tblErr: &fs.PathError{Op: "open", Path: "/proc/self/mountinfo", Err: syscall.ENOENT},
			stats: map[string]fsStat{data: {Type: fuseMagic, Bsize: 4096}}, want: ErrMediumUnknown, says: "/proc/self/mountinfo"},
		{name: "an overlay over tmpfs whose lower layer's path holds Unicode white space", id: 60,
			table: "60 1 0:60 / / rw - overlay overlay rw,lowerdir=/lower\u00a0dir,upperdir=/ram/upper,workdir=/ram/work\n",
			stats: map[string]fsStat{data: overlay(tmpfs), "/ram/upper": tmpfs}, want: ErrVolatile, says: "/ram/upper is on tmpfs"},
		{name: "a writable overlay whose line names no upper layer", id: 62,
			table: "62 1 0:62 / / rw - overlay overlay rw,lowerdir=/l\n",
			stats: map[string]fsStat{data: overlay(tmpfs)}, want: ErrMediumUnknown, says: "no upper layer"},
		{name: "Volumio's shape: the root an overlay over a disk, its own mount", id: 90,
			table: "90 1 0:90 / / rw,relatime shared:1 - overlay overlay rw,lowerdir=/static,upperdir=/data/dyn,workdir=/data/work\n",
			stats: map[string]fsStat{data: overlay(ext4), "/data/dyn": ext4}},
		{name: "a bind of a directory inside an overlay", id: 91,
			table: "91 90 0:90 /home/pi/.local/share/1-bit-bridge /home/pi/.local/share/1-bit-bridge rw,relatime shared:1 - overlay overlay rw,lowerdir=/static,upperdir=/data/dyn,workdir=/data/work\n",
			stats: map[string]fsStat{data: overlay(ext4), "/data/dyn": ext4},
			want:  ErrOverlayBind, says: "a bind of /home/pi/.local/share/1-bit-bridge, a directory inside an overlay, not the overlay's own mount: a copy-up the write makes creates entries above it that this mount cannot reach to sync: put the data directory on a filesystem that is not an overlay, or on the overlay's own mount"},
		{name: "a bind of an overlay's directory whose upper layer is tmpfs, refused for its storage first", id: 92,
			table: "92 22 0:22 /srv/data /srv/data rw - overlay overlayroot rw,lowerdir=/media/root-ro,upperdir=/media/root-rw/overlay,workdir=/media/root-rw/overlay-workdir/_\n",
			stats: map[string]fsStat{data: overlay(tmpfs), "/media/root-rw/overlay": tmpfs},
			want:  ErrVolatile, says: "/media/root-rw/overlay is on tmpfs"},
		{name: "an overlay over a disk mounted with no source", id: 93,
			table: "93 1 0:93 / / rw - overlay  rw,lowerdir=/l,upperdir=/srv/disk/upper,workdir=/srv/disk/work\n",
			stats: map[string]fsStat{data: overlay(ext4), "/srv/disk/upper": ext4}},
		{name: "fuse-overlayfs built with no subtype", id: 82,
			table: "82 1 0:82 / / rw - fuse fuse-overlayfs rw,user_id=0,group_id=0\n",
			stats: map[string]fsStat{data: {Type: fuseMagic, Bsize: 4096}},
			want:  ErrMediumUnknown, says: "fuse-overlayfs"},
		{name: "a FUSE filesystem mounted with no source", id: 83,
			table: "83 1 0:83 / /home/pi rw - fuse.sshfs  rw,user_id=1000,group_id=1000\n",
			stats: map[string]fsStat{data: {Type: fuseMagic, Bsize: 4096}}},
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

// TestMediumTakesTotalsThatMoveWithWhatIsWritten pins both reviewers' round
// 10 finding: some filesystems' totals move with ordinary writes (XFS's
// f_files counts the inodes it could still make from its free blocks, and
// ZFS's f_blocks is what is referenced plus what is available), so an
// overlay's totals and its upper layer's, read one after the other, differ
// though they are one filesystem's, and round 10 refused such an overlay at
// random, at New and at any reservation write. The overlay, its upper path
// and the overlay again are read for up to three rounds: the path is taken as
// soon as it matches an overlay read beside it; totals that moved between
// two reads of the overlay are no tmpfs's or ramfs's, which no write moves,
// and the upper layer the path reaches is taken for what it is; and only
// exhausted retries remain inconclusive (ErrMediumUnknown) with retry advice.
// An agreement on the third upper read must still be accepted. Each statfs
// answer here is the next of its path's, as a filesystem moves between calls.
func TestMediumTakesTotalsThatMoveWithWhatIsWritten(t *testing.T) {
	const data = "/var/lib/1-bit-bridge"
	xfs := func(files uint64) fsStat { return fsStat{Type: 0x58465342, Bsize: 4096, Blocks: 2621440, Files: files} }
	zfs := func(blocks uint64) fsStat {
		return fsStat{Type: 0x2fc12fc1, Bsize: 131072, Blocks: blocks, Files: 1 << 40}
	}
	overlay := func(over fsStat) fsStat { over.Type = overlayfsMagic; return over }
	const table = "30 1 0:30 / / rw - overlay overlay rw,lowerdir=/l,upperdir=/srv/up,workdir=/srv/work\n"
	for _, tc := range []struct {
		name  string
		reads map[string][]fsStat
		want  error
		says  string
	}{
		{"XFS whose inode total moves with each allocation",
			map[string][]fsStat{data: {overlay(xfs(1000)), overlay(xfs(998))}, "/srv/up": {xfs(999)}}, nil, ""},
		{"ZFS whose block total moves with each write",
			map[string][]fsStat{data: {overlay(zfs(5000)), overlay(zfs(5003))}, "/srv/up": {zfs(5001)}}, nil, ""},
		{"totals that moved and came back, then agree",
			map[string][]fsStat{data: {overlay(xfs(1000)), overlay(xfs(1000)), overlay(xfs(1000))}, "/srv/up": {xfs(999), xfs(1000)}}, nil, ""},
		{"agreement only on the third upper read",
			map[string][]fsStat{data: {overlay(xfs(1000)), overlay(xfs(1000)), overlay(xfs(1000))}, "/srv/up": {xfs(999), xfs(999), xfs(1000)}}, nil, ""},
		{"three rounds that held still while the path differed",
			map[string][]fsStat{data: {overlay(xfs(1000)), overlay(xfs(1000)), overlay(xfs(1000)), overlay(xfs(1000))}, "/srv/up": {xfs(999), xfs(999), xfs(999)}}, ErrMediumUnknown, "inconclusive"},
		{"a path that reaches tmpfs while the overlay's totals move",
			map[string][]fsStat{data: {overlay(xfs(1000)), overlay(xfs(998))}, "/srv/up": {{Type: tmpfsMagic, Bsize: 4096, Blocks: 1000, Files: 1000}}}, ErrMediumUnknown, "tmpfs"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := map[string]int{}
			statfs := func(p string) (fsStat, error) {
				reads, ok := tc.reads[p]
				if !ok {
					return fsStat{}, &fs.PathError{Op: "statfs", Path: p, Err: syscall.ENOENT}
				}
				i := min(calls[p], len(reads)-1)
				calls[p]++
				return reads[i], nil
			}
			idOf := func(string) (uint64, bool, error) { return 0, false, nil }
			err := mediumFrom(data, 30, statfs, func() ([]byte, error) { return []byte(table), nil }, idOf)
			if tc.says == "inconclusive" && err != nil && (!strings.Contains(err.Error(), "retry") || strings.Contains(err.Error(), "another filesystem")) {
				t.Errorf("sampling cannot prove another filesystem: %v", err)
			}
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

// stubCopyUps replaces the directories the platform lists above a root, up to
// the root of its mount, with of's answers, and restores them on cleanup.
// Callers MUST NOT call `t.Parallel()`, as with stubSyncParentDir.
func stubCopyUps(t *testing.T, of func(root string, mountID uint64) ([]string, error)) {
	t.Helper()
	prev := copyUpAncestorsOf
	copyUpAncestorsOf = of
	t.Cleanup(func() { copyUpAncestorsOf = prev })
}

// TestWriteBytesDurable_SyncsUpToTheRootOfItsMountOnAnOverlay pins Codex's
// round 10 finding, closed as the orchestrator chose. On an overlay a write
// below a root found in the lower layer alone copies the root up, and the
// directories above it up to the overlay's own root, creating entries the
// chain up to the root never syncs: a crash could take the copy, and the
// reservation in it, while the player-uuid below it stayed. Every directory
// above the root up to the root of its mount, as the platform lists them
// (copyUpAncestorsOf, asked with the root and its mount's ID), is synced after
// the chain, once the write lands and in the probe, which New runs. A
// directory there that cannot be synced fails the write and the probe, and a
// walk the platform cannot make refuses before anything is written.
func TestWriteBytesDurable_SyncsUpToTheRootOfItsMountOnAnOverlay(t *testing.T) {
	root := realTempDir(t)
	parent := filepath.Dir(root)
	grand := filepath.Dir(parent)
	synced := stubSyncDir(t, nil)
	stubMountIDs(t, func(string) (uint64, bool, error) { return 7, true, nil })
	var asked []string
	stubCopyUps(t, func(r string, id uint64) ([]string, error) {
		asked = append(asked, fmt.Sprintf("%s %d", r, id))
		return []string{parent, grand}, nil
	})
	path := filepath.Join(root, "ids")
	if err := WriteBytesDurable(root, path, []byte("reserved"), ".durable-*.tmp"); err != nil {
		t.Fatalf("WriteBytesDurable: %v", err)
	}
	if want := []string{root, parent, grand}; !slices.Equal(*synced, want) {
		t.Errorf("the write synced %v, want %v: the chain, then every directory above the root up to its mount's root", *synced, want)
	}
	*synced = nil
	if err := ProbeDurable(root, path); err != nil {
		t.Fatalf("ProbeDurable: %v", err)
	}
	if want := []string{root, parent, grand}; !slices.Equal(*synced, want) {
		t.Errorf("the probe synced %v, want %v", *synced, want)
	}
	if want := []string{root + " 7", root + " 7"}; !slices.Equal(asked, want) {
		t.Errorf("the directories above the root were asked for %v, want %v: by the write and the probe, with the root and its mount's ID", asked, want)
	}
	t.Run("permission", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("POSIX permission bits")
		}
		if os.Geteuid() == 0 {
			t.Skip("root can read a mode-0 directory")
		}
		gone := filepath.Join(root, "gone")
		if err := os.Mkdir(gone, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(gone, 0o700) })
		stubCopyUps(t, func(string, uint64) ([]string, error) { return []string{gone}, nil })
		for what, err := range map[string]error{
			"WriteBytesDurable": WriteBytesDurable(root, path, []byte("reserved again"), ".durable-*.tmp"),
			"ProbeDurable":      ProbeDurable(root, path),
		} {
			if !errors.Is(err, fs.ErrPermission) || !strings.Contains(err.Error(), gone+", above the root on an overlay, could not be opened for its sync") || !strings.Contains(err.Error(), "give the service read access") {
				t.Errorf("%s = %v, want permission failure naming the directory and read-access advice", what, err)
			}
		}
	})
	t.Run("ENOENT", func(t *testing.T) {
		gone := filepath.Join(root, "missing")
		stubCopyUps(t, func(string, uint64) ([]string, error) { return []string{gone}, nil })
		for what, err := range map[string]error{
			"WriteBytesDurable": WriteBytesDurable(root, path, []byte("reserved again"), ".durable-*.tmp"),
			"ProbeDurable":      ProbeDurable(root, path),
		} {
			if !errors.Is(err, fs.ErrNotExist) || !strings.Contains(err.Error(), gone+", above the root on an overlay, could not be opened for its sync") || strings.Contains(err.Error(), "read access") || strings.Contains(err.Error(), "put the data directory") {
				t.Errorf("%s = %v, want ENOENT naming the directory without permission or relocation advice", what, err)
			}
		}
	})
	stubCopyUps(t, func(string, uint64) ([]string, error) { return nil, fmt.Errorf("%w: the walk's own", ErrMountsUnknown) })
	*synced = nil
	other := filepath.Join(root, "other")
	if err := WriteBytesDurable(root, other, []byte("reserved"), ".durable-*.tmp"); !errors.Is(err, ErrMountsUnknown) {
		t.Errorf("WriteBytesDurable with a walk the platform cannot make = %v, want its ErrMountsUnknown", err)
	}
	if _, err := os.Lstat(other); !errors.Is(err, fs.ErrNotExist) || len(*synced) != 0 {
		t.Errorf("the refused write left %s (%v) and synced %v, want nothing", other, err, *synced)
	}
}

// TestAncestorsOnMountStopAtTheRootOfTheirMount pins the walk above a root
// that the copy-up syncs take (ancestorsOnMount): every directory above the
// root while it is on the root's mount, by the mount ID the platform gives
// it, never its spelling, the mount's own root the last; none for a root that
// is its mount's root, or the filesystem's; and ErrMountsUnknown for a
// directory on the way the platform gives no mount ID for.
func TestAncestorsOnMountStopAtTheRootOfTheirMount(t *testing.T) {
	p := filepath.FromSlash
	ids := map[string]uint64{p("/"): 1, p("/srv"): 1, p("/srv/data"): 1, p("/home"): 5, p("/home/pi"): 5, p("/home/pi/data"): 5}
	idOf := func(path string) (uint64, bool, error) {
		id, ok := ids[path]
		return id, ok, nil
	}
	for _, tc := range []struct {
		root string
		id   uint64
		want []string
	}{
		{p("/home/pi/data"), 5, []string{p("/home/pi"), p("/home")}},
		{p("/srv/data"), 1, []string{p("/srv"), p("/")}},
		{p("/home"), 5, nil},
		{p("/"), 1, nil},
	} {
		if got, err := ancestorsOnMount(tc.root, tc.id, idOf); err != nil || !slices.Equal(got, tc.want) {
			t.Errorf("ancestorsOnMount(%s) = %v, %v, want %v", tc.root, got, err, tc.want)
		}
	}
	if got, err := ancestorsOnMount(p("/home/pi/unlisted/data"), 5, idOf); !errors.Is(err, ErrMountsUnknown) {
		t.Errorf("ancestorsOnMount past a directory with no mount ID = %v, %v, want ErrMountsUnknown", got, err)
	}
}

// TestCopyUpsFromWalkOnlyOnAnOverlay pins the platform's copy-up default
// (copyUpsFrom, which platformCopyUps wires to statfs and the mount IDs), so
// that it is held where the namespace tests skip (the Claude API on round 11:
// a platformCopyUps answering none passed every test those tests did not
// run): on an overlay, the walk above the root (ancestorsOnMount), by the
// root's mount ID; on anything else, none, without a walk; and a statfs or a
// walk that fails, its error.
func TestCopyUpsFromWalkOnlyOnAnOverlay(t *testing.T) {
	p := filepath.FromSlash
	root := p("/home/pi/data")
	ids := map[string]uint64{p("/home/pi"): 5, p("/home"): 5, p("/"): 1}
	idOf := func(path string) (uint64, bool, error) {
		id, ok := ids[path]
		return id, ok, nil
	}
	statfsOf := func(st fsStat, err error) func(string) (fsStat, error) {
		return func(path string) (fsStat, error) {
			if path != root {
				t.Errorf("copyUpsFrom asked statfs for %s, want the root", path)
			}
			return st, err
		}
	}
	if got, err := copyUpsFrom(root, 5, statfsOf(fsStat{Type: overlayfsMagic}, nil), idOf); err != nil || !slices.Equal(got, []string{p("/home/pi"), p("/home")}) {
		t.Errorf("copyUpsFrom on an overlay = %v, %v, want the walk to the root of its mount", got, err)
	}
	walked := false
	if got, err := copyUpsFrom(root, 5, statfsOf(fsStat{Type: 0xef53}, nil), func(string) (uint64, bool, error) { walked = true; return 0, false, nil }); err != nil || got != nil || walked {
		t.Errorf("copyUpsFrom on ext4 = %v, %v (walked: %v), want none and no walk", got, err, walked)
	}
	statErr := &fs.PathError{Op: "statfs", Path: root, Err: syscall.EIO}
	if _, err := copyUpsFrom(root, 5, statfsOf(fsStat{}, statErr), idOf); !errors.Is(err, syscall.EIO) {
		t.Errorf("copyUpsFrom with statfs failing = %v, want its error", err)
	}
	walkErr := errors.New("statx: input/output error")
	if _, err := copyUpsFrom(root, 5, statfsOf(fsStat{Type: overlayfsMagic}, nil), func(string) (uint64, bool, error) { return 0, false, walkErr }); !errors.Is(err, walkErr) {
		t.Errorf("copyUpsFrom with the walk failing = %v, want its error", err)
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

// TestOverlayBindStillMatchesMediumUnknown pins the more specific refusal's
// compatibility with callers that already handle ErrMediumUnknown.
func TestOverlayBindStillMatchesMediumUnknown(t *testing.T) {
	if !errors.Is(ErrOverlayBind, ErrMediumUnknown) {
		t.Fatal("bind refusal must still match ErrMediumUnknown")
	}
	if strings.Contains(ErrOverlayBind.Error(), "reboot") {
		t.Fatal("a bind is a copy-up crash risk, not evidence of volatile storage")
	}
}

// TestMediumMismatchesNeedStableEvidence keeps repeated A,B,A sampling
// inconclusive, even when the visible upper path is independently XFS/ZFS.
// Its own mount ID selects the evidence, never a path spelling or device.
func TestMediumMismatchesNeedStableEvidence(t *testing.T) {
	const root = "/data"
	const table = "30 1 0:30 / / rw - overlay overlay rw,lowerdir=/l,upperdir=/up,workdir=/w\n" +
		"40 1 8:1 / /elsewhere rw - %s disk rw\n" +
		"41 1 8:2 / /up rw - tmpfs decoy rw\n"
	for _, tc := range []struct {
		name, fstype, device string
		id                   uint64
		idErr                error
		want                 string
	}{
		{"XFS repeated ABA", "xfs", "8:1", 40, nil, "xfs"},
		{"ZFS repeated ABA", "zfs", "8:1", 40, nil, "zfs"},
		{"tmpfs path still cannot pass", "tmpfs", "8:1", 40, nil, "tmpfs"},
		{"ramfs path still cannot pass", "ramfs", "8:1", 40, nil, "ramfs"},
		{"device differs", "xfs", "8:2", 40, nil, "xfs"},
		{"btrfs subvolume device differs", "btrfs", "0:45", 40, nil, "btrfs"},
		{"no device", "xfs", "", 40, nil, "xfs"},
		{"no mount ID", "xfs", "8:1", 0, nil, "mount ID"},
		{"mount ID read failed", "xfs", "8:1", 0, syscall.EIO, "input/output error"},
		{"missing mount line", "xfs", "8:1", 99, nil, "99"},
		{"malformed mount line", "", "8:1", 40, nil, "does not read"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			statfs := func(p string) (fsStat, error) {
				if p == root {
					return fsStat{Type: overlayfsMagic, Bsize: 4096, Blocks: 100, Files: 100}, nil
				}
				calls++
				typ := uint32(0x58465342)
				switch tc.fstype {
				case "btrfs":
					typ = 0x9123683e
				case "zfs":
					typ = 0x2fc12fc1
				case "tmpfs":
					typ = tmpfsMagic
				case "ramfs":
					typ = ramfsMagic
				}
				return fsStat{Type: typ, Bsize: 4096, Blocks: 100, Files: 99, Device: tc.device}, nil
			}
			idOf := func(p string) (uint64, bool, error) {
				if p != "/up" {
					t.Fatalf("identity asked for %s", p)
				}
				return tc.id, tc.id != 0, tc.idErr
			}
			err := mediumFrom(root, 30, statfs, func() ([]byte, error) { return []byte(fmt.Sprintf(table, tc.fstype)), nil }, idOf)
			if !errors.Is(err, ErrMediumUnknown) || !strings.Contains(err.Error(), "inconclusive") || !strings.Contains(err.Error(), "retry") || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "another filesystem") {
				t.Fatalf("medium = %v, want inconclusive with retry and evidence %q", err, tc.want)
			}
			if tc.idErr != nil && !errors.Is(err, tc.idErr) {
				t.Errorf("lost read error: %v", err)
			}
			if tc.id == 40 && tc.fstype != "" {
				if !strings.Contains(err.Error(), "mount 40") || strings.Contains(err.Error(), "cannot be confirmed") || strings.Contains(err.Error(), "does not match") {
					t.Errorf("mount ID selected a valid line, want its filesystem named without a device disagreement failure: %v", err)
				}
				if tc.device != "" && (!strings.Contains(err.Error(), tc.device) || !strings.Contains(err.Error(), "8:1")) {
					t.Errorf("want both device numbers as detail: %v", err)
				}
			}
			if calls != 3 {
				t.Errorf("upper read %d times, want 3", calls)
			}
		})
	}
}

// TestMediumFollowsASymlinkedUpper uses a real link and fixture mount IDs:
// the link is on ext4, its target on another filesystem. No host mount table
// or real cross-filesystem mount is needed. The statfs fixture follows the
// same link as Linux statfs/stat, and mount identity must describe its target.
func TestMediumFollowsASymlinkedUpper(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating a symlink requires privileges on Windows")
	}
	base := realTempDir(t)
	target := filepath.Join(base, "target")
	link := filepath.Join(base, "up")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	// mountIDOf deliberately describes the link itself, as the strict
	// writer requires; following it must be the upper reader's decision.
	stubMountIDs(t, func(p string) (uint64, bool, error) {
		switch p {
		case link:
			return 41, true, nil
		case target:
			return 40, true, nil
		default:
			t.Fatalf("unexpected mount lookup %s", p)
			return 0, false, nil
		}
	})
	for _, tc := range []struct {
		name, fstype, options, want string
		typ                         uint32
		mismatch                    bool
		err                         error
	}{
		{"XFS evidence", "xfs", "rw", "visible here as xfs (mount 40, mount device 8:2, path device 8:2)", 0x58465342, true, ErrMediumUnknown},
		{"nested overlay", "overlay", "rw,lowerdir=/l,upperdir=/ram,workdir=/w", "upper layer /ram is on tmpfs", overlayfsMagic, false, ErrVolatile},
		{"FUSE", "fuse.fuse-overlayfs", "rw", "fuse-overlayfs", fuseMagic, false, ErrMediumUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			escaped := strings.NewReplacer(`\`, `\134`, " ", `\040`, "\t", `\011`, "\n", `\012`, ",", `\054`, "=", `\075`).Replace(filepath.ToSlash(link))
			table := fmt.Sprintf("30 1 0:30 / / rw - overlay overlay rw,lowerdir=/l,upperdir=%s,workdir=/w\n"+
				"40 1 8:2 / /target rw - %s disk %s\n"+
				"41 1 8:1 / /links rw - ext4 disk rw\n", escaped, tc.fstype, tc.options)
			statfs := func(p string) (fsStat, error) {
				st := fsStat{Type: overlayfsMagic, Bsize: 4096, Blocks: 100, Files: 100}
				switch p {
				case "/data":
				case link:
					resolved, err := filepath.EvalSymlinks(p)
					if err != nil || resolved != target {
						t.Fatalf("statfs target = %q, %v; want %q", resolved, err, target)
					}
					st.Type, st.Device = tc.typ, "8:2"
					if tc.mismatch {
						st.Files--
					}
				case "/ram":
					st.Type = tmpfsMagic
				default:
					t.Fatalf("unexpected statfs %s", p)
				}
				return st, nil
			}
			err := mediumFrom("/data", 30, statfs, func() ([]byte, error) { return []byte(table), nil }, mediumUpperMountID)
			if !errors.Is(err, tc.err) || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "visible here as ext4") {
				t.Fatalf("medium = %v; want %v naming %q", err, tc.err, tc.want)
			}
			if id, ok, err := mountIDOf(link); err != nil || !ok || id != 41 {
				t.Fatalf("strict reader = %d, %v, %v; want link's mount 41", id, ok, err)
			}
		})
	}
}
