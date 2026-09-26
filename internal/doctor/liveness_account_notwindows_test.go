//go:build !windows

package doctor

import (
	"context"
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
}

// requireNoCapabilityClaim fails when text blames a capability-bound binary
// anywhere it cannot be the reason: in every account on a platform without
// file capabilities, and on Linux wherever the probe saw the port's
// listeners and they were other processes.
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
// see, as an unprivileged user. Root sees what the others do not, and the
// hint then says nothing about it.
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

// TestLivenessArmGivesLsofsAccount: when the recorded bridge is alive and
// lsof ran cleanly without naming it, checkPort's liveness arm said "pid
// attribution blocked — capability-bound binary" (ok) or blamed
// cap_net_bind_service and dumpable=0 (warn) whatever lsof had reported.
// That is one shape, a bridge granted the capability. The hint was printed
// on macOS, which has no such capability; for a bridge running as another
// user; and when lsof named the process that holds the port, which is not
// a failure to attribute it at all.
//
// So the line says what lsof reported. Nothing listed leaves the recorded
// bridge possible, and the hint says what lsof cannot see and keeps the
// "this is expected" hedge. Pids listed rule it out, and the hint names them
// and says to stop that process. The verdicts are the arm's own: ok when a
// listener runs as this user, warn otherwise.
func TestLivenessArmGivesLsofsAccount(t *testing.T) {
	for _, tc := range []struct {
		name     string
		stdout   string
		code     int
		account  string
		ruledOut bool
	}{
		{"lsof lists nothing", "", 1, "lsof lists no process listening on this port", false},
		{"lsof lists another pid", "1305\n", 0, "lsof lists pid 1305 listening on this port", true},
		{"lsof lists other pids", "1400\n1305\n", 0, "lsof lists pids 1305, 1400 listening on this port", true},
		// busybox's applet ignores -t and the rest, and lists every open
		// file it can read.
		{"output not lsof -t's", "1 /usr/local/bin/bridge 0 /dev/null\n", 0, "lsof's output does not name pid 4242", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withLsofAnswering(t, tc.stdout, tc.code)
			withPIDAlive(t, true)
			pidFile := writePIDFile(t, 4242)

			t.Run("listener runs as this user", func(t *testing.T) {
				withPortOwner(t, true, nil)
				c := checkPort(t.Context(), "port-test", bindPort(t), pidFile)
				want := fmt.Sprintf("in use by a process running as this user (uid %d; %s)", os.Getuid(), tc.account)
				if c.Status != OK || c.Summary != want {
					t.Errorf("got %v %q, want ok %q", c.Status, c.Summary, want)
				}
				requireNoCapabilityClaim(t, c.Summary, tc.ruledOut)
			})
			t.Run("listener not attributable to this user", func(t *testing.T) {
				withPortOwner(t, false, nil)
				c := checkPort(t.Context(), "port-test", bindPort(t), pidFile)
				if c.Status != Warn {
					t.Fatalf("got %v (%s / %s), want warn", c.Status, c.Summary, c.Hint)
				}
				if !strings.HasPrefix(c.Hint, "our bridge (pid 4242) is still running, but "+tc.account) {
					t.Errorf("hint does not open with lsof's account %q: %s", tc.account, c.Hint)
				}
				requireNoCapabilityClaim(t, c.Hint, tc.ruledOut)
				if tc.ruledOut {
					if !strings.Contains(c.Hint, "stop the process that holds the port") || strings.Contains(c.Hint, "this is expected") {
						t.Errorf("lsof named the holder, so the hint must say to stop it and not call the port expected: %s", c.Hint)
					}
					return
				}
				if !strings.Contains(c.Hint, "If our bridge is what holds the port, this is expected") {
					t.Errorf("nothing ruled the bridge out, so the hint must keep the hedge: %s", c.Hint)
				}
				requireBlindSpot(t, c.Hint)
			})
		})
	}
}

// TestLivenessArmWithoutLsofGivesTheHostsAccount: with no lsof, Linux reads
// /proc itself (pidListensOnPort) and every other unix has nothing to ask.
// The recorded pid here is this test's parent: alive, of this user,
// readable, and holding no port of ours. So /proc rules it out, and a host
// with nothing to ask says so rather than blaming a capability.
func TestLivenessArmWithoutLsofGivesTheHostsAccount(t *testing.T) {
	withoutLsof(t)
	withPortOwner(t, false, nil)
	ppid := os.Getppid()
	c := checkPort(t.Context(), "port-test", bindPort(t), writePIDFile(t, ppid))
	if c.Status != Warn {
		t.Fatalf("got %v (%s / %s), want warn", c.Status, c.Summary, c.Hint)
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
// every answer the lsof probe can give, with the recorded pid alive or not
// and the listener this user's or not. The explanation of a miss comes from
// the probe; the verdict must not. This table passes on the code before the
// accounts existed, unchanged.
func TestPortVerdictsDoNotDependOnTheAccount(t *testing.T) {
	type answer struct {
		name   string
		stdout string
		code   int
	}
	for _, a := range []answer{
		{"probe failed", "", 2},
		{"recorded pid listed", "4242\n", 0},
		{"nothing listed", "", 1},
		{"another pid listed", "1305\n", 0},
		{"output not lsof -t's", "1 /bin/sh 0 /dev/null\n", 0},
	} {
		for _, alive := range []bool{true, false} {
			for _, owned := range []bool{true, false} {
				var port, chosen Status
				switch {
				case a.code == 2:
					port, chosen = Warn, Fail
				case a.name == "recorded pid listed":
					port, chosen = OK, OK
				case !alive:
					port, chosen = Fail, Fail
				case owned:
					port, chosen = OK, Fail
				default:
					port, chosen = Warn, Fail
				}
				t.Run(fmt.Sprintf("%s/alive=%v/owned=%v", a.name, alive, owned), func(t *testing.T) {
					withLsofAnswering(t, a.stdout, a.code)
					withPIDAlive(t, alive)
					withPortOwner(t, owned, nil)
					pidFile, held := writePIDFile(t, 4242), bindPort(t)
					if c := checkPort(t.Context(), "port-test", held, pidFile); c.Status != port {
						t.Errorf("checkPort: got %v (%s / %s), want %v", c.Status, c.Summary, c.Hint, port)
					}
					if c := checkChosenPort(t.Context(), "port-test", held, pidFile); c.Status != chosen {
						t.Errorf("checkChosenPort: got %v (%s / %s), want %v", c.Status, c.Summary, c.Hint, chosen)
					}
				})
			}
		}
	}
}
