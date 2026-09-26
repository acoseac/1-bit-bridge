package doctor

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestUnifiedCgroupReadsTheV2Line pins which cgroup the census takes a
// process to run in: the path of the "0::" line of its /proc/<pid>/cgroup,
// relative to the reader's cgroup namespace, and nothing where that line is
// missing (a host mounting only v1) or names a cgroup no mount of this
// process can hold. The lines are as the kernel renders them: a unified
// host's, a hybrid host's (v1 hierarchies and a "0::" line), and the two a
// reader inside a container sees for a process outside its cgroup
// namespace, and for a deleted cgroup.
func TestUnifiedCgroupReadsTheV2Line(t *testing.T) {
	for _, tc := range []struct {
		name, file, want string
		ok               bool
	}{
		{"a unified host", "0::/system.slice/1-bit-bridge.service\n", "/system.slice/1-bit-bridge.service", true},
		{"the root of a container's namespace", "0::/\n", "/", true},
		{"a hybrid host",
			"12:pids:/system.slice/x.service\n1:name=systemd:/system.slice/x.service\n0::/system.slice/x.service\n",
			"/system.slice/x.service", true},
		{"a v1-only host", "12:pids:/system.slice/x.service\n1:name=systemd:/system.slice/x.service\n", "", false},
		{"outside the reader's cgroup namespace", "0::/../cgA.service\n", "", false},
		{"a deleted cgroup", "0::/system.slice/x.service (deleted)\n", "", false},
		{"not absolute", "0::system.slice/x.service\n", "", false},
		{"empty", "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := unifiedCgroup(strings.NewReader(tc.file))
			if got != tc.want || ok != tc.ok {
				t.Errorf("got %q, %v; want %q, %v", got, ok, tc.want, tc.ok)
			}
		})
	}
}

// mountinfoFixture is a mountinfo in the kernel's layout, the cgroup2 line
// as dido's host renders it, with a v1 cgroup mount, a cgroup2 mount of a
// subtree, and one whose mount point the kernel escaped, around it.
const mountinfoFixture = `22 28 0:21 / /proc rw,nosuid,nodev,noexec,relatime shared:12 - proc proc rw
30 25 0:26 / /sys/fs/cgroup rw,nosuid,nodev,noexec,relatime shared:9 - cgroup2 cgroup2 rw,nsdelegate,memory_recursiveprot
31 25 0:27 / /sys/fs/cgroup/pids rw,nosuid,nodev,noexec,relatime shared:10 - cgroup cgroup rw,pids
590 589 0:30 /system.slice /mnt/system\040slice ro,nosuid,nodev,noexec,relatime - cgroup2 cgroup rw
`

// TestCgroup2MountsReadsMountinfo reads the cgroup2 mounts out of a
// mountinfo: the filesystem type after the " - " separator, the mount point
// and root before it, unescaped, and nothing from a v1 cgroup mount.
func TestCgroup2MountsReadsMountinfo(t *testing.T) {
	want := []cgroupMount{
		{point: "/sys/fs/cgroup", root: "/"},
		{point: "/mnt/system slice", root: "/system.slice"},
	}
	if got := cgroup2Mounts(strings.NewReader(mountinfoFixture)); !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// TestUnescapeMountinfo undoes the kernel's octal escapes, and leaves alone
// a backslash that does not start one.
func TestUnescapeMountinfo(t *testing.T) {
	for in, want := range map[string]string{
		`/sys/fs/cgroup`: "/sys/fs/cgroup",
		`/a\040b`:        "/a b",
		`/a\011b\012c`:   "/a\tb\nc",
		`/a\134b`:        `/a\b`,
		`/a\04`:          `/a\04`,
		`/a\xyz`:         `/a\xyz`,
		`/a\999`:         `/a\999`,
	} {
		if got := unescapeMountinfo(in); got != want {
			t.Errorf("unescapeMountinfo(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestCgroupNestedIsContainmentEitherWay: two cgroups nest when one is the
// other or an ancestor of it, in either order, by whole path components, so
// a sibling whose name only starts the same does not.
func TestCgroupNestedIsContainmentEitherWay(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"/system.slice/cgbr.service", "/system.slice/cgbr.service", true},
		{"/", "/system.slice/cgbr.service", true},
		{"/system.slice", "/system.slice/cgbr.service", true},
		{"/system.slice/cgbr.service", "/system.slice", true},
		{"/system.slice/cgbr.service/sub", "/system.slice/cgbr.service", true},
		{"/system.slice/cgbr.service", "/system.slice/cghold.service", false},
		{"/system.slice/cgbr", "/system.slice/cgbr.service", false},
		{"/user.slice/user-1000.slice/session-3.scope", "/user.slice/user-1000.slice/session-4.scope", false},
	} {
		if got := cgroupNested(tc.a, tc.b); got != tc.want {
			t.Errorf("cgroupNested(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

// TestMountOfCgroupPrefersTheWidestMount: the census walks the mount that
// holds the recorded pid's cgroup, and of two that do, the one whose root is
// highest, which sees the most; a mount whose root does not hold the cgroup
// is no answer, and nor is a mountinfo that does not read.
func TestMountOfCgroupPrefersTheWidestMount(t *testing.T) {
	name := filepath.Join(t.TempDir(), "mountinfo")
	if err := os.WriteFile(name, []byte(mountinfoFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		cgroup string
		want   cgroupMount
		ok     bool
	}{
		{"/system.slice/x.service", cgroupMount{point: "/sys/fs/cgroup", root: "/"}, true},
		{"/user.slice", cgroupMount{point: "/sys/fs/cgroup", root: "/"}, true},
	} {
		if got, ok := mountOfCgroup(name, tc.cgroup); got != tc.want || ok != tc.ok {
			t.Errorf("mountOfCgroup(%q) = %+v, %v; want %+v, %v", tc.cgroup, got, ok, tc.want, tc.ok)
		}
	}
	subtree := filepath.Join(t.TempDir(), "mountinfo")
	if err := os.WriteFile(subtree, []byte(strings.Split(mountinfoFixture, "\n")[3]+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, ok := mountOfCgroup(subtree, "/system.slice/x.service"); !ok || got.root != "/system.slice" {
		t.Errorf("a subtree mount that holds the cgroup: got %+v, %v", got, ok)
	}
	if _, ok := mountOfCgroup(subtree, "/user.slice"); ok {
		t.Error("a mount whose root does not hold the cgroup was taken")
	}
	if _, ok := mountOfCgroup(filepath.Join(t.TempDir(), "absent"), "/"); ok {
		t.Error("a mountinfo that does not read was taken for a mount")
	}
}

// TestCgroupListRendersPaths renders cgroups as pidList renders pids.
func TestCgroupListRendersPaths(t *testing.T) {
	if got, want := cgroupList([]string{"/b.service", "/a.service", "/b.service"}), "cgroups /a.service, /b.service"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if got, want := cgroupList([]string{"/system.slice/x.service"}), "cgroup /system.slice/x.service"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
