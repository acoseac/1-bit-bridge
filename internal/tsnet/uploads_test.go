package tsnet

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"tailscale.com/envknob"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tsnet"
)

// The diagnostic-log uploads (backlog B749): NewServer sets NoLogsEnv true
// unless the config allows the uploads, leaves an allowed config's inherited
// environment as it is, and refuses an inherited value Tailscale's code would
// stop the process on; a tailnet that requires the logs gets the bridge's own
// words, at a start and while the node runs. No go test case can see an
// upload itself (upstream's startLogger returns at once in a test binary), so
// these tests read the environment the upstream start would read, through
// Config.BeforeStart; scripts/tsnet-uploads is the evidence from a production
// binary.

// unset is an inherited NoLogsEnv that is not set at all.
const unset = "\x00unset"

// setInherited sets NoLogsEnv as the environment the bridge inherits holds
// it, put back as it was when the test ends.
func setInherited(t *testing.T, v string) {
	t.Helper()
	keepNoLogsEnv(t)
	if v != unset {
		t.Setenv(NoLogsEnv, v)
	}
}

// envNow is NoLogsEnv as it stands, with unset for a variable that is not set.
func envNow() string {
	v, ok := os.LookupEnv(NoLogsEnv)
	if !ok {
		return unset
	}
	return v
}

