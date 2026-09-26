package doctor

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"testing"
)

// realHolderAccount is the account this host's own owner probe gives of a
// port the test process holds, asked about the test's parent (alive, of this
// user, holding no port of ours), and whether it rules the parent out. Each
// probe that can see the port's listeners names this process; Windows' table
// and Linux's /proc, which reads the parent's descriptors, rule the parent
// out, and lsof, which lists only what it can see, does not. A host with
// nothing to ask names no one, and the test skips.
func realHolderAccount(t *testing.T) (string, bool) {
	t.Helper()
	switch {
	case runtime.GOOS == "windows":
		return fmt.Sprintf("Windows' TCP listener table lists pid %d on this port", os.Getpid()), true
	case lsofResolved():
		return fmt.Sprintf("lsof lists pid %d listening on this port", os.Getpid()), false
	case runtime.GOOS == "linux":
		return fmt.Sprintf("this host has no lsof, and /proc shows no descriptor of pid %d listening on this port", os.Getppid()), true
	}
	t.Skip("no lsof here and no /proc to read, so nothing can say who holds the port")
	return "", false
}

// TestLivenessArmNamesTheListenerThePlatformProbeSaw drives the real owner
// probe, not a stub: GetExtendedTcpTable on Windows, lsof where it resolves,
// /proc on Linux without it. The recorded bridge is alive and does not hold
// the port; this process does, and every one of those probes can see it.
// The hint used to say that the host "would not attribute the port" and
// blame a binary granted cap_net_bind_service, on Windows and macOS too,
// where there is no such capability, and while the probe had in fact named
// the holder. Now it gives the probe's account, with the advice that
// account leaves.
func TestLivenessArmNamesTheListenerThePlatformProbeSaw(t *testing.T) {
	account, ruledOut := realHolderAccount(t)
	withPortOwner(t, false, nil)
	ppid := os.Getppid()
	c := checkPort(t.Context(), "port-test", bindPort(t), writePIDFile(t, ppid))
	if c.Status != Warn {
		t.Fatalf("got %v (%s / %s), want warn", c.Status, c.Summary, c.Hint)
	}
	if want := fmt.Sprintf("our bridge (pid %d) is still running, but %s", ppid, account); !strings.HasPrefix(c.Hint, want) {
		t.Errorf("hint does not give the probe's account:\n got %s\nwant %s…", c.Hint, want)
	}
	requireNoCapabilityClaim(t, c.Hint, ruledOut)
	requireAdviceFitsTheAccount(t, c.Hint, ruledOut)
}

// TestChosenPortRefusalOfAPortTheProbeSawAnotherHold is the same facts over
// an install whose config did not load (checkChosenPort). Where the probe
// rules the recorded bridge out, stopping it frees nothing, and the hint
// says to stop the holder instead; where lsof only names the holder, the
// bridge may still be listening unseen, and stopping it stays on offer.
func TestChosenPortRefusalOfAPortTheProbeSawAnotherHold(t *testing.T) {
	account, ruledOut := realHolderAccount(t)
	ppid := os.Getppid()
	pidFile := writePIDFile(t, ppid)
	c := checkChosenPort(t.Context(), "port-test", bindPort(t), pidFile)
	if c.Status != Fail {
		t.Fatalf("got %v (%s / %s), want fail", c.Status, c.Summary, c.Hint)
	}
	if want := fmt.Sprintf("the bridge recorded in %s (pid %d) is running, but %s", pidFile, ppid, account); !strings.HasPrefix(c.Hint, want) {
		t.Errorf("hint does not give the probe's account:\n got %s\nwant %s…", c.Hint, want)
	}
	requireNoCapabilityClaim(t, c.Hint, ruledOut)
	// Ruled out, stopping the recorded bridge frees nothing, so the hint
	// does not offer it; otherwise it is still a way through.
	if offered := strings.Contains(c.Hint, "stop that bridge and re-run"); offered == ruledOut {
		t.Errorf("ruled out %v, and stopping the recorded bridge offered %v; want the one to exclude the other: %s",
			ruledOut, offered, c.Hint)
	}
}

// requireNoCapabilityClaim fails when text blames a capability-bound binary
// anywhere it cannot be the reason: in every account on a platform without
// file capabilities, and on Linux wherever the probe ruled the recorded
// bridge out.
func requireNoCapabilityClaim(t *testing.T, text string, ruledOut bool) {
	t.Helper()
	if strings.Contains(text, "capability-bound") {
		t.Errorf("names a capability-bound binary, a cause the probe did not establish: %s", text)
	}
	if (runtime.GOOS != "linux" || ruledOut) && (strings.Contains(text, "cap_net_bind_service") || strings.Contains(text, "dumpable")) {
		t.Errorf("gives a capability as the reason where none can be (%s, ruled out %v): %s", runtime.GOOS, ruledOut, text)
	}
}

// requireBlindSpot checks the part of a hedged hint that says why the probe
// could have missed the recorded bridge: what this platform's probe cannot
// see, as an unprivileged user. As root the hint says nothing about it
// (blindSpot), and Windows' probe has none.
func requireBlindSpot(t *testing.T, hint string) {
	t.Helper()
	if os.Geteuid() == 0 {
		return
	}
	var want []string
	switch runtime.GOOS {
	case "linux":
		want = []string{fmt.Sprintf("uid %d cannot read the descriptors", os.Geteuid()), "another user or group", "dumpable=0"}
	case "darwin":
		want = []string{fmt.Sprintf("lsof run as uid %d rather than root sees only that user's processes", os.Geteuid())}
	default:
		return
	}
	for _, w := range want {
		if !strings.Contains(hint, w) {
			t.Errorf("hint does not say what the probe cannot see (%q): %s", w, hint)
		}
	}
}

// requireAdviceFitsTheAccount: a probe whose account rules the recorded
// bridge out leaves one thing to do, stop the holder; one that could have
// missed the bridge keeps the hedge and says what it cannot see.
func requireAdviceFitsTheAccount(t *testing.T, hint string, ruledOut bool) {
	t.Helper()
	if ruledOut {
		if !strings.Contains(hint, "stop the process that holds the port") || strings.Contains(hint, "this is expected") {
			t.Errorf("the probe ruled the bridge out, so the hint must say to stop the holder and not call the port expected: %s", hint)
		}
		return
	}
	if !strings.Contains(hint, "If our bridge is what holds the port, this is expected") {
		t.Errorf("nothing ruled the bridge out, so the hint must keep the hedge: %s", hint)
	}
	requireBlindSpot(t, hint)
}
