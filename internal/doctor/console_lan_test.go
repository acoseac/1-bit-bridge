package doctor

import (
	"context"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/consolelan"
)

// lanFacts is a running bridge listening on its home-network socket.
func lanFacts() ConsoleLANFacts {
	return ConsoleLANFacts{
		Port: 7789, Running: true, Reported: true,
		FileKey: consolelan.FileKeyAddress, FileAddress: "192.168.1.20",
		State: consolelan.State{
			State: consolelan.StateListening, Address: "192.168.1.20", URL: "http://192.168.1.20:7789",
			Source: consolelan.SourceFile, FileKeyAtBoot: consolelan.FileKeyAddress, FileAddressAtBoot: "192.168.1.20",
			Network: "192.168.1.0/24", Interface: "eth0", Hold: consolelan.HoldBindToIfindex,
		},
	}
}

func findCheck(cs []Check, name string) (Check, bool) {
	for _, c := range cs {
		if c.Name == name {
			return c, true
		}
	}
	return Check{}, false
}

func lanChecks(f ConsoleLANFacts) []Check { return consoleLANChecks(Deps{ConsoleLAN: &f}) }

// TestTheConsoleLANLineSaysEachState holds console-lan to the plan's §5.3
// table, state by state (R9): a socket that listens warns with its URL, no
// password, the interface and the hold; one that waits says this machine
// only and the reason's words, and the retry but for not_supported_here; a
// Windows box off the strong host model names the interface; a bridge that
// does not report says it was not checked; a stopped bridge says what the
// next start opens; and a run that says nothing of the posture has no line.
func TestTheConsoleLANLineSaysEachState(t *testing.T) {
	t.Run("listening", func(t *testing.T) {
		c, ok := findCheck(lanChecks(lanFacts()), checkNameConsoleLAN)
		if !ok || c.Status != Warn {
			t.Fatalf("console-lan = %+v, %v", c, ok)
		}
		for _, want := range []string{"http://192.168.1.20:7789", "no password", "plain HTTP", "eth0", "SO_BINDTOIFINDEX"} {
			if !strings.Contains(c.Summary, want) {
				t.Errorf("the line %q does not say %q", c.Summary, want)
			}
		}
		if !strings.Contains(c.Hint, "forward") {
			t.Errorf("the hint %q does not warn against a port-forward", c.Hint)
		}
	})
	t.Run("waiting", func(t *testing.T) {
		f := lanFacts()
		f.State.State, f.State.Reason, f.State.URL, f.State.Interface, f.State.Hold = consolelan.StateWaiting, consolelan.ReasonAddressNotOnBox, "", "", ""
		c, _ := findCheck(lanChecks(f), checkNameConsoleLAN)
		if c.Status != Warn || !strings.Contains(c.Summary, "this machine only") ||
			!strings.Contains(c.Summary, "192.168.1.20 is not on this box") || !strings.Contains(c.Summary, "every 5 s") {
			t.Fatalf("console-lan = %+v", c)
		}
	})
	t.Run("not supported here", func(t *testing.T) {
		f := lanFacts()
		f.State.State, f.State.Reason = consolelan.StateWaiting, consolelan.ReasonNotSupportedHere
		c, _ := findCheck(lanChecks(f), checkNameConsoleLAN)
		if !strings.Contains(c.Summary, "cannot hold") || strings.Contains(c.Summary, "every 5 s") {
			t.Fatalf("console-lan = %+v", c)
		}
	})
	t.Run("Windows off the strong host model", func(t *testing.T) {
		f := lanFacts()
		f.State.State, f.State.Reason, f.State.Interface = consolelan.StateWaiting, consolelan.ReasonInterfaceNotStrongHost, "vEthernet"
		c, _ := findCheck(lanChecks(f), checkNameConsoleLAN)
		if !strings.Contains(c.Summary, "vEthernet") || !strings.Contains(c.Summary, "strong host model") || !strings.Contains(c.Hint, "vEthernet") {
			t.Fatalf("console-lan = %+v", c)
		}
	})
	t.Run("a bridge that does not report", func(t *testing.T) {
		f := lanFacts()
		f.Reported, f.State = false, consolelan.State{}
		c, _ := findCheck(lanChecks(f), checkNameConsoleLAN)
		if c.Status != OK || !strings.Contains(c.Summary, "not checked") {
			t.Fatalf("console-lan = %+v", c)
		}
	})
	t.Run("not running", func(t *testing.T) {
		f := lanFacts()
		f.Running, f.Reported, f.State, f.HeldNow = false, false, consolelan.State{}, "no"
		c, _ := findCheck(lanChecks(f), checkNameConsoleLAN)
		for _, want := range []string{"when the bridge runs", "http://192.168.1.20:7789", "no password", "does not hold 192.168.1.20", "may differ"} {
			if !strings.Contains(c.Summary, want) {
				t.Errorf("the line %q does not say %q", c.Summary, want)
			}
		}
	})
	t.Run("nothing says anything", func(t *testing.T) {
		f := ConsoleLANFacts{Port: 7789, FileKey: consolelan.FileKeyAbsent}
		if cs := lanChecks(f); len(cs) != 0 {
			t.Fatalf("a run that says nothing of the posture has lines: %+v", cs)
		}
		f.Running, f.Reported, f.State = true, true, consolelan.State{State: consolelan.StateOff, FileKeyAtBoot: consolelan.FileKeyAbsent}
		if cs := lanChecks(f); len(cs) != 0 {
			t.Fatalf("a running bridge with the posture off has lines: %+v", cs)
		}
	})
	t.Run("a config that does not load runs neither line", func(t *testing.T) {
		f := lanFacts()
		f.NotGraded = "the config does not load"
		if cs := lanChecks(f); len(cs) != 0 {
			t.Fatalf("lines over a config that does not load: %+v", cs)
		}
	})
}

