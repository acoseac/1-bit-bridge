package atomicwrite

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"path"
	"slices"
	"strings"
	"syscall"
	"testing"
)

// fakeBoot is a box's own files as the Volumio check reads them (bootFiles):
// each file's bytes by its path, the links among them, and the paths whose
// every read fails. A directory is implied by the paths below it, so it
// stands for the kernel's trees without a filesystem of the test's, and runs
// wherever the tests do (a sysfs name holds a colon, which Windows refuses).
type fakeBoot struct {
	files map[string]string
	// links maps a link's path to its target, absolute or relative to the
	// link's directory, as readlink gives it.
	links map[string]string
	fail  map[string]error
}

// clone is a copy of b a case can change.
func (b *fakeBoot) clone() *fakeBoot {
	return &fakeBoot{files: maps.Clone(b.files), links: maps.Clone(b.links), fail: maps.Clone(b.fail)}
}

// walk is name with its links resolved, element by element, a link's target
// walked again from the root, as the kernel resolves a path.
func (b *fakeBoot) walk(name string) (string, error) {
	cur := "/"
	rest := strings.Split(strings.TrimPrefix(path.Clean(name), "/"), "/")
	for hops := 0; len(rest) > 0; {
		el := rest[0]
		rest = rest[1:]
		if el == "" {
			continue
		}
		next := path.Join(cur, el)
		target, ok := b.links[next]
		if !ok {
			cur = next
			continue
		}
		if hops++; hops > 40 {
			return "", &fs.PathError{Op: "resolve", Path: name, Err: syscall.ELOOP}
		}
		if !path.IsAbs(target) {
			target = path.Join(cur, target)
		}
		rest = append(strings.Split(strings.TrimPrefix(path.Clean(target), "/"), "/"), rest...)
		cur = "/"
	}
	return cur, nil
}

// kind says what p is: a file, a directory (one the paths below it imply),
// or nothing.
func (b *fakeBoot) kind(p string) (file, dir bool) {
	if _, ok := b.files[p]; ok {
		return true, false
	}
	if p == "/" {
		return false, true
	}
	for _, keys := range []map[string]string{b.files, b.links} {
		for k := range keys {
			if strings.HasPrefix(k, p+"/") {
				return false, true
			}
		}
	}
	return false, false
}

// reach is name resolved, or the failure a case set for it.
func (b *fakeBoot) reach(op, name string) (string, error) {
	if err, ok := b.fail[name]; ok {
		return "", &fs.PathError{Op: op, Path: name, Err: err}
	}
	p, err := b.walk(name)
	if err != nil {
		return "", err
	}
	if err, ok := b.fail[p]; ok {
		return "", &fs.PathError{Op: op, Path: name, Err: err}
	}
	return p, nil
}

func (b *fakeBoot) ReadFile(name string) ([]byte, error) {
	p, err := b.reach("open", name)
	if err != nil {
		return nil, err
	}
	switch file, dir := b.kind(p); {
	case file:
		return []byte(b.files[p]), nil
	case dir:
		return nil, &fs.PathError{Op: "read", Path: name, Err: syscall.EISDIR}
	}
	return nil, &fs.PathError{Op: "open", Path: name, Err: syscall.ENOENT}
}

func (b *fakeBoot) ReadDir(name string) ([]bootEntry, error) {
	p, err := b.reach("open", name)
	if err != nil {
		return nil, err
	}
	if _, dir := b.kind(p); !dir {
		return nil, &fs.PathError{Op: "open", Path: name, Err: syscall.ENOTDIR}
	}
	seen := map[string]bool{}
	for _, keys := range []map[string]string{b.files, b.links} {
		for k := range keys {
			rest, ok := strings.CutPrefix(k, strings.TrimSuffix(p, "/")+"/")
			if !ok {
				continue
			}
			child, below, deeper := strings.Cut(rest, "/")
			_, isLink := b.links[path.Join(p, child)]
			seen[child] = seen[child] || (deeper && below != "" && !isLink)
		}
	}
	var out []bootEntry
	for _, n := range slices.Sorted(maps.Keys(seen)) {
		out = append(out, bootEntry{name: n, dir: seen[n]})
	}
	return out, nil
}

