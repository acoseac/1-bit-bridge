package tsnet

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tsnet"
)

// The uploads of a start a stop gave up on (backlog B830). Upstream's
// Server.Start builds the node's log transport first (startLogger), in the
// part of its start that takes no context, which can stall after it; serve's
// stop then gives up waiting and closes the wrapper, whose Close does not wait
// for a start it has not published. Where the config allowed the uploads, that
// transport could upload past the stop, and while a later node whose config
// withdrew the consent runs. AbandonStart is what the stop calls once its wait
// has run out. These tests hold the wrapper's call to upstream's Start
// unreturned (fakeNode.start) and read the decision through a stand-in for
// logtail's switch (standInForLogtail): upstream's startLogger builds no
// logger in a test binary, and the real switch lasts the process.

// logtailStandIn is what a case puts in the place of logtail's switch for its
// length: it counts the times the switch was turned.
type logtailStandIn struct{ turned atomic.Int32 }

// standInForLogtail puts a logtailStandIn in the place of logtail's switch
// until the case ends.
func standInForLogtail(t *testing.T) *logtailStandIn {
	t.Helper()
	sw := &logtailStandIn{}
	t.Cleanup(SetLogtailDisableForTest(func() { sw.turned.Add(1) }))
	return sw
}

func (sw *logtailStandIn) times() int { return int(sw.turned.Load()) }

// holdUpstreamStart is a node whose Start, the part of upstream's start that
// takes no context, is held unreturned from the moment it is entered until
// release, as a start stalled after it built the node's log transport is.
// release is idempotent, and the case's cleanup runs it.
func holdUpstreamStart(t *testing.T) (n *fakeNode, entered <-chan struct{}, release func()) {
	t.Helper()
	in, out := make(chan struct{}), make(chan struct{})
	var enter, leave sync.Once
	n = &fakeNode{start: func() error {
		enter.Do(func() { close(in) })
		<-out
		return nil
	}}
	release = func() { leave.Do(func() { close(out) }) }
	return n, in, release
}

// waitFor waits for ch to be closed, and fails the case if it is not within
// five seconds, as the package's other waits on a start do.
func waitFor(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s was not reached within 5s", what)
	}
}

