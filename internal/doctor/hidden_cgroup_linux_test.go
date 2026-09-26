//go:build linux

package doctor

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// ownCgroup is this process's cgroup on the cgroup2 hierarchy, the mount
// that holds it, and its id, read as the census reads a recorded pid's. It
// skips where there is none to read (a v1-only host, no cgroup2 mount).
func ownCgroup(t *testing.T) (cg string, m cgroupMount, id uint64) {
	t.Helper()
	cg, ok := readUnifiedCgroup("/proc/self/cgroup")
	if !ok {
		t.Skip("this process's /proc/self/cgroup has no cgroup v2 line")
	}
	m, ok = mountOfCgroup("/proc/self/mountinfo", cg)
	if !ok {
		t.Skip("no cgroup2 mount this process can see holds its cgroup")
	}
	id, ok = cgroupID(m, cg)
	if !ok {
		t.Skipf("%s has no directory on the cgroup2 mount at %s", cg, m.point)
	}
	return cg, m, id
}

// listenerCgroupOf is the cgroup the kernel reports for the one socket
// listening on port, or skips where it reports none (a kernel before 5.8).
func listenerCgroupOf(t *testing.T, port int) uint64 {
	t.Helper()
	sockets, _, err := listenerSockets(procNetTCPFiles, port)
	if err != nil || len(sockets) != 1 {
		t.Fatalf("the socket tables list %v on :%d (err %v), want one socket", sockets, port, err)
	}
	created, err := listenerCgroups(port)
	if err != nil {
		t.Fatalf("asking the kernel for its listeners' cgroups: %v", err)
	}
	for s := range sockets {
		id, ok := created[s]
		if !ok {
			t.Skipf("the kernel reports no cgroup for %s (INET_DIAG_CGROUP_ID came in 5.8)", s)
		}
		return id
	}
	return 0
}

// TestListenerCgroupsReportsTheCgroupAListenerWasCreatedIn pins, on the real
// kernel, the fact the census's third accounting rests on: the cgroup the
// kernel's socket diagnostics report for a listener is the id of the
// cgroup's directory on the cgroup2 mount, the one /proc/<pid>/cgroup names
// for the process that created it. A disagreement here would FAIL the
// bridge's own port.
func TestListenerCgroupsReportsTheCgroupAListenerWasCreatedIn(t *testing.T) {
	cg, _, id := ownCgroup(t)
	if got := listenerCgroupOf(t, bindPort(t)); got != id {
		t.Errorf("the kernel reports cgroup %d for this process's listener; its cgroup, %s, has id %d", got, cg, id)
	}
}

// TestAHiddenBridgesCgroupReadsWhereItsDescriptorsDoNot is the same fact for
// a process with dumpable=0, the stand-in for a bridge granted
// cap_net_bind_service: its /proc/<pid>/cgroup reads where its descriptors
// may not, and names the cgroup the kernel reports for its listener.
func TestAHiddenBridgesCgroupReadsWhereItsDescriptorsDoNot(t *testing.T) {
	if mode := os.Getenv(undumpableChildEnv); mode != "" {
		runUndumpable(mode)
	}
	own, m, _ := ownCgroup(t)
	bridge, port := startUndumpable(t, true)
	cg, ok := readUnifiedCgroup(filepath.Join("/proc", strconv.Itoa(bridge), "cgroup"))
	if !ok || cg != own {
		t.Fatalf("pid %d's cgroup reads %q, %v; want this process's, %s, which it inherited", bridge, cg, ok, own)
	}
	id, ok := cgroupID(m, cg)
	if !ok {
		t.Fatalf("%s has no directory on the cgroup2 mount", cg)
	}
	if got := listenerCgroupOf(t, port); got != id {
		t.Errorf("the kernel reports cgroup %d for the stand-in's listener; its cgroup, %s, has id %d", got, cg, id)
	}
}

