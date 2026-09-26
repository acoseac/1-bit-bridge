//go:build !windows

package doctor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// withLsofAnswering points the lsof probe at sh(1) printing stdout and
// exiting with code, so a test chooses what lsof reports: exit 1 with no
// output is lsof's "matched nothing", a pid a line is `lsof -t`'s answer.
// The output goes through a file, so the shell prints it byte for byte.
//
// It also sets what /proc answers after lsof misses (procOwnerFunc) to an
// answer that adds nothing, so the test grades lsof's answer alone on every
// unix. Left to the host, /proc on Linux would read the recorded pid, and
// pid 4242 may be a process of the test's own user, which it rules out. A
// test that wants /proc's opinion sets it after this (withProcAnswering).
func withLsofAnswering(t *testing.T, stdout string, code int) {
	t.Helper()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skipf("no sh(1) on PATH to stand in for lsof: %v", err)
	}
	out := filepath.Join(t.TempDir(), "lsof.out")
	if err := os.WriteFile(out, []byte(stdout), 0o600); err != nil {
		t.Fatal(err)
	}
	origPath, origCmd := lsofPath, lsofCommand
	t.Cleanup(func() { lsofPath, lsofCommand = origPath, origCmd })
	lsofPath = sh
	lsofCommand = func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, sh, "-c", `cat "$0"; exit "$1"`, out, strconv.Itoa(code))
	}
	withProcAnswering(t, false, procCannotTell, nil)
}

// TestLivenessArmGivesLsofsAccount: when the recorded bridge is alive and
// lsof ran cleanly without naming it, checkPort's liveness arm said "pid
// attribution blocked — capability-bound binary" (ok) or blamed
// cap_net_bind_service and dumpable=0 (warn) whatever lsof had reported.
// That is one shape, a bridge granted the capability. The hint was printed
// on macOS, which has no such capability; for a bridge running as another
// user; and when lsof named the process that holds the port, which is not
// a failure to attribute it at all.
//
// So the line says what lsof reported, and names the pids it listed.
// Nothing lsof reports rules the recorded bridge out: it lists only the
// processes this user may inspect, and a bridge it cannot see can listen on
// the same port at another address (CodeRabbit on #1028). So every hint
// says what lsof cannot see and keeps the "this is expected" hedge. The
// verdicts are the arm's own: ok when a listener runs as this user, warn
// otherwise.
func TestLivenessArmGivesLsofsAccount(t *testing.T) {
	for _, tc := range lsofAccountCases {
		t.Run(tc.name, func(t *testing.T) {
			withLsofAnswering(t, tc.stdout, tc.code)
			withPIDAlive(t, true)
			pidFile := writePIDFile(t, 4242)
			t.Run("listener runs as this user", func(t *testing.T) { requireOwnedPortAccount(t, pidFile, tc) })
			t.Run("listener not attributable to this user", func(t *testing.T) { requireUnownedPortAccount(t, pidFile, tc) })
		})
	}
}

// lsofAccountCase is one answer lsof gives about a port on which it did
// not name the recorded bridge (pid 4242), and the account the check
// should give.
type lsofAccountCase struct {
	name     string
	stdout   string
	code     int
	account  string
	ruledOut bool
}

var lsofAccountCases = []lsofAccountCase{
	{"lsof lists nothing", "", 1, "lsof lists no process listening on this port", false},
	{"lsof lists another pid", "1305\n", 0, "lsof lists pid 1305 listening on this port", false},
	{"lsof lists other pids", "1400\n1305\n", 0, "lsof lists pids 1305, 1400 listening on this port", false},
	// busybox's applet ignores -t and the rest, and lists every open file
	// it can read.
	{"output not lsof -t's", "1 /usr/local/bin/bridge 0 /dev/null\n", 0, "lsof's output does not name pid 4242", false},
}