func (b *fakeBoot) Resolve(name string) (string, error) {
	p, err := b.reach("resolve", name)
	if err != nil {
		return "", err
	}
	if file, dir := b.kind(p); !file && !dir {
		return "", &fs.PathError{Op: "resolve", Path: name, Err: syscall.ENOENT}
	}
	return p, nil
}

func (b *fakeBoot) Exists(name string) (bool, error) {
	p, err := b.reach("stat", name)
	if err != nil {
		return false, err
	}
	file, dir := b.kind(p)
	return file || dir, nil
}

// piVolumioMountInfo is the owner's Volumio 4.231 on a Raspberry Pi 5
// (B603), each line whole as its kernel wrote it: the five lines
// pi-mountinfo.txt captured on the box (the image partition at /imgpart, the
// squashfs, the root overlay, /boot and the NVMe's partition 3), and the
// four tmpfs lines pi-evidence.txt captured beside the root's. The root
// overlay's upper layer, /mnt/ext/dyn, is on partition 3 of the SD card
// (179:3), which the initramfs mounted at /mnt/ext before the pivot and no
// line lists.
const piVolumioMountInfo = `26 31 179:2 / /imgpart rw,relatime shared:3 - ext4 /dev/mmcblk0p2 rw
27 31 7:0 / /static rw,relatime shared:2 - squashfs /dev/loop0 ro,errors=continue
31 1 0:23 / / rw,relatime shared:1 - overlay overlay rw,lowerdir=/mnt/static,upperdir=/mnt/ext/dyn,workdir=/mnt/ext/work
80 31 179:1 / /boot rw,nosuid,nodev,noexec,relatime shared:34 - vfat /dev/mmcblk0p1 rw,fmask=0111,dmask=0000,allow_utime=0022,codepage=437,iocharset=ascii,shortname=mixed,utf8,errors=remount-ro
110 31 259:3 / /media/NVME rw,noatime shared:57 - ext4 /dev/nvme0n1p3 rw
98 31 0:36 / /tmp rw,noatime shared:49 - tmpfs tmpfs rw,mode=755
101 31 0:37 / /var/log rw,nodev,relatime shared:51 - tmpfs tmpfs rw,size=20480k,mode=777,uid=1000,gid=4
104 31 0:38 / /var/spool/cups rw,noatime shared:53 - tmpfs tmpfs rw,mode=755
107 104 0:39 / /var/spool/cups/tmp rw,noatime shared:55 - tmpfs tmpfs rw,mode=755
`

// The box's two disks in sysfs, where pi-mountinfo.txt's links of 179:2,
// 179:3 and 259:3 lead: the SD card and the NVMe.
const (
	piSD   = "/sys/devices/platform/soc@107c000000/1000fff000.mmc/mmc_host/mmc0/mmc0:59b4/block/mmcblk0"
	piNVMe = "/sys/devices/platform/axi/1000110000.pcie/pci0001:00/0001:00:00.0/0001:01:00.0/nvme/nvme0/nvme0n1"
)

// The data directory on the box, and the os-release files the cases read:
// Volumio's, synthetic but for the two VOLUMIO_ lines measured on the box,
// and Raspberry Pi OS's, which names no VOLUMIO_VERSION.
const (
	piData             = "/data/1-bit-bridge/config/data"
	piVolumioOSRelease = "PRETTY_NAME=\"Debian GNU/Linux 12 (bookworm)\"\nNAME=\"Debian GNU/Linux\"\nVERSION_ID=\"12\"\nVERSION_CODENAME=bookworm\nID=debian\n# VOLUMIO_VERSION=\"\"\nVOLUMIO_VERSION=\"4.231\"\nVOLUMIO_HARDWARE=\"pi\"\n"
	piRaspiOSOSRelease = "PRETTY_NAME=\"Debian GNU/Linux 12 (bookworm)\"\nNAME=\"Debian GNU/Linux\"\nVERSION_ID=\"12\"\nVERSION_CODENAME=bookworm\nID=debian\n"
	piUdevVolumioData  = "E:ID_FS_TYPE=ext4\nE:ID_FS_LABEL=volumio_data\n"
)

