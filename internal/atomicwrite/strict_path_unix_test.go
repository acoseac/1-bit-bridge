//go:build unix

package atomicwrite

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
)

// onAnotherDevice is an entry as a stat reports it on another device
// (st_dev) than the one it is on: what overlayfs reports for an entry that is
// not a directory, a pseudo device per layer, and btrfs for a subvolume's
// directory, each on the one mount.
type onAnotherDevice struct {
	fs.FileInfo
	st syscall.Stat_t
}

func (d onAnotherDevice) Sys() any { return &d.st }

// TestWriteBytesDurable_DecidesAMountByItsIDNotItsDevice pins the Claude
// API's round 8 finding: overlayfs with its layers on different filesystems
// and xino off, the default and the Raspberry Pi OS "Overlay File System"
// option, reports the overlay's device for a directory and a pseudo device
// per layer for every other entry, and btrfs a device per subvolume, each on
// one mount. Round 8 compared the destination's device with the root's, and
// refused the reservation file once it existed there, and the restart's probe
// with it. Here a stat reports every entry but a directory on another device,
// and a directory below the root on a device of its own, the mount IDs all
// the root's: the probe and a write over the destination already there
// succeed, syncing what they always sync, and so does a write that makes a
// directory below that one.
func TestWriteBytesDurable_DecidesAMountByItsIDNotItsDevice(t *testing.T) {
	root := realTempDir(t)
	sub := filepath.Join(root, "sub")
	mkdirs(t, sub)
	dst := filepath.Join(sub, "ids")
	if err := os.WriteFile(dst, []byte("reserved"), 0o600); err != nil {
		t.Fatal(err)
	}
	synced := stubSyncDir(t, nil)
	stubMountIDs(t, func(string) (uint64, bool, error) { return 1, true, nil })
	prev := lstat
	t.Cleanup(func() { lstat = prev })
	lstat = func(path string) (fs.FileInfo, error) {
		fi, err := os.Lstat(path)
		if err != nil {
			return nil, err
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		switch {
		case !ok:
			t.Fatalf("Lstat(%s) gives no syscall.Stat_t", path)
		case !fi.IsDir():
			moved := *st
			moved.Dev++
			return onAnotherDevice{fi, moved}, nil
		case path == sub:
			moved := *st
			moved.Dev += 2
			return onAnotherDevice{fi, moved}, nil
		}
		return fi, nil
	}
	if err := ProbeDurable(root, dst); err != nil {
		t.Fatalf("ProbeDurable of a destination a stat reports on another device, on the root's mount = %v, want nil", err)
	}
	if want := []string{sub, root}; !slices.Equal(*synced, want) {
		t.Errorf("the probe synced %v, want %v", *synced, want)
	}
	*synced = nil
	if err := WriteBytesDurable(root, dst, []byte("reserved again"), ".durable-*.tmp"); err != nil {
		t.Fatalf("WriteBytesDurable over that destination = %v, want nil", err)
	}
	if want := []string{root, sub}; !slices.Equal(*synced, want) {
		t.Errorf("the write synced %v, want %v", *synced, want)
	}
	if got, err := os.ReadFile(dst); err != nil || string(got) != "reserved again" {
		t.Errorf("the destination holds %q (%v), want the write's bytes", got, err)
	}
	deeper := filepath.Join(sub, "new", "ids")
	if err := WriteBytesDurable(root, deeper, []byte("reserved"), ".durable-*.tmp"); err != nil {
		t.Fatalf("WriteBytesDurable below a directory a stat reports on a device of its own = %v, want nil", err)
	}
	if got, err := os.ReadFile(deeper); err != nil || string(got) != "reserved" {
		t.Errorf("%s holds %q (%v), want the write's bytes", deeper, got, err)
	}
}
