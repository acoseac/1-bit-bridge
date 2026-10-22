//go:build linux

package atomicwrite

import (
	"os"
	"testing"
)

// TestPlatformMountTableReadsTheKernels holds the mount IDs the writer reads
// on Linux to the kernel's own mount table (/proc/self/mountinfo, read by the
// tests' parseMountInfo): the ID from statx's STATX_MNT_ID (Linux 5.8) and
// the one from the fdinfo listing of an O_PATH descriptor (Linux 3.15), the
// fallback, each give / and /proc the IDs the table lists for those mount
// points. A kernel that gives no ID through statx (older than 5.8, or a statx
// a policy refuses) says so in the log, and the fallback is still held to the
// table.
func TestPlatformMountTableReadsTheKernels(t *testing.T) {
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		t.Fatalf("the kernel's mount table: %v", err)
	}
	table, err := parseMountInfo(data)
	if err != nil {
		t.Fatalf("the kernel's mount table: %v", err)
	}
	for _, p := range []string{"/", "/proc"} {
		want, listed := table[p]
		if !listed {
			t.Fatalf("the kernel's mount table lists no %s: %v", p, table)
		}
		if id, ok, err := fdinfoMountID(p); !ok || err != nil || id != want {
			t.Errorf("fdinfoMountID(%s) = %d, %v, %v, want the table's %d", p, id, ok, err, want)
		}
		switch id, ok, err := statxMountID(p); {
		case err != nil:
			t.Errorf("statxMountID(%s): %v", p, err)
		case !ok:
			t.Logf("statx gives no mount ID for %s here (a kernel older than Linux 5.8, or a statx a policy refuses): the writer reads fdinfo's", p)
		case id != want:
			t.Errorf("statxMountID(%s) = %d, want the table's %d", p, id, want)
		}
	}
}
