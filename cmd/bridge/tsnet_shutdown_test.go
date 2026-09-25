package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
	"github.com/acoseac/1-bit-bridge/internal/metrics"
	"tailscale.com/ipn/ipnstate"
)

// serve's tailnet side runs on one goroutine: the embedded node's start (up
// to five minutes, interactive auth included), the status query and binds
// for HTTP/3, then the HTTPS listen and Serve. serve's teardown closes the
// node, and nothing joined that goroutine first, so the close could land
// while it was still starting the node, binding, or opening a listener, and
// on an error exit its context was still live, so nothing stopped it at all.
// These tests boot serve in tsnet mode with a fake node (serveOpts.tsnetNode)
// and hold that goroutine at each of those points across the exit.

const (
	msgAdminServer    = "admin server:"
	msgTsnetClose     = "tsnet close:"
	msgTsnetGaveUp    = "shutdown: the tsnet goroutine did not stop within grace"
	msgH3BindFailed   = "Failed to bind tsnet UDP socket for h3, continuing with remaining IPs"
	msgH3NoneBound    = "No tsnet HTTP/3 listeners bound on any tailnet IP, running HTTP/2 only on tailnet"
	msgH3Bound        = "tsnet HTTP/3 listeners bound"
	msgH3ServeFailed  = "h3 serve tsnet"
	errWrapperClosed  = "tsnet: ListenPacket called before Start"
	tsnetStateFileRel = "data/tailscale/tailscaled.state"
)

// TestServeStopsItsTsnetStartOnAnErrorExit: serve exits on an error (the
// admin console cannot bind) while the node is still waiting for
// interactive auth. serve's own context is live on that path, so only a
// cancel of serve's teardown reaches the start. It must stop the start and
// wait for it before closing the node, which upstream forbids closing
// "before or concurrently with Start".
func TestServeStopsItsTsnetStartOnAnErrorExit(t *testing.T) {
	node := newFakeTsnetNode()
	node.start = waitForContext
	cfgPath := writeTsnetConfig(t, takenAddress(t), true)
	stderr := &safeBuffer{}
	// serve prints its exit reason before its teardown; holding that print
	// until the start is running puts the start in flight at the exit.
	out := &holdingWriter{buf: stderr, holds: []*printHold{holdPrint(msgAdminServer, node.startEntered)}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		done <- runServe(ctx, serveOpts{configPath: cfgPath, addrOverride: "127.0.0.1:0", tsnetNode: node},
			&safeBuffer{}, out)
	}()
	drainServeOnCleanup(t, cancel, exited, done, stderr)

	if code := waitServeExit(t, exited, done, stderr); code != 1 {
		t.Errorf("serve exit code = %d, want 1 (the admin console could not bind); stderr=%s", code, stderr.String())
	}
	if s := stderr.String(); !strings.Contains(s, msgAdminServer) {
		t.Fatalf("serve did not exit on the admin console's bind error; stderr=%s", s)
	}
	select {
	case <-node.startReturned:
	default:
		t.Error("runServe returned with its tsnet start still running: nothing cancelled it, " +
			"so the node it is bringing up would come up after serve had gone")
	}
	node.mustHaveBeenClosedOnceAfterUse(t)
}

