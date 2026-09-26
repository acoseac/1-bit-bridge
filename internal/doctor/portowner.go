package doctor

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// This file holds the PARSER for Linux's /proc/net/tcp{,6} socket tables.
// Only portowner_linux.go can actually read those files, but the parsing is
// deliberately kept UNTAGGED so it compiles and is tested on every platform.
//
// That is the lesson from the scanner fixtures that lived in a
// `//go:build !windows` file: a build-tagged helper is only ever exercised
// where the tag matches, and the breakage stays invisible until someone
// builds the other platform. Everything subtle here — the byte order of the
// port, the state filter, the column indices — is pure string work with no
// Linux dependency, so there is no reason to hide it from macOS and Windows
// CI.
//
// The same holds for the table reader, the fd walk and the census of a
// port's holders below them, which take their paths as arguments:
// portowner_linux.go hands them /proc, and the tests hand them fixture
// files.

// tcpStateListen is TCP_LISTEN as rendered in the `st` column.
const tcpStateListen = "0A"

// Column indices in /proc/net/tcp{,6} after strings.Fields:
//
//	sl  local_address rem_address st tx_queue:rx_queue tr:tm->when retrnsmt uid timeout inode
//	 0        1             2      3          4              5         6     7      8      9
const (
	colLocalAddress = 1
	colState        = 3
	colUID          = 7
	// colInode is the socket's inode: the number a process's
	// /proc/<pid>/fd link names as `socket:[<inode>]`, which is how a
	// socket in the table is tied to the process holding it.
	colInode = 9
)

// eachListenRow calls fn with the fields of every LISTEN-state row whose
// local port matches: the one row filter scanListenRows reads the inode and
// the uid through, so no reader of the tables can disagree with another
// about which rows are the port's listeners.
func eachListenRow(r io.Reader, port int, fn func(f []string)) error {
	// The port half of local_address is BIG-endian hex, zero-padded to four
	// digits: 443 -> "01BB", 7789 -> "1E6D". (The address half is
	// little-endian for IPv4, but it is never parsed — matching on port
	// alone across all local addresses is the same question lsof answers
	// for `-iTCP:<port>`, and checkPort has already established that the
	// port is occupied.) Comparing the rendered suffix avoids parsing a
	// number out of every row.
	want := fmt.Sprintf(":%04X", port)
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) <= colState {
			// A row truncated by a concurrent read.
			continue
		}
		// The state filter is load-bearing, not tidiness: an ephemeral
		// OUTBOUND connection FROM this port would otherwise match on the
		// local-port column and contribute a foreign UID, or the inode of
		// a socket that is not the listener. It also drops the header,
		// whose `st` column reads "st".
		if !strings.EqualFold(f[colState], tcpStateListen) {
			continue
		}
		if !strings.HasSuffix(strings.ToUpper(f[colLocalAddress]), want) {
			continue
		}
		fn(f)
	}
	return sc.Err()
}

// listenRow is one LISTEN row of a socket table: the socket's inode, as the
// table renders it (decimal), and the uid that created the socket, or -1
// where that column does not parse. The inode is what a process's
// /proc/<pid>/fd link names, `socket:[<inode>]`, which is how a listener is
// tied to the process holding it. The uid is the creator's fsuid, fixed at
// socket(2) and mapped into the reader's user namespace, as getuid is.
type listenRow struct {
	inode string
	uid   int
}

// scanListenRows returns every LISTEN row whose local port matches. Split
// from the table reader so it can be tested against captured real /proc
// output without needing a matching live socket, a particular uid, or
// Linux.
func scanListenRows(r io.Reader, port int) ([]listenRow, error) {
	var rows []listenRow
	err := eachListenRow(r, port, func(f []string) {
		if len(f) <= colInode {
			return
		}
		uid, err := strconv.Atoi(f[colUID])
		if err != nil {
			uid = -1
		}
		rows = append(rows, listenRow{inode: f[colInode], uid: uid})
	})
	if err != nil {
		return nil, err
	}
	return rows, nil
}

