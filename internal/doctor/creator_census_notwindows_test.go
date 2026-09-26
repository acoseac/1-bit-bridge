//go:build !windows

package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// These drive the census's second half, the uid that created each listener,
// on fixture tables and a fixture /proc, so they run wherever the helpers
// compile rather than only on Linux. Not on Windows, for the reason
// portowner_fd_notwindows_test.go gives: the fd fixtures are directories of
// symlinks.

// listening is one LISTEN row for tablesWith: a socket on port 7788
// (0x1E6C), on loopback in tcp, or in tcp6 when v6, with the uid that
// created it as the table renders that column, and its inode.
type listening struct {
	v6    bool
	uid   string
	inode int
}

// tablesWith writes socket tables holding rows, and returns their paths,
// tcp's first.
func tablesWith(t *testing.T, rows ...listening) []string {
	t.Helper()
	var v4, v6 strings.Builder
	v4.WriteString("  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n")
	v6.WriteString("  sl  local_address                         remote_address                        st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n")
	for i, r := range rows {
		if r.v6 {
			fmt.Fprintf(&v6, "%4d: 00000000000000000000000001000000:1E6C 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000 %5s        0 %d 1 0000000000000000 100 0 0 10 0\n",
				i, r.uid, r.inode)
			continue
		}
		fmt.Fprintf(&v4, "%4d: 0100007F:1E6C 00000000:0000 0A 00000000:00000000 00:00000000 00000000 %5s        0 %d 1 0000000000000000 100 0 0 10 0\n",
			i, r.uid, r.inode)
	}
	dir := t.TempDir()
	return []string{writeTable(t, dir, "tcp", v4.String()), writeTable(t, dir, "tcp6", v6.String())}
}

