package doctor

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// ownerSighting is the owner probe's account of a held port on which it did
// not find the pid it was asked about: isPIDListeningOnPort's second result.
// The probe that looked writes it, because only that probe knows what it
// could see, and the two arms that explain a miss read it: checkPort's
// liveness arm (liveUnseenHint and its ok summary) and checkChosenPort's
// last one (chosenUnseenHint).
//
// Until this existed the liveness arm gave every miss the one cause it was
// written for (#640): "pid attribution blocked — capability-bound binary",
// and a hint naming cap_net_bind_service and dumpable=0. That is one shape,
// a bridge granted the capability, and the arm printed it for a bridge
// running as another user, for a port whose holder lsof had just named, and
// on macOS and Windows, which have no such capability.
//
// One verdict turns on it: a live recorded pid that the sighting rules out
// FAILs the port in checkPort, as a dead one does, since the port is then
// another process's (the liveness arm). The words never decide anything.
//
// Untagged, like the /proc parser in portowner.go, so what each account says
// is tested on every platform rather than only where its probe runs.
type ownerSighting struct {
	// saw says what the probe looked at and what it found, as a clause
	// that reads after "but": "lsof lists pid 1305 listening on this port".
	saw string
	// blind says why the probe could have missed the pid: what it cannot
	// see, as the user running it (blindSpot). Empty when there is nothing
	// to add, and always when ruledOut.
	blind string
	// ruledOut reports that what the probe saw excludes the pid as the
	// port's holder: it saw every listener on the port and they are other
	// processes' (Windows' listener table, or, where /proc could not read
	// the pid, its census: each listener held by a process it can read, or
	// created by a uid the pid does not run as or in a cgroup that does not
	// nest with the pid's), or it read every one of
	// the pid's descriptors, against every socket table, and none is a
	// listener on the port (/proc, asked on Linux after lsof misses or in
	// its place). Never lsof alone, which lists only the processes it can
	// see (lsofSighting). The zero value keeps the hedged advice ("if our
	// bridge is what holds the port, this is expected") and the verdict
	// that goes with it, which is the safe one to fall back on.
	ruledOut bool
}

// account is the sighting's clause, with a fallback for a probe that set
// none, so a hint never reads "…is still running, but ." about it.
func (s ownerSighting) account() string {
	if s.saw == "" {
		return "the owner probe did not see it on this port"
	}
	return s.saw
}

// because is the blind spot as a parenthesis to follow the account, or ""
// when there is none.
func (s ownerSighting) because() string {
	if s.blind == "" {
		return ""
	}
	return " (" + s.blind + ")"
}

// lsofSighting is lsof's account of a port on which `lsof -t` did not name
// pid, from what it printed: nothing (its exit 1, "matched nothing"), a pid
// a line, or something else.
//
// A pid a line is all `lsof -t` prints, and names what holds the port. It
// does not rule pid out: lsof lists only the processes this user may
// inspect, and a bridge it cannot see (another user's, or one with
// dumpable=0) can listen on the same port at another address, a
// `listenAddress` on one interface beside a holder on loopback (CodeRabbit
// on #1028). So every account from lsof keeps the blind spot and the
// hedge. Nothing listed is what a listener hidden from this user looks
// like. Anything else is not `lsof -t`'s output: busybox's applet ignores
// every option and lists every open file (the image's lsof bullet in
// CLAUDE.md), so all it shows is that pid is not in it.
func lsofSighting(out []byte, pid int, blind string) ownerSighting {
	pids, ok := lsofPIDs(out)
	switch {
	case !ok:
		return ownerSighting{saw: fmt.Sprintf("lsof's output does not name pid %d", pid), blind: blind}
	case len(pids) == 0:
		return ownerSighting{saw: "lsof lists no process listening on this port", blind: blind}
	default:
		return ownerSighting{saw: "lsof lists " + pidList(pids) + " listening on this port", blind: blind}
	}
}

// lsofPIDs parses `lsof -t` output, one pid a line, and answers false for
// output of any other shape.
func lsofPIDs(out []byte) ([]int, bool) {
	var pids []int
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		n, err := strconv.Atoi(line)
		if err != nil || n <= 0 {
			return nil, false
		}
		pids = append(pids, n)
	}
	return pids, true
}