// piBoot is the box's own files as the check reads them: os-release, the
// links of /sys/dev/block that pi-mountinfo.txt captured and the partitions
// under each disk (each partition's dev and partition files, the disk's own
// dev file, and a directory and a file of the disk's that are no partition),
// udev's database entries for the two partitions labelled volumio_data
// (their two lines measured on the box), and /sys/fs/ext4, which listed
// mmcblk0p2, mmcblk0p3 and nvme0n1p3. The SD card's partition 1 (179:1, at
// /boot) and the disks' own numbers are as the kernel gives them, written
// for the tests.
func piBoot() *fakeBoot {
	return &fakeBoot{
		files: map[string]string{
			"/etc/os-release":                     piVolumioOSRelease,
			piSD + "/dev":                         "179:0\n",
			piSD + "/size":                        "62333952\n",
			piSD + "/queue/rotational":            "0\n",
			piSD + "/mmcblk0p1/dev":               "179:1\n",
			piSD + "/mmcblk0p1/partition":         "1\n",
			piSD + "/mmcblk0p2/dev":               "179:2\n",
			piSD + "/mmcblk0p2/partition":         "2\n",
			piSD + "/mmcblk0p3/dev":               "179:3\n",
			piSD + "/mmcblk0p3/partition":         "3\n",
			piNVMe + "/dev":                       "259:0\n",
			piNVMe + "/nvme0n1p3/dev":             "259:3\n",
			piNVMe + "/nvme0n1p3/partition":       "3\n",
			"/run/udev/data/b179:3":               piUdevVolumioData,
			"/run/udev/data/b259:3":               piUdevVolumioData,
			"/sys/fs/ext4/mmcblk0p2/errors_count": "0\n",
			"/sys/fs/ext4/mmcblk0p3/errors_count": "0\n",
			"/sys/fs/ext4/nvme0n1p3/errors_count": "0\n",
		},
		links: map[string]string{
			"/sys/dev/block/179:2": piSD + "/mmcblk0p2",
			"/sys/dev/block/179:3": piSD + "/mmcblk0p3",
			"/sys/dev/block/259:3": piNVMe + "/nvme0n1p3",
		},
		fail: map[string]error{},
	}
}

// volumioBox is the box a case inspects: its mount table, the mount IDs a
// path reaches (following a final link, as upperMountID does), statfs's
// answers, its own files, and the root with its mount's ID.
type volumioBox struct {
	table  string
	ids    map[string]uint64
	statfs func(string) (fsStat, error)
	boot   *fakeBoot
	root   string
	rootID uint64
}

// piOverlay is statfs of the root overlay, which gives its upper layer's
// totals: the data partition's.
var piOverlay = fsStat{Type: overlayfsMagic, Bsize: 4096, Blocks: 7225344, Files: 1806336}

// statfsOf answers from stats, and with ENOENT for a path it does not hold,
// as statfs answers for /mnt/ext/dyn on the box.
func statfsOf(stats map[string]fsStat) func(string) (fsStat, error) {
	return func(p string) (fsStat, error) {
		if st, ok := stats[p]; ok {
			return st, nil
		}
		return fsStat{}, &fs.PathError{Op: "statfs", Path: p, Err: syscall.ENOENT}
	}
}

// piVolumio is the box as measured, its data directory on the root overlay.
func piVolumio() *volumioBox {
	return &volumioBox{
		table:  piVolumioMountInfo,
		ids:    map[string]uint64{"/": 31, "/imgpart": 26},
		statfs: statfsOf(map[string]fsStat{piData: piOverlay}),
		boot:   piBoot(),
		root:   piData,
		rootID: 31,
	}
}

