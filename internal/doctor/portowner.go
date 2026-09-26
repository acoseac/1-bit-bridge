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
// bridge out: when a listener of root's shares the port at another address,
// say. A listener that a process this user can read holds is that
// process's.
//
// Unlike the census it is handed no pid, so it does not check /proc's pid
// namespace (procOfAnotherPIDNamespace): a readable holder is one whatever
// number /proc gives it.
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

// heldByOthers returns the pids under procRoot whose descriptors hold the
// sockets, and whether every socket has a holder there other than pid
// (socketHolders): the census procSighting rules out a pid it cannot read
// with.
func heldByOthers(procRoot string, sockets map[string]int, pid int) ([]int, bool) {
	held := socketHolders(procRoot, sockets, pid)
	var holders []int
	for s := range sockets {
		if len(held[s]) == 0 {
			return nil, false
		}
		holders = append(holders, held[s]...)
	}
	return holders, true
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
// reads a missing holder as an answer: the census leaves the recorded pid
// possible, and the uid arm counts the socket as hidden, as before.
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