// TestServeWaitsForItsTsnetStartBeforeClosingTheNode pins the join on the
// shutdown path. The start sees the cancel and returns only once the part
// of a real start that takes no context has run (upstream builds the node
// before it can wait on anything, and the wrapper closes a half-built one
// after), writing the state dir as it goes. runServe must neither close the
// node nor return before that.
func TestServeWaitsForItsTsnetStartBeforeClosingTheNode(t *testing.T) {
	node := newFakeTsnetNode()
	tail := newGate()
	cfgPath := writeTsnetConfig(t, "127.0.0.1:0", true)
	state := filepath.Join(filepath.Dir(cfgPath), filepath.FromSlash(tsnetStateFileRel))
	node.start = func(ctx context.Context) error {
		<-ctx.Done()
		tail.wait()
		if err := os.MkdirAll(filepath.Dir(state), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(state, []byte("written as the start unwound\n"), 0o600); err != nil {
			return err
		}
		return ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	stderr := &safeBuffer{}
	done := make(chan int, 1)
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		done <- runServe(ctx, serveOpts{configPath: cfgPath, addrOverride: "127.0.0.1:0", tsnetNode: node},
			&safeBuffer{}, stderr)
	}()
	drainServeOnCleanup(t, cancel, exited, done, stderr)
	// Registered after the drain, so it runs before it: a failing run lets
	// the start go first, and the drain then finds a serve that can finish.
	t.Cleanup(tail.open)

	waitForServe(t, node.startEntered, "the tsnet start", exited, done, stderr)
	cancel()
	select {
	case <-exited:
		t.Fatalf("runServe returned while its tsnet start was still running. Nothing waited "+
			"for it, so the node closed under a start that goes on writing its state dir "+
			"after serve has returned. stderr=%s", stderr.String())
	case <-time.After(time.Second):
	}
	tail.open()
	if code := waitServeExit(t, exited, done, stderr); code != 0 {
		t.Errorf("serve exit code = %d, want 0; stderr=%s", code, stderr.String())
	}
	select {
	case <-node.startReturned:
	default:
		t.Error("runServe returned before the tsnet start it was waiting for had returned")
	}
	if _, err := os.Stat(state); err != nil {
		t.Errorf("the start's write had not landed when serve returned: %v", err)
	}
	node.mustHaveBeenClosedOnceAfterUse(t)
	if s := stderr.String(); strings.Contains(s, msgTsnetGaveUp) {
		t.Errorf("the start returned inside the grace, yet shutdown reported giving up on it; stderr=%s", s)
	}
}

// TestServeGivesUpOnAWedgedTsnetStartAfterTheGrace pins the other half of
// the join: it is BOUNDED. A start that never returns costs shutdown the
// grace and a line, never a hung exit, and the node is closed anyway (the
// wrapper stops a start that Close lands on).
func TestServeGivesUpOnAWedgedTsnetStartAfterTheGrace(t *testing.T) {
	node := newFakeTsnetNode()
	tail := newGate()
	node.start = func(ctx context.Context) error {
		<-ctx.Done()
		tail.wait()
		return ctx.Err()
	}
	cfgPath := writeTsnetConfig(t, "127.0.0.1:0", true)
	ctx, cancel := context.WithCancel(context.Background())
	stderr := &safeBuffer{}
	done := make(chan int, 1)
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		done <- runServe(ctx, serveOpts{configPath: cfgPath, addrOverride: "127.0.0.1:0", tsnetNode: node},
			&safeBuffer{}, stderr)
	}()
	drainServeOnCleanup(t, cancel, exited, done, stderr)
	// Runs before the drain: let the abandoned start return, and wait for
	// it, so nothing it does overlaps the removal of the data dir.
	t.Cleanup(func() {
		tail.open()
		select {
		case <-node.startReturned:
		case <-time.After(5 * time.Second):
			t.Error("the released start did not return")
		}
	})

	waitForServe(t, node.startEntered, "the tsnet start", exited, done, stderr)
	cancelled := time.Now()
	cancel()
	if code := waitServeExit(t, exited, done, stderr); code != 0 {
		t.Errorf("serve exit code = %d, want 0; stderr=%s", code, stderr.String())
	}
	if took := time.Since(cancelled); took < shutdownGrace {
		t.Errorf("runServe returned %v after the cancel, inside the %v grace, with the tsnet "+
			"start still running: it did not wait for it at all", took, shutdownGrace)
	}
	if s := stderr.String(); !strings.Contains(s, msgTsnetGaveUp) {
		t.Errorf("shutdown abandoned a running tsnet start without saying so; stderr=%s", s)
	}
	if n := node.closeCount(); n != 1 {
		t.Errorf("the node was closed %d time(s), want once: a wedged start must not keep it open", n)
	}
}