// TestTheConsoleLANLinesNameTheSourceAndWhatClearsIt: the running bridge's
// source in one wording, with what clears it (D7), for each source and,
// beside a variable, what the file's key held at the start (F7); and the
// shell's variable said apart where it is not the running bridge's (D14),
// which the console's own doctor, whose environment is the service's, never
// says.
func TestTheConsoleLANLinesNameTheSourceAndWhatClearsIt(t *testing.T) {
	for _, c := range []struct {
		name, source, key, addr string
		want                    []string
	}{
		{"file", consolelan.SourceFile, consolelan.FileKeyAddress, "192.168.1.20",
			[]string{"set in bridge.yaml (adminLANAddress)", "remove the adminLANAddress line"}},
		{"environment", consolelan.SourceEnvironment, consolelan.FileKeyAbsent, "",
			[]string{"set by BRIDGE_ADMIN_LAN_ADDRESS in the bridge's environment", "unset BRIDGE_ADMIN_LAN_ADDRESS where the service is started"}},
		{"both over an address", consolelan.SourceBoth, consolelan.FileKeyAddress, "192.168.1.30",
			[]string{"over bridge.yaml's adminLANAddress", "opens the console at bridge.yaml's address, 192.168.1.30,"}},
		{"both over an empty key", consolelan.SourceBoth, consolelan.FileKeyEmpty, "",
			[]string{"turns the console off the home network", "empty adminLANAddress line still costs the rollback"}},
		{"both over a key that is no address", consolelan.SourceBoth, consolelan.FileKeyNotAnAddress, "",
			[]string{"makes the next start refuse bridge.yaml"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := lanFacts()
			f.State.Source, f.State.FileKeyAtBoot, f.State.FileAddressAtBoot = c.source, c.key, c.addr
			f.FileKey, f.FileAddress = c.key, c.addr
			f.EnvSet = c.source != consolelan.SourceFile
			line, _ := findCheck(lanChecks(f), checkNameConsoleLAN)
			text := line.Summary + " | " + line.Hint
			for _, w := range c.want {
				if !strings.Contains(text, w) {
					t.Errorf("console-lan %q does not say %q", text, w)
				}
			}
			if strings.Contains(text, "this shell's") {
				t.Errorf("a shell that matches the running bridge was told it does not: %q", text)
			}
		})
	}
	t.Run("this shell's variable is not the running bridge's", func(t *testing.T) {
		f := lanFacts()
		f.EnvSet = true
		line, _ := findCheck(lanChecks(f), checkNameConsoleLAN)
		if !strings.Contains(line.Summary, "this shell's BRIDGE_ADMIN_LAN_ADDRESS is not the running bridge's") {
			t.Fatalf("console-lan = %+v", line)
		}
		f.Console = true
		line, _ = findCheck(lanChecks(f), checkNameConsoleLAN)
		if strings.Contains(line.Summary, "this shell's") {
			t.Fatalf("the console's own doctor spoke of a shell: %+v", line)
		}
	})
	t.Run("the file edited since the start", func(t *testing.T) {
		f := lanFacts()
		f.FileAddress = "192.168.1.40"
		line, _ := findCheck(lanChecks(f), checkNameConsoleLAN)
		for _, w := range []string{"started with bridge.yaml's adminLANAddress 192.168.1.20", "192.168.1.40 now", "the next start binds 192.168.1.40"} {
			if !strings.Contains(line.Summary, w) {
				t.Errorf("console-lan %q does not say %q", line.Summary, w)
			}
		}
	})
}