// readSocketTables hands each socket table in paths to scan, in order, and
// stops at the first that answers done. A table that cannot be opened or
// read is skipped, because a kernel built without IPv6 has no
// /proc/net/tcp6 and the other family may still answer; the first such
// error is returned only when no table could be read at all.
//
// unread reports a table skipped for any reason but its absence. A match
// the scan found in the tables it read stands, and so does the error
// contract above, which the verdicts rest on: the uid scan and the /proc
// attribution answer as they always have. What unread changes is only
// what a MISS may claim. The scan saw less than the namespace's sockets,
// so "not in the tables" rules nothing out (procSighting; CodeRabbit on
// #1028, which proposed returning the error instead: that turns a bridge
// found in /proc/net/tcp beside an unreadable tcp6 from ok into a probe
// failure).
func readSocketTables(paths []string, scan func(r io.Reader) (done bool, err error)) (unread bool, err error) {
	var firstErr error
	readAny := false
	for _, path := range paths {
		// Wrapped in a closure so the Close is deferred: this runs in a
		// loop, so a plain `defer` would hold every descriptor until the
		// function returns, and a bare post-call Close is skipped on a
		// panic.
		done, tableErr := func() (bool, error) {
			f, err := os.Open(path)
			if err != nil {
				return false, err
			}
			defer func() { _ = f.Close() }()
			return scan(f)
		}()
		if tableErr != nil {
			if firstErr == nil {
				firstErr = tableErr
			}
			if !errors.Is(tableErr, fs.ErrNotExist) {
				unread = true
			}
			continue
		}
		readAny = true
		if done {
			return unread, nil
		}
	}
	if !readAny {
		return unread, firstErr
	}
	return unread, nil
}

// listenerSockets returns every socket listening on port in the given
// tables, as its fd-link text, `socket:[<inode>]`, which is how
// /proc/<pid>/fd renders a socket descriptor, mapped to the uid that
// created it (listenRow); and whether a table that is there could not be
// read (readSocketTables' unread).
func listenerSockets(paths []string, port int) (sockets map[string]int, unread bool, err error) {
	sockets = map[string]int{}
	unread, err = readSocketTables(paths, func(r io.Reader) (bool, error) {
		rows, scanErr := scanListenRows(r, port)
		if scanErr != nil {
			return false, scanErr
		}
		for _, row := range rows {
			sockets["socket:["+row.inode+"]"] = row.uid
		}
		return false, nil
	})
	if err != nil {
		return nil, unread, err
	}
	return sockets, unread, nil
}

// hiddenListenerOf reports whether a socket listening on port in the given
// tables was created by uid AND is held by no process under procRoot whose
// descriptors this user can read (socketHolders). It is the uid arm's
// question (hiddenListenerOfThisUser), asked of this host's /proc there and
// of fixtures in the tests. An error means no table could be read.
//
// Both marks are needed, and they are the marks of a bridge granted
// cap_net_bind_service, which runs as this user and with dumpable=0, so
// that no process of this user's can read its descriptors. The first alone
// is any process of this user's, and the arm answered ok on it for another
// process's listener beside a capability-bound bridge that held no socket
// on the port (#1028's row L6), wherever the census could not rule that
// bridge out: when hidepid hides the bridge's uid and a listener of root's
// shares the port at another address, say. A listener that a process this
// user can read holds is that process's.
//
// Unlike the census it is handed no pid, so it does not check /proc's pid
// namespace (procOfAnotherPIDNamespace), and neither direction needs it. A
// readable holder is one whatever number /proc gives it. And a /proc that
// could omit a holder this namespace's /proc would list never gets here:
// the socket tables are /proc/self/net's, and only a /proc of this pid
// namespace or of an ancestor resolves self, and an ancestor's lists every
// process of this one. Measured on dido (#1030): with a container's /proc
// mounted over this process's (`nsenter -m`), readlink /proc/self fails and
// both tables are unreadable, so this answers an error; under `unshare
// --pid --fork` without --mount-proc, the tables read and /proc lists all
// 321 host processes. The census's guard here would only turn the
// capability-bound bridge's own port from ok to a warn under that ancestor
// /proc.
func hiddenListenerOf(tables []string, procRoot string, port, uid int) (bool, error) {
	sockets, _, err := listenerSockets(tables, port)
	if err != nil {
		return false, err
	}
	mine := map[string]int{}
	for s, u := range sockets {
		if u == uid {
			mine[s] = u
		}
	}
	if len(mine) == 0 {
		return false, nil
	}
	held := socketHolders(procRoot, mine, 0)
	for s := range mine {
		if len(held[s]) == 0 {
			return true, nil
		}
	}
	return false, nil
}

