//go:build linux

package atomicwrite

import (
	"os"
	"slices"
	"testing"
)

// kernelMountTable is the kernel's mount table for this process, the mount ID
// of each mount point (parseMountInfo), with / and /proc in it.
func kernelMountTable(t *testing.T) map[string]uint64 {
	t.Helper()
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		t.Fatalf("the kernel's mount table: %v", err)
	}
	table, err := parseMountInfo(data)
	if err != nil {
		t.Fatalf("the kernel's mount table: %v", err)
	}
	for _, p := range []string{"/", "/proc"} {
		if _, listed := table[p]; !listed {
			t.Fatalf("the kernel's mount table lists no %s: %v", p, table)
		}
	}
	return table
}

// TestPlatformMountTableReadsTheKernels holds the mount IDs the writer reads
// on Linux to the kernel's own mount table (/proc/self/mountinfo, read by the
// tests' parseMountInfo): the ID from statx's STATX_MNT_ID (Linux 5.8) and
// the one from the fdinfo listing of an O_PATH descriptor (Linux 3.15), the
// fallback, each give / and /proc the IDs the table lists for those mount
// points. A kernel that gives no ID through statx (older than 5.8, or a statx
// a policy refuses) says so in the log, and the fallback is still held to the
// table.
func TestPlatformMountTableReadsTheKernels(t *testing.T) {
	table := kernelMountTable(t)
	for _, p := range []string{"/", "/proc"} {
		want := table[p]
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

// TestPlatformMountIDAsksFdinfoWhereStatxGivesNone pins the wiring of
// platformMountID's two readers, the Claude API's round 9 finding: with statx
// giving no mount ID, as on a kernel older than 5.8 or under a seccomp
// profile that refuses statx, platformMountID asks fdinfo, whose IDs for /
// and /proc are the ones the kernel's mount table lists. Round 9's tests held
// each reader to the table and the order to stubs (mountIDFrom), so a
// platformMountID that asked statx alone passed on every kernel from 5.8 up,
// and on an older one refused every install (ErrMountsUnknown).
func TestPlatformMountIDAsksFdinfoWhereStatxGivesNone(t *testing.T) {
	table := kernelMountTable(t)
	var statxAsked, fdinfoAsked []string
	prevStatx, prevFdinfo := statxMountIDOf, fdinfoMountIDOf
	t.Cleanup(func() { statxMountIDOf, fdinfoMountIDOf = prevStatx, prevFdinfo })
	statxMountIDOf = func(p string) (uint64, bool, error) {
		statxAsked = append(statxAsked, p)
		return 0, false, nil
	}
	fdinfoMountIDOf = func(p string) (uint64, bool, error) {
		fdinfoAsked = append(fdinfoAsked, p)
		return fdinfoMountID(p)
	}
	paths := []string{"/", "/proc"}
	for _, p := range paths {
		if id, ok, err := platformMountID(p); !ok || err != nil || id != table[p] {
			t.Errorf("platformMountID(%s) with statx giving none = %d, %v, %v, want the table's %d", p, id, ok, err, table[p])
		}
	}
	if !slices.Equal(statxAsked, paths) || !slices.Equal(fdinfoAsked, paths) {
		t.Errorf("platformMountID asked statx for %v and fdinfo for %v, want each for %v", statxAsked, fdinfoAsked, paths)
	}
}