// TestServeClosesATailnetListenerThatOpensAfterAnErrorExit: serve exits on
// an error while the goroutine is in ListenTLS, which takes no context, and
// the listen succeeds just after serve has closed the node. Upstream's
// Close closes the listeners registered so far and does not refuse a later
// one (CodeRabbit, #1005). serve's context is still live then: runServe's
// own cancel is its first defer, so it runs last, after the node is closed.
// The listener must be closed, never published and served.
func TestServeClosesATailnetListenerThatOpensAfterAnErrorExit(t *testing.T) {
	t.Cleanup(func() { metrics.RegisterTsnetProvider(nil) })
	node := newFakeTsnetNode()
	node.closeErr = errors.New("fakeTsnetNode: close failed")
	hold := newGate()
	node.listen = func() (net.Listener, error) {
		hold.wait()
		return net.Listen("tcp", "127.0.0.1:0")
	}
	cfgPath := writeTsnetConfig(t, takenAddress(t), false)
	stderr := &safeBuffer{}
	// serve prints its exit reason before its teardown, and, since the
	// fake's Close fails, `tsnet close:` right after closing the node.
	// Holding the first until the listen is running puts the listen in
	// flight at the exit; holding the second holds serve between closing
	// the node and returning, which is when the listen lands.
	afterClose := newGate()
	exitHold := holdPrint(msgAdminServer, node.listenEntered)
	closeHold := holdPrint(msgTsnetClose, afterClose.ch)
	out := &holdingWriter{buf: stderr, holds: []*printHold{exitHold, closeHold}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		done <- runServe(ctx, serveOpts{configPath: cfgPath, addrOverride: "127.0.0.1:0", tsnetNode: node},
			&safeBuffer{}, out)
	}()
	drainServeOnCleanup(t, cancel, exited, done, stderr)
	t.Cleanup(hold.open)
	t.Cleanup(afterClose.open)
	// Ends a Serve a failing run left on the listener.
	t.Cleanup(node.closeHandedOut)

	waitForServe(t, node.listenEntered, "the tailnet listen", exited, done, stderr)
	// Serve has closed the node now, or it is waiting for the listen.
	select {
	case <-closeHold.held:
	case <-time.After(time.Second):
	}
	hold.open()
	select {
	case <-node.listenReturned:
	case <-time.After(5 * time.Second):
		t.Fatal("the released listen did not return")
	}
	lis := node.listener()
	if lis == nil {
		t.Fatal("precondition: the held listen opened no listener")
	}
	// Whatever serve does with the listener, it does now: it closes it,
	// or it serves it.
	select {
	case <-lis.closed:
	case <-lis.accepted:
	case <-time.After(5 * time.Second):
	}
	afterClose.open()
	if code := waitServeExit(t, exited, done, stderr); code != 1 {
		t.Errorf("serve exit code = %d, want 1 (the admin console could not bind); stderr=%s", code, stderr.String())
	}
	select {
	case <-lis.closed:
	default:
		t.Error("runServe returned with the tailnet listener that opened after it closed the node still open")
	}
	if n := lis.accepts.Load(); n != 0 {
		t.Errorf("the tailnet listener that opened after serve closed the node was served (%d Accept calls)", n)
	}
	node.mustHaveBeenClosedOnceAfterUse(t)
}

// TestServeReportsNoTailnetHTTP3BindTheShutdownCutShort: the shutdown lands
// while the goroutine is binding HTTP/3 on the node's second tailnet
// address, after the first bind succeeded. That bind then fails the way
// the wrapper answers once the node has closed. Nothing may be reported,
// nothing bound after the shutdown began, and nothing served on the first
// address: a server serving there as the node closes under it reports
// `h3 serve tsnet` with the transport's closed error.
func TestServeReportsNoTailnetHTTP3BindTheShutdownCutShort(t *testing.T) {
	t.Cleanup(func() { metrics.RegisterTsnetProvider(nil) })
	rec := loggingtest.Record(t)
	node := newFakeTsnetNode()
	node.ips = twoTailnetAddrs()
	second := make(chan struct{})
	hold := newGate()
	node.bind = func(n int) (net.PacketConn, error) {
		if n == 1 {
			return net.ListenPacket("udp", "127.0.0.1:0")
		}
		if n == 2 {
			close(second)
			hold.wait()
		}
		return nil, errors.New(errWrapperClosed)
	}
	cfgPath := writeTsnetConfig(t, "127.0.0.1:0", true)
	ctx, cancel := context.WithCancel(context.Background())
	stderr := &safeBuffer{}
	done := make(chan int, 1)
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		done <- runServe(ctx, serveOpts{configPath: cfgPath, addrOverride: "127.0.0.1:0", tsnetNode: node},
			&safeBuffer{}, stderr)
	}()
	drainServeOnCleanup(t, cancel, exited, done, stderr)
	t.Cleanup(hold.open)
	t.Cleanup(node.closeHandedOut)

	waitForServe(t, second, "the second HTTP/3 bind", exited, done, stderr)
	cancel()
	select {
	case <-exited:
		t.Errorf("runServe returned while a tailnet HTTP/3 bind was still running; stderr=%s", stderr.String())
	case <-time.After(time.Second):
	}
	hold.open()
	if code := waitServeExit(t, exited, done, stderr); code != 0 {
		t.Errorf("serve exit code = %d, want 0; stderr=%s", code, stderr.String())
	}
	mustNotReportServe(t, rec, msgH3BindFailed, msgH3NoneBound, msgH3ServeFailed)
	if n := node.bindCount(); n != 2 {
		t.Errorf("the node took %d HTTP/3 bind(s), want 2: none after the shutdown began", n)
	}
	for i, c := range node.handedOutConns() {
		select {
		case <-c.closed:
		default:
			t.Errorf("HTTP/3 conn %d was still open when serve returned", i+1)
		}
	}
	node.mustHaveBeenClosedOnceAfterUse(t)
}

