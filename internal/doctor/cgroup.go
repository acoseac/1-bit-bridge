package doctor

import (
	"bufio"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// This file holds the census's third accounting (cgroupsNotOf), untagged
// like the rest of the /proc attribution: it takes its paths as arguments,
// so the tests hand it a fixture /proc and a fixture cgroup tree.

// socketCgroups returns the cgroup each socket listening on port was created
// in, as the id of that cgroup's directory on the cgroup2 mount (its inode
// number), keyed by the socket's fd-link text, `socket:[<inode>]`. A socket
// it has no cgroup for is absent. listenerCgroups asks the kernel on Linux;
// the tests hand in fixtures, and nil means nothing asks at all.
type socketCgroups func(port int) (map[string]uint64, error)

// cgroupMount is a cgroup2 mount this process can see: the directory it is
// mounted on, and the cgroup at its root as mountinfo renders it, a path in
// this process's cgroup namespace, as /proc/<pid>/cgroup's paths are.
type cgroupMount struct {
	point, root string
}

// cgroupWalkBudget bounds the directories findCgroups visits, which bounds
// the time a port check can spend looking for a listener's cgroup. The
// walk reads each directory whole, the controller files included: 62
// directories took 4.5 to 4.8 ms on dido (2026-09-26), so this is a few
// hundred milliseconds at worst, spent only on a port the check is about to
// FAIL. A cgroup past the budget is one this process did not find, which
// counts nothing.
const cgroupWalkBudget = 4096

// cgroupsNotOf is the census's third accounting, for the listeners that
// neither a readable holder nor another uid accounts for (rest): whether
// every one of them was created in a cgroup that is not pid's, and not one
// that contains pid's or that pid's contains (cgroupNested), both cgroups
// being ones this process can see. cgroups are the paths of those cgroups,
// and pidCgroup the one pid runs in.
//
// That is row L6h of the port-verdict matrix: a bridge granted
// cap_net_bind_service runs with dumpable=0, so nothing reads its
// descriptors, and when its config is edited to a port that ANOTHER hidden
// process of the same uid holds (a second capability-bound binary of the
// service user, or one of its processes in another group, or in a
// container), that listener has no readable holder and carries the
// bridge's own uid, which is how the bridge on its own port looks (row L2).
// The cgroup tells them apart wherever the two run in different cgroups: a
// socket is stamped with the cgroup of the process that called socket(2),
// and that stamp never moves (sockdiag_linux.go), so a listener created in
// a cgroup the bridge is not in is not the bridge's.
//
// That rests on a premise about the bridge, like the uid's: it creates
// every listener itself, in the cgroup it runs in, and its process never
// moves to another. It calls net.Listen, takes no socket from another
// process (neither systemd's socket activation nor a handoff), and nothing
// in it writes cgroup.procs or asks systemd to move it; systemd moves no
// running service (measured on systemd 259: a Slice= edit and
// daemon-reload leave the process where it was, until a restart). Only
// root, or a delegated manager moving a process it started, moves one. A
// bridge moved to another cgroup after it listened FAILs its own port here,
// and the hint names both cgroups.
//
// Nesting counts nothing, in either direction. The kernels from 5.8 to 5.14
// (and 5.10.y before 5.10.226) stamp every new socket with the ROOT cgroup
// once net_cls or net_prio v1 tagging is in use (fixed by 8520e224f547),
// and the root contains every cgroup; a process's threads or a delegated
// subtree put its own sockets in a cgroup below the one /proc/<pid>/cgroup
// names; and a process moved into a child of its own cgroup, or out of
// one, nests with the cgroup it listened in. None of those is another
// process's socket.
//
// Anything this process cannot see counts nothing either: a socket the
// kernel gave no cgroup for (a kernel before 5.8), pid's /proc/<pid>/cgroup
// not readable (hidepid) or without a cgroup v2 line (a v1-only host), a
// path outside every cgroup2 mount (another cgroup namespace's, which a
// container's /proc renders with a leading "/.."), and a cgroup the walk
// does not find: one outside this cgroup namespace, a deleted one (a
// socket keeps the cgroup it was created in after that is removed), or one
// past the budget. So inside a container, where every process shares the
// container's one cgroup, this answers nothing, as before.
//
// cgroupsOf is asked only here, and only when rest is not empty, which
// the bridge's own port always gives it.
func cgroupsNotOf(procRoot string, pid int, rest []string, port int, cgroupsOf socketCgroups) (cgroups []string, pidCgroup string, all bool) {
	if cgroupsOf == nil {
		return nil, "", false
	}
	pidCgroup, ok := readUnifiedCgroup(filepath.Join(procRoot, strconv.Itoa(pid), "cgroup"))
	if !ok {
		return nil, "", false
	}
	m, ok := mountOfCgroup(filepath.Join(procRoot, "self", "mountinfo"), pidCgroup)
	if !ok {
		return nil, "", false
	}
	pidID, ok := cgroupID(m, pidCgroup)
	if !ok {
		return nil, "", false
	}
	created, err := cgroupsOf(port)
	if err != nil {
		return nil, "", false
	}
	want := map[uint64]bool{}
	for _, s := range rest {
		id, ok := created[s]
		if !ok || id == pidID {
			return nil, "", false
		}
		want[id] = true
	}
	paths := findCgroups(m, want, cgroupWalkBudget)
	for id := range want {
		p, ok := paths[id]
		if !ok || cgroupNested(p, pidCgroup) {
			return nil, "", false
		}
		cgroups = append(cgroups, p)
	}
	slices.Sort(cgroups)
	return cgroups, pidCgroup, true
}

// readUnifiedCgroup is unifiedCgroup over the file at name, false when it
// does not read.
func readUnifiedCgroup(name string) (string, bool) {
	f, err := os.Open(name)
	if err != nil {
		return "", false
	}
	defer func() { _ = f.Close() }()
	return unifiedCgroup(f)
}

// unifiedCgroup returns the cgroup a /proc/<pid>/cgroup file places the
// process in on the cgroup2 hierarchy: the path of its "0::" line, which is
// relative to the reader's cgroup namespace. A host that mounts only the v1
// hierarchies shows no such line, and false is the answer there. So it is
// for a path no mount this process can see holds: one that is not clean,
// which is how the kernel renders a cgroup outside the reader's namespace
// ("/../cgA.service", measured from inside a container, whose own cgroup is
// "/"), and a deleted one (" (deleted)"). A path that climbs would
// otherwise be joined onto the mount point and name a directory outside it.
//
// The file reads where the process's descriptors do not: it is mode 0444
// and ptrace's check does not guard it, so it reads for a process of
// another user and for one with dumpable=0 (measured on dido: a
// capability-bound holder of uid 1000, whose fd directory uid 1000 cannot
// list). hidepid hides it with the rest of /proc/<pid>.
func unifiedCgroup(r io.Reader) (string, bool) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		p, found := strings.CutPrefix(sc.Text(), "0::")
		if !found {
			continue
		}
		if !strings.HasPrefix(p, "/") || path.Clean(p) != p || strings.HasSuffix(p, " (deleted)") {
			return "", false
		}
		return p, true
	}
	return "", false
}

