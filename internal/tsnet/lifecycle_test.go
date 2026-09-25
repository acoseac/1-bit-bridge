package tsnet

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"tailscale.com/client/local"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tsnet"
)

// Start and Close, when Close lands while Start is still bringing the node
// up. Upstream's Close "must not be called before or concurrently with
// Start", and the wrapper's Start runs the long part of upstream's start
// (interactive auth can hold it for minutes) outside its lock, so a Close in
// that window found nothing published and closed nothing. The start then
// published a node nothing would ever close. Each test drives the real Start
// and Close with a fake upstream node (Server.newNode).

// TestCloseStopsAStartInFlight: the node is waiting for interactive auth,
// and the caller's context never ends, when Close lands. Close must stop the
// start, and the start must close the node it was building, after upstream's
// start has returned rather than concurrently with it.
func TestCloseStopsAStartInFlight(t *testing.T) {
	up := make(chan struct{})
	n := &fakeNode{up: func(ctx context.Context) (*ipnstate.Status, error) {
		close(up)
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	s := newFakeNodeServer(t, n)
	started := startInBackground(t, s)
	<-up

	closed := make(chan error, 1)
	go func() { closed <- s.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Errorf("Close = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return while a start was in flight")
	}
	select {
	case err := <-started:
		if err == nil {
			t.Error("a start that Close stopped reported the node up")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the start was still running 5s after Close: nothing stopped it, " +
			"so the node it is building would be published with nothing left to close it")
	}
	n.mustHaveBeenClosedOnceAfterItsStart(t)
	mustNotBeStarted(t, s)
}

// TestCloseThatLandsAsTheNodeComesUpClosesIt: upstream's start has just
// succeeded when Close lands, before Start publishes the node. The node is
// closed, not published.
func TestCloseThatLandsAsTheNodeComesUpClosesIt(t *testing.T) {
	var s *Server
	n := &fakeNode{up: func(context.Context) (*ipnstate.Status, error) {
		if err := s.Close(); err != nil {
			t.Errorf("Close = %v, want nil", err)
		}
		return &ipnstate.Status{}, nil
	}}
	s = newFakeNodeServer(t, n)
	if err := s.Start(context.Background()); err == nil {
		t.Error("a start Close landed on reported the node up")
	}
	n.mustHaveBeenClosedOnceAfterItsStart(t)
	mustNotBeStarted(t, s)
}

// TestStartAfterCloseBuildsNoNode: nothing is built once Close has run.
func TestStartAfterCloseBuildsNoNode(t *testing.T) {
	n := &fakeNode{}
	s := newFakeNodeServer(t, n)
	if err := s.Close(); err != nil {
		t.Fatalf("Close = %v, want nil", err)
	}
	if err := s.Start(context.Background()); err == nil {
		t.Error("a start after Close reported the node up")
	}
	if b := n.built(); b != 0 {
		t.Errorf("a start after Close built %d node(s), want none", b)
	}
	mustNotBeStarted(t, s)
}

// TestStartOnACancelledContextBuildsNoNode: a caller that has already given
// up gets its cancellation back, and no node is built only to be closed.
func TestStartOnACancelledContextBuildsNoNode(t *testing.T) {
	n := &fakeNode{}
	s := newFakeNodeServer(t, n)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Start(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("Start on a cancelled context = %v, want the cancellation", err)
	}
	if b := n.built(); b != 0 {
		t.Errorf("a start on a cancelled context built %d node(s), want none", b)
	}
	mustNotBeStarted(t, s)
}

// TestStartThenCloseClosesTheNodeOnce is the twin: an ordinary start
// publishes the node, and Close closes it, once.
func TestStartThenCloseClosesTheNodeOnce(t *testing.T) {
	n := &fakeNode{}
	s := newFakeNodeServer(t, n)
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start = %v, want nil", err)
	}
	if got := s.MetricsState(); got != 2 {
		t.Errorf("MetricsState after Start = %d, want 2 (running)", got)
	}
	if err := s.Close(); err != nil {
		t.Errorf("Close = %v, want nil", err)
	}
	if err := s.Close(); err != nil {
		t.Errorf("a second Close = %v, want nil", err)
	}
	n.mustHaveBeenClosedOnceAfterItsStart(t)
	mustNotBeStarted(t, s)
}

// newFakeNodeServer is a Server whose upstream node is n. Whatever Start
// publishes is closed when the test ends.
func newFakeNodeServer(t *testing.T, n *fakeNode) *Server {
	t.Helper()
	s, err := NewServer(Config{Logger: silentLogger(), StateDir: filepath.Join(t.TempDir(), "tailscale")})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	s.newNode = func(*tsnet.Server) node {
		n.mu.Lock()
		n.builds++
		n.mu.Unlock()
		return n
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// startInBackground runs s.Start on its own goroutine, on a context nothing
// cancels until the test ends (t.Context, cancelled just before the
// cleanups run), and returns its result. The test waits for that goroutine
// before it ends.
func startInBackground(t *testing.T, s *Server) <-chan error {
	t.Helper()
	ctx := t.Context()
	result := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		result <- s.Start(ctx)
	}()
	t.Cleanup(func() {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("the start did not return on its context's cancel")
		}
	})
	return result
}

// mustNotBeStarted fails the test unless s reports no running node and
// refuses what needs one.
func mustNotBeStarted(t *testing.T, s *Server) {
	t.Helper()
	if got := s.MetricsState(); got != 0 {
		t.Errorf("MetricsState = %d, want 0 (down)", got)
	}
	if _, err := s.ListenTLS(":443"); err == nil {
		t.Error("ListenTLS succeeded on a server with no running node")
	}
}

// fakeNode is an upstream node whose Up runs the test's up func, and which
// records how it was closed.
type fakeNode struct {
	up func(context.Context) (*ipnstate.Status, error) // nil: Up answers at once, as the real one does for a node already up

	mu      sync.Mutex
	builds  int  // times newNode handed this node to a start
	running int  // Start or Up calls that have not returned
	closes  int  // Close calls
	overlap bool // Close ran while Start or Up was running
}

func (n *fakeNode) enter() {
	n.mu.Lock()
	n.running++
	n.mu.Unlock()
}

func (n *fakeNode) leave() {
	n.mu.Lock()
	n.running--
	n.mu.Unlock()
}

func (n *fakeNode) built() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.builds
}