// line replaces the mount table's line for mount id with s, or adds s
// where the table has no such line, or drops the line where s is empty.
func (b *volumioBox) line(id uint64, s string) {
	var out []string
	found := false
	for _, l := range strings.Split(strings.TrimSuffix(b.table, "\n"), "\n") {
		if strings.HasPrefix(l, fmt.Sprintf("%d ", id)) {
			found = true
			if s != "" {
				out = append(out, s)
			}
			continue
		}
		out = append(out, l)
	}
	if !found && s != "" {
		out = append(out, s)
	}
	b.table = strings.Join(out, "\n") + "\n"
}

// inspect is the medium check of the box's root, on what the box answers.
func (b *volumioBox) inspect() Medium {
	return inspectMedium(b.root, b.rootID, mediumReaders{
		statfs:    b.statfs,
		mountinfo: func() ([]byte, error) { return []byte(b.table), nil },
		idOf: func(p string) (uint64, bool, error) {
			id, ok := b.ids[p]
			return id, ok, nil
		},
		boot: b.boot,
	})
}

// TestMediumTakesVolumiosHiddenDataPartition pins B603's acceptance, the
// orchestrator's decision: on the owner's Volumio Pi 5 the data directory is
// on the root overlay, whose upper layer /mnt/ext/dyn no path reaches after
// the pivot (statfs answers ENOENT), and on main the check refused it as one
// whose upper layer "cannot be read here", so the player never started. The
// box shows Volumio's boot layout, every fact as measured: its os-release
// names a VOLUMIO_VERSION; the root overlay is mounted read-write over
// /mnt/static with /mnt/ext/dyn its upper layer; /imgpart is partition 2 of
// the SD card; the card's partition 3 is ext4 labelled volumio_data in
// udev's database; and its ext4 superblock is live while no line of the
// mount table lists it. The check takes the root, naming the partition, and
// a label lookup alone would have named the NVMe's.
func TestMediumTakesVolumiosHiddenDataPartition(t *testing.T) {
	m := piVolumio().inspect()
	if m.Err != nil {
		t.Fatalf("the medium of Volumio's data directory = %v, want it taken", m.Err)
	}
	if want := "Volumio's data partition (mmcblk0p3, ext4)"; m.Storage != want {
		t.Errorf("the medium names the storage %q, want %q", m.Storage, want)
	}
}

