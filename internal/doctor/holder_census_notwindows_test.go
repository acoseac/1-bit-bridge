//go:build !windows

package doctor

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// These drive the census of a port's holders on fixture tables and a
// fixture /proc, so they run wherever the helpers compile rather than only
// on Linux. Not on Windows, for the reason portowner_fd_notwindows_test.go
// gives: the fd fixtures are directories of symlinks.

// twoListenerTables are socket tables in which port 7788 (0x1E6C) is
// listened on twice: 127.0.0.1 in tcp (inode 100, uid 1000) and ::1 in tcp6
// (inode 200, uid 0), as a process of this user and one of root's can hold
// it side by side, since the two addresses do not overlap.
func twoListenerTables(t *testing.T) []string {
	t.Helper()
	dir := t.TempDir()
	return []string{
		writeTable(t, dir, "tcp", `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0100007F:1E6C 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 100 1 0000000000000000 100 0 0 10 0
`),
		writeTable(t, dir, "tcp6", `  sl  local_address                         remote_address                        st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000000000000000000001000000:1E6C 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 200 1 0000000000000000 100 0 0 10 0
`),
	}
}

// capturedTables are procNetTCPFixture and procNetTCP6Fixture, written out:
// port 7789 is listened on by uid 1000 (inode 24680), 22 by uid 0 (13579),
// 443 by uid 1000 (6213098).
func capturedTables(t *testing.T) []string {
	t.Helper()
	dir := t.TempDir()
	return []string{writeTable(t, dir, "tcp", procNetTCPFixture), writeTable(t, dir, "tcp6", procNetTCP6Fixture)}
}