// TestServeStillReportsATailnetHTTP3BindThatFails is the twin: a bind that
// fails on a live context is reported once and the next address is bound
// and served, and an ordinary shutdown then stops that server without a
// word.
func TestServeStillReportsATailnetHTTP3BindThatFails(t *testing.T) {
	t.Cleanup(func() { metrics.RegisterTsnetProvider(nil) })
	rec := loggingtest.Record(t)
	node := newFakeTsnetNode()
	node.ips = twoTailnetAddrs()
	node.bind = func(n int) (net.PacketConn, error) {
		if n == 1 {
			return nil, errors.New("listen udp 100.64.0.1:0: address already in use")
		}
		return net.ListenPacket("udp", "127.0.0.1:0")
	}
	node.listen = func() (net.Listener, error) { return net.Listen("tcp", "127.0.0.1:0") }
	cfgPath := writeTsnetConfig(t, "127.0.0.1:0", true)
	ctx, cancel := context.WithCancel(context.Background())
	stderr := &safeBuffer{}
	done := make(chan int, 1)
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		done <- runServe(ctx, serveOpts{configPath: cfgPath, addrOverride: "127.0.0.1:0", tsnetNode: node},
			&safeBuffer{}, stderr)
	}()
	drainServeOnCleanup(t, cancel, exited, done, stderr)
	t.Cleanup(node.closeHandedOut)

	waitForServe(t, node.listenEntered, "the tailnet listen", exited, done, stderr)
	cancel()
	if code := waitServeExit(t, exited, done, stderr); code != 0 {
		t.Errorf("serve exit code = %d, want 0; stderr=%s", code, stderr.String())
	}
	mustReportOnceServe(t, rec, msgH3BindFailed)
	mustNotReportServe(t, rec, msgH3NoneBound, msgH3ServeFailed)
	if got := rec.Lines(msgH3Bound); len(got) != 1 || !strings.Contains(got[0], "count=1") {
		t.Errorf("want one HTTP/3 listener bound and served, got:\n%s", strings.Join(got, "\n"))
	}
	if s := stderr.String(); strings.Contains(s, msgTsnetGaveUp) {
		t.Errorf("an ordinary shutdown gave up on the tailnet goroutine; stderr=%s", s)
	}
	node.mustHaveBeenClosedOnceAfterUse(t)
}

// fakeTsnetNode is the embedded tailnet node for serve's boot tests
// (serveOpts.tsnetNode). A test holds its Start, its ListenTLS or one of its
// ListenPacket binds open across serve's exit. It records the calls that
// were running when Close ran and those that reached it after, and its
// Close closes every listener and conn it handed out, as upstream's does.
type fakeTsnetNode struct {
	start    func(context.Context) error         // nil: the node comes up at once
	listen   func() (net.Listener, error)        // nil: ListenTLS fails
	bind     func(n int) (net.PacketConn, error) // n counts ListenPacket calls from 1; nil: every bind fails
	ips      []netip.Addr                        // the tailnet addresses Status reports
	closeErr error                               // what Close returns

	startEntered   chan struct{}
	startReturned  chan struct{}
	listenEntered  chan struct{}
	listenReturned chan struct{}
	startOnce      sync.Once
	returnOnce     sync.Once
	listenOnce     sync.Once
	listenDoneOnce sync.Once

	mu       sync.Mutex
	running  map[string]int // calls in flight, by method
	binds    int
	closes   int
	overlaps []string // methods running when Close ran
	late     []string // methods called after Close
	lis      *watchedListener
	conns    []*watchedConn
}

func newFakeTsnetNode() *fakeTsnetNode {
	return &fakeTsnetNode{
		startEntered:   make(chan struct{}),
		startReturned:  make(chan struct{}),
		listenEntered:  make(chan struct{}),
		listenReturned: make(chan struct{}),
		running:        map[string]int{},
	}
}