// TestMediumRefusesWhatVolumiosLayoutDoesNotShow pins the fallback's
// refusals: every one of its conditions must hold, and where one does not
// the root is refused as on main (ErrMediumUnknown, the upper layer that
// cannot be read here, and statfs's ENOENT), with a reason naming the first
// condition that failed. Each case changes the measured box in one fact, so
// it fails that condition alone: the box is not Volumio (a lookalike upper
// layer on Raspberry Pi OS, a VOLUMIO_VERSION that is empty, no os-release
// or one that cannot be read); the root overlay is not as Volumio mounts it
// (mounted read-only, its superblock read-only, another lower or upper
// layer, not the overlay at /); /imgpart is not partition 2 of a disk (no
// /imgpart mount, the NVMe's decoy partition the only one labelled
// volumio_data left; another partition number; an overmount; sysfs naming
// another device); partition 3 is not the data partition (absent, not ext4,
// labelled otherwise, a udev entry that cannot be read, a sibling whose
// device sysfs puts under the NVMe); and its superblock is not live and
// hidden (no /sys/fs/ext4 entry, as after a boot that fell back to tmpfs;
// mounted where the process sees it). The refusals are preservation cases:
// main refused each of them for the same upper layer.
func TestMediumRefusesWhatVolumiosLayoutDoesNotShow(t *testing.T) {
	const today = "/data/1-bit-bridge/config/data is on an overlay whose upper layer /mnt/ext/dyn, as the mount table names it, cannot be read here: statfs /mnt/ext/dyn: no such file or directory"
	for _, tc := range []struct {
		name   string
		change func(b *volumioBox)
		reason string
		// today is main's refusal where it is not the const's: for a case
		// that changes the upper layer the table names.
		today string
	}{
		{name: "a lookalike upper layer on a box that is not Volumio", change: func(b *volumioBox) {
			b.boot.files["/etc/os-release"] = piRaspiOSOSRelease
		}, reason: "/etc/os-release names no VOLUMIO_VERSION"},
		{name: "an empty VOLUMIO_VERSION", change: func(b *volumioBox) {
			b.boot.files["/etc/os-release"] = strings.ReplaceAll(piVolumioOSRelease, `VOLUMIO_VERSION="4.231"`, `VOLUMIO_VERSION=""`)
		}, reason: "/etc/os-release names no VOLUMIO_VERSION"},
		{name: "no os-release", change: func(b *volumioBox) {
			delete(b.boot.files, "/etc/os-release")
		}, reason: "no /etc/os-release"},
		{name: "an os-release that cannot be read", change: func(b *volumioBox) {
			b.boot.fail["/etc/os-release"] = syscall.EACCES
		}, reason: "/etc/os-release cannot be read"},
		{name: "the root overlay mounted read-only", change: func(b *volumioBox) {
			b.line(31, "31 1 0:23 / / ro,relatime shared:1 - overlay overlay ro,lowerdir=/mnt/static,upperdir=/mnt/ext/dyn,workdir=/mnt/ext/work")
		}, reason: "not mounted read-write"},
		{name: "the root overlay's mount read-only over a writable superblock", change: func(b *volumioBox) {
			b.line(31, "31 1 0:23 / / ro,relatime shared:1 - overlay overlay rw,lowerdir=/mnt/static,upperdir=/mnt/ext/dyn,workdir=/mnt/ext/work")
		}, reason: "not mounted read-write"},
		{name: "another lower layer", change: func(b *volumioBox) {
			b.line(31, "31 1 0:23 / / rw,relatime shared:1 - overlay overlay rw,lowerdir=/media/root-ro,upperdir=/mnt/ext/dyn,workdir=/mnt/ext/work")
		}, reason: "lower layer is /media/root-ro, not /mnt/static"},
		{name: "another upper layer", change: func(b *volumioBox) {
			b.line(31, "31 1 0:23 / / rw,relatime shared:1 - overlay overlay rw,lowerdir=/mnt/static,upperdir=/media/root-rw/overlay,workdir=/media/root-rw/work")
		}, reason: "upper layer is /media/root-rw/overlay, not /mnt/ext/dyn",
			today: "/data/1-bit-bridge/config/data is on an overlay whose upper layer /media/root-rw/overlay, as the mount table names it, cannot be read here: statfs /media/root-rw/overlay: no such file or directory"},
		{name: "an overlay that is not the one at /", change: func(b *volumioBox) {
			b.ids["/"] = 120
			b.line(120, "120 1 179:2 / / rw,relatime shared:90 - ext4 /dev/mmcblk0p2 rw")
		}, reason: "not the overlay at /"},
		{name: "the NVMe's decoy partition, with no /imgpart mount", change: func(b *volumioBox) {
			b.line(26, "")
			b.ids["/imgpart"] = 31
			b.line(110, "")
		}, reason: "/imgpart is no mount of its own"},
		{name: "/imgpart not partition 2", change: func(b *volumioBox) {
			b.boot.files[piSD+"/mmcblk0p2/partition"] = "4\n"
		}, reason: "partition 4 of its disk"},
		{name: "an overmounted /imgpart", change: func(b *volumioBox) {
			b.line(130, "130 26 0:60 / /imgpart rw,relatime shared:70 - tmpfs tmpfs rw")
			b.ids["/imgpart"] = 130
		}, reason: "no block device"},
		{name: "sysfs naming /imgpart's device another", change: func(b *volumioBox) {
			b.boot.files[piSD+"/mmcblk0p2/dev"] = "179:5\n"
		}, reason: "sysfs lists /imgpart's device 179:2 as 179:5"},
		{name: "partition 3 absent", change: func(b *volumioBox) {
			for k := range b.boot.files {
				if strings.HasPrefix(k, piSD+"/mmcblk0p3/") {
					delete(b.boot.files, k)
				}
			}
			delete(b.boot.links, "/sys/dev/block/179:3")
		}, reason: "no partition 3"},
		{name: "partition 3 not ext4", change: func(b *volumioBox) {
			b.boot.files["/run/udev/data/b179:3"] = "E:ID_FS_TYPE=vfat\nE:ID_FS_LABEL=volumio_data\n"
		}, reason: "is vfat, not ext4"},
		{name: "partition 3 labelled otherwise", change: func(b *volumioBox) {
			b.boot.files["/run/udev/data/b179:3"] = "E:ID_FS_TYPE=ext4\nE:ID_FS_LABEL=volumio\n"
		}, reason: "labelled \"volumio\", not volumio_data"},
		{name: "a udev entry that cannot be read", change: func(b *volumioBox) {
			b.boot.fail["/run/udev/data/b179:3"] = syscall.EACCES
		}, reason: "udev's database entry for partition 3 (mmcblk0p3, 179:3) cannot be read"},
		{name: "a sibling whose device sysfs puts under the NVMe", change: func(b *volumioBox) {
			b.boot.files[piSD+"/mmcblk0p3/dev"] = "259:3\n"
			b.line(110, "")
		}, reason: "not under mmcblk0"},
		{name: "no live ext4 superblock, as after a boot that fell back to tmpfs", change: func(b *volumioBox) {
			delete(b.boot.files, "/sys/fs/ext4/mmcblk0p3/errors_count")
		}, reason: "no ext4 superblock of partition 3 (mmcblk0p3) is live"},
		{name: "partition 3 mounted where the process sees it", change: func(b *volumioBox) {
			b.line(140, "140 31 179:3 / /media/data rw,relatime shared:60 - ext4 /dev/mmcblk0p3 rw")
		}, reason: "mounted where this process sees it, at /media/data"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := piVolumio()
			b.boot = b.boot.clone()
			tc.change(b)
			m := b.inspect()
			said := today
			if tc.today != "" {
				said = tc.today
			}
			if !errors.Is(m.Err, ErrMediumUnknown) || errors.Is(m.Err, ErrOverlayBind) || !errors.Is(m.Err, fs.ErrNotExist) || !strings.Contains(m.Err.Error(), said) {
				t.Fatalf("the medium = %v, want ErrMediumUnknown with statfs's ENOENT, saying %q", m.Err, said)
			}
			if m.Storage != "" {
				t.Errorf("a refused medium names its storage %q", m.Storage)
			}
			if !strings.Contains(m.Err.Error(), "not Volumio's data partition") || !strings.Contains(m.Err.Error(), tc.reason) {
				t.Errorf("the refusal %q does not name the condition that failed, %q", m.Err, tc.reason)
			}
		})
	}
}

