//go:build unix

package atomicwrite

import (
	"errors"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/fsutil/fsutiltest"
)

// materialize writes b's files and links below root as a real tree, each
// link relative to its own directory, as the kernel writes sysfs's links
// (/sys/dev/block/179:3 -> ../../devices/...), so the production reader
// resolves them inside root. A sysfs name holds a colon, which Windows
// refuses, so only a POSIX host can make the tree.
func materialize(t *testing.T, root string, b *fakeBoot) {
	t.Helper()
	for name, body := range b.files {
		p := filepath.Join(root, filepath.FromSlash(name))
		mkdirs(t, filepath.Dir(p))
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for name, target := range b.links {
		if path.IsAbs(target) {
			rel, err := filepath.Rel(filepath.FromSlash(path.Dir(name)), filepath.FromSlash(target))
			if err != nil {
				t.Fatal(err)
			}
			target = rel
		}
		p := filepath.Join(root, filepath.FromSlash(name))
		mkdirs(t, filepath.Dir(p))
		if err := os.Symlink(target, p); err != nil {
			t.Fatal(err)
		}
	}
}

// TestTheHostsOwnFilesShowVolumiosLayout runs the production reader of the
// box's own files (hostBootFiles) on the measured box written out as a real
// tree, its sysfs links relative as the kernel's are and the SD card with a
// link of its own (device) beside its partitions, which is no partition: the
// check takes the root and names the card's partition 3. Through the same
// reader, the NVMe's device named by the card's partition 3, and partition
// 3's /sys/fs/ext4 entry gone, are refused, and a sysfs link that leads out
// of the tree is no answer.
func TestTheHostsOwnFilesShowVolumiosLayout(t *testing.T) {
	inspectOn := func(t *testing.T, boot *fakeBoot) Medium {
		t.Helper()
		root := realTempDir(t)
		materialize(t, root, boot)
		if err := os.Symlink("../../../mmc0:59b4", filepath.Join(root, filepath.FromSlash(piSD), "device")); err != nil {
			t.Fatal(err)
		}
		b := piVolumio()
		return inspectMedium(b.root, b.rootID, mediumReaders{
			statfs:    b.statfs,
			mountinfo: func() ([]byte, error) { return []byte(b.table), nil },
			idOf: func(p string) (uint64, bool, error) {
				id, ok := b.ids[p]
				return id, ok, nil
			},
			boot: hostBootFiles{root: root},
		})
	}
	if m := inspectOn(t, piBoot()); m.Err != nil || m.Storage != "Volumio's data partition (mmcblk0p3, ext4)" {
		t.Fatalf("the medium through the host's own files = %+v, want Volumio's data partition taken", m)
	}
	nvme := piBoot()
	nvme.files[piSD+"/mmcblk0p3/dev"] = "259:3\n"
	if m := inspectOn(t, nvme); !errors.Is(m.Err, ErrMediumUnknown) || !strings.Contains(m.Err.Error(), "which sysfs puts under nvme0n1, not under mmcblk0") {
		t.Errorf("the NVMe's device named by the card's partition 3 = %+v, want refused", m)
	}
	gone := piBoot()
	delete(gone.files, "/sys/fs/ext4/mmcblk0p3/errors_count")
	if m := inspectOn(t, gone); !errors.Is(m.Err, ErrMediumUnknown) || !strings.Contains(m.Err.Error(), "no ext4 superblock of partition 3 (mmcblk0p3) is live") {
		t.Errorf("no live superblock = %+v, want refused", m)
	}
	out := realTempDir(t)
	root := realTempDir(t)
	mkdirs(t, filepath.Join(root, "sys", "dev", "block"))
	symlink(t, out, filepath.Join(root, "sys", "dev", "block", "179:2"))
	if got, err := (hostBootFiles{root: root}).Resolve("/sys/dev/block/179:2"); err == nil {
		t.Errorf("a link out of the tree resolved to %q", got)
	}
}

// TestTheHostsOwnFilesAreNeverWaitedOn pins the production reader's opens: a
// named pipe where udev's database entry for partition 3 should be is
// refused at once (fsutil.OpenAsFile), never waited on for a writer, so the
// check, which a console's doctor runs on a page render, refuses rather
// than hangs; and a file past the reader's bound is refused rather than read
// whole.
func TestTheHostsOwnFilesAreNeverWaitedOn(t *testing.T) {
	root := realTempDir(t)
	boot := piBoot()
	delete(boot.files, "/run/udev/data/b179:3")
	materialize(t, root, boot)
	pipe := filepath.Join(root, "run", "udev", "data", "b179:3")
	fsutiltest.MakeFIFO(t, pipe)
	b := piVolumio()
	var m Medium
	done := make(chan struct{})
	go func() {
		defer close(done)
		m = inspectMedium(b.root, b.rootID, mediumReaders{
			statfs:    b.statfs,
			mountinfo: func() ([]byte, error) { return []byte(b.table), nil },
			idOf: func(p string) (uint64, bool, error) {
				id, ok := b.ids[p]
				return id, ok, nil
			},
			boot: hostBootFiles{root: root},
		})
	}()
	if fsutiltest.AwaitPastFIFOs(t, "the medium check", 5*time.Second, done, pipe) {
		if !errors.Is(m.Err, ErrMediumUnknown) || !strings.Contains(m.Err.Error(), "udev's database entry for partition 3 (mmcblk0p3, 179:3) cannot be read") {
			t.Errorf("a named pipe for udev's entry = %+v, want refused", m)
		}
	}
	big := filepath.Join(root, "big")
	if err := os.WriteFile(big, make([]byte, bootFileLimit+1), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := (hostBootFiles{root: root}).ReadFile("/big"); err == nil {
		t.Error("a file past the reader's bound was read")
	}
}