// mountOfCgroup returns the cgroup2 mount in the mountinfo file at name that
// holds the cgroup at path cg, preferring the one whose root is highest,
// whose walk sees the most: on a host that is the whole hierarchy, mounted
// on /sys/fs/cgroup with root "/".
func mountOfCgroup(name, cg string) (cgroupMount, bool) {
	f, err := os.Open(name)
	if err != nil {
		return cgroupMount{}, false
	}
	defer func() { _ = f.Close() }()
	var best cgroupMount
	found := false
	for _, m := range cgroup2Mounts(f) {
		if !cgroupContains(m.root, cg) {
			continue
		}
		if !found || len(m.root) < len(best.root) {
			best, found = m, true
		}
	}
	return best, found
}

// cgroup2Mounts returns the cgroup2 mounts a mountinfo file lists: the
// fifth field, the mount point, and the fourth, the root of the mount
// within the filesystem, both unescaped. The filesystem type is the first
// field after the " - " separator, since the optional fields before it
// vary in number (proc(5)).
func cgroup2Mounts(r io.Reader) []cgroupMount {
	var mounts []cgroupMount
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		pre, post, ok := strings.Cut(sc.Text(), " - ")
		if !ok {
			continue
		}
		fields, fsFields := strings.Fields(pre), strings.Fields(post)
		if len(fields) < 5 || len(fsFields) < 1 || fsFields[0] != "cgroup2" {
			continue
		}
		mounts = append(mounts, cgroupMount{point: unescapeMountinfo(fields[4]), root: unescapeMountinfo(fields[3])})
	}
	return mounts
}

// unescapeMountinfo undoes mountinfo's escaping of a path: the kernel
// writes a space, tab, newline and backslash as a backslash and three
// octal digits (proc(5), seq_path's "\t\n\\ ").
func unescapeMountinfo(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// cgroupContains reports whether the cgroup at path outer is the cgroup at
// path inner or one of its ancestors.
func cgroupContains(outer, inner string) bool {
	return outer == "/" || inner == outer || strings.HasPrefix(inner, outer+"/")
}

// cgroupNested reports whether one of two cgroups is the other or contains
// it.
func cgroupNested(a, b string) bool {
	return cgroupContains(a, b) || cgroupContains(b, a)
}

// cgroupDir is the directory holding the cgroup at path cg on mount m, which
// cgroupContains(m.root, cg) must hold for.
func cgroupDir(m cgroupMount, cg string) string {
	rel := strings.TrimPrefix(strings.TrimPrefix(cg, m.root), "/")
	return filepath.Join(m.point, filepath.FromSlash(rel))
}

// cgroupID is the id of the cgroup at path cg on mount m: its directory's
// inode number, which on a 64-bit kernel is the cgroup's whole id, the
// value INET_DIAG_CGROUP_ID reports (measured on dido: equal for all 60
// directories, and equal to the id name_to_handle_at returns, which is how
// ss maps it).
func cgroupID(m cgroupMount, cg string) (uint64, bool) {
	fi, err := os.Stat(cgroupDir(m, cg))
	if err != nil || !fi.IsDir() {
		return 0, false
	}
	return inodeOf(fi)
}

// findCgroups walks mount m for the cgroups whose ids are in want and
// returns the path of each it finds, in the form /proc/<pid>/cgroup gives
// (m.root, then the directories below it). It stops once it has found them
// all, and after budget directories whether or not it has.
func findCgroups(m cgroupMount, want map[uint64]bool, budget int) map[uint64]string {
	found := map[uint64]string{}
	visited := 0
	_ = filepath.WalkDir(m.point, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		if visited++; visited > budget {
			return filepath.SkipAll
		}
		fi, err := d.Info()
		if err != nil {
			return nil
		}
		if id, ok := inodeOf(fi); ok && want[id] {
			rel, err := filepath.Rel(m.point, p)
			if err != nil {
				return nil
			}
			found[id] = path.Join(m.root, filepath.ToSlash(rel))
			if len(found) == len(want) {
				return filepath.SkipAll
			}
		}
		return nil
	})
	return found
}
