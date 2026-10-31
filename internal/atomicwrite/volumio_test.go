package atomicwrite

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"strings"
	"testing"
)

// TestVolumioVersionReadsOSReleaseAsTheShellDoes pins the one key the check
// reads of /etc/os-release (os-release(5)): VOLUMIO_VERSION's last
// assignment, a line's leading blanks ignored, a comment never one, a
// double-quoted value's escapes undone, a single-quoted value taken as it
// is, a bare word ending at a blank or a '#', and a quote that does not
// close, an empty value and another key's line no version.
func TestVolumioVersionReadsOSReleaseAsTheShellDoes(t *testing.T) {
	for _, tc := range []struct{ file, want string }{
		{piVolumioOSRelease, "4.231"},
		{`VOLUMIO_VERSION="4.231"` + "\r\n", "4.231"},
		{"  VOLUMIO_VERSION=4.231 # the release\n", "4.231"},
		{"VOLUMIO_VERSION='4.2\"31'\n", `4.2"31`},
		{`VOLUMIO_VERSION="4.\"2\\31"`, `4."2\31`},
		{"VOLUMIO_VERSION=\"4.1\"\nVOLUMIO_VERSION=\"4.231\"\n", "4.231"},
		{"VOLUMIO_VERSION=\"4.231\"\nVOLUMIO_VERSION=\n", ""},
		{"# VOLUMIO_VERSION=\"4.231\"\n", ""},
		{"VOLUMIO_VERSION=\"4.231\n", ""},
		{"VOLUMIO_VERSION_ID=\"4.231\"\nXVOLUMIO_VERSION=\"4.231\"\n", ""},
		{piRaspiOSOSRelease, ""},
		{"", ""},
	} {
		if got := volumioVersion([]byte(tc.file)); got != tc.want {
			t.Errorf("volumioVersion(%q) = %q, want %q", tc.file, got, tc.want)
		}
	}
}

// TestUdevPropertiesAreTheEntrysAssignments pins the reading of udev's
// database entry for a device: its E: lines, each a KEY=value, the other
// kinds of line (S:, I:, G: and the rest) passed over, and an entry with an
// E: line that is no assignment, or that gives a key twice, no reading.
func TestUdevPropertiesAreTheEntrysAssignments(t *testing.T) {
	got, ok := udevProperties([]byte("S:disk/by-label/volumio_data\nI:5821036\nE:ID_FS_TYPE=ext4\nE:ID_FS_LABEL=volumio_data\nE:ID_FS_LABEL_ENC=volumio_data\nG:systemd\n"))
	want := map[string]string{"ID_FS_TYPE": "ext4", "ID_FS_LABEL": "volumio_data", "ID_FS_LABEL_ENC": "volumio_data"}
	if !ok || !maps.Equal(got, want) {
		t.Errorf("udevProperties = %v, %v, want %v", got, ok, want)
	}
	for _, entry := range []string{
		"E:ID_FS_TYPE=ext4\nE:ID_FS_TYPE=vfat\n",
		"E:ID_FS_TYPE\n",
		"E:=ext4\n",
	} {
		if got, ok := udevProperties([]byte(entry)); ok {
			t.Errorf("udevProperties(%q) = %v, want no reading", entry, got)
		}
	}
}

// TestMediumRefusesWhatTheBoxsFilesLeaveUncertain pins the check's refusals
// where a fact of Volumio's layout cannot be read whole, beside the measured
// box: two directories under the disk whose partition files both read 3, a
// partition number that cannot be read, a mount table with a line that does
// not read as one (so a mount of partition 3 could hide there), a /sys/fs/ext4
// that cannot be read, and an /imgpart whose partition is the disk itself.
// Each is refused, never taken.
func TestMediumRefusesWhatTheBoxsFilesLeaveUncertain(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(b *volumioBox)
		reason string
	}{
		{"two partitions 3", func(b *volumioBox) {
			b.boot.files[piSD+"/mmcblk0p4/dev"] = "179:4\n"
			b.boot.files[piSD+"/mmcblk0p4/partition"] = "3\n"
		}, "mmcblk0 lists more than one partition 3 (mmcblk0p3, mmcblk0p4)"},
		{"a partition number that cannot be read", func(b *volumioBox) {
			b.boot.fail[piSD+"/mmcblk0p1/partition"] = fs.ErrPermission
		}, "the partition number of mmcblk0p1 cannot be read"},
		{"a mount table line that does not read as one", func(b *volumioBox) {
			b.table += "170 31 179:3 / /media/cut rw\n"
		}, "has a line that does not read as one"},
		{"a /sys/fs/ext4 that cannot be read", func(b *volumioBox) {
			b.boot.fail["/sys/fs/ext4/mmcblk0p3"] = fs.ErrPermission
		}, "whether an ext4 superblock of partition 3 (mmcblk0p3) is live cannot be told"},
		{"an /imgpart on a whole disk", func(b *volumioBox) {
			b.line(26, "26 31 179:0 / /imgpart rw,relatime shared:3 - ext4 /dev/mmcblk0 rw")
			b.boot.links["/sys/dev/block/179:0"] = piSD
		}, "/imgpart is on no partition: its device 179:0 (mmcblk0) is a whole disk"},
		{"a disk that is a partition itself", func(b *volumioBox) {
			b.boot.files[piSD+"/partition"] = "1\n"
		}, "is no disk: it is a partition itself"},
		{"udev naming no filesystem", func(b *volumioBox) {
			b.boot.files["/run/udev/data/b179:3"] = "E:ID_FS_LABEL=volumio_data\n"
		}, "udev names no filesystem on partition 3 (mmcblk0p3)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := piVolumio()
			tc.change(b)
			m := b.inspect()
			if !errors.Is(m.Err, ErrMediumUnknown) || m.Storage != "" || !strings.Contains(m.Err.Error(), "not Volumio's data partition") || !strings.Contains(m.Err.Error(), tc.reason) {
				t.Fatalf("the medium = %+v, want ErrMediumUnknown naming %q", m, tc.reason)
			}
		})
	}
}

