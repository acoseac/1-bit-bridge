package doctor

import (
	"bufio"
	"fmt"
	"io"
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
// local port matches. The row filter is shared by the uid and inode scans
// below, so the two cannot disagree about which rows are the port's
// listeners.
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

// scanListenerUIDs returns the owning UID of every LISTEN row whose local
// port matches. Split from portOwnedByThisUser so it can be tested against
// captured real /proc output without needing a matching live socket, a
// particular uid, or Linux.
func scanListenerUIDs(r io.Reader, port int) ([]int, error) {
	var uids []int
	err := eachListenRow(r, port, func(f []string) {
		if len(f) <= colUID {
			return
		}
		uid, err := strconv.Atoi(f[colUID])
		if err != nil {
			return
		}
		uids = append(uids, uid)
	})
	if err != nil {
		return nil, err
	}
	return uids, nil
}

// scanListenerInodes returns the socket inode of every LISTEN row whose
// local port matches, as the table renders it (decimal).
func scanListenerInodes(r io.Reader, port int) ([]string, error) {
	var inodes []string
	err := eachListenRow(r, port, func(f []string) {
		if len(f) <= colInode {
			return
		}
		inodes = append(inodes, f[colInode])
	})
	if err != nil {
		return nil, err
	}
	return inodes, nil
}

// readSocketTables hands each socket table in paths to scan, in order, and
// stops at the first that answers done. A table that cannot be opened or
// read is skipped, because a kernel built without IPv6 has no
// /proc/net/tcp6 and the other family may still answer; the first such
// error is returned only when no table could be read at all.
func readSocketTables(paths []string, scan func(r io.Reader) (done bool, err error)) error {
	var firstErr error
	readAny := false
	for _, path := range paths {
		// Wrapped in a closure so the Close is deferred: this runs in a
		// loop, so a plain `defer` would hold every descriptor until the
		// function returns, and a bare post-call Close is skipped on a
		// panic.
		done, err := func() (bool, error) {
			f, err := os.Open(path)
			if err != nil {
				return false, err
			}
			defer func() { _ = f.Close() }()
			return scan(f)
		}()
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		readAny = true
		if done {
			return nil
		}
	}
	if !readAny {
		return firstErr
	}
	return nil
}

// listenerSockets returns the fd-link text of every socket listening on
// port in the given tables, `socket:[<inode>]`, which is how
// /proc/<pid>/fd renders a socket descriptor.
func listenerSockets(paths []string, port int) (map[string]bool, error) {
	sockets := map[string]bool{}
	err := readSocketTables(paths, func(r io.Reader) (bool, error) {
		inodes, err := scanListenerInodes(r, port)
		if err != nil {
			return false, err
		}
		for _, inode := range inodes {
			sockets["socket:["+inode+"]"] = true
		}
		return false, nil
	})
	if err != nil {
		return nil, err
	}
	return sockets, nil
}

// fdDirHoldsSocket reports whether any descriptor in fdDir, a process's
// /proc/<pid>/fd, links to one of sockets.
//
// An inode names one socket, so a match proves the process holds that
// listener; it cannot come from another process's socket. A directory that
// cannot be listed answers false, as lsof's "ran, matched nothing" does:
// the process is gone, or belongs to another user, or runs with dumpable=0
// (a binary granted cap_net_bind_service), and the kernel denies its fd
// table to an unprivileged observer. That is the case checkPort's liveness
// arm exists for, so it must reach that arm rather than read as a broken
// probe. A descriptor closed between the listing and its readlink is
// skipped for the same reason.
func fdDirHoldsSocket(fdDir string, sockets map[string]bool) bool {
	entries, err := os.ReadDir(fdDir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		link, err := os.Readlink(filepath.Join(fdDir, e.Name()))
		if err == nil && sockets[link] {
			return true
		}
	}
	return false
}
