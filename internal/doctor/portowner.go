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
// The same holds for the table reader and the fd walk below them, which
// take their paths as arguments: portowner_linux.go hands them /proc, and
// the tests hand them fixture files.

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
// tables was created by uid (hiddenListenerOfThisUser). procRoot is not
// read yet. An error means no table could be read.
func hiddenListenerOf(tables []string, procRoot string, port, uid int) (bool, error) {
	_ = procRoot
	sockets, _, err := listenerSockets(tables, port)
	if err != nil {
		return false, err
	}
	for _, u := range sockets {
		if u == uid {
			return true, nil
		}
	}
	return false, nil
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
