package atomicwrite

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

// Volumio's boot layout, as the owner's Volumio 4.231 on a Raspberry Pi 5
// showed it (B603), the one layout the medium check takes where no path here
// reaches the root overlay's upper layer: Volumio's initramfs mounts the boot
// disk's partition 3, ext4 labelled volumio_data, at /mnt/ext, and the root
// overlay over the squashfs at /mnt/static with /mnt/ext/dyn its upper layer;
// it mounts the image's partition, 2, at /imgpart, which stays after the
// pivot; and nothing mounts /mnt/ext again after it, so that mount, and the
// upper layer on it, is hidden from the running system.
const (
	volumioLowerDir   = "/mnt/static"
	volumioUpperDir   = "/mnt/ext/dyn"
	volumioImageMount = "/imgpart"
	volumioImagePart  = "2"
	volumioDataPart   = "3"
	volumioDataFSType = "ext4"
	volumioDataLabel  = "volumio_data"
)

// volumioUpper answers whether the upper layer of the overlay that is the
// root's own mount, which no path here reaches, is Volumio's data partition
// (B603): its name where every one of Volumio's facts holds, in this order,
// and otherwise the first that does not. The box is Volumio: its os-release
// names a VOLUMIO_VERSION. The overlay is the root overlay as Volumio mounts
// it: read-write, its mount and its superblock, over /mnt/static, its upper
// layer /mnt/ext/dyn, and the overlay at /, the mount / lies on (by its mount
// ID) showing that superblock's own root. /imgpart, by its own mount's line
// (by its mount ID, never a line picked by its spelling), is partition 2 of a
// disk: sysfs's device for the line's major:minor gives that major:minor
// again, its partition number is 2, and the directory above it is a disk.
// The disk's partition 3, a directory under it in sysfs that is the very
// device sysfs names by its own major:minor, is ext4 labelled volumio_data
// in udev's database. And its ext4 superblock is live, /sys/fs/ext4 listing
// it, and hidden: no line of the mount table lists its major:minor, so it is
// the mount the initramfs made before the pivot, not one made since, where
// the upper path, mounted at /mnt/ext, decides. A device's name is never
// edited as a string (mmcblk0p2 to p3, which /dev/root and by-uuid spellings
// break), and the partition is never found by its label, which the owner's
// NVMe, imaged with Volumio too, shares: /dev/disk/by-label/volumio_data
// points at it, not at the card's partition that holds the upper layer.
//
// This trusts Volumio's boot layout: the evidence shows the box booted that
// way, and a modified boot that keeps every one of these facts and puts a
// memory filesystem at /mnt/ext is what it cannot tell (the orchestrator's
// residual).
func volumioUpper(table []byte, overlay mountRecord, r mediumReaders) (string, error) {
	if r.boot == nil {
		return "", errors.New("none of the box's own files that would show it were read")
	}
	if err := volumioRelease(r.boot); err != nil {
		return "", err
	}
	rootID, err := volumioRootOverlay(table, overlay, r.idOf)
	if err != nil {
		return "", err
	}
	disk, err := volumioImageDisk(table, rootID, r)
	if err != nil {
		return "", err
	}
	name, dev, err := volumioDataPartition(disk, r.boot)
	if err != nil {
		return "", err
	}
	if err := liveAndHidden(table, name, dev, r.boot); err != nil {
		return "", err
	}
	return "Volumio's data partition (" + name + ", " + volumioDataFSType + ")", nil
}

// volumioRelease answers whether the box is Volumio: its /etc/os-release
// names a VOLUMIO_VERSION that is not empty.
func volumioRelease(boot bootFiles) error {
	b, err := boot.ReadFile("/etc/os-release")
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return errors.New("this box has no /etc/os-release, so it is not Volumio")
	case err != nil:
		return fmt.Errorf("/etc/os-release cannot be read: %w", err)
	case volumioVersion(b) == "":
		return errors.New("/etc/os-release names no VOLUMIO_VERSION, so this box is not Volumio")
	}
	return nil
}

// volumioVersion is the VOLUMIO_VERSION an os-release file gives
// (os-release(5)), "" for none: its last assignment, a line's leading blanks
// ignored and a comment never one, its value unquoted as the shell unquotes
// the forms the file takes (a double-quoted string, whose backslash escapes
// a backslash, a quote, a dollar sign or a backquote; a single-quoted one; a
// bare word, which ends at a blank or a '#'). A quote that does not close is
// no value.
func volumioVersion(osRelease []byte) string {
	v := ""
	for _, line := range strings.Split(string(osRelease), "\n") {
		line = strings.TrimLeft(strings.TrimSuffix(line, "\r"), " \t")
		if raw, ok := strings.CutPrefix(line, "VOLUMIO_VERSION="); ok {
			v = osReleaseWord(raw)
		}
	}
	return v
}

