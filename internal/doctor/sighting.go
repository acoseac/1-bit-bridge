package doctor

import (
	"errors"
	"fmt"
	"io/fs"
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
	// processes (Windows' listener table), or it read every one of the
	// pid's descriptors, against every socket table, and none is a
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
// on the port, the pid holds none on any address, and the verdict that
// follows (checkPort's liveness arm) must be the same on a host with lsof as
// on one without: #1028's row L4 was ruled out where lsof was missing and
// not where it was installed, so a verdict on the ruling-out alone would
// have been chosen by the tool.
//
// So a /proc match is a match: an inode names one socket. A /proc ruling-out
// is joined to lsof's account, which keeps the pids lsof named, and carries
// no blind spot. Anything else (a pid /proc cannot read, or neither socket
// table readable) leaves lsof's account as it was: /proc could see no more
// than lsof did. Off Linux, pidListensOnPort's stub neither finds nor rules
// out, so lsof's account stands there.
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
// listening on port, read from the socket tables and the fd directory it is
// handed (pidListensOnPort hands it /proc's; the tests hand it fixtures),
// and when it does not, what /proc showed.
//
// No listener in the tables rules pid out: a bridge's listener would be
// there. So does an fd directory read in full without one. A directory that
// is not there, or that cannot be read in full, leaves pid possible, and
// blind says what hides a process's descriptors. So does a socket table
// that is there and could not be read (listenerSockets' unread), since the
// listener may be in it: the account then says it covers the tables read
// (CodeRabbit on #1028). An error means neither socket table could be read.
func procSighting(tables []string, fdDir string, port, pid int, blind string) (bool, ownerSighting, error) {
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
	held, readAll, err := fdDirHoldsSocket(fdDir, sockets)
	switch {
	case held:
		return true, ownerSighting{}, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, ownerSighting{saw: fmt.Sprintf("/proc has no pid %d", pid)}, nil
	case err != nil && !errors.Is(err, fs.ErrPermission):
		return false, ownerSighting{saw: fmt.Sprintf("/proc could not list pid %d's descriptors (%s)", pid, oneLine(err.Error()))}, nil
	case err != nil || !readAll:
		return false, ownerSighting{saw: fmt.Sprintf("/proc does not let this user read pid %d's descriptors", pid), blind: blind}, nil
	case unread:
		return false, ownerSighting{saw: fmt.Sprintf("/proc shows no descriptor of pid %d listening on this port", pid) + inTheTablesRead}, nil
	default:
		return false, ownerSighting{saw: fmt.Sprintf("/proc shows no descriptor of pid %d listening on this port", pid), ruledOut: true}, nil
	}
}

// inTheTablesRead scopes a /proc account to the socket tables it could read,
// when one that is there could not be.
const inTheTablesRead = " in the socket tables it could read"

// pidList renders pids as "pid 5123" or "pids 5123, 6000", in order and
// without repeats: a process listening on both address families is one
// holder.
func pidList(pids []int) string {
	sorted := slices.Compact(slices.Sorted(slices.Values(pids)))
	words := make([]string, len(sorted))
	for i, p := range sorted {
		words[i] = strconv.Itoa(p)
	}
	if len(words) == 1 {
		return "pid " + words[0]
	}
	return "pids " + strings.Join(words, ", ")
}