// TestTheRollbackLineIsReadFromTheFileAsItIsNow: console-lan-rollback warns
// while the file holds the key now, an address, empty or not an address,
// whatever the running bridge does, and says nothing for a file without it
// (§2.7, D16).
func TestTheRollbackLineIsReadFromTheFileAsItIsNow(t *testing.T) {
	for _, c := range []struct {
		key, holds string
	}{
		{consolelan.FileKeyAddress, "bridge.yaml holds adminLANAddress:"},
		{consolelan.FileKeyEmpty, "bridge.yaml holds adminLANAddress, empty:"},
		{consolelan.FileKeyNotAnAddress, "which does not read as an address"},
	} {
		for _, running := range []bool{true, false} {
			f := lanFacts()
			f.FileKey, f.Running, f.Reported = c.key, running, running
			if !running {
				f.State = consolelan.State{}
			}
			line, ok := findCheck(lanChecks(f), checkNameConsoleLANRollback)
			if !ok || line.Status != Warn || !strings.Contains(line.Summary, c.holds) ||
				!strings.Contains(line.Summary, "0.2.3 and earlier refuse this file") ||
				!strings.Contains(line.Hint, "remove the whole adminLANAddress line") || !strings.Contains(line.Hint, "emptying it is not enough") {
				t.Errorf("key %s, running %v: console-lan-rollback = %+v, %v", c.key, running, line, ok)
			}
		}
	}
	f := lanFacts()
	f.FileKey = consolelan.FileKeyAbsent
	if _, ok := findCheck(lanChecks(f), checkNameConsoleLANRollback); ok {
		t.Error("a file without the key has a rollback line")
	}
}

// TestRunPrintsTheConsoleLANLinesAfterTheAdminPort: Run places both lines
// right after port-admin, and none where the facts say nothing.
func TestRunPrintsTheConsoleLANLinesAfterTheAdminPort(t *testing.T) {
	f := lanFacts()
	rep := Run(context.Background(), Deps{ConsoleLAN: &f})
	at := -1
	for i, c := range rep.Checks {
		if c.Name == checkNamePortAdmin {
			at = i
		}
	}
	if at < 0 || at+2 >= len(rep.Checks) {
		t.Fatalf("no port-admin line, or nothing after it: %d of %d", at, len(rep.Checks))
	}
	if rep.Checks[at+1].Name != checkNameConsoleLAN || rep.Checks[at+2].Name != checkNameConsoleLANRollback {
		t.Fatalf("after port-admin come %q and %q", rep.Checks[at+1].Name, rep.Checks[at+2].Name)
	}
	base := Run(context.Background(), Deps{})
	if _, ok := findCheck(base.Checks, checkNameConsoleLAN); ok {
		t.Error("a run with no facts has a console-lan line")
	}
}