// listenersNotOf is the census procSighting rules out a pid it cannot read
// with: whether every socket in sockets, all listening on port, is shown to
// be another process's (all), and by what (othersListening). A socket is
// another process's when a process under procRoot other than pid holds it
// and this user can read that process's descriptors (socketHolders); or,
// where no such process holds it, when the uid that created it is not the
// one pid runs as (createdByAnother, against pidFSUID); or, where that uid
// is pid's own or not shown, when the cgroup it was created in is not pid's
// and does not nest with it (cgroupsNotOf, from cgroupsOf). A socket that
// is none of these leaves all false.
//
// A holder is named in preference to a creator, since the process that holds
// the port is what an operator stops. The walk runs whatever the uids say,
// for that reason, and a uid is read before a cgroup, which costs a question
// to the kernel and, where the cgroups differ, a walk of the cgroup tree.
func listenersNotOf(procRoot string, sockets map[string]int, pid, port int, cgroupsOf socketCgroups) (othersListening, bool) {
	held := socketHolders(procRoot, sockets, pid)
	o := othersListening{pidUID: pidFSUID(procRoot, pid)}
	var rest []string
	for s, creator := range sockets {
		switch {
		case len(held[s]) > 0:
			o.holders = append(o.holders, held[s]...)
		case createdByAnother(creator, o.pidUID):
			o.creators = append(o.creators, creator)
		default:
			rest = append(rest, s)
		}
	}
	if len(rest) > 0 {
		cgroups, pidCgroup, all := cgroupsNotOf(procRoot, pid, rest, port, cgroupsOf)
		if !all {
			return othersListening{}, false
		}
		o.cgroups, o.pidCgroup = cgroups, pidCgroup
	}
	return o, true
}

// othersListening is the census's account of a port it rules the recorded
// pid out of (listenersNotOf): what showed each listener on the port to be
// another process's.
type othersListening struct {
	// holders are the processes, other than the recorded pid, that hold a
	// listener and whose descriptors this user can read.
	holders []int
	// creators are the uids that created a listener no readable process
	// holds, and pidUID the fsuid the recorded pid runs as, -1 where /proc
	// does not show it.
	creators []int
	pidUID   int
	// cgroups are the cgroups the listeners neither of those accounts for
	// were created in, and pidCgroup the one the recorded pid runs in
	// (cgroupsNotOf).
	cgroups   []string
	pidCgroup string
}

// createdByAnother reports whether a socket whose table row gives creator as
// the uid that created it was created by a uid other than pidUID, a
// process's fsuid: the uid a socket it creates now carries. -1 on either
// side is a value /proc did not show, which says nothing.
//
// Both values are rendered through the user namespace of the process that
// opened the file (from_kuid_munged(seq_user_ns(…))), and this process opens
// both, so each is the same function of the kernel's uid. Different values
// therefore name different uids, the overflow uid (65534, what a uid
// unmapped in that namespace renders as) included: only EQUAL values are
// ambiguous, since two unmapped uids both render 65534, and equal is never
// "another". Measured on dido (2026-09-26): in a user namespace that maps
// only root, a uid-1000 process's listener row and its status Uid line both
// read 65534, and in one mapping root to 1000, root's sshd listener reads
// 1000 while the uid-1000 process reads 65534.
func createdByAnother(creator, pidUID int) bool {
	return creator >= 0 && pidUID >= 0 && creator != pidUID
}

// pidFSUID returns pid's fsuid as /proc/<pid>/status under procRoot shows
// it, or -1 when the file does not read or does not show one (statusFSUID).
//
// The file reads where the pid's descriptors do not: it is mode 0444, and
// the kernel's ptrace check guards the descriptors, not it. So it reads for
// a process of another user, and for one with dumpable=0 (a binary granted
// cap_net_bind_service), to its own user and to root without CAP_SYS_PTRACE
// (measured on dido for #1030). It does not read under hidepid=1 or 2, which
// hide a process that fails that ptrace check, dumpable=0 ones from their own
// user included: the uid is then unknown, and nothing is ruled out by it.
func pidFSUID(procRoot string, pid int) int {
	f, err := os.Open(filepath.Join(procRoot, strconv.Itoa(pid), "status"))
	if err != nil {
		return -1
	}
	defer func() { _ = f.Close() }()
	return statusFSUID(f)
}

// statusFSUID reads the fsuid from a /proc/<pid>/status file: the fourth
// value of its "Uid:" line, which lists the real, effective, saved and
// filesystem uids in that order. -1 when there is no such line or that value
// does not parse.
//
// The fsuid, not the real or effective uid, because it is what a socket is
// stamped with: sock_alloc() takes current_fsuid() for the socket's inode,
// and the socket's sk_uid, the uid column of /proc/net/tcp, is set from it.
// It follows the effective uid unless setfsuid(2) is called, which the
// bridge never does.
func statusFSUID(r io.Reader) int {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		rest, found := strings.CutPrefix(sc.Text(), "Uid:")
		if !found {
			continue
		}
		f := strings.Fields(rest)
		if len(f) < 4 {
			return -1
		}
		uid, err := strconv.Atoi(f[3])
		if err != nil || uid < 0 {
			return -1
		}
		return uid
	}
	return -1
}