// TestProcSightingRulesOutAPidItCannotReadByThePortsOtherHolders is row L6
// of #1028's matrix on fixtures: the recorded bridge (pid 4242) is one this
// user cannot read, as a bridge granted cap_net_bind_service is (it runs
// with dumpable=0), and the port it was configured onto is held by another
// process of the same user. The uid arm answered ok for that holder's
// listener, since it runs as this user, and `bridge doctor --config`, the
// runbook's check before a restart, exited 0 over a port the restarted
// bridge could not bind.
//
// An inode names one socket, so where every socket listening on the port is
// held by a process this user CAN read, other than the recorded pid, that
// pid holds none of them: a listener of its own would be held by it alone,
// and nothing here could read it. The one shape this cannot see is a socket
// the recorded pid SHARES with a readable process, and a bridge never
// shares its listener (net.Listen, close-on-exec, no descriptor handed on).
// Anything that leaves one listener without a readable holder leaves the
// pid possible here: a holder this user cannot read either, a table that
// did not read. These fixtures give the recorded pid no status, so /proc
// shows no uid for it and only the holders can rule it out; where it does
// show one, a listener another uid created rules it out too, which
// TestProcSightingRulesOutAPidItCannotReadByTheUIDThatCreatedEachListener
// pins.
func TestProcSightingRulesOutAPidItCannotReadByThePortsOtherHolders(t *testing.T) {
	const blind = "the blind spot"
	unreadable := ownerSighting{saw: "/proc does not let this user read pid 4242's descriptors", blind: blind}
	for _, tc := range []struct {
		name   string
		tables func(t *testing.T) []string
		port   int
		// procs are the fixture's processes. The recorded pid, 4242, is
		// there unless the case says it is not, with its fd directory at
		// mode recorded.
		procs     map[int]map[string]string
		recorded  os.FileMode
		holderOff int  // a holder whose fd directory is made unreadable; 0 = none
		needsUser bool // permission bits bind only a user other than root
		want      ownerSighting
	}{
		{"its descriptors cannot be listed, and another process holds the listener", capturedTables, 7789,
			map[int]map[string]string{4242: bridgeElsewhere, 5000: {"3": "socket:[24680]"}}, 0o000, 0, true,
			ownerSighting{saw: "/proc shows every socket listening on this port held by pid 5000", ruledOut: true}},
		{"its links do not read, and another process holds the listener", capturedTables, 7789,
			map[int]map[string]string{4242: bridgeElsewhere, 5000: {"3": "socket:[24680]"}}, 0o400, 0, true,
			ownerSighting{saw: "/proc shows every socket listening on this port held by pid 5000", ruledOut: true}},
		{"it is not in /proc, and another process holds the listener", capturedTables, 7789,
			map[int]map[string]string{5000: {"0": "/dev/null", "7": "socket:[24680]"}}, 0, 0, false,
			ownerSighting{saw: "/proc shows every socket listening on this port held by pid 5000", ruledOut: true}},
		{"two listeners, each held by another process", twoListenerTables, 7788,
			map[int]map[string]string{4242: bridgeElsewhere, 5000: {"3": "socket:[100]"}, 6000: {"4": "socket:[200]"}}, 0o000, 0, true,
			ownerSighting{saw: "/proc shows every socket listening on this port held by pids 5000, 6000", ruledOut: true}},
		{"one socket held by two processes", capturedTables, 7789,
			map[int]map[string]string{4242: bridgeElsewhere, 6000: {"3": "socket:[24680]"}, 5000: {"9": "socket:[24680]"}}, 0o000, 0, true,
			ownerSighting{saw: "/proc shows every socket listening on this port held by pids 5000, 6000", ruledOut: true}},
		// Row L2, the NUC's ordinary state: the listener is the hidden
		// bridge's own, and no process this user can read holds it.
		{"no other process holds the listener", capturedTables, 7789,
			map[int]map[string]string{4242: fdFixture, 5000: {"3": "socket:[11111]"}}, 0o000, 0, true, unreadable},
		{"two listeners, one held by no process this user can read", twoListenerTables, 7788,
			map[int]map[string]string{4242: bridgeElsewhere, 5000: {"3": "socket:[100]"}}, 0o000, 0, true, unreadable},
		{"the only holder is a process this user cannot read either", capturedTables, 7789,
			map[int]map[string]string{4242: bridgeElsewhere, 5000: {"3": "socket:[24680]"}}, 0o000, 5000, true, unreadable},
		{"not in /proc, and no other process holds the listener", capturedTables, 7789,
			map[int]map[string]string{5000: {"3": "socket:[11111]"}}, 0, 0, false,
			ownerSighting{saw: "/proc has no pid 4242"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.needsUser && os.Geteuid() == 0 {
				t.Skip("root reads a directory whatever its mode")
			}
			root := writeProcRoot(t, tc.procs)
			if _, there := tc.procs[4242]; there {
				chmodForTest(t, procFdDir(root, 4242), tc.recorded)
			}
			if tc.holderOff != 0 {
				chmodForTest(t, procFdDir(root, tc.holderOff), 0o000)
			}
			found, seen, err := procSighting(tc.tables(t), root, tc.port, 4242, blind)
			if err != nil || found || seen != tc.want {
				t.Errorf("got %v, %+v, %v; want false, %+v, no error", found, seen, err, tc.want)
			}
		})
	}
}

// bridgeElsewhere is the descriptors of a bridge whose listener is on
// another port: none of them is a socket listening on the port a case
// probes.
var bridgeElsewhere = map[string]string{
	"0": "/dev/null",
	"5": "socket:[31337]",
}

// TestProcSightingRulesNothingOutByHoldersOverATableItCouldNotRead: a socket
// table that is there and did not read may list a listener that only the
// recorded pid holds, so the port's other holders cannot rule it out, as its
// own descriptors could not (CodeRabbit on #1028).
func TestProcSightingRulesNothingOutByHoldersOverATableItCouldNotRead(t *testing.T) {
	dir := t.TempDir()
	tables := []string{writeTable(t, dir, "tcp", procNetTCPFixture), t.TempDir()}
	root := writeProcRoot(t, map[int]map[string]string{5000: {"3": "socket:[24680]"}})
	want := ownerSighting{saw: "/proc has no pid 4242"}
	if found, seen, err := procSighting(tables, root, 7789, 4242, "the blind spot"); err != nil || found || seen != want {
		t.Errorf("got %v, %+v, %v; want false, %+v, no error", found, seen, err, want)
	}
}