// osReleaseWord is an os-release value unquoted (volumioVersion).
func osReleaseWord(raw string) string {
	switch {
	case strings.HasPrefix(raw, `"`):
		var b strings.Builder
		for i := 1; i < len(raw); i++ {
			switch c := raw[i]; {
			case c == '"':
				return b.String()
			case c == '\\' && i+1 < len(raw) && strings.IndexByte("\\\"$`", raw[i+1]) >= 0:
				i++
				b.WriteByte(raw[i])
			default:
				b.WriteByte(c)
			}
		}
		return ""
	case strings.HasPrefix(raw, "'"):
		if end := strings.IndexByte(raw[1:], '\''); end >= 0 {
			return raw[1 : 1+end]
		}
		return ""
	}
	if i := strings.IndexAny(raw, " \t#"); i >= 0 {
		return raw[:i]
	}
	return raw
}

// volumioRootOverlay answers whether overlay, the line of the root's own
// mount, is the root overlay as Volumio mounts it, and returns the mount ID /
// lies on: the mount and its superblock read-write, its lower layer
// /mnt/static and its upper layer /mnt/ext/dyn, each named once, and the
// mount / lies on an overlay of the same superblock, at its own root.
func volumioRootOverlay(table []byte, overlay mountRecord, idOf func(string) (uint64, bool, error)) (uint64, error) {
	lower, lowerOnce := soleOption(overlay.super, "lowerdir")
	upper, upperOnce := soleOption(overlay.super, "upperdir")
	switch {
	case overlay.opts[0] != "rw" || overlay.super[0] != "rw":
		return 0, errors.New("the overlay is not mounted read-write")
	case !lowerOnce || lower != volumioLowerDir:
		return 0, fmt.Errorf("the overlay's lower layer is %s, not %s", described(lower, lowerOnce), volumioLowerDir)
	case !upperOnce || upper != volumioUpperDir:
		return 0, fmt.Errorf("the overlay's upper layer is %s, not %s", described(upper, upperOnce), volumioUpperDir)
	}
	id, ok, err := idOf("/")
	switch {
	case err != nil:
		return 0, fmt.Errorf("the mount / lies on cannot be told: %w", err)
	case !ok:
		return 0, errors.New("the platform gives no mount ID for /")
	}
	at, err := mountLine(table, id)
	switch {
	case err != nil:
		return 0, fmt.Errorf("the mount / lies on: %w", err)
	case at.fstype != "overlay" || at.device != overlay.device || at.root != "/":
		return 0, fmt.Errorf("it is not the overlay at /: / lies on mount %d, %s %s", id, at.fstype, at.device)
	}
	return id, nil
}

// soleOption is the value, unescaped, of the one option named key among a
// mount's options, and false where none or more than one names it.
func soleOption(opts []string, key string) (string, bool) {
	value, n := "", 0
	for _, opt := range opts {
		if k, v, _ := strings.Cut(opt, "="); k == key {
			value, n = unescapeMountInfo(v), n+1
		}
	}
	return value, n == 1
}

// described is an option's value for a refusal, or that it is not named once.
func described(value string, once bool) string {
	if !once {
		return "not named once"
	}
	return value
}

// volumioImageDisk answers whether /imgpart's mount is partition 2 of a disk,
// and returns that disk's directory in sysfs: /imgpart a mount of its own
// (not the one / lies on, rootID), whose line, by its mount ID, gives a
// major:minor sysfs lists as a block device (/sys/dev/block) whose own dev
// file gives the same major:minor, and whose partition file reads 2, in a
// directory that is a disk.
func volumioImageDisk(table []byte, rootID uint64, r mediumReaders) (string, error) {
	id, ok, err := r.idOf(volumioImageMount)
	switch {
	case err != nil:
		return "", fmt.Errorf("%s's mount cannot be told: %w", volumioImageMount, err)
	case !ok:
		return "", fmt.Errorf("the platform gives no mount ID for %s", volumioImageMount)
	case id == rootID:
		return "", fmt.Errorf("%s is no mount of its own: it lies on mount %d, as / does", volumioImageMount, id)
	}
	rec, err := mountLine(table, id)
	if err != nil {
		return "", fmt.Errorf("%s: %w", volumioImageMount, err)
	}
	part, err := r.boot.Resolve("/sys/dev/block/" + rec.device)
	if err != nil {
		return "", fmt.Errorf("%s's device %s (mount %d, %s) is no block device sysfs lists: %w", volumioImageMount, rec.device, id, rec.fstype, err)
	}
	switch dev, err := sysfsWord(r.boot, part+"/dev"); {
	case err != nil:
		return "", fmt.Errorf("the device of %s's partition (%s) cannot be read: %w", volumioImageMount, path.Base(part), err)
	case dev != rec.device:
		return "", fmt.Errorf("sysfs lists %s's device %s as %s", volumioImageMount, rec.device, dev)
	}
	switch number, err := sysfsWord(r.boot, part+"/partition"); {
	case errors.Is(err, fs.ErrNotExist):
		return "", fmt.Errorf("%s is on no partition: its device %s (%s) is a whole disk", volumioImageMount, rec.device, path.Base(part))
	case err != nil:
		return "", fmt.Errorf("the partition number of %s's device %s (%s) cannot be read: %w", volumioImageMount, rec.device, path.Base(part), err)
	case number != volumioImagePart:
		return "", fmt.Errorf("%s is on partition %s of its disk (%s), not partition %s", volumioImageMount, number, path.Base(part), volumioImagePart)
	}
	disk := path.Dir(part)
	if err := isDisk(r.boot, disk); err != nil {
		return "", fmt.Errorf("the directory above %s's partition in sysfs, %s, is no disk: %w", volumioImageMount, disk, err)
	}
	return disk, nil
}