// enter records a call to method; the returned func records its return.
func (n *fakeTsnetNode) enter(method string) func() {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closes > 0 {
		n.late = append(n.late, method)
	}
	n.running[method]++
	return func() {
		n.mu.Lock()
		defer n.mu.Unlock()
		n.running[method]--
	}
}

func (n *fakeTsnetNode) Start(ctx context.Context) error {
	defer n.returnOnce.Do(func() { close(n.startReturned) })
	defer n.enter("Start")()
	n.startOnce.Do(func() { close(n.startEntered) })
	if n.start == nil {
		return ctx.Err()
	}
	return n.start(ctx)
}

func (n *fakeTsnetNode) Close() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.closes++
	for method, running := range n.running {
		if running > 0 {
			n.overlaps = append(n.overlaps, method)
		}
	}
	if n.lis != nil {
		_ = n.lis.Close()
	}
	for _, c := range n.conns {
		_ = c.Close()
	}
	return n.closeErr
}

func (n *fakeTsnetNode) Status(context.Context) (*ipnstate.Status, error) {
	return &ipnstate.Status{Self: &ipnstate.PeerStatus{TailscaleIPs: n.ips}}, nil
}

func (n *fakeTsnetNode) CertDomains() []string { return nil }

func (n *fakeTsnetNode) ListenTLS(string) (net.Listener, error) {
	defer n.listenDoneOnce.Do(func() { close(n.listenReturned) })
	defer n.enter("ListenTLS")()
	n.listenOnce.Do(func() { close(n.listenEntered) })
	if n.listen == nil {
		return nil, errors.New("fakeTsnetNode: no listener")
	}
	lis, err := n.listen()
	if err != nil {
		return nil, err
	}
	w := &watchedListener{Listener: lis, closed: make(chan struct{}), accepted: make(chan struct{})}
	n.mu.Lock()
	n.lis = w
	n.mu.Unlock()
	return w, nil
}

func (n *fakeTsnetNode) ListenPacket(string, string) (net.PacketConn, error) {
	defer n.enter("ListenPacket")()
	n.mu.Lock()
	n.binds++
	call := n.binds
	n.mu.Unlock()
	if n.bind == nil {
		return nil, errors.New("fakeTsnetNode: no packet listener")
	}
	pc, err := n.bind(call)
	if err != nil {
		return nil, err
	}
	w := &watchedConn{PacketConn: pc, closed: make(chan struct{})}
	n.mu.Lock()
	n.conns = append(n.conns, w)
	n.mu.Unlock()
	return w, nil
}

func (n *fakeTsnetNode) HTTP3TLSConfig() *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS13}
}

func (n *fakeTsnetNode) MetricsState() int                        { return 0 }
func (n *fakeTsnetNode) MetricsPeersOnline() int                  { return 0 }
func (n *fakeTsnetNode) MetricsDERPLatencies() map[string]float64 { return map[string]float64{} }

func (n *fakeTsnetNode) closeCount() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.closes
}

func (n *fakeTsnetNode) bindCount() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.binds
}

func (n *fakeTsnetNode) listener() *watchedListener {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.lis
}

func (n *fakeTsnetNode) handedOutConns() []*watchedConn {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]*watchedConn(nil), n.conns...)
}

// closeHandedOut closes what the node handed out, for a test's cleanup.
func (n *fakeTsnetNode) closeHandedOut() {
	if l := n.listener(); l != nil {
		_ = l.Close()
	}
	for _, c := range n.handedOutConns() {
		_ = c.Close()
	}
}

// mustHaveBeenClosedOnceAfterUse fails the test unless serve closed the
// node exactly once, with no Start, ListenTLS or ListenPacket still running
// and none arriving after.
func (n *fakeTsnetNode) mustHaveBeenClosedOnceAfterUse(t *testing.T) {
	t.Helper()
	n.mu.Lock()
	closes, overlaps, late := n.closes, n.overlaps, n.late
	n.mu.Unlock()
	if closes != 1 {
		t.Errorf("serve closed the tsnet node %d time(s), want once", closes)
	}
	if len(overlaps) > 0 {
		t.Errorf("serve closed the tsnet node while %v was still running on it", overlaps)
	}
	if len(late) > 0 {
		t.Errorf("%v reached the tsnet node after serve had closed it", late)
	}
}

// watchedListener is a listener the fake node handed out. It counts Accept
// calls, which is how a test sees that something served it.
type watchedListener struct {
	net.Listener
	accepts    atomic.Int32
	accepted   chan struct{} // closed at the first Accept
	closed     chan struct{}
	acceptOnce sync.Once
	closeOnce  sync.Once
}

