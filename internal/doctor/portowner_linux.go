//go:build linux

package doctor

import (
	"fmt"
	"os"
)

// procNetTCPFiles are the per-network-namespace socket tables scanned for a
// listener's owning UID. Both families are read because a Go server binding
// the IPv6 wildcard (`[::]:port`, which is dual-stack unless IPV6_V6ONLY is
// set) appears ONLY in /proc/net/tcp6 — it is never duplicated into
// /proc/net/tcp — while an IPv4-only listener appears only in the latter.
// Package var so tests can point it at fixture files.
var procNetTCPFiles = []string{"/proc/net/tcp", "/proc/net/tcp6"}

// hiddenListenerOfThisUser reports whether a LISTEN socket on this TCP port
// was created by the current user and is held by no process this user can
// read (hiddenListenerOf, over this host's /proc).
//
// This is the fallback for a bridge that binds a privileged port through a
// file capability (`setcap cap_net_bind_service=+ep`, which the deployment
// runbook prescribes). Such a process runs with dumpable=0, so the kernel
// denies PTRACE_MODE_READ on /proc/<pid>/ to a same-UID unprivileged
// observer and EVERY port→pid route fails identically — lsof exits 1 with
// no output, `ss -ltnp` omits the users:((...)) column, and a direct
// readlink of /proc/<pid>/fd gives EPERM. Only root can attribute the port,
// and there is no supported unprivileged way around that.
//
// /proc/net/tcp{,6} is a different kind of file: namespace-wide, mode 0444,
// with no per-process permission check, so its `uid` column stays readable.
// hidepid=1|2 does not change this — it restricts /proc/<pid> directories,
// not the socket tables. The kernel stamps that column from the creating
// process's fsuid at socket-creation time, which for a capability binary
// (no setuid, so real == effective == fsuid) is the service user.
//
// UID equality alone is WEAKER than PID equality: another process running
// as the same user matches too. So the listener must also be hidden, held
// by no process this user can read, as the capability-bound bridge's is.
// One that a readable process holds is that process's, and until #1030 it
// matched as well: beside a capability-bound bridge whose config was edited
// to a port another process of the same user holds, it answered ok (#1028's
// row L6). The caller words its verdict to say what matched, and asks only
// where the owner probe could not rule the recorded bridge out: where /proc
// read every one of the bridge's descriptors and none is a listener on the
// port (row L4), or found every listener on the port held by processes it
// can read (row L6), created by a uid the bridge does not run as (row L7),
// or created in a cgroup that does not nest with the bridge's (row L6h),
// the port FAILs without asking. Even so this is the ceiling of what an
// unprivileged observer can learn: another hidden process of this user's
// in the bridge's OWN cgroup still matches (a second process the bridge's
// unit starts, one started from the same login session, or any process of
// a container the bridge runs in, since all of a container's share its one
// cgroup), and its listener carries the bridge's uid and cgroup, so nothing
// unprivileged tells it from the bridge.
func hiddenListenerOfThisUser(port int) (bool, error) {
	return hiddenListenerOf(procNetTCPFiles, "/proc", port, os.Getuid())
}

// pidListensOnPort reports whether pid holds a LISTEN socket on this TCP
// port, from /proc alone: the port's listener inodes from the socket
// tables, then the process's own descriptors, each of which links to
// `socket:[<inode>]` for a socket. It is the question lsof answers for
// isPIDListeningOnPort, asked of the same kernel tables lsof reads: in
// lsof's place where no usable lsof resolved, and after lsof ran and did
// not name the pid, since lsof's miss cannot rule the pid out and this can
// (procSecondOpinion).
//
// So attribution on Linux does not depend on whether lsof is installed. It
// is Priority standard on Debian and Ubuntu, so their minimal installs and
// container images lack it, and there the "is it us?" ladder could only
// reach its liveness arm. That arm is enough for a port the running
// bridge's config names, and not for one a caller is choosing, where only
// the recorded bridge seen listening may excuse a held port
// (checkChosenPort). Without this, such a host would refuse a bridge's own
// port that a host with lsof accepts: one set of facts, two verdicts,
// chosen by a tool.
//
// Its limits are lsof's own: a process of another user, or one with
// dumpable=0 (a binary granted cap_net_bind_service), keeps its descriptors
// from an unprivileged observer, and the answer is then false, as lsof's
// exit 1 is. The sighting says which it was (procSighting), and where every
// socket listening on the port is held by a process this user CAN read, or
// was created by a uid such a pid does not run as or in a cgroup that does
// not nest with its (the kernel's socket diagnostics, listenerCgroups), it
// rules the pid out all the same (procSighting's census). An error means
// neither socket table could be read.
func pidListensOnPort(port, pid int) (bool, ownerSighting, error) {
	if pid <= 0 {
		return false, ownerSighting{saw: fmt.Sprintf("/proc has no pid %d", pid)}, nil
	}
	return procSighting(procNetTCPFiles, "/proc", port, pid, blindSpot(), listenerCgroups)
}

// blindSpot says what an owner probe run as this user cannot see on Linux,
// for the account of a port it did not attribute (ownerSighting.blind).
// lsof and pidListensOnPort share it, since both read /proc/<pid>/fd, and
// the kernel shows a process's descriptor links only to a reader whose uid
// AND gid match the process's, and only while the process is dumpable
// (ptrace's read check), unless the reader holds CAP_SYS_PTRACE. A binary
// granted cap_net_bind_service runs with dumpable=0: the case #640's arm
// was written for, which is why it is named, as an example.
//
// Root gets "": outside a container it reads every process, and inside one
// Docker does not grant CAP_SYS_PTRACE, so it may not. Nothing short of a
// probe says which, and saying nothing is not false.
func blindSpot() string {
	uid := os.Geteuid()
	if uid == 0 {
		return ""
	}
	return fmt.Sprintf("uid %d cannot read the descriptors of a process that runs as another user or group, "+
		"or with dumpable=0, which is how a binary granted cap_net_bind_service runs", uid)
}
