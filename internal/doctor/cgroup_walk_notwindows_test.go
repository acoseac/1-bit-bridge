//go:build !windows

package doctor

import (
	"path/filepath"
	"testing"
)

// TestFindCgroupsStopsAtItsBudget: the walk of the cgroup tree is bounded,
// so a host with a very large tree costs a port check a bounded time, and a
// cgroup past the budget is one it did not find, which counts nothing. It
// also stops as soon as it has found every cgroup it was asked for.
func TestFindCgroupsStopsAtItsBudget(t *testing.T) {
	f := newCgroupFixture(t)
	m := cgroupMount{point: f.point, root: "/"}
	hold := f.id(t, "/system.slice/cghold.service")
	if got := findCgroups(m, map[uint64]bool{hold: true}, cgroupWalkBudget); got[hold] != "/system.slice/cghold.service" {
		t.Errorf("within the budget: got %v", got)
	}
	if got := findCgroups(m, map[uint64]bool{hold: true}, 2); len(got) != 0 {
		t.Errorf("past a budget of two directories: got %v, want nothing", got)
	}
	sub := cgroupMount{point: filepath.Join(f.point, "system.slice"), root: "/system.slice"}
	if got := findCgroups(sub, map[uint64]bool{hold: true}, cgroupWalkBudget); got[hold] != "/system.slice/cghold.service" {
		t.Errorf("a mount of a subtree renders its paths from the mount's root: got %v", got)
	}
}