// requireOwnedPortAccount grades a held port whose listener runs as this
// user: the uid arm's ok, with lsof's account in place of a cause.
func requireOwnedPortAccount(t *testing.T, pidFile string, tc lsofAccountCase) {
	t.Helper()
	withPortOwner(t, true, nil)
	c := checkPort(t.Context(), "port-test", bindPort(t), pidFile)
	want := fmt.Sprintf("in use by a process running as this user (uid %d; %s)", os.Getuid(), tc.account)
	if c.Status != OK || c.Summary != want {
		t.Errorf("got %v %q, want ok %q", c.Status, c.Summary, want)
	}
	requireNoCapabilityClaim(t, c.Summary, tc.ruledOut)
}

// requireUnownedPortAccount grades a held port whose listener is not
// attributable to this user: the warn, whose hint opens with lsof's account
// and gives the advice that account leaves.
func requireUnownedPortAccount(t *testing.T, pidFile string, tc lsofAccountCase) {
	t.Helper()
	withPortOwner(t, false, nil)
	c := checkPort(t.Context(), "port-test", bindPort(t), pidFile)
	if c.Status != Warn {
		t.Fatalf("got %v (%s / %s), want warn", c.Status, c.Summary, c.Hint)
	}
	if !strings.HasPrefix(c.Hint, "our bridge (pid 4242) is still running, but "+tc.account) {
		t.Errorf("hint does not open with lsof's account %q: %s", tc.account, c.Hint)
	}
	requireNoCapabilityClaim(t, c.Hint, tc.ruledOut)
	requireAdviceFitsTheAccount(t, c.Hint, tc.ruledOut)
}

// TestLivenessArmWithoutLsofGivesTheHostsAccount: with no lsof, Linux reads
// /proc itself (pidListensOnPort) and every other unix has nothing to ask.
// The recorded pid here is this test's parent: alive, of this user,
// readable, and holding no port of ours. So /proc rules it out, and the port
// FAILs as another process's; a host with nothing to ask says so rather
// than blaming a capability, and warns.
func TestLivenessArmWithoutLsofGivesTheHostsAccount(t *testing.T) {
	withoutLsof(t)
	withPortOwner(t, false, nil)
	ppid := os.Getppid()
	c := checkPort(t.Context(), "port-test", bindPort(t), writePIDFile(t, ppid))
	if want := unseenVerdict(runtime.GOOS == "linux"); c.Status != want {
		t.Fatalf("got %v (%s / %s), want %v", c.Status, c.Summary, c.Hint, want)
	}
	lead := fmt.Sprintf("our bridge (pid %d) is still running, but this host has no lsof, and ", ppid)
	if runtime.GOOS == "linux" {
		want := lead + fmt.Sprintf("/proc shows no descriptor of pid %d listening on this port: stop the process that holds the port", ppid)
		if !strings.HasPrefix(c.Hint, want) {
			t.Errorf("hint does not give /proc's account:\n got %s\nwant %s…", c.Hint, want)
		}
		requireNoCapabilityClaim(t, c.Hint, true)
		return
	}
	want := lead + "nothing else here matches a process to a port. If our bridge is what holds the port, this is expected"
	if !strings.HasPrefix(c.Hint, want) {
		t.Errorf("hint does not say nothing looked:\n got %s\nwant %s…", c.Hint, want)
	}
	requireNoCapabilityClaim(t, c.Hint, false)
}

// TestChosenPortRefusalNamesTheRecordedBridge pins the hint on the row that
// is most likely to be the operator's own bridge: running, with a port the
// probe cannot attribute to anyone (lsof lists nothing). The FAIL stands,
// since nothing says the port is that bridge's, but the hint has to say
// which bridge was found and how to settle it, or it reads "another process
// owns this port" about what may be the operator's own listener. It gives
// lsof's account, and names a capability only where one can be the reason.
func TestChosenPortRefusalNamesTheRecordedBridge(t *testing.T) {
	withLsofAnswering(t, "", 1)
	withPIDAlive(t, true)
	withPortOwner(t, false, nil)
	pidFile := writePIDFile(t, 4242)
	c := checkChosenPort(t.Context(), "port-test", bindPort(t), pidFile)
	if c.Status != Fail {
		t.Fatalf("got %v (%s), want fail", c.Status, c.Summary)
	}
	for _, want := range []string{pidFile, "pid 4242", "lsof lists no process listening on this port", "stop that bridge"} {
		if !strings.Contains(c.Hint, want) {
			t.Errorf("hint does not say %q: %s", want, c.Hint)
		}
	}
	requireNoCapabilityClaim(t, c.Hint, false)
	requireBlindSpot(t, c.Hint)
}