// TestTheVolumioCheckReadsNothingButWhatItNames pins what the fallback reads
// of the box: os-release, the two sysfs links of /sys/dev/block it resolves
// (/imgpart's device and partition 3's), the dev and partition files under
// the SD card, udev's entry for partition 3 and /sys/fs/ext4's for it, and
// never a by-label link, the NVMe, or anything under /mnt/ext.
func TestTheVolumioCheckReadsNothingButWhatItNames(t *testing.T) {
	b := piVolumio()
	var asked []string
	b.boot.fail = map[string]error{}
	rec := &recordingBoot{fakeBoot: b.boot, asked: &asked}
	m := inspectMedium(b.root, b.rootID, mediumReaders{
		statfs:    b.statfs,
		mountinfo: func() ([]byte, error) { return []byte(b.table), nil },
		idOf: func(p string) (uint64, bool, error) {
			id, ok := b.ids[p]
			return id, ok, nil
		},
		boot: rec,
	})
	if m.Err != nil {
		t.Fatalf("the medium of the measured box = %v", m.Err)
	}
	for _, p := range asked {
		if strings.Contains(p, "by-label") || strings.Contains(p, "nvme") || strings.HasPrefix(p, "/mnt/ext") {
			t.Errorf("the check read %s", p)
		}
	}
	if want := []string{"read /etc/os-release", "resolve /sys/dev/block/179:2", "resolve /sys/dev/block/179:3", "read /run/udev/data/b179:3", "exists /sys/fs/ext4/mmcblk0p3"}; !containsInOrder(asked, want) {
		t.Errorf("the check read %v, want %v among them, in that order", asked, want)
	}
}

// recordingBoot is a fakeBoot that records each read, as "read", "list",
// "resolve" or "exists" and the path.
type recordingBoot struct {
	*fakeBoot
	asked *[]string
}

func (r *recordingBoot) note(op, name string) { *r.asked = append(*r.asked, op+" "+name) }

func (r *recordingBoot) ReadFile(name string) ([]byte, error) {
	r.note("read", name)
	return r.fakeBoot.ReadFile(name)
}

func (r *recordingBoot) ReadDir(name string) ([]bootEntry, error) {
	r.note("list", name)
	return r.fakeBoot.ReadDir(name)
}

func (r *recordingBoot) Resolve(name string) (string, error) {
	r.note("resolve", name)
	return r.fakeBoot.Resolve(name)
}

func (r *recordingBoot) Exists(name string) (bool, error) {
	r.note("exists", name)
	return r.fakeBoot.Exists(name)
}

// containsInOrder reports whether want's items appear in got in that order.
func containsInOrder(got, want []string) bool {
	i := 0
	for _, g := range got {
		if i < len(want) && g == want[i] {
			i++
		}
	}
	return i == len(want)
}

// TestFakeBootIsTheKernelsShape keeps the fake the decision tests run on to
// the trees it stands for: a link resolved relative to its own directory, a
// directory implied by the paths below it, a link listed as no directory,
// and a path that is not there ENOENT.
func TestFakeBootIsTheKernelsShape(t *testing.T) {
	b := piBoot()
	if got, err := b.Resolve("/sys/dev/block/179:3"); err != nil || got != piSD+"/mmcblk0p3" {
		t.Errorf("Resolve(179:3) = %q, %v", got, err)
	}
	b.links["/sys/block/mmcblk0"] = "../devices/platform/soc@107c000000/1000fff000.mmc/mmc_host/mmc0/mmc0:59b4/block/mmcblk0"
	if got, err := b.ReadFile("/sys/block/mmcblk0/mmcblk0p2/partition"); err != nil || string(got) != "2\n" {
		t.Errorf("a read through a relative link = %q, %v", got, err)
	}
	entries, err := b.ReadDir("/sys/block")
	if err != nil || len(entries) != 1 || entries[0] != (bootEntry{name: "mmcblk0"}) {
		t.Errorf("ReadDir(/sys/block) = %v, %v, want the link as no directory", entries, err)
	}
	entries, err = b.ReadDir(piSD)
	var names []string
	for _, e := range entries {
		names = append(names, fmt.Sprintf("%s:%v", e.name, e.dir))
	}
	if want := "dev:false mmcblk0p1:true mmcblk0p2:true mmcblk0p3:true queue:true size:false"; err != nil || strings.Join(names, " ") != want {
		t.Errorf("ReadDir(the SD card) = %v, %v, want %s", names, err, want)
	}
	if _, err := b.ReadFile("/run/udev/data/b179:9"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a read of a path that is not there = %v, want ENOENT", err)
	}
	if ok, err := b.Exists("/sys/fs/ext4/mmcblk0p3"); !ok || err != nil {
		t.Errorf("Exists(an implied directory) = %v, %v", ok, err)
	}
}
