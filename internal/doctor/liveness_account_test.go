package doctor

import (
	"context"
	"errors"
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

// withOwnerProbe forces the owner probe's answer for a test (ownerProbeFunc).
func withOwnerProbe(t *testing.T, found bool, seen ownerSighting, err error) {
	t.Helper()
	orig := ownerProbeFunc
	t.Cleanup(func() { ownerProbeFunc = orig })
	ownerProbeFunc = func(context.Context, int, int) (bool, ownerSighting, error) { return found, seen, err }
}

// probeAnswer is one answer the owner probe can give about a held port
// whose recorded bridge is pid 4242.
type probeAnswer struct {
	name  string
	found bool
	seen  ownerSighting
	err   error
}

var probeAnswers = []probeAnswer{
	{"found", true, ownerSighting{}, nil},
	{"probe failed", false, ownerSighting{}, errors.New("the probe broke")},
	{"miss, ruled out", false, ownerSighting{saw: "the table lists pid 1305 on this port", ruledOut: true}, nil},
	{"miss, hedged", false, ownerSighting{saw: "lsof lists no process listening on this port", blind: "the blind spot"}, nil},
	{"miss, no account", false, ownerSighting{}, nil},
}

// ladderVerdicts is what both port ladders answer for a held port whose
// recorded bridge is pid 4242, from the probe's found and its error, the
// pid's liveness and the uid arm alone: the verdicts as they were before
// the probe had an account to give. checkPort warns on a failed probe, is
// ok on a match, and otherwise leans on liveness and the uid arm;
// checkChosenPort is ok on a match and refuses everything else.
func ladderVerdicts(found, probeFailed, alive, owned bool) (port, chosen Status) {
	switch {
	case probeFailed:
		return Warn, Fail
	case found:
		return OK, OK
	case !alive:
		return Fail, Fail
	case owned:
		return OK, Fail
	default:
		return Warn, Fail
	}
}

// TestPortVerdictsIgnoreTheSighting hands both ladders every kind of answer
// the owner probe gives, accounts of every kind included, with the recorded
// pid alive or not and the listener this user's or not, and requires the
// verdicts ladderVerdicts gives. A ruled-out miss comes for real only from
// Windows' table and Linux's /proc, so this is the one pin, on a Mac, that
// no verdict turns on the account (ownerProbeFunc).
func TestPortVerdictsIgnoreTheSighting(t *testing.T) {
	for _, a := range probeAnswers {
		for _, alive := range []bool{true, false} {
			for _, owned := range []bool{true, false} {
				t.Run(fmt.Sprintf("%s/alive=%v/owned=%v", a.name, alive, owned), func(t *testing.T) {
					requireLadderVerdicts(t, a, alive, owned)
				})
			}
		}
	}
}

// requireLadderVerdicts grades one held port through both ladders with the
// probe forced to a, and requires ladderVerdicts' answer.
func requireLadderVerdicts(t *testing.T, a probeAnswer, alive, owned bool) {
	t.Helper()
	withOwnerProbe(t, a.found, a.seen, a.err)
	withPIDAlive(t, alive)
	withPortOwner(t, owned, nil)
	port, chosen := ladderVerdicts(a.found, a.err != nil, alive, owned)
	pidFile, held := writePIDFile(t, 4242), bindPort(t)
	if c := checkPort(t.Context(), "port-test", held, pidFile); c.Status != port {
		t.Errorf("checkPort: got %v (%s / %s), want %v", c.Status, c.Summary, c.Hint, port)
	}
	if c := checkChosenPort(t.Context(), "port-test", held, pidFile); c.Status != chosen {
		t.Errorf("checkChosenPort: got %v (%s / %s), want %v", c.Status, c.Summary, c.Hint, chosen)
	}
}
