package tsnet

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
		want      string // the variable when upstream would start
		uploads   Uploads
		refused   bool
	}{
		{false, unset, "true", Uploads{}, false},
		{false, "true", "true", Uploads{EnvOff: true}, false},
		{false, "false", "true", Uploads{}, false},
		{false, "1", "true", Uploads{EnvOff: true}, false},
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
			if got := <-seen; got != c.want {
				t.Errorf("the upstream start would see %s=%q, want %q", NoLogsEnv, got, c.want)
			}
		})
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
// holds the uploads off, what the switch costs, and none of upstream's
// tailscaled advice.
func mustSayTheRefusal(t *testing.T, words string, envNamed bool) {
	t.Helper()
	for _, want := range []string{"tailscale.uploadDiagnosticLogs", "restart", "uploads Tailscale's diagnostic logs",
		"never turns the uploads on itself"} {
		if !strings.Contains(words, want) {
			t.Errorf("the refusal does not say %q: %q", want, words)
		}
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
	if got := envNow(); got != "true" {
		t.Errorf("envknob.SetNoLogsNoSupport set %s to %q: the bridge's constant names another variable", NoLogsEnv, got)
	}
	t.Setenv(NoLogsEnv, "false")
	if envknob.NoLogsNoSupport() {
		t.Errorf("envknob.NoLogsNoSupport does not read %s", NoLogsEnv)
	}
}