// procSecondOpinion is what the /proc attribution adds to lsof's account
// (lsofSeen) of a port on which lsof did not name the pid, given /proc's own
// answer about that pid: whether it found the pid holding a listener on the
// port, its sighting, and its error. isPIDListeningOnPort asks it after
// every clean lsof miss.
//
// lsof lists only the processes this user may inspect, so its miss never
// rules the pid out (lsofSighting). /proc reads the pid's own descriptors,
// under the kernel check lsof's readlinks meet too (proc_fd_access_allowed,
// ptrace's read check). Where every one of them read and none is a listener
// on the port, or where it could not read them and every listener on the
// port is held by a process it can read, or was created by a uid the pid
// does not run as or in a cgroup that does not nest with the pid's
// (procSighting's census), the pid holds none on any
// address, and the verdict that follows (checkPort's liveness arm) must be
// the same on a host with lsof as on one without: #1028's row L4 was ruled
// out where lsof was missing and not where it was installed, so a verdict
// on the ruling-out alone would have been chosen by the tool. The census
// sits inside /proc's answer for the same reason.
//
// So a /proc match is a match: an inode names one socket. A /proc ruling-out
// is joined to lsof's account, which keeps the pids lsof named, and carries
// no blind spot. Anything else (a pid /proc cannot read, on a port whose
// listeners it cannot account for, or neither socket table readable)
// leaves lsof's account as it was: /proc could see no more than lsof did.
// Off Linux, pidListensOnPort's stub neither finds nor rules out, so lsof's
// account stands there.
func procSecondOpinion(lsofSeen ownerSighting, procFound bool, procSeen ownerSighting, procErr error) (bool, ownerSighting) {
	switch {
	case procErr != nil:
		return false, lsofSeen
	case procFound:
		return true, ownerSighting{}
	case procSeen.ruledOut:
		return false, ownerSighting{saw: lsofSeen.account() + ", and " + procSeen.account(), ruledOut: true}
	default:
		return false, lsofSeen
	}
}

// listenerTableSighting is Windows' account (doctor_windows.go) of a port
// whose rows in GetExtendedTcpTable's listener tables do not carry the pid
// asked about. Each row carries its listener's owning pid, the one
// `netstat -ano` prints, so the pid is ruled out and there is no blind spot
// to report. owners are the processes listening on the port; none at all
// means the port is held by a socket that is bound and not listening.
func listenerTableSighting(owners []int) ownerSighting {
	if len(owners) == 0 {
		return ownerSighting{saw: "Windows' TCP listener table lists no process listening on this port", ruledOut: true}
	}
	return ownerSighting{saw: "Windows' TCP listener table lists " + pidList(owners) + " on this port", ruledOut: true}
}

// procSighting is the /proc attribution: whether pid holds a socket
// listening on port, read from the socket tables and the process
// directories under procRoot (pidListensOnPort hands it /proc; the tests
// hand it fixtures), and when it does not, what /proc showed.
//
// No listener in the tables rules pid out: a bridge's listener would be
// there. So does an fd directory read in full without one. A directory that
// is not there, or that cannot be read in full, leaves pid possible, and
// blind says what hides a process's descriptors. So does a socket table
// that is there and could not be read (listenerSockets' unread), since the
// listener may be in it: the account then says it covers the tables read
// (CodeRabbit on #1028). An error means neither socket table could be read.
//
// Where pid's descriptors could not be read in full, the port's OTHER
// listeners can still rule it out (listenersNotOf): every socket listening
// on the port held by a process this user can read, other than pid, or
// created by a uid pid does not run as, or, created by pid's own uid, in a
// cgroup that does not nest with pid's (cgroupsNotOf, which asks the
// kernel's socket diagnostics through cgroupsOf, since /proc/net/tcp{,6}
// carries no cgroup). That is the capability-bound bridge:
// it runs with dumpable=0, no probe can read it, and when its config was
// edited to a port something else holds, the check said ok (#1028's row L6,
// a holder of the same user) or warned (#1030's row L7, a holder of another
// user's, or of root's), or read ok again (row L6h, a hidden holder of the
// same user in another cgroup).
//
// The holders: an inode names one socket, and a listener of pid's own would
// be held by pid alone, which nothing here can read, so it would have no
// holder. The one shape this cannot see is a socket pid SHARES with a
// readable process, and a bridge shares no listener: it makes each with
// net.Listen, close-on-exec, and hands none on, and no process of this user
// can take one from a dumpable=0 process (pidfd_getfd needs CAP_SYS_PTRACE
// there). Windows' listener table rules a pid out on the same terms, since
// a duplicated socket keeps the binder's pid.
//
// The creators, for a listener no readable process holds, which is what a
// process of another user holds: the table's uid column is the fsuid that
// created the socket, and /proc/<pid>/status shows pid's, readable where its
// descriptors are not (pidFSUID). A bridge's listener carries the bridge's
// own: it creates each itself, never changes uid (no setuid-family call is
// linked into it), takes no listener from another process (nothing parses
// SCM_RIGHTS or makes a listener from an fd), and never fchowns a socket,
// the one call that re-stamps the column. So a listener another uid created
// is not the bridge's. Equal uids say nothing, and nor does a uid /proc
// does not show (createdByAnother).
//
// The cgroups, for a listener of pid's own uid that no readable process
// holds, which is what another hidden process of the same user holds: the
// kernel's socket diagnostics give the cgroup that created the socket, and
// /proc/<pid>/cgroup the one pid runs in, readable where its descriptors
// are not. A bridge creates its listeners in the cgroup it runs in and
// never moves, so a listener created in a cgroup that neither is pid's,
// contains it, nor sits below it is not the bridge's (cgroupsNotOf, which
// says why nesting and anything this process cannot see count nothing).
//
// A listener that is none of these, or a table that did not read, leaves
// pid possible, as before.
//
// Everything that reads a pid's directory, its status and its cgroup
// included, first checks that procRoot numbers processes as this process
// does (procOfAnotherPIDNamespace): under another pid namespace's /proc,
// <pid> is some other process.
func procSighting(tables []string, procRoot string, port, pid int, blind string, cgroupsOf socketCgroups) (bool, ownerSighting, error) {
	sockets, unread, err := listenerSockets(tables, port)
	if err != nil {
		return false, ownerSighting{}, err
	}
	if len(sockets) == 0 {
		if unread {
			return false, ownerSighting{saw: "/proc lists no socket listening on this port" + inTheTablesRead}, nil
		}
		return false, ownerSighting{saw: "/proc lists no socket listening on this port", ruledOut: true}, nil
	}
	if other := procOfAnotherPIDNamespace(procRoot); other != "" {
		return false, ownerSighting{saw: other}, nil
	}
	held, readAll, err := fdDirHoldsSocket(filepath.Join(procRoot, strconv.Itoa(pid), "fd"), sockets)
	var possible ownerSighting
	switch {
	case held:
		return true, ownerSighting{}, nil
	case errors.Is(err, fs.ErrNotExist):
		possible = ownerSighting{saw: fmt.Sprintf("/proc has no pid %d", pid)}
	case err != nil && !errors.Is(err, fs.ErrPermission):
		possible = ownerSighting{saw: fmt.Sprintf("/proc could not list pid %d's descriptors (%s)", pid, oneLine(err.Error()))}
	case err != nil || !readAll:
		possible = ownerSighting{saw: fmt.Sprintf("/proc does not let this user read pid %d's descriptors", pid), blind: blind}
	case unread:
		return false, ownerSighting{saw: fmt.Sprintf("/proc shows no descriptor of pid %d listening on this port", pid) + inTheTablesRead}, nil
	default:
		return false, ownerSighting{saw: fmt.Sprintf("/proc shows no descriptor of pid %d listening on this port", pid), ruledOut: true}, nil
	}
	if !unread {
		if others, all := listenersNotOf(procRoot, sockets, pid, port, cgroupsOf); all {
			return false, ownerSighting{saw: others.account(pid), ruledOut: true}, nil
		}
	}
	return false, possible, nil
}