// TestTheUploadPolicyByTheConfigAndTheInheritedValue: the environment the
// upstream start sees, or NewServer's refusal, for the config's switch and
// each inherited value.
func TestTheUploadPolicyByTheConfigAndTheInheritedValue(t *testing.T) {
	for _, c := range []struct {
		allow     bool
		inherited string
		want      string // with allow, the variable as inherited, byte for byte; without, it reads true
		uploads   Uploads
		refused   bool
	}{
		{false, unset, "", Uploads{}, false},
		{false, "true", "", Uploads{EnvOff: true}, false},
		{false, "false", "", Uploads{}, false},
		{false, "1", "", Uploads{EnvOff: true}, false},
		{false, "maybe", "", Uploads{}, true},
		{true, unset, unset, Uploads{Allowed: true}, false},
		{true, "true", "true", Uploads{Allowed: true, EnvOff: true}, false},
		{true, "false", "false", Uploads{Allowed: true}, false},
		{true, "", "", Uploads{Allowed: true}, false},
		{true, "enabled", "", Uploads{}, true},
	} {
		t.Run(fmt.Sprintf("allow=%v inherited=%q", c.allow, strings.ReplaceAll(c.inherited, unset, "unset")), func(t *testing.T) {
			setInherited(t, c.inherited)
			seen := make(chan string, 1)
			s, err := NewServer(Config{
				Logger: silentLogger(), StateDir: filepath.Join(t.TempDir(), "tailscale"),
				UploadDiagnosticLogs: c.allow,
				BeforeStart: func(*tsnet.Server) error {
					seen <- envNow()
					return errors.New("stopped before the upstream start")
				},
			})
			if c.refused {
				if !errors.Is(err, ErrNoLogsEnvUnreadable) {
					t.Fatalf("NewServer = %v, want ErrNoLogsEnvUnreadable", err)
				}
				if !strings.Contains(err.Error(), NoLogsEnv) || strings.Contains(err.Error(), c.inherited) {
					t.Errorf("the refusal must name the variable and not its value: %q", err)
				}
				if got := envNow(); got != c.inherited {
					t.Errorf("a refused NewServer left %s as %q, want it as inherited", NoLogsEnv, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("NewServer: %v", err)
			}
			if s.uploads != c.uploads {
				t.Errorf("the policy kept %+v, want %+v", s.uploads, c.uploads)
			}
			s.newNode = func(*tsnet.Server) node { return &fakeNode{} }
			if err := s.Start(context.Background()); err == nil {
				t.Fatal("a start BeforeStart stopped succeeded")
			}
			select {
			case got := <-seen:
				switch {
				case !c.allow && !readsOff(t, got):
					t.Errorf("the upstream start would read %s=%q as the uploads on, want them off", NoLogsEnv, got)
				case c.allow && got != c.want:
					t.Errorf("the upstream start would see %s=%q, want it as inherited, %q", NoLogsEnv, got, c.want)
				}
			default:
				t.Error("the start ended without reaching the upstream start")
			}
		})
	}
}

// held is what holds a node's uploads off, as the policy's status and a
// tailnet's refusal say it.
type held int

const (
	heldByNothing held = iota // the config allows them and nothing holds them off
	heldByConfig              // the config does not allow them
	heldByEnv                 // the environment the process was launched with
	heldByLatch               // this process turned them off earlier, and they stay off
	heldByOther               // something else in this process turned them off
)

// readsOff is NoLogsEnv's value as Tailscale's code reads it
// (envknob.NoLogsNoSupport: strconv.ParseBool, empty or unset false).
func readsOff(t *testing.T, v string) bool {
	t.Helper()
	if v == unset || v == "" {
		return false
	}
	off, err := strconv.ParseBool(v)
	if err != nil {
		t.Fatalf("%s=%q does not read as Tailscale reads it: %v", NoLogsEnv, v, err)
	}
	return off
}

// TestOnceThisProcessTurnsTheUploadsOffTheyStayOff: once the policy has set
// NoLogsEnv in this process, every later NewServer sets it again before
// anything reads it, until the process exits. The menu's "Start now", and
// bridge init's start before it, run serve in the process, and the menu runs
// it again after a stop: a later node whose config allows the uploads starts
// with them off all the same, whatever anything in the process wrote to the
// variable since (a false, or a value Tailscale's code would stop the process
// on), and the policy says what holds them off, in its status
// (Server.Uploads, Uploads.HeldOff, which the console's tile shows) and in a
// tailnet's refusal of the node, each reason in words of its own: the
// environment the process was launched with; this process, earlier, which a
// new launch undoes; something else in this process (upstream's own setter,
// which the coordination server's DisableLogTail message calls), which a new
// launch may undo, read when the status is asked for, so a setter that runs
// after the node is built shows. Each inherited value runs through each
// sequence, and the variable is read as Tailscale reads it, never by its
// spelling: right after the last node's NewServer and at its upstream start.
func TestOnceThisProcessTurnsTheUploadsOffTheyStayOff(t *testing.T) {
	type row struct {
		name      string
		inherited string
		allows    []bool // each node's config, in the order the nodes are built
		between   func() // after the first node, what else in the process does
		afterLast func() // after the last node's NewServer, what else in the process does
		newOff    bool   // right after the last node's NewServer the variable reads the uploads off
		off       bool   // the last node's upstream start reads the uploads off
		uploads   Uploads
		held      held
	}
	r := func(name, inherited string, allows []bool, between func(), off bool, u Uploads, h held) row {
		return row{name: name, inherited: inherited, allows: allows, between: between, newOff: off, off: off,
			uploads: u, held: h}
	}
	var rows []row
	for _, inherited := range []string{unset, "false", "true", "1"} {
		envOff := inherited == "true" || inherited == "1"
		allowedHeld := heldByLatch
		if envOff {
			allowedHeld = heldByEnv
		}
		label := strings.ReplaceAll(inherited, unset, "unset")
		rows = append(rows,
			r("no then yes, inherited "+label, inherited, []bool{false, true}, nil, true,
				Uploads{Allowed: true, EnvOff: envOff, LatchedOff: true}, allowedHeld),
			r("yes, no, then yes, inherited "+label, inherited, []bool{true, false, true}, nil, true,
				Uploads{Allowed: true, EnvOff: envOff, LatchedOff: true}, allowedHeld),
			r("no then no, inherited "+label, inherited, []bool{false, false}, nil, true,
				Uploads{EnvOff: envOff}, heldByConfig),
		)
	}
	rows = append(rows,
		r("yes then yes, inherited unset", unset, []bool{true, true}, nil, false, Uploads{Allowed: true}, heldByNothing),
		r("yes then yes, inherited true", "true", []bool{true, true}, nil, true,
			Uploads{Allowed: true, EnvOff: true}, heldByEnv),
		r("upstream's setter, then a node with the key", unset, []bool{true, true}, envknob.SetNoLogsNoSupport, true,
			Uploads{Allowed: true, OtherOff: true}, heldByOther),
		r("upstream's setter, then a node without the key and one with it", unset, []bool{true, false, true},
			envknob.SetNoLogsNoSupport, true, Uploads{Allowed: true, LatchedOff: true}, heldByLatch),
		r("a false set in the process after the policy's true", unset, []bool{false, true},
			func() { _ = os.Setenv(NoLogsEnv, "false") }, true, Uploads{Allowed: true, LatchedOff: true}, heldByLatch),
		r("an unreadable value set in the process after the policy's true", unset, []bool{false, true},
			func() { _ = os.Setenv(NoLogsEnv, "yes") }, true, Uploads{Allowed: true, LatchedOff: true}, heldByLatch),
		row{name: "upstream's setter after a node with the key is built", inherited: unset, allows: []bool{true},
			afterLast: envknob.SetNoLogsNoSupport, newOff: false, off: true,
			uploads: Uploads{Allowed: true, OtherOff: true}, held: heldByOther},
	)
	for _, c := range rows {
		t.Run(c.name, func(t *testing.T) {
			setInherited(t, c.inherited)
			var last *Server
			var off bool
			var startErr error
			for i, allow := range c.allows {
				if i == 1 && c.between != nil {
					c.between()
				}
				reached := false
				s, err := NewServer(Config{
					Logger: silentLogger(), StateDir: filepath.Join(t.TempDir(), "tailscale"),
					UploadDiagnosticLogs: allow,
					BeforeStart: func(*tsnet.Server) error {
						reached, off = true, readsOff(t, envNow())
						return nil
					},
				})
				if err != nil {
					t.Fatalf("node %d: NewServer: %v", i+1, err)
				}
				if i == len(c.allows)-1 {
					if got := readsOff(t, envNow()); got != c.newOff {
						t.Errorf("right after the last node's NewServer the variable reads the uploads off: %v, want %v",
							got, c.newOff)
					}
					if c.afterLast != nil {
						c.afterLast()
					}
				}
				s.newNode = func(*tsnet.Server) node {
					return &fakeNode{up: func(context.Context) (*ipnstate.Status, error) {
						return nil, fmt.Errorf("tsnet.Up: backend: %s", upstreamRefusal)
					}}
				}
				startErr = s.Start(context.Background())
				_ = s.Close()
				if !reached {
					t.Fatalf("node %d: the start ended before its upstream start", i+1)
				}
				last = s
			}
			if off != c.off {
				t.Errorf("the last node's upstream start reads the uploads off: %v, want %v", off, c.off)
			}
			got := last.Uploads()
			if got != c.uploads {
				t.Errorf("the last node's policy status is %+v, want %+v", got, c.uploads)
			}
			if got.Off() != (c.held != heldByNothing) {
				t.Errorf("Off() = %v for %+v", got.Off(), got)
			}
			var refused *LogsRequiredError
			if !errors.As(startErr, &refused) {
				t.Fatalf("the last node's start = %v, want the tailnet's refusal in the bridge's words", startErr)
			}
			mustSayWhatHoldsTheUploadsOff(t, got.HeldOff(), refused.Error(), c.held, c.uploads.EnvOff)
		})
	}
}

// mustSayWhatHoldsTheUploadsOff fails the test unless status (the policy's
// status, which the console's tile shows) and words (a tailnet's refusal) say
// what holds the uploads off, each reason in its own words, and the advice is
// a new launch of the bridge, never a restart of serve in its process.
func mustSayWhatHoldsTheUploadsOff(t *testing.T, status, words string, h held, envOff bool) {
	t.Helper()
	mustSayTheRefusal(t, words, envOff)
	switch h {
	case heldByNothing, heldByConfig:
		if status != "" {
			t.Errorf("the status says the uploads are held off where only the config decides: %q", status)
		}
	}
	switch h {
	case heldByNothing:
		if !strings.Contains(words, "tailscale.uploadDiagnosticLogs is true") || !strings.Contains(words, "after the node started") {
			t.Errorf("the refusal of a node nothing held off does not say the uploads were turned off after it started: %q", words)
		}
	case heldByConfig:
		if !strings.Contains(words, "set tailscale.uploadDiagnosticLogs: true") {
			t.Errorf("the refusal of a node without the key does not say to set it: %q", words)
		}
	case heldByEnv:
		for _, want := range []string{NoLogsEnv + "=true", "the environment the bridge was launched with", "launch it again"} {
			if !strings.Contains(status, want) {
				t.Errorf("the status does not say %q: %q", want, status)
			}
		}
	case heldByLatch:
		for _, want := range []string{"this process turned", "until it exits", "next launch", "launch it again"} {
			if !strings.Contains(status, want) {
				t.Errorf("the status does not say %q: %q", want, status)
			}
		}
	case heldByOther:
		for _, want := range []string{"something in this process turned", "may turn them back on", "launch it again"} {
			if !strings.Contains(status, want) {
				t.Errorf("the status does not say %q: %q", want, status)
			}
		}
		if strings.Contains(status, "next launch") {
			t.Errorf("the status promises the next launch for uploads something else in the process turned off: %q", status)
		}
	}
	if h == heldByLatch || h == heldByOther {
		if strings.Contains(status, NoLogsEnv) {
			t.Errorf("the status tells the operator to remove a variable the launch environment did not hold: %q", status)
		}
	}
	if h == heldByEnv || h == heldByLatch || h == heldByOther {
		if !strings.Contains(words, status) {
			t.Errorf("the refusal does not say what the status says.\nstatus: %q\nrefusal: %q", status, words)
		}
	}
}

// TestBeforeStartRunsOnTheUpstreamServerAndCanStopIt: the hook gets the
// upstream server Start built, just before its Start, and an error from it
// ends the start there, the node closed unstarted.
func TestBeforeStartRunsOnTheUpstreamServerAndCanStopIt(t *testing.T) {
	keepNoLogsEnv(t)
	dir := filepath.Join(t.TempDir(), "tailscale")
	n := &fakeNode{}
	var got *tsnet.Server
	s, err := NewServer(Config{Logger: silentLogger(), StateDir: dir, Hostname: "named",
		BeforeStart: func(ts *tsnet.Server) error {
			got = ts
			return errors.New("the test's stop")
		}})
	if err != nil {
		t.Fatal(err)
	}
	s.newNode = func(*tsnet.Server) node { return n }
	err = s.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "the test's stop") {
		t.Fatalf("Start = %v, want the hook's error", err)
	}
	if got == nil || got.Dir != dir || got.Hostname != "named" {
		t.Fatalf("the hook got %+v, want the upstream server Start built", got)
	}
	if n.startCalls() != 0 {
		t.Error("the node's Start ran after the hook stopped the start")
	}
	n.mustHaveBeenClosedOnceAfterItsStart(t)
	mustNotBeStarted(t, s)
}

// upstreamRefusal is upstream's refusal as ipnlocal words it, for the stand-in
// nodes; TestTheLogsRequirementIsUpstreamsWords holds the part the bridge
// matches to the source.
const upstreamRefusal = "tailnet requires logging to be enabled. Remove --no-logs-no-support from tailscaled command line."

// TestARefusedStartSaysWhatTheTailnetRequires: a start the tailnet refuses for
// the logs ends in the bridge's words, kept as the last start error; the
// words name the config's switch always and the inherited variable where it
// holds the uploads off, and the refusal changes nothing in the environment.
func TestARefusedStartSaysWhatTheTailnetRequires(t *testing.T) {
	for _, c := range []struct {
		allow     bool
		inherited string
		envNamed  bool
	}{
		{false, unset, false},
		{false, "true", true},
		{true, "true", true},
		{true, "false", false},
	} {
		t.Run(fmt.Sprintf("allow=%v inherited=%q", c.allow, strings.ReplaceAll(c.inherited, unset, "unset")), func(t *testing.T) {
			setInherited(t, c.inherited)
			var atStart string
			s, err := NewServer(Config{Logger: silentLogger(), StateDir: filepath.Join(t.TempDir(), "tailscale"),
				UploadDiagnosticLogs: c.allow,
				BeforeStart:          func(*tsnet.Server) error { atStart = envNow(); return nil }})
			if err != nil {
				t.Fatal(err)
			}
			s.newNode = func(*tsnet.Server) node {
				return &fakeNode{up: func(context.Context) (*ipnstate.Status, error) {
					return nil, fmt.Errorf("tsnet.Up: backend: %s", upstreamRefusal)
				}}
			}
			err = s.Start(context.Background())
			var refused *LogsRequiredError
			if !errors.As(err, &refused) {
				t.Fatalf("Start = %v, want a *LogsRequiredError", err)
			}
			if last := s.LastStartError(); !errors.As(last, &refused) {
				t.Errorf("LastStartError = %v, want the refusal", last)
			}
			mustSayTheRefusal(t, err.Error(), c.envNamed)
			if got := envNow(); got != atStart {
				t.Errorf("the refusal changed %s from %q to %q", NoLogsEnv, atStart, got)
			}
		})
	}
}

// mustSayTheRefusal fails the test unless words are the bridge's refusal:
// the config's switch named, the inherited variable named exactly where it
// holds the uploads off, a new launch of the bridge as what makes a change
// apply (never a restart of serve, which the menu makes in this process),
// what the switch costs, and none of upstream's tailscaled advice.
func mustSayTheRefusal(t *testing.T, words string, envNamed bool) {
	t.Helper()
	for _, want := range []string{"tailscale.uploadDiagnosticLogs", "launch it again", "restart its service",
		"uploads Tailscale's diagnostic logs", "never turns the uploads on itself"} {
		if !strings.Contains(words, want) {
			t.Errorf("the refusal does not say %q: %q", want, words)
		}
	}
	if strings.Contains(words, "restart the bridge") {
		t.Errorf("the refusal advises a restart of the bridge, which the menu's Start now makes in this process: %q", words)
	}
	if named := strings.Contains(words, NoLogsEnv); named != envNamed {
		t.Errorf("the refusal names %s: %v, want %v: %q", NoLogsEnv, named, envNamed, words)
	}
	if strings.Contains(words, "--no-logs-no-support") {
		t.Errorf("the refusal carries upstream's tailscaled advice: %q", words)
	}
}

// TestARunningNodeTheTailnetRefusesIsSaidInTheBridgesWords: the refusal that
// arrives while the node runs, in its status's health.
func TestARunningNodeTheTailnetRefusesIsSaidInTheBridgesWords(t *testing.T) {
	setInherited(t, unset)
	s, err := NewServer(Config{Logger: silentLogger(), StateDir: filepath.Join(t.TempDir(), "tailscale")})
	if err != nil {
		t.Fatal(err)
	}
	refused := &ipnstate.Status{BackendState: "Stopped",
		Health: []string{"some other warning", "The local log is misconfigured: " + upstreamRefusal}}
	err = s.LogsRequired(refused)
	if err == nil {
		t.Fatal("LogsRequired = nil for a status whose health reports the refusal")
	}
	mustSayTheRefusal(t, err.Error(), false)
	if err := s.LogsRequired(&ipnstate.Status{BackendState: "Running", Health: []string{"some other warning"}}); err != nil {
		t.Errorf("LogsRequired = %v for a status that reports no refusal", err)
	}
	if err := s.LogsRequired(nil); err != nil {
		t.Errorf("LogsRequired(nil) = %v", err)
	}
}

// TestAStartThatSucceedsClearsTheLastStartError: a refusal is the last start's
// error until a start succeeds.
func TestAStartThatSucceedsClearsTheLastStartError(t *testing.T) {
	keepNoLogsEnv(t)
	s, err := NewServer(Config{Logger: silentLogger(), StateDir: filepath.Join(t.TempDir(), "tailscale")})
	if err != nil {
		t.Fatal(err)
	}
	refuse := true
	s.newNode = func(*tsnet.Server) node {
		return &fakeNode{up: func(context.Context) (*ipnstate.Status, error) {
			if refuse {
				return nil, fmt.Errorf("tsnet.Up: backend: %s", upstreamRefusal)
			}
			return &ipnstate.Status{}, nil
		}}
	}
	t.Cleanup(func() { _ = s.Close() })
	if s.LastStartError() != nil {
		t.Fatal("LastStartError before any start")
	}
	if err := s.Start(context.Background()); err == nil {
		t.Fatal("the refused start succeeded")
	}
	refuse = false
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("the second start: %v", err)
	}
	if err := s.LastStartError(); err != nil {
		t.Errorf("LastStartError after a start succeeded = %v", err)
	}
}

