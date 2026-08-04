//go:build linux

package doctor

import "os"

// procNetTCPFiles are the per-network-namespace socket tables scanned for a
// listener's owning UID. Both families are read because a Go server binding
// the IPv6 wildcard (`[::]:port`, which is dual-stack unless IPV6_V6ONLY is
// set) appears ONLY in /proc/net/tcp6 — it is never duplicated into
// /proc/net/tcp — while an IPv4-only listener appears only in the latter.
// Package var so tests can point it at fixture files.
var procNetTCPFiles = []string{"/proc/net/tcp", "/proc/net/tcp6"}

// portOwnedByThisUser reports whether any process running as the current
// user holds a LISTEN socket on this TCP port.
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
// UID equality is deliberately WEAKER than PID equality — another process
// running as the same user matches too. The caller words its verdict to say
// exactly that. It is the ceiling of what an unprivileged observer can
// learn, and strictly more than the "unknown owner" it replaces.
func portOwnedByThisUser(port int) (bool, error) {
	me := os.Getuid()
	var firstErr error
	readAny := false
	for _, path := range procNetTCPFiles {
		f, err := os.Open(path)
		if err != nil {
			// A kernel built without IPv6 has no /proc/net/tcp6. That is
			// not a failure — the other family may still answer — so the
			// error is kept only in case NEITHER file could be read.
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		uids, scanErr := scanListenerUIDs(f, port)
		_ = f.Close()
		if scanErr != nil {
			if firstErr == nil {
				firstErr = scanErr
			}
			continue
		}
		readAny = true
		for _, uid := range uids {
			if uid == me {
				return true, nil
			}
		}
	}
	if !readAny {
		return false, firstErr
	}
	return false, nil
}
