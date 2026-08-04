package doctor

import (
	"bufio"
	"fmt"
	"io"
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

// tcpStateListen is TCP_LISTEN as rendered in the `st` column.
const tcpStateListen = "0A"

// Column indices in /proc/net/tcp{,6} after strings.Fields:
//
//	sl  local_address rem_address st tx_queue:rx_queue tr:tm->when retrnsmt uid
//	 0        1             2      3          4              5         6     7
const (
	colLocalAddress = 1
	colState        = 3
	colUID          = 7
)

// scanListenerUIDs returns the owning UID of every LISTEN-state row whose
// local port matches. Split from portOwnedByThisUser so it can be tested
// against captured real /proc output without needing a matching live
// socket, a particular uid, or Linux.
func scanListenerUIDs(r io.Reader, port int) ([]int, error) {
	// The port half of local_address is BIG-endian hex, zero-padded to four
	// digits: 443 -> "01BB", 7789 -> "1E6D". (The address half is
	// little-endian for IPv4, but it is never parsed — matching on port
	// alone across all local addresses is the same question lsof answers
	// for `-iTCP:<port>`, and checkPort has already established that the
	// port is occupied.) Comparing the rendered suffix avoids parsing a
	// number out of every row.
	want := fmt.Sprintf(":%04X", port)
	var uids []int
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) <= colUID {
			// The header line, or a row truncated by a concurrent read.
			continue
		}
		// The state filter is load-bearing, not tidiness: an ephemeral
		// OUTBOUND connection FROM this port would otherwise match on the
		// local-port column and contribute a foreign UID.
		if !strings.EqualFold(f[colState], tcpStateListen) {
			continue
		}
		if !strings.HasSuffix(strings.ToUpper(f[colLocalAddress]), want) {
			continue
		}
		uid, err := strconv.Atoi(f[colUID])
		if err != nil {
			continue
		}
		uids = append(uids, uid)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return uids, nil
}
