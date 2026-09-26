//go:build !windows

package doctor

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// These drive the census's third accounting, the cgroup each listener was
// created in (cgroupsNotOf), on fixture tables, a fixture /proc and a
// fixture cgroup tree, whose directories' inode numbers stand in for the
// cgroup ids the kernel reports, as they are on a real cgroup2 mount. Not on
// Windows, for portowner_fd_notwindows_test.go's reason, and because a
// FileInfo there carries no inode number.

// cgroupFixture is a cgroup tree in a temporary directory, which a fixture
// /proc's mountinfo mounts as the whole hierarchy.
type cgroupFixture struct {
	point string
	ids   map[string]uint64
}

// fixtureCgroups are the cgroups every fixture tree holds: the slice, two
// services in it beside the recorded bridge's (cgbr.service), a child of the
// bridge's cgroup, and a login session's scope.
var fixtureCgroups = []string{
	"/system.slice",
	"/system.slice/cgbr.service",
	"/system.slice/cgbr.service/sub",
	"/system.slice/cghold.service",
	"/system.slice/cgother.service",
	"/user.slice/user-1000.slice/session-3.scope",
}

// newCgroupFixture builds the tree and reads each cgroup's id, its
// directory's inode number, the root's included. Beside the mount point it
// also makes a directory named like the bridge's service, which a path
// that climbs out of the mount ("/../cgbr.service", how a container renders
// a cgroup outside its namespace) would reach if it were ever joined onto
// the mount point.
func newCgroupFixture(t *testing.T) cgroupFixture {
	t.Helper()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "cgbr.service"), 0o755); err != nil {
		t.Fatal(err)
	}
	f := cgroupFixture{point: filepath.Join(dir, "cgroup"), ids: map[string]uint64{}}
	for _, cg := range append([]string{"/"}, fixtureCgroups...) {
		dir := filepath.Join(f.point, filepath.FromSlash(cg))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		var st syscall.Stat_t
		if err := syscall.Stat(dir, &st); err != nil {
			t.Fatal(err)
		}
		f.ids[cg] = uint64(st.Ino)
	}
	return f
}

// id is the id of the fixture cgroup at path cg.
func (f cgroupFixture) id(t *testing.T, cg string) uint64 {
	t.Helper()
	id, ok := f.ids[cg]
	if !ok {
		t.Fatalf("the fixture has no cgroup %s", cg)
	}
	return id
}

// mountAt writes a mountinfo into the fixture /proc at root, as pid self
// reads it, mounting the tree as the whole cgroup2 hierarchy beside a proc
// mount.
func (f cgroupFixture) mountAt(t *testing.T, root, self string) {
	t.Helper()
	writeMountinfo(t, root, self, "30 25 0:26 / "+strings.ReplaceAll(f.point, " ", `\040`)+
		" rw,nosuid,nodev,noexec,relatime shared:9 - cgroup2 cgroup2 rw,nsdelegate\n")
}

// writeMountinfo writes pid self's mountinfo in the fixture /proc at root: a
// proc mount, then lines.
func writeMountinfo(t *testing.T, root, self, lines string) {
	t.Helper()
	dir := filepath.Join(root, self)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	info := "22 28 0:21 / /proc rw,nosuid,nodev,noexec,relatime shared:12 - proc proc rw\n" + lines
	if err := os.WriteFile(filepath.Join(dir, "mountinfo"), []byte(info), 0o600); err != nil {
		t.Fatal(err)
	}
}

