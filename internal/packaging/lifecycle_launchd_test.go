//go:build !windows

package packaging

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// launchctlCalls is a stand-in launchctl that records every call and
// answers each verb as answers says: a missing verb succeeds.
type launchctlCalls struct {
	calls   [][]string
	answers map[string]launchctlAnswer
}

// launchctlAnswer is one verb's answer: its output and its error.
type launchctlAnswer struct {
	out string
	err error
}

// run is the launchctlRunner the stand-in hands startLaunchdAgent.
func (l *launchctlCalls) run(args ...string) ([]byte, error) {
	l.calls = append(l.calls, args)
	a := l.answers[args[0]]
	return []byte(a.out), a.err
}

// refusedBootstrap is how launchctl on macOS 27 refuses to bootstrap an
// agent that is already loaded (measured, 2026-09-30).
var refusedBootstrap = launchctlAnswer{out: "Bootstrap failed: 5: Input/output error\nTry re-running the command as root for richer errors.\n", err: errors.New("exit status 5")}

// TestStartBootstrapsAnAgentThatIsNotLoaded — the ordinary `bridge start`:
// the bootstrap loads the agent (RunAtLoad starts it), and nothing more is
// asked.
func TestStartBootstrapsAnAgentThatIsNotLoaded(t *testing.T) {
	l := &launchctlCalls{}
	if err := startLaunchdAgent("/Users/x/Library/LaunchAgents/agent.plist", l.run); err != nil {
		t.Fatal(err)
	}
	want := [][]string{{"bootstrap", "gui/" + uidString(), "/Users/x/Library/LaunchAgents/agent.plist"}}
	if !reflect.DeepEqual(l.calls, want) {
		t.Errorf("launchctl calls %q, want %q", l.calls, want)
	}
}

// TestStartKickstartsAnAgentThatIsLoadedAndStopped — the agent an exit 0
// leaves behind is loaded and not running, and launchctl refuses to
// bootstrap it again; `bridge start` kickstarts it. Until backlog B201 it
// reported the refusal (exit 5) and the bridge stayed down.
func TestStartKickstartsAnAgentThatIsLoadedAndStopped(t *testing.T) {
	l := &launchctlCalls{answers: map[string]launchctlAnswer{"bootstrap": refusedBootstrap}}
	if err := startLaunchdAgent("/a.plist", l.run); err != nil {
		t.Fatalf("a refused bootstrap of a loaded agent: %v, want the kickstart to start it", err)
	}
	want := [][]string{
		{"bootstrap", "gui/" + uidString(), "/a.plist"},
		{"kickstart", "gui/" + uidString() + "/" + ServiceLabel},
	}
	if !reflect.DeepEqual(l.calls, want) {
		t.Errorf("launchctl calls %q, want %q", l.calls, want)
	}
}

// TestStartReportsBothAnswersWhenNeitherStartsTheAgent — no agent loaded
// and a bootstrap that fails (a plist launchd will not read): the
// kickstart finds nothing to start, and the error carries both answers.
func TestStartReportsBothAnswersWhenNeitherStartsTheAgent(t *testing.T) {
	l := &launchctlCalls{answers: map[string]launchctlAnswer{
		"bootstrap": {out: "Bootstrap failed: 22: Invalid argument\n", err: errors.New("exit status 22")},
		"kickstart": {out: "Could not find service \"" + ServiceLabel + "\" in domain for user gui: 501\n", err: errors.New("exit status 113")},
	}}
	err := startLaunchdAgent("/a.plist", l.run)
	if err == nil {
		t.Fatal("neither verb started the agent, and startLaunchdAgent reported nothing")
	}
	for _, want := range []string{"Bootstrap failed: 22", "exit status 22", "Could not find service", "exit status 113"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not carry %q", err, want)
		}
	}
}