// writeStatus writes pid's /proc/<pid>/status in a fixture /proc: a Uid
// line listing uids (real, effective, saved and filesystem, tab-separated),
// or none when uids is "", between the lines a real status has around it.
func writeStatus(t *testing.T, root string, pid int, uids string) {
	t.Helper()
	dir := filepath.Join(root, strconv.Itoa(pid))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := "Name:\tbridge\nUmask:\t0077\nState:\tS (sleeping)\nTgid:\t" + strconv.Itoa(pid) + "\n"
	if uids != "" {
		body += "Uid:\t" + uids + "\n"
	}
	body += "Gid:\t1000\t1000\t1000\t1000\nFDSize:\t64\n"
	if err := os.WriteFile(filepath.Join(dir, "status"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// bridgeUIDs is a recorded bridge's Uid values: uid 1000 in all four.
const bridgeUIDs = "1000\t1000\t1000\t1000"

// TestProcSightingRulesOutAPidItCannotReadByTheUIDThatCreatedEachListener is
// row L7 of #1030's matrix on fixtures: the recorded bridge (pid 4242) runs
// as uid 1000 with dumpable=0, as a bridge granted cap_net_bind_service does,
// so no probe can read its descriptors, and the port its config was edited
// to is held by a process of another user, or root's daemon, which this user
// cannot read either. The census of readable holders found nothing, and the
// check warned, exit 0, over a port the restarted bridge could not bind.
//
// The socket tables say which uid created each listener, and the recorded
// pid's status says which it runs as. The bridge creates its listeners
// itself, as its own fsuid, and keeps it, so a listener another uid created
// is not the bridge's. Where every listener is that, or held by a process
// this user can read, the pid is ruled out. A listener of the pid's own uid
// that no readable process holds leaves it possible, as does anything /proc
// does not show: that is also how the bridge on its own port looks (row L2),
// and how a second hidden process of the same user looks (row L6h), which
// nothing unprivileged can tell apart.
func TestProcSightingRulesOutAPidItCannotReadByTheUIDThatCreatedEachListener(t *testing.T) {
	const blind = "the blind spot"
	unreadable := ownerSighting{saw: "/proc does not let this user read pid 4242's descriptors", blind: blind}
	ruledOut := func(account string) ownerSighting {
		return ownerSighting{saw: "/proc shows every socket listening on this port " + account, ruledOut: true}
	}
	for _, tc := range []creatorCase{
		{name: "L7: another user's listener, which no process this user can read holds",
			rows: []listening{{uid: "1001", inode: 300}}, status: bridgeUIDs,
			want: ruledOut("created by uid 1001, while pid 4242 runs as uid 1000")},
		{name: "root's listener",
			rows: []listening{{uid: "0", inode: 300}}, status: bridgeUIDs,
			want: ruledOut("created by uid 0, while pid 4242 runs as uid 1000")},
		{name: "two listeners, created by two other uids",
			rows: []listening{{uid: "0", inode: 300}, {v6: true, uid: "1001", inode: 400}}, status: bridgeUIDs,
			want: ruledOut("created by uids 0, 1001, while pid 4242 runs as uid 1000")},
		{name: "L6m: one held by a process this user can read, one created by another uid",
			rows:    []listening{{uid: "1000", inode: 300}, {v6: true, uid: "1001", inode: 400}},
			holders: map[int]map[string]string{5000: {"3": "socket:[300]"}}, status: bridgeUIDs,
			want: ruledOut("held by pid 5000 or created by uid 1001, while pid 4242 runs as uid 1000")},
		{name: "another uid's listener that a readable process holds is named by its holder",
			rows:    []listening{{uid: "1001", inode: 300}},
			holders: map[int]map[string]string{5000: {"3": "socket:[300]"}}, status: bridgeUIDs,
			want: ruledOut("held by pid 5000")},
		{name: "the fsuid is the uid compared, not the real one",
			rows: []listening{{uid: "1001", inode: 300}}, status: "1001\t1001\t1001\t1000",
			want: ruledOut("created by uid 1001, while pid 4242 runs as uid 1000")},
		{name: "only the pid renders the overflow uid",
			rows: []listening{{uid: "1001", inode: 300}}, status: "65534\t65534\t65534\t65534",
			want: ruledOut("created by uid 1001, while pid 4242 runs as uid 65534")},
		{name: "only the listener renders the overflow uid",
			rows: []listening{{uid: "65534", inode: 300}}, status: bridgeUIDs,
			want: ruledOut("created by uid 65534, while pid 4242 runs as uid 1000")},

		{name: "L2 and L6h: the pid's own uid created the listener",
			rows: []listening{{uid: "1000", inode: 300}}, status: bridgeUIDs, want: unreadable},
		{name: "the pid's own uid created one listener, another uid the other",
			rows: []listening{{uid: "1000", inode: 300}, {v6: true, uid: "1001", inode: 400}}, status: bridgeUIDs,
			want: unreadable},
		{name: "a real uid that differs rules nothing out when the fsuid is the listener's",
			rows: []listening{{uid: "1001", inode: 300}}, status: "1000\t1000\t1000\t1001", want: unreadable},
		{name: "both render the overflow uid",
			rows: []listening{{uid: "65534", inode: 300}}, status: "65534\t65534\t65534\t65534", want: unreadable},
		{name: "the pid's status is not there",
			rows: []listening{{uid: "1001", inode: 300}}, noStatus: true, want: unreadable},
		{name: "the pid's status cannot be read",
			rows: []listening{{uid: "1001", inode: 300}}, status: bridgeUIDs, statusOff: true, want: unreadable},
		{name: "the pid's status shows no Uid line",
			rows: []listening{{uid: "1001", inode: 300}}, status: "", want: unreadable},
		{name: "the listener's uid column does not parse",
			rows: []listening{{uid: "x", inode: 300}}, status: bridgeUIDs, want: unreadable},
		{name: "hidepid=2: the pid is not in /proc, and another uid created the listener",
			rows: []listening{{uid: "1001", inode: 300}}, notInProc: true,
			want: ownerSighting{saw: "/proc has no pid 4242"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !tc.notInProc && os.Geteuid() == 0 {
				t.Skip("root reads a directory whatever its mode")
			}
			found, seen, err := procSighting(tablesWith(t, tc.rows...), tc.procRoot(t), 7788, 4242, blind, nil)
			if err != nil || found || seen != tc.want {
				t.Errorf("got %v, %+v, %v;\nwant false, %+v, no error", found, seen, err, tc.want)
			}
		})
	}
}

// creatorCase is one row of
// TestProcSightingRulesOutAPidItCannotReadByTheUIDThatCreatedEachListener.
type creatorCase struct {
	name string
	rows []listening
	// holders are processes other than the recorded pid, whose descriptors
	// this user can read.
	holders map[int]map[string]string
	// status is the recorded pid's Uid values, "" for a status with no Uid
	// line; noStatus leaves the file out and statusOff makes it unreadable.
	// notInProc leaves the pid out of /proc altogether, as hidepid=2 does;
	// otherwise its fd directory is there and cannot be listed, as a
	// dumpable=0 process's cannot.
	status              string
	noStatus, statusOff bool
	notInProc           bool
	want                ownerSighting
}

// procRoot builds the case's fixture /proc: its holders, and the recorded
// pid, 4242, as the case describes it.
func (c creatorCase) procRoot(t *testing.T) string {
	t.Helper()
	procs := map[int]map[string]string{}
	for pid, links := range c.holders {
		procs[pid] = links
	}
	if c.notInProc {
		return writeProcRoot(t, procs)
	}
	procs[4242] = bridgeElsewhere
	root := writeProcRoot(t, procs)
	chmodForTest(t, procFdDir(root, 4242), 0o000)
	if !c.noStatus {
		writeStatus(t, root, 4242, c.status)
	}
	if c.statusOff {
		statusOff(t, root, 4242)
	}
	return root
}

// statusOff makes pid's status in a fixture /proc unreadable, and restores a
// mode the test's cleanup can remove.
func statusOff(t *testing.T, root string, pid int) {
	t.Helper()
	status := filepath.Join(root, strconv.Itoa(pid), "status")
	if err := os.Chmod(status, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(status, 0o600) })
}

// TestProcSightingRulesNothingOutByCreatorsOverATableItCouldNotRead: a socket
// table that is there and did not read may list a listener the recorded pid
// created, so the uids that created the listeners in the tables that did
// read cannot rule it out, as the port's readable holders cannot. The
// fixture gives the pid a status and no fd directory, so that nothing but
// the unread table stands between the census and a ruling-out, for root as
// well as for a user.
func TestProcSightingRulesNothingOutByCreatorsOverATableItCouldNotRead(t *testing.T) {
	read := tablesWith(t, listening{uid: "1001", inode: 300})
	root := writeProcRoot(t, nil)
	writeStatus(t, root, 4242, bridgeUIDs)
	want := ownerSighting{saw: "/proc has no pid 4242"}
	for _, tc := range []struct {
		name   string
		tables []string
		want   ownerSighting
	}{
		{"both tables read (the control)", read,
			ownerSighting{saw: "/proc shows every socket listening on this port created by uid 1001, while pid 4242 runs as uid 1000", ruledOut: true}},
		{"tcp6 is there and did not read", []string{read[0], t.TempDir()}, want},
	} {
		t.Run(tc.name, func(t *testing.T) {
			found, seen, err := procSighting(tc.tables, root, 7788, 4242, "the blind spot", nil)
			if err != nil || found || seen != tc.want {
				t.Errorf("got %v, %+v, %v; want false, %+v, no error", found, seen, err, tc.want)
			}
		})
	}
}