// account is /proc's account of a port whose every listener it showed to
// be another process's (listenersNotOf): the readable processes that hold
// them, then the uids and the cgroups that created the rest, against the
// uid and the cgroup pid runs as and in. The kernel's socket diagnostics
// join /proc as the source when a cgroup is named, since /proc/net/tcp{,6}
// does not carry one. An account without cgroups keeps #1032's words.
func (o othersListening) account(pid int) string {
	lead := "/proc shows every socket listening on this port "
	if len(o.cgroups) > 0 {
		lead = "/proc and the kernel's socket diagnostics show every socket listening on this port "
	}
	var terms, created, runs []string
	if len(o.holders) > 0 {
		terms = append(terms, "held by "+pidList(o.holders))
	}
	if len(o.creators) > 0 {
		created = append(created, "by "+uidList(o.creators))
		runs = append(runs, fmt.Sprintf("as uid %d", o.pidUID))
	}
	if len(o.cgroups) > 0 {
		created = append(created, "in "+cgroupList(o.cgroups))
		runs = append(runs, "in cgroup "+o.pidCgroup)
	}
	if len(created) == 0 {
		return lead + strings.Join(terms, " or ")
	}
	terms = append(terms, "created "+strings.Join(created, " or "))
	return lead + strings.Join(terms, " or ") + fmt.Sprintf(", while pid %d runs ", pid) + strings.Join(runs, " ")
}

// inTheTablesRead scopes a /proc account to the socket tables it could read,
// when one that is there could not be.
const inTheTablesRead = " in the socket tables it could read"

// pidList renders pids as "pid 5123" or "pids 5123, 6000", in order and
// without repeats: a process listening on both address families is one
// holder.
func pidList(pids []int) string { return idList("pid", pids) }

// uidList renders uids as pidList renders pids: "uid 0" or "uids 0, 1001".
func uidList(uids []int) string { return idList("uid", uids) }

// cgroupList renders cgroup paths as pidList renders pids: "cgroup
// /system.slice/x.service" or "cgroups /a.service, /b.service".
func cgroupList(cgroups []string) string {
	sorted := slices.Compact(slices.Sorted(slices.Values(cgroups)))
	if len(sorted) == 1 {
		return "cgroup " + sorted[0]
	}
	return "cgroups " + strings.Join(sorted, ", ")
}

// idList renders ids after noun, pluralised with an "s" past one, in order
// and without repeats.
func idList(noun string, ids []int) string {
	sorted := slices.Compact(slices.Sorted(slices.Values(ids)))
	words := make([]string, len(sorted))
	for i, id := range sorted {
		words[i] = strconv.Itoa(id)
	}
	if len(words) == 1 {
		return noun + " " + words[0]
	}
	return noun + "s " + strings.Join(words, ", ")
}