// socketHolders walks the process directories under procRoot, /proc in
// production, and returns, for each socket in sockets, the pids whose
// descriptors link to it, as far as this user can read them. It skips the
// pid skip, 0 for none.
//
// A process whose fd directory does not list, or whose links do not read,
// adds nothing: the kernel keeps its descriptors from this user (another
// user's process, or one with dumpable=0, a binary granted
// cap_net_bind_service). So a socket only such a process holds has no
// holder here. So does a socket no descriptor holds: one registered with
// io_uring or kept in a BPF map after its descriptor closed, one in flight
// through a unix socket, a kernel socket, or one held by a process of
// another pid namespace that shares this network namespace. Neither caller
// reads a missing holder as an answer: the census asks next which uid
// created the socket (listenersNotOf), and the uid arm counts the socket as
// hidden, as before.
//
// The walk reads every process this user can: a ReadDir, then a Readlink
// per descriptor. Measured on a 290-process host (dido, 2026-09-26): 1.1 to
// 1.8 ms as a user, most directories refusing the listing; 5 to 6 ms as
// root; 11 to 15 ms as root without CAP_SYS_PTRACE, where every directory
// lists and no link reads.
func socketHolders(procRoot string, sockets map[string]int, skip int) map[string][]int {
	held := map[string][]int{}
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return held
	}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 || pid == skip {
			continue
		}
		fdDir := filepath.Join(procRoot, e.Name(), "fd")
		fds, err := os.ReadDir(fdDir)
		if err != nil {
			continue
		}
		for _, fd := range fds {
			link, err := os.Readlink(filepath.Join(fdDir, fd.Name()))
			if err != nil {
				continue
			}
			if _, listener := sockets[link]; listener {
				held[link] = append(held[link], pid)
			}
		}
	}
	return held
}

// procOfAnotherPIDNamespace says why the processes under procRoot may not
// be numbered as this process's pid namespace numbers them, or returns ""
// when they are.
//
// A recorded pid is a number in this process's namespace, as kill(2) reads
// it, and a /proc mounted for another namespace numbers every process
// differently. Its <pid> is then an unrelated process, and the recorded
// bridge can be among the port's holders under another number, so finding
// the pid there, ruling it out by its own descriptors, or ruling it out by
// the port's holders would each be an answer about some other process.
// Such a /proc names someone else as its self: measured on Linux 7.0 for
// proctest, under `unshare --pid --fork` without --mount-proc, a process
// whose own pid is 1 reads /proc/self as 480456.
func procOfAnotherPIDNamespace(procRoot string) string {
	self, err := os.Readlink(filepath.Join(procRoot, "self"))
	switch {
	case err != nil:
		return fmt.Sprintf("/proc does not say which process reads it, so it may number another pid namespace's processes (%s)",
			oneLine(err.Error()))
	case self != strconv.Itoa(os.Getpid()):
		return fmt.Sprintf("/proc numbers another pid namespace's processes (its self is %s, and this process is pid %d)",
			self, os.Getpid())
	}
	return ""
}

// fdDirHoldsSocket reports whether any descriptor in fdDir, a process's
// /proc/<pid>/fd, links to one of sockets (held), and when none does,
// whether every descriptor could be read (readAll).
//
// An inode names one socket, so a match proves the process holds that
// listener; it cannot come from another process's socket. A directory that
// cannot be listed returns its error and no match, as lsof's "ran, matched
// nothing" does: the process is gone, or belongs to another user, or runs
// with dumpable=0 (a binary granted cap_net_bind_service), and the kernel
// denies its fd table to an unprivileged observer. procSighting tells those
// apart for the account and never passes the error on, because that is the
// case checkPort's liveness arm exists for, and it must reach that arm
// rather than read as a broken probe.
//
// A directory can list and still refuse its links: listing takes the
// process's uid, and a readlink the kernel's full ptrace read check, which
// also compares the groups. Such a descriptor clears readAll, since the
// socket may be behind it. One closed between the listing and its readlink
// does not: it is no longer a socket the process holds.
func fdDirHoldsSocket(fdDir string, sockets map[string]int) (held, readAll bool, err error) {
	entries, err := os.ReadDir(fdDir)
	if err != nil {
		return false, false, err
	}
	readAll = true
	for _, e := range entries {
		link, err := os.Readlink(filepath.Join(fdDir, e.Name()))
		switch {
		case err == nil:
			if _, listener := sockets[link]; listener {
				return true, readAll, nil
			}
		case !errors.Is(err, fs.ErrNotExist):
			readAll = false
		}
	}
	return false, readAll, nil
}