func (n *fakeNode) Start() error {
	n.enter()
	defer n.leave()
	return nil
}

func (n *fakeNode) Up(ctx context.Context) (*ipnstate.Status, error) {
	n.enter()
	defer n.leave()
	if n.up != nil {
		return n.up(ctx)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &ipnstate.Status{}, nil
}

func (n *fakeNode) Close() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.closes++
	if n.running > 0 {
		n.overlap = true
	}
	return nil
}

func (n *fakeNode) ListenTLS(string, string) (net.Listener, error) {
	return nil, errors.New("fakeNode: no listener")
}

func (n *fakeNode) ListenPacket(string, string) (net.PacketConn, error) {
	return nil, errors.New("fakeNode: no packet listener")
}

func (n *fakeNode) LocalClient() (*local.Client, error) {
	return nil, errors.New("fakeNode: no local client")
}

func (n *fakeNode) CertDomains() []string { return nil }

// mustHaveBeenClosedOnceAfterItsStart fails the test unless the node was
// closed exactly once, and not while its start was still running.
func (n *fakeNode) mustHaveBeenClosedOnceAfterItsStart(t *testing.T) {
	t.Helper()
	n.mu.Lock()
	closes, overlap := n.closes, n.overlap
	n.mu.Unlock()
	if closes != 1 {
		t.Errorf("the node was closed %d time(s), want once", closes)
	}
	if overlap {
		t.Error("the node was closed while its start was still running, which upstream forbids")
	}
}