// newAllowingServer is a server whose config allows the uploads, its state
// dir a temp dir of the case's, and logger the one given.
func newAllowingServer(t *testing.T, logs *bytes.Buffer) *Server {
	t.Helper()
	s, err := NewServer(Config{Logger: captureLogger(logs), StateDir: filepath.Join(t.TempDir(), "tailscale"),
		UploadDiagnosticLogs: true})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestAStopThatGivesUpOnAStartThatAllowedTheUploadsTurnsThemOff: the wrapper's
// call to upstream's Start is held unreturned, as a start stalled after it
// built the node's log transport is, when a stop gives up waiting for it.
// AbandonStart turns logtail's switch once, sets NoLogsEnv true and says so in
// one line; every node in the process whose config allows the uploads, one
// built before the stalled one and running, the stalled one and one built
// after it, then reports them off, the abandonment among the reasons, and says
// so in the abandonment's own words, in its status and in a tailnet's refusal,
// with the launch environment's reason first where it held a true, and this
// process's latch first where a node without the setting came before; the
// later node's NewServer sets the variable again over a false written since.
// The held start, let go once its server is closed, publishes nothing and
// closes its node after its start.
func TestAStopThatGivesUpOnAStartThatAllowedTheUploadsTurnsThemOff(t *testing.T) {
	for _, c := range []struct {
		name       string
		inherited  string
		latchFirst bool   // a node without the setting is built first
		between    string // what something in the process writes to the variable after the stop; "" nothing
		held       held   // what the later node's refusal and status say first
	}{
		{name: "inherited unset", inherited: unset, held: heldByAbandon},
		{name: "inherited false", inherited: "false", held: heldByAbandon},
		{name: "inherited true", inherited: "true", held: heldByEnv},
		{name: "a false written in the process after the stop", inherited: unset, between: "false", held: heldByAbandon},
		{name: "after a node built without the setting", inherited: unset, latchFirst: true, held: heldByLatch},
	} {
		t.Run(c.name, func(t *testing.T) {
			setInherited(t, c.inherited)
			sw := standInForLogtail(t)
			if c.latchFirst {
				if _, err := NewServer(Config{Logger: silentLogger(), StateDir: filepath.Join(t.TempDir(), "tailscale")}); err != nil {
					t.Fatalf("NewServer without the setting: %v", err)
				}
			}
			var earlierLogs, logs bytes.Buffer
			earlier := newAllowingServer(t, &earlierLogs)
			earlier.newNode = func(*tsnet.Server) node { return &fakeNode{} }
			if err := earlier.Start(context.Background()); err != nil {
				t.Fatalf("the earlier node's start: %v", err)
			}
			s := newAllowingServer(t, &logs)
			n, entered, release := holdUpstreamStart(t)
			s.newNode = func(*tsnet.Server) node { return n }
			started := startInBackground(t, s)
			t.Cleanup(release) // registered after startInBackground's wait, so it runs before it
			waitFor(t, entered, "the upstream start")

			s.AbandonStart()
			if got := sw.times(); got != 1 {
				t.Errorf("a stop that gave up on a start which allowed the uploads turned logtail's switch %d time(s), "+
					"want once", got)
			}
			if !readsOff(t, envNow()) {
				t.Errorf("after the stop %s=%q reads the uploads on, want it set true for the process", NoLogsEnv, envNow())
			}
			want := Uploads{Allowed: true, EnvOff: c.inherited == "true", LatchedOff: c.latchFirst, AbandonedOff: true}
			for who, srv := range map[string]*Server{"the stalled node": s, "the node built before it": earlier} {
				if got := srv.Uploads(); got != want {
					t.Errorf("%s's status is %+v, want %+v", who, got, want)
				}
			}
			line := logs.String()
			for _, w := range []string{"the bridge stopped", "off for the rest of its life", abandonedUploads} {
				if !strings.Contains(line, w) {
					t.Errorf("the stalled node's line does not say %q: %s", w, line)
				}
			}

			if c.between != "" {
				t.Setenv(NoLogsEnv, c.between)
			}
			later, err := NewServer(Config{Logger: silentLogger(), StateDir: filepath.Join(t.TempDir(), "tailscale"),
				UploadDiagnosticLogs: true})
			if err != nil {
				t.Fatalf("the later node's NewServer: %v", err)
			}
			if !readsOff(t, envNow()) {
				t.Errorf("right after the later node's NewServer %s=%q reads the uploads on, want them off", NoLogsEnv, envNow())
			}
			if got := later.Uploads(); got != want {
				t.Errorf("the later node's status is %+v, want %+v", got, want)
			}
			later.newNode = func(*tsnet.Server) node {
				return &fakeNode{up: func(context.Context) (*ipnstate.Status, error) {
					return nil, fmt.Errorf("tsnet.Up: backend: %s", upstreamRefusal)
				}}
			}
			err = later.Start(context.Background())
			_ = later.Close()
			var refused *LogsRequiredError
			if !errors.As(err, &refused) {
				t.Fatalf("the later node's start = %v, want the tailnet's refusal in the bridge's words", err)
			}
			mustSayWhatHoldsTheUploadsOff(t, later.Uploads().HeldOff(), refused.Error(), c.held, c.inherited == "true")
			if c.held != heldByAbandon {
				mustSayWhatHoldsTheUploadsOff(t, Uploads{Allowed: true, AbandonedOff: true}.HeldOff(),
					(&LogsRequiredError{Uploads: Uploads{Allowed: true, AbandonedOff: true}}).Error(), heldByAbandon, false)
			}

			if err := s.Close(); err != nil {
				t.Errorf("Close = %v, want nil", err)
			}
			release()
			select {
			case err := <-started:
				if err == nil {
					t.Error("the start the stop gave up on reported its node up once let go")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the start the stop gave up on did not return within 5s of its release")
			}
			n.mustHaveBeenClosedOnceAfterItsStart(t)
			mustNotBeStarted(t, s)
			if got := sw.times(); got != 1 {
				t.Errorf("logtail's switch was turned %d time(s) in all, want once", got)
			}
		})
	}
}

// TestAStopThatFindsNoStalledStartWhichAllowedTheUploadsTurnsNothingOff: a stop
// whose wait ran out where the server's call to upstream's Start has returned
// (a start that completed, its node published, before a listener or an HTTP/3
// teardown held the stop's wait; one whose node is built and still coming up;
// one that failed and closed what it built), where no Start was made, or where
// the config never allowed the uploads (its start held all the same) turns
// logtail's switch no time, leaves the variable as it was and says nothing,
// and no node's status reports an abandonment.
func TestAStopThatFindsNoStalledStartWhichAllowedTheUploadsTurnsNothingOff(t *testing.T) {
	for _, c := range []struct {
		name  string
		allow bool
		// stage brings s to the state the stop finds it in; it returns once s is there.
		stage func(t *testing.T, s *Server)
	}{
		{"a start that completed", true, func(t *testing.T, s *Server) {
			s.newNode = func(*tsnet.Server) node { return &fakeNode{} }
			if err := s.Start(context.Background()); err != nil {
				t.Fatalf("Start: %v", err)
			}
		}},
		{"a start whose upstream start returned, still coming up", true, func(t *testing.T, s *Server) {
			up := make(chan struct{})
			s.newNode = func(*tsnet.Server) node {
				return &fakeNode{up: func(ctx context.Context) (*ipnstate.Status, error) {
					close(up)
					<-ctx.Done()
					return nil, ctx.Err()
				}}
			}
			startInBackground(t, s)
			waitFor(t, up, "the node's Up")
		}},
		{"a start that failed before its node came up", true, func(t *testing.T, s *Server) {
			s.newNode = func(*tsnet.Server) node {
				return &fakeNode{start: func() error { return errors.New("the test's failed start") }}
			}
			if err := s.Start(context.Background()); err == nil {
				t.Fatal("a start whose upstream start failed brought the node up")
			}
		}},
		{"no start made", true, func(*testing.T, *Server) {}},
		{"a start the config never allowed to upload, held", false, func(t *testing.T, s *Server) {
			n, entered, release := holdUpstreamStart(t)
			s.newNode = func(*tsnet.Server) node { return n }
			startInBackground(t, s)
			t.Cleanup(release)
			waitFor(t, entered, "the upstream start")
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			setInherited(t, unset)
			sw := standInForLogtail(t)
			var logs bytes.Buffer
			s, err := NewServer(Config{Logger: captureLogger(&logs), StateDir: filepath.Join(t.TempDir(), "tailscale"),
				UploadDiagnosticLogs: c.allow})
			if err != nil {
				t.Fatalf("NewServer: %v", err)
			}
			t.Cleanup(func() { _ = s.Close() })
			c.stage(t, s)
			before := envNow()

			s.AbandonStart()
			if got := sw.times(); got != 0 {
				t.Errorf("logtail's switch was turned %d time(s), want none", got)
			}
			if got := envNow(); got != before {
				t.Errorf("the stop changed %s from %q to %q", NoLogsEnv, before, got)
			}
			if got := s.Uploads(); got.AbandonedOff {
				t.Errorf("the node's status reports an abandonment: %+v", got)
			}
			if strings.Contains(logs.String(), abandonedUploads) {
				t.Errorf("the node said it turned the uploads off: %s", logs.String())
			}
			later, err := NewServer(Config{Logger: silentLogger(), StateDir: filepath.Join(t.TempDir(), "tailscale"),
				UploadDiagnosticLogs: true})
			if err != nil {
				t.Fatalf("the later node's NewServer: %v", err)
			}
			want := Uploads{Allowed: true, LatchedOff: !c.allow}
			if got := later.Uploads(); got != want {
				t.Errorf("a node built after the stop reports %+v, want %+v", got, want)
			}
		})
	}
}