func (l *watchedListener) Accept() (net.Conn, error) {
	l.accepts.Add(1)
	l.acceptOnce.Do(func() { close(l.accepted) })
	return l.Listener.Accept()
}

func (l *watchedListener) Close() error {
	l.closeOnce.Do(func() { close(l.closed) })
	return l.Listener.Close()
}

// watchedConn is a packet conn the fake node handed out.
type watchedConn struct {
	net.PacketConn
	closed chan struct{}
	once   sync.Once
}

func (c *watchedConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return c.PacketConn.Close()
}

// gate holds whatever waits on it until it is opened. open is idempotent.
type gate struct {
	ch   chan struct{}
	once sync.Once
}

func newGate() *gate { return &gate{ch: make(chan struct{})} }

func (g *gate) wait() { <-g.ch }

func (g *gate) open() { g.once.Do(func() { close(g.ch) }) }

// waitForContext is a node start that waits for interactive auth, which
// only its context ends.
func waitForContext(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

// holdingWriter is serve's stderr for a test that needs serve to stop at a
// moment of its choosing: serve prints its exit reason before its teardown,
// and `tsnet close:` inside it, and each hold keeps the first print that
// contains its marker waiting until it is released. Everything is kept in
// buf.
type holdingWriter struct {
	buf   *safeBuffer
	holds []*printHold
}

func (w *holdingWriter) Write(p []byte) (int, error) {
	for _, h := range w.holds {
		if bytes.Contains(p, []byte(h.marker)) {
			h.hold()
		}
	}
	return w.buf.Write(p)
}

// printHold is one of a holdingWriter's holds: the first print containing
// marker waits until `until` is closed, and held is closed while it waits.
type printHold struct {
	marker string
	until  <-chan struct{}
	held   chan struct{}
	once   sync.Once
}

func holdPrint(marker string, until <-chan struct{}) *printHold {
	return &printHold{marker: marker, until: until, held: make(chan struct{})}
}

func (h *printHold) hold() {
	first := false
	h.once.Do(func() { first = true })
	if !first {
		return
	}
	close(h.held)
	select {
	case <-h.until:
	case <-time.After(30 * time.Second): // a run that never gets there still ends
	}
}

// waitForServe blocks until ch is closed. A serve that exits first is
// reported with its exit code, not as a timeout.
func waitForServe(t *testing.T, ch <-chan struct{}, what string, exited <-chan struct{}, done <-chan int, stderr *safeBuffer) {
	t.Helper()
	select {
	case <-ch:
	case <-exited:
		t.Fatalf("serve exited with code %d before it reached %s; stderr=%s", <-done, what, stderr.String())
	case <-time.After(30 * time.Second):
		t.Fatalf("serve never reached %s within 30s; stderr=%s", what, stderr.String())
	}
}

// waitServeExit waits for runServe to return and gives its exit code.
func waitServeExit(t *testing.T, exited <-chan struct{}, done <-chan int, stderr *safeBuffer) int {
	t.Helper()
	select {
	case <-exited:
	case <-time.After(shutdownGrace + 10*time.Second):
		t.Fatalf("runServe did not return; stderr=%s", stderr.String())
	}
	return <-done
}

// writeTsnetConfig drops a bridge.yaml for a serve in tsnet mode beside a
// real library root and returns its path. adminAddress is where the console
// binds; http3 false disables HTTP/3, LAN and tailnet both.
func writeTsnetConfig(t *testing.T, adminAddress string, http3 bool) string {
	t.Helper()
	dir := t.TempDir()
	lib := filepath.Join(dir, "Music")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "libraryRoots:\n  - " + lib + "\nadminAddress: " + adminAddress + "\ntailscale:\n  mode: tsnet\n"
	if !http3 {
		body += "disableHttp3: true\n"
	}
	cfgPath := filepath.Join(dir, "bridge.yaml")
	if err := os.WriteFile(cfgPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return cfgPath
}

// takenAddress is a loopback address something else is already listening
// on, for the test's lifetime: an admin console told to bind it cannot.
func takenAddress(t *testing.T) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lis.Close() })
	return lis.Addr().String()
}

// twoTailnetAddrs is a dual-stack node's pair of tailnet addresses.
func twoTailnetAddrs() []netip.Addr {
	return []netip.Addr{netip.MustParseAddr("100.64.0.1"), netip.MustParseAddr("fd7a:115c:a1e0::1")}
}