// TestPortVerdictsDoNotDependOnTheAccount pins both ladders' verdicts for
// every answer the lsof probe can give, each with every answer /proc can
// give after it, the recorded pid alive or not and the listener this user's
// or not. The explanation of a miss comes from the probe; the verdict turns
// on what it establishes, never on its words. Its first form, lsof alone,
// ran on main before the accounts existed and passed there, 20 of 20.
//
// The /proc answers are what changed the rows. lsof lists only the
// processes this user may inspect, so it never rules the recorded bridge
// out. /proc, asked after an lsof miss, reads the bridge's own descriptors:
// where none is a listener on the port, the port is another process's and
// both ladders FAIL (the L4 rows of #1028's matrix, which the uid arm
// passed); where one is, it is the bridge's own and both are ok. The
// sighting's other half is pinned on every platform by
// TestPortVerdictsReadOnlyRuledOutFromTheSighting.
func TestPortVerdictsDoNotDependOnTheAccount(t *testing.T) {
	for _, a := range lsofAnswers {
		for _, p := range procAnswers {
			for _, alive := range []bool{true, false} {
				for _, owned := range []bool{true, false} {
					t.Run(fmt.Sprintf("%s/%s/alive=%v/owned=%v", a.name, p.name, alive, owned), func(t *testing.T) {
						requireVerdictsBeforeTheAccount(t, a, p, alive, owned)
					})
				}
			}
		}
	}
}

// lsofAnswer is one thing lsof can answer about a held port whose recorded
// bridge is pid 4242.
type lsofAnswer struct {
	name   string
	stdout string
	code   int
}

var lsofAnswers = []lsofAnswer{
	{"probe failed", "", 2},
	{"recorded pid listed", "4242\n", 0},
	{"nothing listed", "", 1},
	{"another pid listed", "1305\n", 0},
	{"output not lsof -t's", "1 /bin/sh 0 /dev/null\n", 0},
}

// procAnswer is one thing /proc can answer about pid 4242 after lsof did
// not name it: that pid holds the listener, its descriptors read and none
// is the listener, or it cannot tell.
type procAnswer struct {
	name  string
	found bool
	seen  ownerSighting
}

var procAnswers = []procAnswer{
	{"proc cannot tell", false, procCannotTell},
	{"proc rules the pid out", false, procRulesOut},
	{"proc sees the pid listening", true, ownerSighting{}},
}

// requireVerdictsBeforeTheAccount grades one held port through both
// ladders, lsof answering a and /proc p when lsof misses, and requires the
// verdicts ladderVerdicts gives. /proc is asked only after a clean miss, so
// it decides nothing where lsof failed or named the pid.
func requireVerdictsBeforeTheAccount(t *testing.T, a lsofAnswer, p procAnswer, alive, owned bool) {
	t.Helper()
	withLsofAnswering(t, a.stdout, a.code)
	withProcAnswering(t, p.found, p.seen, nil)
	withPIDAlive(t, alive)
	withPortOwner(t, owned, nil)
	listed, failed := a.name == "recorded pid listed", a.code == 2
	missed := !listed && !failed
	found := listed || (missed && p.found)
	port, chosen := ladderVerdicts(found, failed, alive, missed && p.seen.ruledOut, owned)
	pidFile, held := writePIDFile(t, 4242), bindPort(t)
	if c := checkPort(t.Context(), "port-test", held, pidFile); c.Status != port {
		t.Errorf("checkPort: got %v (%s / %s), want %v", c.Status, c.Summary, c.Hint, port)
	}
	if c := checkChosenPort(t.Context(), "port-test", held, pidFile); c.Status != chosen {
		t.Errorf("checkChosenPort: got %v (%s / %s), want %v", c.Status, c.Summary, c.Hint, chosen)
	}
}