// writeCgroupFile writes pid's /proc/<pid>/cgroup in a fixture /proc.
func writeCgroupFile(t *testing.T, root string, pid int, body string) {
	t.Helper()
	dir := filepath.Join(root, strconv.Itoa(pid))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cgroup"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// fixedCgroups is a socket-cgroup source that answers created, and counts
// how often it is asked.
func fixedCgroups(created map[string]uint64, asked *int) socketCgroups {
	return func(int) (map[string]uint64, error) {
		*asked++
		return created, nil
	}
}

// cgroupCase is one row of
// TestProcSightingRulesOutAPidItCannotReadByTheCgroupThatCreatedEachListener.
type cgroupCase struct {
	name string
	rows []listening
	// holders are processes other than the recorded pid, whose descriptors
	// this user can read.
	holders map[int]map[string]string
	// pidCgroup is the recorded pid's /proc/<pid>/cgroup, a v2 line by
	// default; noCgroupFile leaves the file out.
	pidCgroup    string
	noCgroupFile bool
	// created maps each listener's inode to the fixture cgroup the source
	// says created it; an inode missing from it gets no cgroup, and one
	// mapped to "?" an id no directory in the tree has.
	created map[int]string
	// sourceErr makes the source fail, noSource passes none, and noMount
	// leaves the cgroup2 mount out of mountinfo.
	sourceErr, noSource, noMount bool
	want                         ownerSighting
}

// sighting builds the case's fixtures and asks procSighting about port 7788
// and the recorded pid, 4242.
func (c cgroupCase) sighting(t *testing.T) (bool, ownerSighting, error) {
	t.Helper()
	f := newCgroupFixture(t)
	procs := map[int]map[string]string{4242: bridgeElsewhere}
	for pid, links := range c.holders {
		procs[pid] = links
	}
	root := writeProcRoot(t, procs)
	chmodForTest(t, procFdDir(root, 4242), 0o000)
	writeStatus(t, root, 4242, bridgeUIDs)
	if !c.noCgroupFile {
		body := c.pidCgroup
		if body == "" {
			body = "0::/system.slice/cgbr.service\n"
		}
		writeCgroupFile(t, root, 4242, body)
	}
	if c.noMount {
		writeMountinfo(t, root, strconv.Itoa(os.Getpid()), "")
	} else {
		f.mountAt(t, root, strconv.Itoa(os.Getpid()))
	}
	created := map[string]uint64{}
	for inode, cg := range c.created {
		id := uint64(1<<62 + inode)
		if cg != "?" {
			id = f.id(t, cg)
		}
		created["socket:["+strconv.Itoa(inode)+"]"] = id
	}
	asked := 0
	source := fixedCgroups(created, &asked)
	switch {
	case c.noSource:
		source = nil
	case c.sourceErr:
		source = func(int) (map[string]uint64, error) { return nil, errors.New("the kernel said no") }
	}
	return procSighting(tablesWith(t, c.rows...), root, 7788, 4242, "the blind spot", source)
}

// TestProcSightingRulesOutAPidItCannotReadByTheCgroupThatCreatedEachListener
// is row L6h of the port-verdict matrix on fixtures: the recorded bridge
// (pid 4242) runs as uid 1000 with dumpable=0, as a bridge granted
// cap_net_bind_service does, so no probe can read its descriptors, and the
// port its config was edited to is held by ANOTHER hidden process of uid
// 1000 (a second capability-bound binary of the service user, or one of its
// processes in another group). That listener has no readable holder and
// carries the bridge's own uid, which is how the bridge on its own port
// looks (row L2), so the uid arm read ok, `bridge doctor --config` exited
// 0, and the restart could not bind (#1032's Out of scope).
//
// The kernel's socket diagnostics say which cgroup created each listener,
// and /proc/<pid>/cgroup which cgroup the pid runs in. The bridge creates
// its listeners itself, in the cgroup it runs in, which it never leaves, so
// a listener created in a cgroup that is not the bridge's, and neither
// contains it nor sits below it, is not the bridge's. Where every listener
// is that, or another process's by #1030's and #1032's accounts, the pid is
// ruled out. Anything nested with the pid's cgroup, and anything this
// process cannot see, leaves it possible: that is also how the bridge on its
// own port looks, and how a hidden process in the bridge's own cgroup
// looks, which nothing unprivileged can tell apart.
func TestProcSightingRulesOutAPidItCannotReadByTheCgroupThatCreatedEachListener(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a directory whatever its mode, so the recorded pid's descriptors would read")
	}
	unreadable := ownerSighting{saw: "/proc does not let this user read pid 4242's descriptors", blind: "the blind spot"}
	ruledOut := func(account string) ownerSighting {
		return ownerSighting{saw: "/proc and the kernel's socket diagnostics show every socket listening on this port " + account, ruledOut: true}
	}
	own := []listening{{uid: "1000", inode: 300}}
	for _, tc := range []cgroupCase{
		{name: "L6h: a listener of the pid's uid, created in another service's cgroup",
			rows: own, created: map[int]string{300: "/system.slice/cghold.service"},
			want: ruledOut("created in cgroup /system.slice/cghold.service, while pid 4242 runs in cgroup /system.slice/cgbr.service")},
		{name: "a listener created in a login session's scope, beside a pid in a service",
			rows: own, created: map[int]string{300: "/user.slice/user-1000.slice/session-3.scope"},
			want: ruledOut("created in cgroup /user.slice/user-1000.slice/session-3.scope, while pid 4242 runs in cgroup /system.slice/cgbr.service")},
		{name: "two listeners, created in two other cgroups",
			rows:    []listening{{uid: "1000", inode: 300}, {v6: true, uid: "1000", inode: 400}},
			created: map[int]string{300: "/system.slice/cgother.service", 400: "/system.slice/cghold.service"},
			want:    ruledOut("created in cgroups /system.slice/cghold.service, /system.slice/cgother.service, while pid 4242 runs in cgroup /system.slice/cgbr.service")},
		{name: "one held by a process this user can read, one created in another cgroup",
			rows:    []listening{{uid: "1000", inode: 300}, {v6: true, uid: "1000", inode: 400}},
			holders: map[int]map[string]string{5000: {"3": "socket:[300]"}},
			created: map[int]string{300: "/system.slice/cgbr.service", 400: "/system.slice/cghold.service"},
			want:    ruledOut("held by pid 5000 or created in cgroup /system.slice/cghold.service, while pid 4242 runs in cgroup /system.slice/cgbr.service")},
		{name: "one created by another uid, one in another cgroup",
			rows:    []listening{{uid: "1001", inode: 300}, {v6: true, uid: "1000", inode: 400}},
			created: map[int]string{300: "/system.slice/cgbr.service", 400: "/system.slice/cghold.service"},
			want:    ruledOut("created by uid 1001 or in cgroup /system.slice/cghold.service, while pid 4242 runs as uid 1000 in cgroup /system.slice/cgbr.service")},
		{name: "the pid in a login session's scope, the listener in a service",
			rows: own, pidCgroup: "0::/user.slice/user-1000.slice/session-3.scope\n", created: map[int]string{300: "/system.slice/cghold.service"},
			want: ruledOut("created in cgroup /system.slice/cghold.service, while pid 4242 runs in cgroup /user.slice/user-1000.slice/session-3.scope")},

		{name: "L2: the listener created in the pid's own cgroup",
			rows: own, created: map[int]string{300: "/system.slice/cgbr.service"}, want: unreadable},
		{name: "the root cgroup, which the kernels from 5.8 to 5.14 stamp every socket with under net_cls",
			rows: own, created: map[int]string{300: "/"}, want: unreadable},
		{name: "an ancestor of the pid's cgroup",
			rows: own, created: map[int]string{300: "/system.slice"}, want: unreadable},
		{name: "a cgroup below the pid's (its threads', or a subtree it was given)",
			rows: own, created: map[int]string{300: "/system.slice/cgbr.service/sub"}, want: unreadable},
		{name: "one listener in another cgroup, one in the pid's",
			rows:    []listening{{uid: "1000", inode: 300}, {v6: true, uid: "1000", inode: 400}},
			created: map[int]string{300: "/system.slice/cghold.service", 400: "/system.slice/cgbr.service"},
			want:    unreadable},
		{name: "a cgroup no directory in the tree has (another namespace's, or deleted)",
			rows: own, created: map[int]string{300: "?"}, want: unreadable},
		{name: "the kernel gave the listener no cgroup (before 5.8)",
			rows: own, created: map[int]string{}, want: unreadable},
		{name: "the kernel could not be asked",
			rows: own, created: map[int]string{300: "/system.slice/cghold.service"}, sourceErr: true, want: unreadable},
		{name: "nothing asks it",
			rows: own, created: map[int]string{300: "/system.slice/cghold.service"}, noSource: true, want: unreadable},
		{name: "the pid's cgroup file is not there",
			rows: own, created: map[int]string{300: "/system.slice/cghold.service"}, noCgroupFile: true, want: unreadable},
		{name: "the pid's cgroup file has no v2 line (a v1-only host)",
			rows: own, pidCgroup: "12:pids:/system.slice/cgbr.service\n", created: map[int]string{300: "/system.slice/cghold.service"},
			want: unreadable},
		{name: "the pid's cgroup is outside this process's cgroup namespace",
			rows: own, pidCgroup: "0::/../cgbr.service\n", created: map[int]string{300: "/system.slice/cghold.service"},
			want: unreadable},
		{name: "the pid's cgroup has no directory on the mount",
			rows: own, pidCgroup: "0::/system.slice/gone.service\n", created: map[int]string{300: "/system.slice/cghold.service"},
			want: unreadable},
		{name: "no cgroup2 mount",
			rows: own, created: map[int]string{300: "/system.slice/cghold.service"}, noMount: true, want: unreadable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			found, seen, err := tc.sighting(t)
			if err != nil || found || seen != tc.want {
				t.Errorf("got %v, %+v, %v;\nwant false, %+v, no error", found, seen, err, tc.want)
			}
		})
	}
}