// cgroupsUnderOwn makes a cgroup for each name under this process's own,
// removed once the test is done, and returns their directories and their
// paths as /proc/<pid>/cgroup renders them. It skips where this user cannot
// make them: that takes root, or a subtree delegated to this user, on a
// cgroup2 mount that is writable, which a container's is not.
func cgroupsUnderOwn(t *testing.T, names ...string) (dirs, paths []string) {
	t.Helper()
	own, m, _ := ownCgroup(t)
	for _, name := range names {
		name = fmt.Sprintf("doctor-test-%d-%s", os.Getpid(), name)
		dir := filepath.Join(cgroupDir(m, own), name)
		if err := os.Mkdir(dir, 0o755); err != nil {
			if errors.Is(err, fs.ErrPermission) || errors.Is(err, syscall.EROFS) {
				t.Skipf("this user cannot make a cgroup under its own (%v): that takes root, or a delegated subtree, on a writable cgroup2 mount", err)
			}
			t.Fatal(err)
		}
		// Registered before any child is started, so it runs after their
		// cleanups have killed and reaped them: a cgroup with a process
		// in it cannot be removed.
		t.Cleanup(func() {
			if err := os.Remove(dir); err != nil {
				t.Errorf("removing the test cgroup %s: %v", dir, err)
			}
		})
		dirs = append(dirs, dir)
		paths = append(paths, path.Join(own, name))
	}
	return dirs, paths
}

// TestPortCheckFailsAPortAHiddenHolderInAnotherCgroupHolds is row L6h of the
// port-verdict matrix on the real kernel: the recorded bridge runs with
// dumpable=0 (the stand-in for a bridge granted cap_net_bind_service), and
// the port its config was edited to is held by ANOTHER process of the same
// uid, hidden the same way, running in another cgroup, as a second service
// of the bridge's user runs in its own unit. No probe can read either, and
// the listener carries the bridge's own uid, so the uid arm answered ok,
// `bridge doctor --config` exited 0, and the restart could not bind.
//
// The kernel reports the cgroup the listener was created in, and the
// bridge's /proc/<pid>/cgroup the one it runs in; they are siblings, so the
// listener is not the bridge's, and the port FAILs. Beside it, the two
// shapes that must stay ok: the bridge on its own port (row L2), and a
// hidden holder in the bridge's own cgroup, which nothing unprivileged
// tells from the bridge. It takes root, to make the cgroups and to run the
// three processes as a uid that is not this one's: doctor runs as the
// bridge's user, as the runbook says to, so root's own view plays no part.
func TestPortCheckFailsAPortAHiddenHolderInAnotherCgroupHolds(t *testing.T) {
	if mode := os.Getenv(undumpableChildEnv); mode != "" {
		runUndumpable(mode)
	}
	if spec := os.Getenv(portCheckChildEnv); spec != "" {
		runPortCheck(t, spec)
	}
	if os.Geteuid() != 0 {
		t.Skip("needs root, to make cgroups and run processes as another uid")
	}
	const uid = 4073
	before := dumpable(t)

	t.Run("a holder in another cgroup", func(t *testing.T) {
		dirs, paths := cgroupsUnderOwn(t, "bridge", "holder")
		_, port := startUndumpableIn(t, uid, dirs[1], true)
		bridge, _ := startUndumpableIn(t, uid, dirs[0], false)
		c := checkPortAs(t, uid, port, bridge)
		if c.Status != Fail {
			t.Fatalf("got %v (%s / %s), want fail", c.Status, c.Summary, c.Hint)
		}
		want := fmt.Sprintf("/proc and the kernel's socket diagnostics show every socket listening on this port "+
			"created in cgroup %s, while pid %d runs in cgroup %s", paths[1], bridge, paths[0])
		if !strings.Contains(c.Hint, want) || !strings.Contains(c.Hint, "stop the process that holds the port") {
			t.Errorf("the hint does not give the cgroups, or does not say to stop the holder:\n got %s\nwant …%s…", c.Hint, want)
		}
	})
	t.Run("a holder in the bridge's own cgroup", func(t *testing.T) {
		dirs, _ := cgroupsUnderOwn(t, "shared")
		_, port := startUndumpableIn(t, uid, dirs[0], true)
		bridge, _ := startUndumpableIn(t, uid, dirs[0], false)
		if c := checkPortAs(t, uid, port, bridge); c.Status != OK {
			t.Errorf("got %v (%s / %s), want ok: a listener in the bridge's own cgroup may be the bridge's", c.Status, c.Summary, c.Hint)
		}
	})
	t.Run("the bridge on its own port", func(t *testing.T) {
		dirs, _ := cgroupsUnderOwn(t, "own")
		bridge, port := startUndumpableIn(t, uid, dirs[0], true)
		if c := checkPortAs(t, uid, port, bridge); c.Status != OK {
			t.Errorf("got %v (%s / %s), want ok", c.Status, c.Summary, c.Hint)
		}
	})
	if after := dumpable(t); after != before {
		t.Errorf("this process's dumpable flag went from %d to %d while its children took another uid (dropToChildUID)", before, after)
	}
}