// isDisk answers whether dir, in sysfs, is a disk: a block device (a dev file
// that reads as one word) that is no partition (no partition file).
func isDisk(boot bootFiles, dir string) error {
	if _, err := sysfsWord(boot, dir+"/dev"); err != nil {
		return err
	}
	switch partition, err := boot.Exists(dir + "/partition"); {
	case err != nil:
		return err
	case partition:
		return errors.New("it is a partition itself")
	}
	return nil
}

// volumioDataPartition answers whether the disk's partition 3 is Volumio's
// data partition, and returns its kernel name and major:minor: the one
// directory under the disk in sysfs whose partition file reads 3, whose dev
// file gives a major:minor that sysfs resolves to that very directory (a
// device under the same disk, never one under another disk that a sibling
// names), and which udev's database entry for that major:minor gives as
// ext4 labelled volumio_data.
func volumioDataPartition(disk string, boot bootFiles) (name, dev string, err error) {
	entries, err := boot.ReadDir(disk)
	if err != nil {
		return "", "", fmt.Errorf("the partitions of %s cannot be listed: %w", path.Base(disk), err)
	}
	var found []string
	for _, e := range entries {
		if !e.dir {
			continue
		}
		switch number, err := sysfsWord(boot, disk+"/"+e.name+"/partition"); {
		case errors.Is(err, fs.ErrNotExist):
		case err != nil:
			return "", "", fmt.Errorf("the partition number of %s cannot be read: %w", e.name, err)
		case number == volumioDataPart:
			found = append(found, e.name)
		}
	}
	switch len(found) {
	case 0:
		return "", "", fmt.Errorf("%s has no partition %s", path.Base(disk), volumioDataPart)
	case 1:
	default:
		return "", "", fmt.Errorf("%s lists more than one partition %s (%s)", path.Base(disk), volumioDataPart, strings.Join(found, ", "))
	}
	name = found[0]
	entry := disk + "/" + name
	if dev, err = sysfsWord(boot, entry+"/dev"); err != nil {
		return "", "", fmt.Errorf("the device of partition 3 (%s) cannot be read: %w", name, err)
	}
	switch at, err := boot.Resolve("/sys/dev/block/" + dev); {
	case err != nil:
		return "", "", fmt.Errorf("partition 3 (%s) names device %s, which sysfs does not list: %w", name, dev, err)
	case at != entry:
		return "", "", fmt.Errorf("partition 3 (%s) names device %s, which sysfs puts under %s, not under %s", name, dev, path.Base(path.Dir(at)), path.Base(disk))
	}
	entryBytes, err := boot.ReadFile("/run/udev/data/b" + dev)
	if err != nil {
		return "", "", fmt.Errorf("udev's database entry for partition 3 (%s, %s) cannot be read: %w", name, dev, err)
	}
	props, ok := udevProperties(entryBytes)
	switch {
	case !ok:
		return "", "", fmt.Errorf("udev's database entry for partition 3 (%s, %s) does not read as one", name, dev)
	case props["ID_FS_TYPE"] == "":
		return "", "", fmt.Errorf("udev names no filesystem on partition 3 (%s)", name)
	case props["ID_FS_TYPE"] != volumioDataFSType:
		return "", "", fmt.Errorf("partition 3 (%s) is %q, not %s", name, props["ID_FS_TYPE"], volumioDataFSType)
	case props["ID_FS_LABEL"] != volumioDataLabel:
		return "", "", fmt.Errorf("partition 3 (%s) is labelled %q, not %s", name, props["ID_FS_LABEL"], volumioDataLabel)
	}
	return name, dev, nil
}