// TestTheCensusAsksForCgroupsOnlyAboutWhatNothingElseAccountsFor: the
// kernel is asked, and the cgroup tree walked, only for listeners that no
// readable holder and no other uid already account for, which the bridge's
// own port always leaves; a port whose listeners the first two accounts
// settle costs neither.
func TestTheCensusAsksForCgroupsOnlyAboutWhatNothingElseAccountsFor(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a directory whatever its mode, so the recorded pid's descriptors would read")
	}
	f := newCgroupFixture(t)
	root := writeProcRoot(t, map[int]map[string]string{4242: bridgeElsewhere, 5000: {"3": "socket:[300]"}})
	chmodForTest(t, procFdDir(root, 4242), 0o000)
	writeStatus(t, root, 4242, bridgeUIDs)
	writeCgroupFile(t, root, 4242, "0::/system.slice/cgbr.service\n")
	f.mountAt(t, root, strconv.Itoa(os.Getpid()))
	created := map[string]uint64{"socket:[300]": f.id(t, "/system.slice/cghold.service"), "socket:[400]": f.id(t, "/system.slice/cghold.service")}
	for _, tc := range []struct {
		name      string
		rows      []listening
		wantAsked int
	}{
		{"every listener held by a readable process", []listening{{uid: "1000", inode: 300}}, 0},
		{"every listener created by another uid", []listening{{uid: "1001", inode: 400}}, 0},
		{"a listener of the pid's uid that nothing holds", []listening{{uid: "1000", inode: 400}}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			asked := 0
			if _, seen, err := procSighting(tablesWith(t, tc.rows...), root, 7788, 4242, "the blind spot", fixedCgroups(created, &asked)); err != nil || !seen.ruledOut {
				t.Fatalf("got %+v, %v; want the pid ruled out", seen, err)
			}
			if asked != tc.wantAsked {
				t.Errorf("the kernel was asked %d times, want %d", asked, tc.wantAsked)
			}
		})
	}
}

// cgroupsBesideBridge gives the fixture /proc at root a recorded pid, 4242,
// running in cgbr.service, and a cgroup2 mount as each of selves reads its
// mountinfo, and returns a source saying the listener with inode was
// created in cghold.service: the facts that rule 4242 out by cgroup, once
// 4242's uid is the listener's.
func cgroupsBesideBridge(t *testing.T, root string, inode int, selves ...string) socketCgroups {
	t.Helper()
	f := newCgroupFixture(t)
	writeCgroupFile(t, root, 4242, "0::/system.slice/cgbr.service\n")
	for _, self := range selves {
		f.mountAt(t, root, self)
	}
	asked := 0
	return fixedCgroups(map[string]uint64{"socket:[" + strconv.Itoa(inode) + "]": f.id(t, "/system.slice/cghold.service")}, &asked)
}