// TestProcSightingTrustsOnlyAProcOfItsOwnPIDNamespace: the recorded pid is
// a number in this process's pid namespace, as kill(2) reads it, and a
// /proc mounted for another namespace numbers every process differently.
// Its <pid> is then some unrelated process, and the port's holders there
// include the recorded bridge itself under another number: finding it,
// ruling it out by its own descriptors, by those holders, or by the uid its
// status shows would each be an answer about some other process. Measured
// on Linux 7.0 for proctest: under `unshare --pid --fork` without
// --mount-proc, a process whose own pid is 1 reads /proc/self as 480456.
func TestProcSightingTrustsOnlyAProcOfItsOwnPIDNamespace(t *testing.T) {
	tables := capturedTables(t)
	// otherUID is a status for pid 4242 whose fsuid is not the uid that
	// created 7789's listener (1000): under a /proc this process can trust,
	// that rules the pid out (the second control).
	const otherUID = "1001\t1001\t1001\t1001"
	for _, tc := range []struct {
		name   string
		procs  map[int]map[string]string
		status string // pid 4242's Uid values; "" writes no status
		port   int
	}{
		{"the pid holds the listener", map[int]map[string]string{4242: fdFixture}, "", 7789},
		{"the pid's descriptors read without one", map[int]map[string]string{4242: fdFixture}, "", 443},
		{"another process holds every listener", map[int]map[string]string{5000: {"3": "socket:[24680]"}}, "", 7789},
		{"another uid than the pid's created every listener", nil, otherUID, 7789},
	} {
		for _, self := range []string{"480456", ""} {
			t.Run(tc.name+"/self "+strconv.Quote(self), func(t *testing.T) {
				root := writeProcRoot(t, tc.procs)
				if tc.status != "" {
					writeStatus(t, root, 4242, tc.status)
				}
				found, seen, err := procSighting(tables, reselfProcRoot(t, root, self), tc.port, 4242, "the blind spot")
				requireNoAnswerFromAnotherNamespace(t, found, seen, err, self)
			})
		}
	}
	t.Run("the control: its self is this process", func(t *testing.T) {
		root := writeProcRoot(t, map[int]map[string]string{4242: fdFixture})
		if found, _, err := procSighting(tables, root, 7789, 4242, "the blind spot"); err != nil || !found {
			t.Errorf("got %v, %v; want the pid found holding the listener", found, err)
		}
	})
	t.Run("the control: its self is this process, and another uid created the listener", func(t *testing.T) {
		root := writeProcRoot(t, nil)
		writeStatus(t, root, 4242, otherUID)
		if found, seen, err := procSighting(tables, root, 7789, 4242, "the blind spot"); err != nil || found || !seen.ruledOut {
			t.Errorf("got %v, %+v, %v; want the pid ruled out by the uid that created the listener", found, seen, err)
		}
	})
}

// reselfProcRoot points a fixture /proc's self link at self, or removes it
// when self is "", and returns the root.
func reselfProcRoot(t *testing.T, root, self string) string {
	t.Helper()
	link := filepath.Join(root, "self")
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if self == "" {
		return root
	}
	if err := os.Symlink(self, link); err != nil {
		t.Fatal(err)
	}
	return root
}

// requireNoAnswerFromAnotherNamespace requires procSighting to have found
// nothing and ruled nothing out, with an account that says why: /proc may
// be another pid namespace's, and names the self it read, if any.
func requireNoAnswerFromAnotherNamespace(t *testing.T, found bool, seen ownerSighting, err error, self string) {
	t.Helper()
	if err != nil || found || seen.ruledOut || !strings.HasPrefix(seen.saw, "/proc ") ||
		!strings.Contains(seen.saw, "pid namespace") {
		t.Errorf("got %v, %+v, %v; want no answer about pid 4242, saying why", found, seen, err)
	}
	if self != "" && !strings.Contains(seen.saw, self) {
		t.Errorf("the account does not name the self it read (%s): %s", self, seen.saw)
	}
}