// TestTheLogsRequirementIsUpstreamsWords pins the part of upstream's refusal
// the bridge matches on to the tailscale.com source this build resolves
// (go list -m, which reads go.mod and the module cache and needs no network,
// GOPROXY=off included): a dependency bump that rewords it fails here, which a
// literal copied into a test cannot.
func TestTheLogsRequirementIsUpstreamsWords(t *testing.T) {
	goBin, err := exec.LookPath("go")
	if err != nil || !filepath.IsAbs(goBin) {
		t.Fatalf("the go command is not on PATH (%v): the pin needs it to resolve the tailscale.com module", err)
	}
	out, err := exec.Command(goBin, "list", "-m", "-f", "{{.Dir}}", "tailscale.com").Output()
	if err != nil {
		t.Fatalf("go list -m tailscale.com: %v", err)
	}
	dir := strings.TrimSpace(string(out))
	if dir == "" {
		t.Fatal("go list -m gave tailscale.com no module directory")
	}
	src, err := os.ReadFile(filepath.Join(dir, "ipn", "ipnlocal", "local.go"))
	if err != nil {
		t.Fatalf("read upstream's ipnlocal: %v", err)
	}
	if !strings.Contains(string(src), logsRequiredText) {
		t.Errorf("tailscale.com at %s no longer words the refusal of a node without logs as %q: "+
			"the bridge would not recognise it, and the tile would show upstream's tailscaled advice", dir, logsRequiredText)
	}
}

// TestTheNoLogsVariableIsUpstreams holds NoLogsEnv to the variable upstream's
// own setter and reader use.
func TestTheNoLogsVariableIsUpstreams(t *testing.T) {
	setInherited(t, unset)
	envknob.SetNoLogsNoSupport()
	if got := envNow(); !readsOff(t, got) {
		t.Errorf("envknob.SetNoLogsNoSupport left %s=%q, which reads as the uploads on: the bridge's constant names "+
			"another variable", NoLogsEnv, got)
	}
	t.Setenv(NoLogsEnv, "false")
	if envknob.NoLogsNoSupport() {
		t.Errorf("envknob.NoLogsNoSupport does not read %s", NoLogsEnv)
	}
}
