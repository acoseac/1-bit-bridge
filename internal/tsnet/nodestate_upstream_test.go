package tsnet

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"tailscale.com/envknob"
	"tailscale.com/ipn"
	"tailscale.com/tailcfg"
	"tailscale.com/tsnet"
	"tailscale.com/tstest/integration/testcontrol"
)

// The keys upstream itself writes for a node it registers (backlog B829).
// readNodeState reads a registered node's name from the current profile
// (ipn.CurrentProfileStateKey) and the known profiles
// (ipn.KnownProfilesStateKey), which upstream's start takes on every OS, since
// tsnet asks for controlclient.LocalBackendStartKeyOSNeutral. The fixtures in
// nodestate_test.go write that state with the test's own keys; here upstream's
// own start registers a node and writes it, against an in-process control
// server (testcontrol, as upstream's tsnet tests use) on loopback, so CI's
// windows-latest and macos-latest jobs read what upstream writes there.

// registrationReserve is what the case's waits on the node leave of the test
// binary's deadline: they give up at the deadline less it, or less half of
// what is left when that is less, as the repository's other waits on an event
// do.
const registrationReserve = 30 * time.Second

// TestTheStateUpstreamWritesForARegisteredNodeReadsAsRegistered: a node
// upstream's tsnet.Server registers with a state dir of its own, against an
// in-process control server and nothing beyond loopback, persists a profile
// that readNodeState, once the node is closed, reads as registered, with the
// hostname the server was given; and a start of the bridge's over that state
// keeps the name.
func TestTheStateUpstreamWritesForARegisteredNodeReadsAsRegistered(t *testing.T) {
	keepNoLogsEnv(t)
	// The portmapper would ask the LAN's gateway for a mapping (NAT-PMP, PCP
	// and UPnP). Its knob is registered, so only envknob's setter reaches it.
	envknob.SetenvForTest(t, "TS_DISABLE_PORTMAPPER", "true")
	log := &nodeLog{}
	control := &testcontrol.Server{
		// No region: the node dials no relay and sends no STUN probe. It then
		// never reaches Running, which asks for a live relay or peer, and it
		// registers all the same: ipnlocal writes the profile its netmap names
		// before its state moves to Starting.
		DERPMap: &tailcfg.DERPMap{},
		Logf:    log.logf,
	}
	control.HTTPTestServer = httptest.NewUnstartedServer(control)
	control.HTTPTestServer.Start()
	t.Cleanup(control.HTTPTestServer.Close)

	const hostname = "B829 Node"
	dir := filepath.Join(t.TempDir(), "tailscale")
	// As startUnlocked builds the bridge's node, its state in Dir through
	// upstream's file store and never ephemeral, but for the control server.
	// An auth key of the test's own keeps TS_AUTHKEY and an OAuth secret the
	// environment may hold out of the start; testcontrol takes any key.
	s := &tsnet.Server{
		Dir:        dir,
		Hostname:   hostname,
		ControlURL: control.HTTPTestServer.URL,
		AuthKey:    "tskey-auth-b829",
		Ephemeral:  false,
		Logf:       log.logf,
		UserLogf:   log.logf,
	}
	err := s.Start()
	// Registered after the control server's, so it runs before it: the node
	// lets go of its long poll before the server waits for its handlers.
	t.Cleanup(func() { _ = s.Close() })
	if err != nil {
		t.Fatalf("upstream's start: %v\nthe log:\n%s", err, log)
	}
	ctx, cancel := registrationContext(t)
	defer cancel()
	lc, err := s.LocalClient()
	if err != nil {
		t.Fatalf("the node's local client: %v", err)
	}
	watcher, err := lc.WatchIPNBus(ctx, ipn.NotifyInitialState)
	if err != nil {
		t.Fatalf("watch the node's backend: %v", err)
	}
	waitForRegistration(t, watcher, log)
	_ = watcher.Close()
	st, err := lc.Status(ctx)
	if err != nil {
		t.Fatalf("the node's status: %v", err)
	}
	if st.Self == nil || st.Self.ID == "" {
		t.Fatalf("the node is %s with no identity from the control server", st.BackendState)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close the node: %v", err)
	}

	keys := stateKeysIn(t, dir)
	t.Logf("the state keys upstream wrote: %v", keys)
	for _, k := range []ipn.StateKey{ipn.CurrentProfileStateKey, ipn.KnownProfilesStateKey} {
		if !slices.Contains(keys, k) {
			t.Errorf("upstream registered the node and wrote no %q key: %v", k, keys)
		}
	}
	got, err := readNodeState(dir)
	if err != nil || !got.registered || got.hostname != hostname {
		t.Fatalf("readNodeState = %+v, %v over the state upstream wrote for a registered node; want it registered "+
			"as %q", got, err, hostname)
	}
	if name, _ := startForName(t, Config{StateDir: dir, UnreadableStateHostname: "Living Room"}); name != hostname {
		t.Errorf("a start over the state upstream wrote names the node %q, want the name it registered with, %q",
			name, hostname)
	}
}

// registrationContext bounds the case's waits on the node: until the test
// binary's deadline less registrationReserve, or less half of what is left
// when that is less, and with no bound where the binary runs without one.
func registrationContext(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	deadline, ok := t.Deadline()
	if !ok {
		return context.WithCancel(context.Background())
	}
	left := time.Until(deadline)
	return context.WithDeadline(context.Background(), deadline.Add(-min(registrationReserve, left/2)))
}

// waitForRegistration waits on the node's backend until its state is Starting
// or Running, which it reaches once the control server has registered it and
// sent its netmap, and fails the test on a backend error or when the
// watcher's context gives up.
func waitForRegistration(t *testing.T, watcher interface{ Next() (ipn.Notify, error) }, log *nodeLog) {
	t.Helper()
	for {
		n, err := watcher.Next()
		if err != nil {
			t.Fatalf("the node did not register before the test's deadline less its reserve: %v\nthe log:\n%s", err, log)
		}
		if n.ErrMessage != nil {
			t.Fatalf("the node's backend: %s\nthe log:\n%s", *n.ErrMessage, log)
		}
		if n.State != nil && (*n.State == ipn.Starting || *n.State == ipn.Running) {
			return
		}
	}
}

// stateKeysIn is the keys the state file in dir holds, sorted.
func stateKeysIn(t *testing.T, dir string) []ipn.StateKey {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, stateFileName))
	if err != nil {
		t.Fatal(err)
	}
	var state map[ipn.StateKey]json.RawMessage
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatalf("the state file upstream wrote does not parse: %v", err)
	}
	keys := make([]ipn.StateKey, 0, len(state))
	for k := range state {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// nodeLog keeps the last lines the node and the control server logged, for a
// failure's message. It is safe to write after the test has ended, which a
// goroutine upstream's Close does not wait for may do, where t.Logf is not.
type nodeLog struct {
	mu    sync.Mutex
	lines []string
}

// nodeLogLines is how many of the last lines a nodeLog keeps.
const nodeLogLines = 200

func (l *nodeLog) logf(format string, args ...any) {
	line := strings.TrimRight(fmt.Sprintf(format, args...), "\n")
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, line)
	if len(l.lines) > nodeLogLines {
		l.lines = slices.Delete(l.lines, 0, len(l.lines)-nodeLogLines)
	}
}

func (l *nodeLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}
