package doctor

import (
	"strings"
	"testing"
)

// TestLsofSightingReadsWhatLsofPrinted pins lsof's account of a port on
// which it did not name the pid asked about, from its output alone: a pid a
// line names the holders, nothing listed says so, and output in any other
// shape (busybox's applet) says only that the pid is not in it. None of them
// rules the pid out, since lsof lists only what this user may inspect, and
// every one keeps the blind spot.
func TestLsofSightingReadsWhatLsofPrinted(t *testing.T) {
	const blind = "the blind spot"
	for _, tc := range []struct {
		name string
		out  string
		want ownerSighting
	}{
		{"nothing listed", "", ownerSighting{saw: "lsof lists no process listening on this port", blind: blind}},
		{"blank lines only", "\n\n", ownerSighting{saw: "lsof lists no process listening on this port", blind: blind}},
		{"one pid", "1305\n", ownerSighting{saw: "lsof lists pid 1305 listening on this port", blind: blind}},
		{"pids out of order, one twice", "1400\n1305\n1400\n", ownerSighting{saw: "lsof lists pids 1305, 1400 listening on this port", blind: blind}},
		{"a line that is no pid", "1305\nCOMMAND\n", ownerSighting{saw: "lsof's output does not name pid 4242", blind: blind}},
		{"busybox's applet", "1  /usr/local/bin/bridge  0  /dev/null\n", ownerSighting{saw: "lsof's output does not name pid 4242", blind: blind}},
		{"a pid that is no pid", "0\n", ownerSighting{saw: "lsof's output does not name pid 4242", blind: blind}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := lsofSighting([]byte(tc.out), 4242, blind); got != tc.want {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestListenerTableSightingRulesThePidOut: Windows' listener table carries
// every listener's owning pid, so a miss there names who holds the port, or
// that nothing listens on it, and never offers a blind spot.
func TestListenerTableSightingRulesThePidOut(t *testing.T) {
	for _, tc := range []struct {
		owners []int
		want   string
	}{
		{nil, "Windows' TCP listener table lists no process listening on this port"},
		{[]int{5123}, "Windows' TCP listener table lists pid 5123 on this port"},
		// A process listening on both families has a row in each table.
		{[]int{5123, 5123}, "Windows' TCP listener table lists pid 5123 on this port"},
		{[]int{6000, 5123}, "Windows' TCP listener table lists pids 5123, 6000 on this port"},
	} {
		if got := listenerTableSighting(tc.owners); got != (ownerSighting{saw: tc.want, ruledOut: true}) {
			t.Errorf("listenerTableSighting(%v) = %+v, want %q, ruled out", tc.owners, got, tc.want)
		}
	}
}

// TestUnseenHintsFitTheSighting renders both hints from one sighting of
// each kind. A ruled-out sighting says to stop the holder and drops the
// hedge and "stop that bridge"; one that leaves the bridge possible keeps
// both and puts its blind spot in parentheses; and a sighting with no
// account still reads as a sentence.
func TestUnseenHintsFitTheSighting(t *testing.T) {
	ruledOut := ownerSighting{saw: "lsof lists pid 1305 listening on this port", ruledOut: true}
	hedged := ownerSighting{saw: "lsof lists no process listening on this port", blind: "the blind spot"}

	for _, tc := range []struct {
		name, hint string
		has, lacks []string
	}{
		{"live, ruled out", liveUnseenHint(4242, ruledOut),
			[]string{"our bridge (pid 4242) is still running, but lsof lists pid 1305 listening on this port: stop the process that holds the port"},
			[]string{"this is expected"}},
		{"live, hedged", liveUnseenHint(4242, hedged),
			[]string{"our bridge (pid 4242) is still running, but lsof lists no process listening on this port (the blind spot). If our bridge is what holds the port, this is expected"},
			nil},
		{"live, no account", liveUnseenHint(4242, ownerSighting{}),
			[]string{"our bridge (pid 4242) is still running, but the owner probe did not see it on this port. If our bridge"},
			[]string{"but ."}},
		{"chosen, ruled out", chosenUnseenHint("/d/server.pid", 4242, ruledOut),
			[]string{"the bridge recorded in /d/server.pid (pid 4242) is running, but lsof lists pid 1305 listening on this port: stop the process that holds the port and re-run"},
			[]string{"stop that bridge"}},
		{"chosen, hedged", chosenUnseenHint("/d/server.pid", 4242, hedged),
			[]string{"is running, but lsof lists no process listening on this port (the blind spot), and with no config", "stop that bridge and re-run"},
			nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, want := range tc.has {
				if !strings.Contains(tc.hint, want) {
					t.Errorf("hint lacks %q: %s", want, tc.hint)
				}
			}
			for _, not := range tc.lacks {
				if strings.Contains(tc.hint, not) {
					t.Errorf("hint says %q: %s", not, tc.hint)
				}
			}
		})
	}
}
