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
// user, holding no port of ours): each probe that can see the port's
// listeners names this process, and Linux's /proc reads the parent's
// descriptors. A host with nothing to ask names no one, and the test skips.
func realHolderAccount(t *testing.T) string {
	t.Helper()
	switch {
	case runtime.GOOS == "windows":
		return fmt.Sprintf("Windows' TCP listener table lists pid %d on this port", os.Getpid())
	case lsofResolved():
		return fmt.Sprintf("lsof lists pid %d listening on this port", os.Getpid())
	case runtime.GOOS == "linux":
		return fmt.Sprintf("this host has no lsof, and /proc shows no descriptor of pid %d listening on this port", os.Getppid())
	}
	t.Skip("no lsof here and no /proc to read, so nothing can say who holds the port")
	return ""
}

// TestLivenessArmNamesTheListenerThePlatformProbeSaw drives the real owner
// probe, not a stub: GetExtendedTcpTable on Windows, lsof where it resolves,
// /proc on Linux without it. The recorded bridge is alive and does not hold
// the port; this process does, and every one of those probes can see it.
// The hint used to say that the host "would not attribute the port" and
// blame a binary granted cap_net_bind_service, on Windows and macOS too,
// where there is no such capability, and while the probe had in fact named
// the holder. Now it gives the probe's account and says to stop the holder.
func TestLivenessArmNamesTheListenerThePlatformProbeSaw(t *testing.T) {
	account := realHolderAccount(t)
	withPortOwner(t, false, nil)
	ppid := os.Getppid()
	c := checkPort(t.Context(), "port-test", bindPort(t), writePIDFile(t, ppid))
	if c.Status != Warn {
		t.Fatalf("got %v (%s / %s), want warn", c.Status, c.Summary, c.Hint)
	}
	want := fmt.Sprintf("our bridge (pid %d) is still running, but %s: stop the process that holds the port", ppid, account)
	if !strings.HasPrefix(c.Hint, want) {
		t.Errorf("hint does not give the probe's account:\n got %s\nwant %s…", c.Hint, want)
	}
	for _, cause := range []string{"cap_net_bind_service", "dumpable", "capability-bound", "this is expected"} {
		if strings.Contains(c.Hint, cause) {
			t.Errorf("the probe saw the port's holder, yet the hint says %q: %s", cause, c.Hint)
		}
	}
}

// TestChosenPortRefusalOfAPortTheProbeSawAnotherHold is the same facts over
// an install whose config did not load (checkChosenPort). The recorded
// bridge was seen not to hold the port, so stopping it frees nothing, and
// the hint says to stop the process the probe saw instead.
func TestChosenPortRefusalOfAPortTheProbeSawAnotherHold(t *testing.T) {
	account := realHolderAccount(t)
	ppid := os.Getppid()
	pidFile := writePIDFile(t, ppid)
	c := checkChosenPort(t.Context(), "port-test", bindPort(t), pidFile)
	if c.Status != Fail {
		t.Fatalf("got %v (%s / %s), want fail", c.Status, c.Summary, c.Hint)
	}
	want := fmt.Sprintf("the bridge recorded in %s (pid %d) is running, but %s: stop the process that holds the port", pidFile, ppid, account)
	if !strings.HasPrefix(c.Hint, want) {
		t.Errorf("hint does not give the probe's account:\n got %s\nwant %s…", c.Hint, want)
	}
	for _, advice := range []string{"stop that bridge", "cap_net_bind_service"} {
		if strings.Contains(c.Hint, advice) {
			t.Errorf("the probe saw another process hold the port, yet the hint says %q: %s", advice, c.Hint)
		}
	}
}