// udevProperties are the properties udev's database entry for a device gives
// it, its E: lines, each KEY=value; false for an entry with an E: line that
// is no assignment, or that gives a key twice.
func udevProperties(entry []byte) (map[string]string, bool) {
	props := map[string]string{}
	for _, line := range strings.Split(string(entry), "\n") {
		rest, ok := strings.CutPrefix(line, "E:")
		if !ok {
			continue
		}
		key, value, ok := strings.Cut(rest, "=")
		if _, twice := props[key]; !ok || key == "" || twice {
			return nil, false
		}
		props[key] = value
	}
	return props, true
}

// liveAndHidden answers whether partition 3's ext4 superblock is live and
// hidden: /sys/fs/ext4 lists it by its kernel name, which ext4 lists a
// superblock by while it is mounted anywhere, and no line of the mount table
// lists its major:minor. A /mnt/ext that fell back to tmpfs, as on a rescue
// or factory-reset boot, leaves no live ext4 superblock of the partition; one
// mounted where this process sees it is not the hidden mount the initramfs
// made, and the upper path decides once it is mounted at /mnt/ext. A line
// that does not read as one leaves that unknown.
func liveAndHidden(table []byte, name, dev string, boot bootFiles) error {
	switch live, err := boot.Exists("/sys/fs/ext4/" + name); {
	case err != nil:
		return fmt.Errorf("whether an ext4 superblock of partition 3 (%s) is live cannot be told: %w", name, err)
	case !live:
		return fmt.Errorf("no ext4 superblock of partition 3 (%s) is live (/sys/fs/ext4/%s): /mnt/ext is not that partition, as after a rescue or factory-reset boot, whose /mnt/ext is tmpfs", name, name)
	}
	for _, line := range strings.Split(string(table), "\n") {
		if line == "" {
			continue
		}
		rec, ok := parseMountLine(line)
		switch {
		case !ok:
			return fmt.Errorf("the mount table has a line that does not read as one, so whether partition 3 (%s) is mounted where this process sees it cannot be told", name)
		case rec.device == dev:
			return fmt.Errorf("partition 3 (%s, %s) is mounted where this process sees it, at %s, so its live superblock is not known to be the hidden mount Volumio's initramfs made at /mnt/ext: mount it at /mnt/ext, where the upper path reaches it, or unmount it", name, dev, rec.point)
		}
	}
	return nil
}

// sysfsWord is the one word a sysfs attribute holds, its newline trimmed: a
// device's major:minor, a partition's number.
func sysfsWord(boot bootFiles, name string) (string, error) {
	b, err := boot.ReadFile(name)
	if err != nil {
		return "", err
	}
	w := strings.TrimSpace(string(b))
	if w == "" || strings.ContainsAny(w, " \t\r\n") {
		return "", fmt.Errorf("%s does not read as one word", name)
	}
	return w, nil
}

// bootFileLimit bounds a file the Volumio check reads: an os-release, a
// sysfs attribute and a udev database entry each hold a few hundred bytes.
const bootFileLimit = 64 << 10

// hostBootFiles is the box's own files below root, "/" in production and a
// test's tree elsewhere: each path joined below root as Linux names it, a
// file opened as one (fsutil.OpenAsFile, so a named pipe is refused rather
// than waited on) and read to at most bootFileLimit bytes, a directory's
// entries as it lists them (a link to a directory is no directory), and a
// path's links resolved as the kernel resolves them (filepath.EvalSymlinks),
// one that leads out of root refused.
type hostBootFiles struct{ root string }

// host is name below h's root, in the platform's own spelling.
func (h hostBootFiles) host(name string) string {
	return filepath.Join(h.root, filepath.FromSlash(name))
}

func (h hostBootFiles) ReadFile(name string) ([]byte, error) {
	f, _, err := fsutil.OpenAsFile(h.host(name))
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, bootFileLimit+1))
	switch {
	case err != nil:
		return nil, err
	case len(b) > bootFileLimit:
		return nil, fmt.Errorf("%s holds more than the %d bytes the check reads", name, bootFileLimit)
	}
	return b, nil
}

func (h hostBootFiles) ReadDir(name string) ([]bootEntry, error) {
	entries, err := os.ReadDir(h.host(name))
	if err != nil {
		return nil, err
	}
	out := make([]bootEntry, 0, len(entries))
	for _, e := range entries {
		out = append(out, bootEntry{name: e.Name(), dir: e.IsDir()})
	}
	return out, nil
}

func (h hostBootFiles) Resolve(name string) (string, error) {
	resolved, err := filepath.EvalSymlinks(h.host(name))
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(h.root, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%s leads out of %s", name, h.root)
	}
	return path.Join("/", filepath.ToSlash(rel)), nil
}

func (h hostBootFiles) Exists(name string) (bool, error) {
	switch _, err := os.Stat(h.host(name)); {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	default:
		return false, err
	}
}