// TestMediumKeepsEveryOtherRefusalBesideVolumiosEvidence pins the
// fallback's place (the orchestrator's amendment 4): it runs only where the
// first statfs of the root overlay's upper path answers ENOENT, and
// outranks no other refusal, whatever the box shows of Volumio's layout:
// the overlay mounted volatile or fsync=volatile, an upper path that reaches
// tmpfs, totals that stay inconclusive, an upper path statfs refuses for
// another reason than its absence, and a root on tmpfs or ramfs are refused
// as before; and a bind of a directory inside the root overlay is refused
// as one (ErrOverlayBind), by the verdict the fallback returns rather than
// a nil that would skip it.
func TestMediumKeepsEveryOtherRefusalBesideVolumiosEvidence(t *testing.T) {
	volumioLine := "31 1 0:23 / / rw,relatime shared:1 - overlay overlay rw,lowerdir=/mnt/static,upperdir=/mnt/ext/dyn,workdir=/mnt/ext/work"
	tmpfs := fsStat{Type: tmpfsMagic, Bsize: piOverlay.Bsize, Blocks: piOverlay.Blocks, Files: piOverlay.Files}
	for _, tc := range []struct {
		name   string
		change func(b *volumioBox)
		want   error
		says   string
	}{
		{"a bind of a directory inside the root overlay", func(b *volumioBox) {
			b.line(150, "150 31 0:23 /data/1-bit-bridge /data/1-bit-bridge rw,relatime shared:1 - overlay overlay rw,lowerdir=/mnt/static,upperdir=/mnt/ext/dyn,workdir=/mnt/ext/work")
			b.rootID = 150
		}, ErrOverlayBind, "is on a bind of /data/1-bit-bridge, a directory inside an overlay"},
		{"the root overlay mounted volatile", func(b *volumioBox) {
			b.line(31, volumioLine+",volatile")
		}, ErrVolatile, "mounted volatile"},
		{"the root overlay mounted fsync=volatile", func(b *volumioBox) {
			b.line(31, volumioLine+",fsync=volatile")
		}, ErrVolatile, "mounted volatile"},
		{"an upper path that reaches tmpfs", func(b *volumioBox) {
			b.statfs = statfsOf(map[string]fsStat{piData: piOverlay, "/mnt/ext/dyn": tmpfs})
		}, ErrVolatile, "whose upper layer /mnt/ext/dyn is on tmpfs"},
		{"an upper path whose totals stay inconclusive", func(b *volumioBox) {
			ext4 := fsStat{Type: 0xef53, Bsize: piOverlay.Bsize, Blocks: piOverlay.Blocks, Files: piOverlay.Files - 1}
			b.statfs = statfsOf(map[string]fsStat{piData: piOverlay, "/mnt/ext/dyn": ext4})
		}, ErrMediumUnknown, "inconclusive"},
		{"an upper path statfs refuses for another reason", func(b *volumioBox) {
			b.statfs = func(p string) (fsStat, error) {
				if p == "/mnt/ext/dyn" {
					return fsStat{}, &fs.PathError{Op: "statfs", Path: p, Err: syscall.EACCES}
				}
				return statfsOf(map[string]fsStat{piData: piOverlay})(p)
			}
		}, ErrMediumUnknown, "cannot be read here: statfs /mnt/ext/dyn: permission denied"},
		{"a root on tmpfs", func(b *volumioBox) {
			b.root, b.rootID = "/tmp/1-bit-bridge", 98
			b.statfs = statfsOf(map[string]fsStat{b.root: tmpfs})
		}, ErrVolatile, "/tmp/1-bit-bridge is on tmpfs"},
		{"a root on ramfs", func(b *volumioBox) {
			b.root, b.rootID = "/run/1-bit-bridge", 160
			b.line(160, "160 31 0:61 / /run/1-bit-bridge rw,relatime shared:80 - ramfs ramfs rw")
			b.statfs = statfsOf(map[string]fsStat{b.root: {Type: ramfsMagic, Bsize: 4096}})
		}, ErrVolatile, "/run/1-bit-bridge is on ramfs"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := piVolumio()
			b.boot = b.boot.clone()
			tc.change(b)
			m := b.inspect()
			if !errors.Is(m.Err, tc.want) || !strings.Contains(m.Err.Error(), tc.says) {
				t.Fatalf("the medium = %v, want %v saying %q", m.Err, tc.want, tc.says)
			}
			if strings.Contains(m.Err.Error(), "Volumio") {
				t.Errorf("a refusal the fallback does not outrank speaks of Volumio: %v", m.Err)
			}
			if m.Storage != "" {
				t.Errorf("a refused medium names its storage %q", m.Storage)
			}
		})
	}
}