// TestLsofMissAsksProcForASecondOpinion drives the owner probe with lsof
// answering through sh(1) and /proc's answer forced, and pins when /proc is
// asked and what its answer does to lsof's.
//
// lsof lists only the processes this user may inspect, so its miss cannot
// rule the recorded bridge out, and in #1028's L4 row the lsof image passed
// a port another process held where the image without lsof, reading /proc,
// ruled the bridge out: one set of facts, two verdicts, chosen by whether
// lsof is installed. So after a clean miss the probe asks /proc too. A
// /proc that sees the pid holding the listener is a match; one that rules
// it out joins its account to lsof's; one that cannot tell, or cannot read
// either socket table, leaves lsof's account as it was. /proc is not asked
// where lsof named the pid, nor where lsof failed: the failure path is the
// one checkPort degrades to a warn, and stays as it was.
func TestLsofMissAsksProcForASecondOpinion(t *testing.T) {
	lsofNothing := "lsof lists no process listening on this port"
	lsofOther := "lsof lists pid 1305 listening on this port"
	for _, tc := range []struct {
		name       string
		stdout     string
		code       int
		procFound  bool
		procSeen   ownerSighting
		procErr    error
		wantAsked  bool
		wantFound  bool
		wantSeen   ownerSighting
		wantErrSet bool
	}{
		{"lsof failed", "", 2, false, procRulesOut, nil, false, false, ownerSighting{}, true},
		{"lsof named the pid", "4242\n", 0, false, procRulesOut, nil, false, true, ownerSighting{}, false},
		{"nothing listed, proc rules out", "", 1, false, procRulesOut, nil, true, false,
			ownerSighting{saw: lsofNothing + ", and " + procRulesOut.saw, ruledOut: true}, false},
		{"another pid listed, proc rules out", "1305\n", 0, false, procRulesOut, nil, true, false,
			ownerSighting{saw: lsofOther + ", and " + procRulesOut.saw, ruledOut: true}, false},
		{"another pid listed, proc sees the pid", "1305\n", 0, true, ownerSighting{}, nil, true, true, ownerSighting{}, false},
		{"another pid listed, proc cannot tell", "1305\n", 0, false, procCannotTell, nil, true, false,
			ownerSighting{saw: lsofOther, blind: blindSpot()}, false},
		{"another pid listed, proc read no table", "1305\n", 0, false, ownerSighting{}, errors.New("no table"), true, false,
			ownerSighting{saw: lsofOther, blind: blindSpot()}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withLsofAnswering(t, tc.stdout, tc.code)
			asked := 0
			orig := procOwnerFunc
			t.Cleanup(func() { procOwnerFunc = orig })
			procOwnerFunc = func(port, pid int) (bool, ownerSighting, error) {
				asked++
				if pid != 4242 {
					t.Errorf("/proc asked about pid %d, want the recorded 4242", pid)
				}
				return tc.procFound, tc.procSeen, tc.procErr
			}
			found, seen, err := isPIDListeningOnPort(t.Context(), 7788, 4242)
			if (err != nil) != tc.wantErrSet || found != tc.wantFound || seen != tc.wantSeen {
				t.Errorf("got %v, %+v, %v; want %v, %+v, error %v", found, seen, err, tc.wantFound, tc.wantSeen, tc.wantErrSet)
			}
			if (asked > 0) != tc.wantAsked || asked > 1 {
				t.Errorf("/proc asked %d times, want asked %v, once at most", asked, tc.wantAsked)
			}
		})
	}
}