// TestHiddenListenerOfCountsOnlyAListenerNoReadableProcessHolds is the uid
// arm (hiddenListenerOf) on fixtures. It exists for a bridge granted
// cap_net_bind_service, whose descriptors no unprivileged probe can read, so
// the listener it answers for has two marks: this user created it, and no
// process this user can read holds it. The first alone is any process of
// this user, and it answered ok for another process's listener beside a
// capability-bound bridge that did not hold the port (row L6), wherever the
// census could not rule the bridge out, as when root's process listens on
// the port at another address too.
func TestHiddenListenerOfCountsOnlyAListenerNoReadableProcessHolds(t *testing.T) {
	for _, tc := range []struct {
		name      string
		tables    func(t *testing.T) []string
		port, uid int
		procs     map[int]map[string]string
		holderOff int // a holder whose fd directory is made unreadable; 0 = none
		want      bool
	}{
		{"this user's listener, held by a process it can read", capturedTables, 7789, 1000,
			map[int]map[string]string{5000: {"3": "socket:[24680]"}}, 0, false},
		{"this user's listener, held by no process it can read", capturedTables, 7789, 1000,
			map[int]map[string]string{5000: {"3": "socket:[11111]"}}, 0, true},
		{"this user's listener, held by a process it cannot read", capturedTables, 7789, 1000,
			map[int]map[string]string{5000: {"3": "socket:[24680]"}}, 5000, true},
		{"another user's listener, held by no one here", capturedTables, 22, 1000, nil, 0, false},
		{"that user asking", capturedTables, 22, 0, nil, 0, true},
		{"this user's listener held, another user's beside it held by no one", twoListenerTables, 7788, 1000,
			map[int]map[string]string{5000: {"3": "socket:[100]"}}, 0, false},
		{"the other user asking", twoListenerTables, 7788, 0,
			map[int]map[string]string{5000: {"3": "socket:[100]"}}, 0, true},
		{"nothing listens", capturedTables, 8080, 1000, nil, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.holderOff != 0 && os.Geteuid() == 0 {
				t.Skip("root reads a directory whatever its mode")
			}
			root := writeProcRoot(t, tc.procs)
			if tc.holderOff != 0 {
				chmodForTest(t, procFdDir(root, tc.holderOff), 0o000)
			}
			got, err := hiddenListenerOf(tc.tables(t), root, tc.port, tc.uid)
			if err != nil || got != tc.want {
				t.Errorf("got %v, %v; want %v, no error", got, err, tc.want)
			}
		})
	}
	t.Run("no table readable", func(t *testing.T) {
		absent := filepath.Join(t.TempDir(), "absent")
		if _, err := hiddenListenerOf([]string{absent, absent}, writeProcRoot(t, nil), 7789, 1000); err == nil {
			t.Error("with neither table readable there is no answer, and that is an error")
		}
	})
}

// TestHiddenListenerOfIgnoresWhichPIDNamespaceProcIsFrom: the uid arm is
// handed no pid, so a /proc whose self names another process changes
// nothing. Where the socket tables read at all, such a /proc is an ancestor
// namespace's, which lists every process this one's would
// (hiddenListenerOf's docblock has the measurement). The census's guard
// here was proposed in review (CodeRabbit on #1030), and would turn the
// capability-bound bridge's own port from ok to a warn there.
func TestHiddenListenerOfIgnoresWhichPIDNamespaceProcIsFrom(t *testing.T) {
	for _, tc := range []struct {
		procs map[int]map[string]string
		want  bool
	}{
		{map[int]map[string]string{5000: {"3": "socket:[11111]"}}, true},
		{map[int]map[string]string{5000: {"3": "socket:[24680]"}}, false},
	} {
		root := reselfProcRoot(t, writeProcRoot(t, tc.procs), "480456")
		if got, err := hiddenListenerOf(capturedTables(t), root, 7789, 1000); err != nil || got != tc.want {
			t.Errorf("holders %v: got %v, %v; want %v, no error", tc.procs, got, err, tc.want)
		}
	}
}
