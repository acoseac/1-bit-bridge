package admin

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/consolelan"
	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
	"github.com/acoseac/1-bit-bridge/internal/player/lan"
)

// The home-network socket on real sockets of the test's (backlog B989): a
// box whose reading the test sets, and a bind on 127.0.0.1 that stands for
// A:P, whose connections report the source the test's dial names, so the
// plan, the fence, the retirement and the rebind run as serve runs them, and
// no test binds the host's LAN addresses or reads its interfaces.

// socketWait bounds every wait of these tests for what the code does.
const socketWait = 10 * time.Second

// enTest is the box's interface that holds A in most cases.
func enTest(index int, name, prefix string) consolelan.Interface {
	return consolelan.Interface{Name: name, Index: index,
		Flags: net.FlagUp | net.FlagRunning | net.FlagBroadcast | net.FlagMulticast,
		Addrs: []netip.Prefix{netip.MustParsePrefix(prefix)}}
}

type lanBox struct {
	t *testing.T

	mu        sync.Mutex
	ifs       []consolelan.Interface
	readErr   error
	table     consolelan.Table
	holds     []int
	listeners []*standIn
	sources   map[int]netip.Addr
	sourced   chan struct{}
	stallNext bool
	conns     []*standInConn
	events    []string
	failNext  bool
}

func newLANBox(t *testing.T) *lanBox {
	return &lanBox{t: t, ifs: []consolelan.Interface{enTest(7, "en-test", testLANAddr+"/24")}, sources: map[int]netip.Addr{},
		sourced: make(chan struct{})}
}

func (b *lanBox) note(e string) {
	b.mu.Lock()
	b.events = append(b.events, e)
	b.mu.Unlock()
}

func (b *lanBox) set(ifs ...consolelan.Interface) {
	b.mu.Lock()
	b.ifs, b.readErr = ifs, nil
	b.mu.Unlock()
}

func (b *lanBox) fail(err error) {
	b.mu.Lock()
	b.readErr = err
	b.mu.Unlock()
}

func (b *lanBox) read() ([]consolelan.Interface, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.readErr != nil {
		return nil, b.readErr
	}
	return append([]consolelan.Interface(nil), b.ifs...), nil
}

func (b *lanBox) hostTable(bool) (consolelan.Table, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append(consolelan.Table(nil), b.table...), nil
}

// hold records the index the plan holds a socket to, and holds nothing.
func (b *lanBox) hold(index int, _ bool) func(string, string, syscall.RawConn) error {
	b.mu.Lock()
	b.holds = append(b.holds, index)
	b.mu.Unlock()
	return func(string, string, syscall.RawConn) error { return nil }
}

// listen binds 127.0.0.1:0 through lc, so the plan's hold runs on a real
// socket, and stands for address.
func (b *lanBox) listen(ctx context.Context, lc *net.ListenConfig, network, address string) (net.Listener, error) {
	real, err := lc.Listen(ctx, "tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	ap := netip.MustParseAddrPort(address)
	l := &standIn{box: b, real: real, lan: ap}
	b.mu.Lock()
	b.listeners = append(b.listeners, l)
	b.events = append(b.events, "bind")
	b.mu.Unlock()
	b.t.Cleanup(func() { _ = real.Close() })
	return l, nil
}

// current is the newest listener.
func (b *lanBox) current() *standIn {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.listeners) == 0 {
		return nil
	}
	return b.listeners[len(b.listeners)-1]
}

func (b *lanBox) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.listeners)
}

// dial connects to the newest listener from the source from.
func (b *lanBox) dial(from netip.Addr) (net.Conn, error) {
	l := b.current()
	if l == nil {
		return nil, errors.New("no listener")
	}
	c, err := net.Dial("tcp4", l.real.Addr().String())
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	b.sources[c.LocalAddr().(*net.TCPAddr).Port] = from
	close(b.sourced)
	b.sourced = make(chan struct{})
	b.mu.Unlock()
	return c, nil
}

// sourceOf is the source a dial from port stands for, waiting for the dial
// to record it: its connect can return after the listener accepts.
func (b *lanBox) sourceOf(port int) netip.Addr {
	deadline := time.NewTimer(socketWait)
	defer deadline.Stop()
	for {
		b.mu.Lock()
		src, ok := b.sources[port]
		wait := b.sourced
		b.mu.Unlock()
		if ok {
			return src
		}
		select {
		case <-wait:
		case <-deadline.C:
			return netip.Addr{}
		}
	}
}

type standIn struct {
	box  *lanBox
	real net.Listener
	lan  netip.AddrPort
}

func (l *standIn) Accept() (net.Conn, error) {
	c, err := l.real.Accept()
	if err != nil {
		return nil, err
	}
	l.box.mu.Lock()
	if l.box.failNext {
		l.box.failNext = false
		l.box.mu.Unlock()
		_ = c.Close()
		return nil, errors.New("the stand-in's accept failed")
	}
	port := c.RemoteAddr().(*net.TCPAddr).Port
	l.box.mu.Unlock()
	src := l.box.sourceOf(port)
	l.box.mu.Lock()
	sc := &standInConn{Conn: c, local: net.TCPAddrFromAddrPort(l.lan),
		remote: net.TCPAddrFromAddrPort(netip.AddrPortFrom(src, uint16(port))), box: l.box,
		stall: l.box.stallNext, released: make(chan struct{})}
	l.box.stallNext = false
	l.box.conns = append(l.box.conns, sc)
	l.box.mu.Unlock()
	return sc, nil
}

func (l *standIn) Close() error   { return l.real.Close() }
func (l *standIn) Addr() net.Addr { return net.TCPAddrFromAddrPort(l.lan) }

// standInConn is an accepted connection with the addresses it stands for;
// one made to stall blocks every write after the first until it is closed,
// as a write blocks on a full socket buffer, which a cancellation does not
// end and a close does.
type standInConn struct {
	net.Conn
	local, remote net.Addr
	box           *lanBox

	mu       sync.Mutex
	stall    bool
	writes   int
	closed   bool
	released chan struct{}
	blocked  chan struct{}
}

func (c *standInConn) LocalAddr() net.Addr  { return c.local }
func (c *standInConn) RemoteAddr() net.Addr { return c.remote }

func (c *standInConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	c.writes++
	stall := c.stall && c.writes > 1
	if stall && c.blocked != nil {
		close(c.blocked)
		c.blocked = nil
	}
	c.mu.Unlock()
	if stall {
		<-c.released
		return 0, net.ErrClosed
	}
	return c.Conn.Write(p)
}

func (c *standInConn) Close() error {
	c.mu.Lock()
	if !c.closed {
		c.closed = true
		close(c.released)
		c.box.note("conn closed")
	}
	c.mu.Unlock()
	return c.Conn.Close()
}

// lanRig is a console serving loopback and the home-network socket its plan
// binds on the box, its loop driven by the test's ticks.
type lanRig struct {
	t      *testing.T
	srv    *Server
	box    *lanBox
	tick   chan time.Time
	ticked chan struct{}
	seams  *ConsoleLANSeams
	cancel context.CancelFunc
	served chan error
	loop   net.Listener

	stopOnce sync.Once
	stopped  bool
}

// newLANRig opens the plan and starts the console; seams, when set, are
// the test's own, to which the rig adds its Ticked. It requires the plan
// open, the precondition of every case that drives the socket.
func newLANRig(t *testing.T, box *lanBox, seams *ConsoleLANSeams, edit func(*ConsoleLANPlan)) *lanRig {
	t.Helper()
	return openLANRig(t, box, seams, edit, true)
}

// newLANRigAnyPlan is newLANRig for a case that asks only the state the
// console reports and what listens, which it can ask whether or not a plan
// opened.
func newLANRigAnyPlan(t *testing.T, box *lanBox, edit func(*ConsoleLANPlan)) *lanRig {
	t.Helper()
	return openLANRig(t, box, nil, edit, false)
}

func openLANRig(t *testing.T, box *lanBox, seams *ConsoleLANSeams, edit func(*ConsoleLANPlan), needPlan bool) *lanRig {
	t.Helper()
	srv, _, _ := newTestServer(t)
	if seams == nil {
		seams = &ConsoleLANSeams{}
	}
	r := &lanRig{t: t, srv: srv, box: box, tick: make(chan time.Time), ticked: make(chan struct{}, 16), seams: seams}
	seams.Ticked = func() { r.ticked <- struct{}{} }
	plan := &ConsoleLANPlan{
		Address: netip.MustParseAddr(testLANAddr), Port: 7789, Source: consolelan.SourceFile,
		FileKey: config.LANKey{State: config.LANKeyAddress, Address: testLANAddr},
		Read:    box.read, HostTable: box.hostTable, Hold: box.hold, Listen: box.listen,
		GOOS: "linux", Tick: r.tick, RetireWait: 2 * time.Second, ShutdownWindow: 2 * time.Second, Seams: seams,
	}
	if edit != nil {
		edit(plan)
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	srv.OpenConsoleLAN(ctx, plan)
	if needPlan && srv.consoleLAN == nil {
		cancel()
		t.Fatal("precondition: the console opened no home-network plan")
	}
	loop, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	r.loop = loop
	r.served = make(chan error, 1)
	go func() { r.served <- srv.ServeListener(ctx, loop) }()
	t.Cleanup(r.stop)
	return r
}

// stop shuts the console down and waits for it, once.
func (r *lanRig) stop() {
	r.stopOnce.Do(func() {
		r.cancel()
		select {
		case <-r.served:
			r.stopped = true
		case <-time.After(socketWait):
			r.t.Errorf("the console did not stop within %v", socketWait)
		}
	})
}

// step sends one tick and waits for the loop to have done it.
func (r *lanRig) step() {
	r.t.Helper()
	select {
	case r.tick <- time.Now():
	case <-time.After(socketWait):
		r.t.Fatal("the loop took no tick")
	}
	select {
	case <-r.ticked:
	case <-time.After(socketWait):
		r.t.Fatal("the loop did not finish its tick")
	}
}

// client is an HTTP/1.1 client to the newest LAN listener from from,
// keeping its connection alive between requests.
type lanClient struct {
	t    *testing.T
	conn net.Conn
	rd   *bufio.Reader
}

func (r *lanRig) client(from string) *lanClient {
	r.t.Helper()
	c, err := r.box.dial(netip.MustParseAddr(from))
	if err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(func() { _ = c.Close() })
	return &lanClient{t: r.t, conn: c, rd: bufio.NewReader(c)}
}

// get sends a GET on the kept-alive connection and reads its answer.
func (c *lanClient) get(path string, header ...string) (*http.Response, string, error) {
	req := "GET " + path + " HTTP/1.1\r\nHost: " + testLANHost + "\r\n"
	for _, h := range header {
		req += h + "\r\n"
	}
	req += "\r\n"
	_ = c.conn.SetDeadline(time.Now().Add(socketWait))
	if _, err := io.WriteString(c.conn, req); err != nil {
		return nil, "", err
	}
	resp, err := http.ReadResponse(c.rd, nil)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	return resp, string(body), err
}

func (r *lanRig) state() ConsoleLANState { return r.srv.ConsoleLAN() }

func (r *lanRig) inflight(p *lanPolicy) int {
	c := r.srv.consoleLAN
	c.mu.Lock()
	defer c.mu.Unlock()
	return p.inflight
}

func (r *lanRig) policy() *lanPolicy {
	c := r.srv.consoleLAN
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.current
}

// TestAHoldTheKernelRefusesLeavesTheLANSocketClosed: a hold the kernel
// refuses (EPERM, ENOPROTOOPT), an index of 0 and a read-back of another
// index each leave nothing listening, the state waiting with its reason,
// and the loopback socket serving (D11).
func TestAHoldTheKernelRefusesLeavesTheLANSocketClosed(t *testing.T) {
	for _, c := range []struct {
		name   string
		index  int
		set    error
		got    int
		reason string
	}{
		{"EPERM", 7, syscall.EPERM, 7, consolelan.ReasonRestrictionRefused},
		{"ENOPROTOOPT", 7, syscall.ENOPROTOOPT, 7, consolelan.ReasonRestrictionRefused},
		{"an index of 0", 0, nil, 0, consolelan.ReasonInterfaceUnknown},
		{"a read-back of another index", 7, nil, 3, consolelan.ReasonRestrictionUnconfirmed},
	} {
		t.Run(c.name, func(t *testing.T) {
			box := newLANBox(t)
			box.set(enTest(c.index, "en-test", testLANAddr+"/24"))
			r := newLANRigAnyPlan(t, box, func(p *ConsoleLANPlan) {
				p.Hold = consolelan.HoldUsing(consolelan.OptionCalls{
					Name: "TEST",
					Set:  func(uintptr, int) error { return c.set },
					Get:  func(uintptr) (int, error) { return c.got, nil },
				})
			})
			if st := r.state(); st.State != ConsoleLANWaiting || st.Reason != c.reason {
				t.Fatalf("state %+v, want waiting %s", st, c.reason)
			}
			if r.box.count() != 0 {
				t.Fatalf("%d listeners after a refused hold", r.box.count())
			}
			resp, err := http.Get("http://" + r.loop.Addr().String() + "/api/stats")
			if err != nil || resp.StatusCode != http.StatusOK {
				t.Fatalf("the loopback socket: %v %v", resp, err)
			}
			_ = resp.Body.Close()
		})
	}
}

// lanReadingOf is a plan's reading made by internal/player/lan's whole or
// failing reader over listing and addrs.
func lanReadingOf(list func() ([]net.Interface, error), addrs func(net.Interface) ([]net.Addr, error)) func() ([]consolelan.Interface, error) {
	return func() ([]consolelan.Interface, error) {
		ifs, err := lan.ReadInterfacesFrom(list, addrs)
		if err != nil {
			return nil, err
		}
		out := make([]consolelan.Interface, 0, len(ifs))
		for _, in := range ifs {
			out = append(out, consolelan.Interface{Name: in.Name, Index: in.Index, Flags: in.Flags, Addrs: in.Addrs})
		}
		return out, nil
	}
}

var (
	lanEth  = net.Interface{Index: 7, Name: "en-test", Flags: net.FlagUp | net.FlagRunning | net.FlagBroadcast | net.FlagMulticast}
	lanWlan = net.Interface{Index: 8, Name: "wl-test", Flags: net.FlagUp | net.FlagRunning | net.FlagBroadcast | net.FlagMulticast}
)

func cidrNet(cidr string) *net.IPNet {
	ip, n, err := net.ParseCIDR(cidr)
	if err != nil {
		panic(err)
	}
	n.IP = ip
	return n
}

// TestAReadingThatFailsBindsNothing: a reading that fails whole, and one
// whose one interface's addresses fail, bind nothing:
// interfaces_unreadable (D12).
func TestAReadingThatFailsBindsNothing(t *testing.T) {
	for _, c := range []struct {
		name string
		read func() ([]consolelan.Interface, error)
	}{
		{"the listing fails", lanReadingOf(func() ([]net.Interface, error) { return nil, errors.New("netlink") }, nil)},
		{"one interface's addresses fail", lanReadingOf(
			func() ([]net.Interface, error) { return []net.Interface{lanEth, lanWlan}, nil },
			func(in net.Interface) ([]net.Addr, error) {
				if in.Name == "wl-test" {
					return nil, errors.New("no such network interface")
				}
				return []net.Addr{cidrNet(testLANAddr + "/24")}, nil
			})},
	} {
		t.Run(c.name, func(t *testing.T) {
			box := newLANBox(t)
			r := newLANRigAnyPlan(t, box, func(p *ConsoleLANPlan) { p.Read = c.read })
			if st := r.state(); st.State != ConsoleLANWaiting || st.Reason != consolelan.ReasonInterfacesUnreadable {
				t.Fatalf("state %+v", st)
			}
			if r.box.count() != 0 {
				t.Fatalf("%d listeners over a reading that failed", r.box.count())
			}
		})
	}
}

// TestAReadingOfAWithNoNetworkBindsNothing: A kept as the address alone (no
// *net.IPNet, a mask that is no prefix, a mask of no ones) binds nothing:
// address_has_no_network (F8).
func TestAReadingOfAWithNoNetworkBindsNothing(t *testing.T) {
	for name, a := range map[string]net.Addr{
		"no IPNet":            &net.IPAddr{IP: net.ParseIP(testLANAddr)},
		"a mask that is none": &net.IPNet{IP: net.ParseIP(testLANAddr).To4(), Mask: net.IPv4Mask(255, 0, 255, 0)},
		"a mask of no ones":   &net.IPNet{IP: net.ParseIP(testLANAddr).To4(), Mask: net.IPv4Mask(0, 0, 0, 0)},
	} {
		t.Run(name, func(t *testing.T) {
			box := newLANBox(t)
			r := newLANRigAnyPlan(t, box, func(p *ConsoleLANPlan) {
				p.Read = lanReadingOf(func() ([]net.Interface, error) { return []net.Interface{lanEth}, nil },
					func(net.Interface) ([]net.Addr, error) { return []net.Addr{a}, nil })
			})
			if st := r.state(); st.State != ConsoleLANWaiting || st.Reason != consolelan.ReasonAddressNoNetwork {
				t.Fatalf("state %+v", st)
			}
			if r.box.count() != 0 {
				t.Fatal("bound with no network")
			}
		})
	}
}

// TestTheLoopbackGuardsKeepTheirAddressWhileTheLANSocketServes: the LAN
// socket's bind leaves the loopback socket's bound address, its Origin and
// its hint, as they were (R2).
func TestTheLoopbackGuardsKeepTheirAddressWhileTheLANSocketServes(t *testing.T) {
	r := newLANRig(t, newLANBox(t), nil, nil)
	if r.state().State != ConsoleLANListening {
		t.Fatalf("state %+v", r.state())
	}
	// The loopback socket's hint names its own bound address (boundAdminAddr),
	// never the LAN socket's: read through a request, which ServeListener's
	// write of it happens before.
	bad, _ := http.NewRequest(http.MethodPost, "http://"+r.loop.Addr().String()+"/logout", nil)
	bad.Header.Set("Origin", "https://evil.example")
	bresp, err := http.DefaultClient.Do(bad)
	if err != nil {
		t.Fatal(err)
	}
	hint, _ := io.ReadAll(bresp.Body)
	_ = bresp.Body.Close()
	if !strings.Contains(string(hint), "opened at http://"+r.loop.Addr().String()+".") {
		t.Fatalf("the loopback hint %q does not name the loopback socket %s", hint, r.loop.Addr())
	}
	req, _ := http.NewRequest(http.MethodPost, "http://"+r.loop.Addr().String()+"/logout", nil)
	req.Header.Set("Origin", "http://"+r.loop.Addr().String())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode == http.StatusForbidden {
		t.Fatal("the loopback socket's own Origin was refused once the LAN socket listened")
	}
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(socketWait)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal("waited past the bound")
		}
		time.Sleep(time.Millisecond)
	}
}

// TestALANSocketThatFailsLeavesLoopbackServingAndClosesItsConnections: the
// LAN server's Serve failing retires the socket as a reading's change does,
// its kept-alive connection closed, the state waiting stopped_serving, and
// the loopback socket serving; the next tick binds again (§3.6, R2).
func TestALANSocketThatFailsLeavesLoopbackServingAndClosesItsConnections(t *testing.T) {
	r := newLANRig(t, newLANBox(t), nil, nil)
	kept := r.client(testLANPeer[:len(testLANPeer)-6])
	if resp, _, err := kept.get("/api/console"); err != nil || resp.StatusCode != 200 {
		t.Fatalf("first request: %v %v", resp, err)
	}
	r.box.mu.Lock()
	r.box.failNext = true
	r.box.mu.Unlock()
	if c, err := r.box.dial(netip.MustParseAddr("192.168.77.6")); err == nil {
		_ = c.Close()
	}
	waitFor(t, func() bool { return r.state().Reason == consolelan.ReasonStoppedServing })
	if _, _, err := kept.get("/api/console"); err == nil {
		t.Fatal("the kept-alive connection was answered after its socket failed")
	}
	resp, err := http.Get("http://" + r.loop.Addr().String() + "/api/console")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("the loopback socket after the failure: %v %v", resp, err)
	}
	_ = resp.Body.Close()
	r.step()
	if r.state().State != ConsoleLANListening {
		t.Fatalf("the next tick did not bind again: %+v", r.state())
	}
}

// heldDoctor makes GET /api/doctor a handler that returns only once release
// is closed, whatever its context says.
func heldDoctor(srv *Server) (entered chan struct{}, release chan struct{}) {
	entered, release = make(chan struct{}, 8), make(chan struct{})
	srv.deps.DoctorRun = func(context.Context) *DoctorReport {
		entered <- struct{}{}
		<-release
		return &DoctorReport{}
	}
	return entered, release
}

// TestAShutdownStopsBothSocketsInOneWindowAndBindsNothingAfter: the stop
// joins the plan's loop, retires the LAN socket through the fence and shuts
// the loopback socket down, all within one window, a LAN handler that never
// returns holding no second one (I7); nothing binds after.
func TestAShutdownStopsBothSocketsInOneWindowAndBindsNothingAfter(t *testing.T) {
	const window = 1500 * time.Millisecond
	r := newLANRig(t, newLANBox(t), nil, func(p *ConsoleLANPlan) { p.ShutdownWindow, p.RetireWait = window, window })
	entered, release := heldDoctor(r.srv)
	defer close(release)
	c := r.client("192.168.77.5")
	go func() { _, _, _ = c.get("/api/doctor") }()
	select {
	case <-entered:
	case <-time.After(socketWait):
		t.Fatal("the held handler never ran")
	}
	binds := r.box.count()
	start := time.Now()
	r.stop()
	if !r.stopped {
		t.Fatal("the console did not stop")
	}
	took := time.Since(start)
	if took > window+window/2 {
		t.Fatalf("the stop took %v, past its one window of %v", took, window)
	}
	if r.box.count() != binds {
		t.Fatalf("%d binds after the stop", r.box.count()-binds)
	}
	if st := r.state(); st.Reason != consolelan.ReasonStopping {
		t.Fatalf("state after the stop %+v", st)
	}
	select {
	case <-r.srv.consoleLAN.loopDone:
	default:
		t.Fatal("the plan's loop was not joined")
	}
}

// TestEveryLANResponseRefusesToBeFramed: every answer of the home-network
// socket, a page, an API answer, the source's 403 and the fence's 503
// included, says no page may frame it (I6); a loopback page keeps today's
// headers.
func TestEveryLANResponseRefusesToBeFramed(t *testing.T) {
	r := newLANRig(t, newLANBox(t), nil, nil)
	check := func(name string, resp *http.Response) {
		t.Helper()
		if resp.Header.Get("X-Frame-Options") != "DENY" || resp.Header.Get("Content-Security-Policy") != "frame-ancestors 'none'" {
			t.Errorf("%s: X-Frame-Options %q, CSP %q", name, resp.Header.Get("X-Frame-Options"), resp.Header.Get("Content-Security-Policy"))
		}
	}
	c := r.client("192.168.77.5")
	for _, path := range []string{"/", "/devices", "/api/console"} {
		resp, _, err := c.get(path)
		if err != nil {
			t.Fatal(err)
		}
		check(path, resp)
	}
	out := r.client("10.9.9.9")
	resp, _, err := out.get("/api/console")
	if err != nil || resp.StatusCode != 403 {
		t.Fatalf("outside source: %v %v", resp, err)
	}
	check("the source's 403", resp)
	p := r.policy()
	r.box.set(enTest(9, "en-other", testLANAddr+"/24"))
	held := make(chan struct{})
	var heldOnce sync.Once
	r.seams.RetirementMarked = func() { heldOnce.Do(func() { <-held }) }
	go r.step()
	waitFor(t, func() bool {
		r.srv.consoleLAN.mu.Lock()
		defer r.srv.consoleLAN.mu.Unlock()
		return p.retired
	})
	resp, _, err = c.get("/api/console")
	close(held)
	if err != nil || resp.StatusCode != 503 {
		t.Fatalf("after the retirement's section: %v %v", resp, err)
	}
	check("the fence's 503", resp)
	lresp, err := http.Get("http://" + r.loop.Addr().String() + "/devices")
	if err != nil {
		t.Fatal(err)
	}
	_ = lresp.Body.Close()
	if lresp.Header.Get("X-Frame-Options") != "" {
		t.Errorf("the loopback page gained X-Frame-Options %q", lresp.Header.Get("X-Frame-Options"))
	}
}

// retireCase is one change of the reading that retires the socket.
func retireCase(t *testing.T, change func(*lanBox), check func(t *testing.T, r *lanRig)) {
	t.Helper()
	r := newLANRig(t, newLANBox(t), nil, nil)
	kept := r.client("192.168.77.5")
	if resp, _, err := kept.get("/api/console"); err != nil || resp.StatusCode != 200 {
		t.Fatalf("before the change: %v %v", resp, err)
	}
	change(r.box)
	r.step()
	if resp, _, err := kept.get("/api/console"); err == nil {
		t.Fatalf("the kept-alive connection from before the change was answered %d", resp.StatusCode)
	}
	check(t, r)
}

// TestTheLANSocketIsRetiredWhenItsAddressIsRenumbered: A gone from the box
// retires the socket, its kept-alive connection gets no answer, and nothing
// binds: waiting, address_not_on_this_box (D2).
func TestTheLANSocketIsRetiredWhenItsAddressIsRenumbered(t *testing.T) {
	retireCase(t, func(b *lanBox) { b.set(enTest(7, "en-test", "192.168.77.21/24")) }, func(t *testing.T, r *lanRig) {
		if st := r.state(); st.State != ConsoleLANWaiting || st.Reason != consolelan.ReasonAddressNotOnBox {
			t.Fatalf("state %+v", st)
		}
	})
}

// TestTheLANSocketIsRetiredWhenItsMaskNarrows: a prefix narrowed from /24 to
// /25 retires the socket and binds on the new reading, which judges a new
// connection by the new network (D2).
func TestTheLANSocketIsRetiredWhenItsMaskNarrows(t *testing.T) {
	retireCase(t, func(b *lanBox) { b.set(enTest(7, "en-test", testLANAddr+"/25")) }, func(t *testing.T, r *lanRig) {
		if st := r.state(); st.State != ConsoleLANListening || st.Network != "192.168.77.0/25" {
			t.Fatalf("state %+v", st)
		}
		in := r.client("192.168.77.5")
		if resp, _, err := in.get("/api/console"); err != nil || resp.StatusCode != 200 {
			t.Fatalf("a source in the new /25: %v %v", resp, err)
		}
		out := r.client("192.168.77.200")
		if resp, _, err := out.get("/api/console"); err != nil || resp.StatusCode != 403 {
			t.Fatalf("a source in the old /24 and outside the new /25: %v %v", resp, err)
		}
	})
}

// TestTheLANSocketIsRetiredWhenItsAddressMovesToAnotherInterface: A on
// another interface retires the socket and binds one held to the new
// interface's index (D2).
func TestTheLANSocketIsRetiredWhenItsAddressMovesToAnotherInterface(t *testing.T) {
	retireCase(t, func(b *lanBox) { b.set(enTest(9, "en-other", testLANAddr+"/24")) }, func(t *testing.T, r *lanRig) {
		if st := r.state(); st.State != ConsoleLANListening || st.Interface != "en-other" {
			t.Fatalf("state %+v", st)
		}
		r.box.mu.Lock()
		holds := append([]int(nil), r.box.holds...)
		r.box.mu.Unlock()
		if len(holds) != 2 || holds[1] != 9 {
			t.Fatalf("the holds were %v, want the new socket held to index 9", holds)
		}
	})
}

// TestAKeptAliveConnectionIsClosedBeforeTheNewSocketBinds: the old socket's
// connections are closed before the new one binds (D2).
func TestAKeptAliveConnectionIsClosedBeforeTheNewSocketBinds(t *testing.T) {
	r := newLANRig(t, newLANBox(t), nil, nil)
	kept := r.client("192.168.77.5")
	if resp, _, err := kept.get("/api/console"); err != nil || resp.StatusCode != 200 {
		t.Fatalf("before: %v %v", resp, err)
	}
	r.box.mu.Lock()
	r.box.events = nil
	r.box.mu.Unlock()
	r.box.set(enTest(9, "en-other", testLANAddr+"/24"))
	r.step()
	r.box.mu.Lock()
	events := append([]string(nil), r.box.events...)
	r.box.mu.Unlock()
	if len(events) < 2 || events[0] != "conn closed" || events[len(events)-1] != "bind" {
		t.Fatalf("events %v, want the kept-alive connection closed, then the new bind", events)
	}
}

// TestAReadingThatFailsAfterTheBindClosesTheLANSocket: after admission, a
// listing that fails and one interface whose addresses fail each retire the
// socket and its kept-alive connection within one tick; a whole reading
// binds again (D12).
func TestAReadingThatFailsAfterTheBindClosesTheLANSocket(t *testing.T) {
	var mu sync.Mutex
	mode := "whole"
	read := lanReadingOf(
		func() ([]net.Interface, error) {
			mu.Lock()
			defer mu.Unlock()
			if mode == "listing" {
				return nil, errors.New("netlink")
			}
			return []net.Interface{lanEth, lanWlan}, nil
		},
		func(in net.Interface) ([]net.Addr, error) {
			mu.Lock()
			defer mu.Unlock()
			if in.Name == "wl-test" {
				if mode == "one" {
					return nil, errors.New("no such network interface")
				}
				return nil, nil
			}
			return []net.Addr{cidrNet(testLANAddr + "/24")}, nil
		})
	r := newLANRig(t, newLANBox(t), nil, func(p *ConsoleLANPlan) { p.Read = read })
	for _, m := range []string{"listing", "one"} {
		kept := r.client("192.168.77.5")
		if resp, _, err := kept.get("/api/console"); err != nil || resp.StatusCode != 200 {
			t.Fatalf("%s, before: %v %v", m, resp, err)
		}
		mu.Lock()
		mode = m
		mu.Unlock()
		r.step()
		if st := r.state(); st.State != ConsoleLANWaiting || st.Reason != consolelan.ReasonInterfacesUnreadable {
			t.Fatalf("%s: state %+v", m, st)
		}
		if _, _, err := kept.get("/api/console"); err == nil {
			t.Fatalf("%s: the kept-alive connection was answered after the reading failed", m)
		}
		mu.Lock()
		mode = "whole"
		mu.Unlock()
		r.step()
		if r.state().State != ConsoleLANListening {
			t.Fatalf("%s: a whole reading did not bind again: %+v", m, r.state())
		}
	}
}

// policyCases are a case for each part of the policy (§3.5), each with the
// field of lanPolicyParts it changes, so the structural half finds every
// field named.
var policyCases = []struct {
	name, field string
	windows     bool
	change      func(b *lanBox)
}{
	{"the index", "Index", false, func(b *lanBox) { b.set(enTest(9, "en-test", testLANAddr+"/24")) }},
	{"the name", "Name", false, func(b *lanBox) { b.set(enTest(7, "en-renamed", testLANAddr+"/24")) }},
	{"up", "Flags", false, func(b *lanBox) { i := enTest(7, "en-test", testLANAddr+"/24"); i.Flags &^= net.FlagUp; b.set(i) }},
	{"running", "Flags", false, func(b *lanBox) { i := enTest(7, "en-test", testLANAddr+"/24"); i.Flags &^= net.FlagRunning; b.set(i) }},
	{"loopback", "Flags", false, func(b *lanBox) { i := enTest(7, "en-test", testLANAddr+"/24"); i.Flags |= net.FlagLoopback; b.set(i) }},
	{"point-to-point", "Flags", false, func(b *lanBox) {
		i := enTest(7, "en-test", testLANAddr+"/24")
		i.Flags |= net.FlagPointToPoint
		b.set(i)
	}},
	{"multicast", "Flags", false, func(b *lanBox) {
		i := enTest(7, "en-test", testLANAddr+"/24")
		i.Flags &^= net.FlagMulticast
		b.set(i)
	}},
	{"the prefix", "Prefix", false, func(b *lanBox) { b.set(enTest(7, "en-test", testLANAddr+"/16")) }},
	{"A held twice", "Prefix", false, func(b *lanBox) {
		b.set(enTest(7, "en-test", testLANAddr+"/24"), enTest(9, "en-two", testLANAddr+"/24"))
	}},
	{"weak host send on A's row", "HostTable", true, func(b *lanBox) { b.mu.Lock(); b.table[0].WeakHostSend = true; b.mu.Unlock() }},
	{"weak host receive on A's row", "HostTable", true, func(b *lanBox) { b.mu.Lock(); b.table[0].WeakHostReceive = true; b.mu.Unlock() }},
	{"forwarding on A's row", "HostTable", true, func(b *lanBox) { b.mu.Lock(); b.table[0].Forwarding = true; b.mu.Unlock() }},
	{"weak host send on another's row", "HostTable", true, func(b *lanBox) { b.mu.Lock(); b.table[1].WeakHostSend = true; b.mu.Unlock() }},
	{"weak host receive on another's row", "HostTable", true, func(b *lanBox) { b.mu.Lock(); b.table[1].WeakHostReceive = true; b.mu.Unlock() }},
	{"forwarding on another's row", "HostTable", true, func(b *lanBox) { b.mu.Lock(); b.table[1].Forwarding = true; b.mu.Unlock() }},
	{"a row added", "HostTable", true, func(b *lanBox) { b.mu.Lock(); b.table = append(b.table, consolelan.Row{Index: 31}); b.mu.Unlock() }},
	{"a row removed", "HostTable", true, func(b *lanBox) { b.mu.Lock(); b.table = b.table[:1]; b.mu.Unlock() }},
	{"the .local name", "LocalName", false, nil},
}

// TestEachPolicyFieldRetiresTheSocketWhenItChanges: a case for each part of
// the policy, each with a kept-alive connection that gets no answer after
// the change, and a structural half that fails when the policy's type gains
// a field no case names (F8, F11).
func TestEachPolicyFieldRetiresTheSocketWhenItChanges(t *testing.T) {
	named := map[string]bool{}
	for _, c := range policyCases {
		named[c.field] = true
	}
	rt := reflect.TypeOf(lanPolicyParts{})
	for i := 0; i < rt.NumField(); i++ {
		if !named[rt.Field(i).Name] {
			t.Errorf("the policy's field %s has no case", rt.Field(i).Name)
		}
	}
	for _, c := range policyCases {
		t.Run(c.name, func(t *testing.T) {
			box := newLANBox(t)
			var name struct {
				sync.Mutex
				v string
			}
			name.v = "box.local"
			edit := func(p *ConsoleLANPlan) {
				p.LocalName = func() string { name.Lock(); defer name.Unlock(); return name.v }
				if c.windows {
					p.GOOS = "windows"
					box.table = consolelan.Table{{Index: 7}, {Index: 12}}
				}
			}
			r := newLANRig(t, box, nil, edit)
			if r.state().State != ConsoleLANListening {
				t.Fatalf("not listening: %+v", r.state())
			}
			kept := r.client("192.168.77.5")
			if resp, _, err := kept.get("/api/console"); err != nil || resp.StatusCode != 200 {
				t.Fatalf("before: %v %v", resp, err)
			}
			before := r.policy()
			if c.change != nil {
				c.change(box)
			} else {
				name.Lock()
				name.v = "other.local"
				name.Unlock()
			}
			r.step()
			if resp, _, err := kept.get("/api/console"); err == nil {
				t.Fatalf("the kept-alive connection was answered %d after the change", resp.StatusCode)
			}
			if r.policy() == before {
				t.Fatal("the policy was not retired")
			}
		})
	}
}

// TestNoRequestIsAdmittedOnceARetirementBegins: a connection opened before a
// retirement sends its request once the retirement's section has passed and
// before its Close (the retirement held at its seam there): 503 with
// Connection: close, and no request is admitted (F1).
func TestNoRequestIsAdmittedOnceARetirementBegins(t *testing.T) {
	seams := &ConsoleLANSeams{}
	r := newLANRig(t, newLANBox(t), seams, nil)
	kept := r.client("192.168.77.5")
	if resp, _, err := kept.get("/api/console"); err != nil || resp.StatusCode != 200 {
		t.Fatalf("before: %v %v", resp, err)
	}
	var mu sync.Mutex
	admitted := 0
	seams.InAdmission = func() { mu.Lock(); admitted++; mu.Unlock() }
	marked, release := make(chan struct{}), make(chan struct{})
	var markOnce sync.Once
	seams.RetirementMarked = func() { markOnce.Do(func() { close(marked); <-release }) }
	r.box.set(enTest(9, "en-other", testLANAddr+"/24"))
	stepped := make(chan struct{})
	go func() { r.step(); close(stepped) }()
	select {
	case <-marked:
	case <-time.After(socketWait):
		t.Fatal("the retirement never marked its policy")
	}
	resp, _, err := kept.get("/api/console")
	close(release)
	<-stepped
	if err != nil {
		t.Fatalf("the request after the retirement's section: %v", err)
	}
	if resp.StatusCode != http.StatusServiceUnavailable || !resp.Close {
		t.Fatalf("answered %d (close %v), want 503 with Connection: close", resp.StatusCode, resp.Close)
	}
	mu.Lock()
	defer mu.Unlock()
	if admitted != 0 {
		t.Fatalf("%d requests admitted after the retirement began", admitted)
	}
}

// TestAnAdmissionParkedBetweenItsCheckAndItsCountIsSeenByTheRetirement: a
// request parked at the admission's seam, inside the one section of the
// plan's mutex that holds its check and its count (I4); a retirement begun
// meanwhile waits for that section, then finds the request counted, cancels
// its context and waits for it, never completing with a count of 0 while
// the request runs; a request after the retirement's section is refused.
// Ordered by the seams alone.
func TestAnAdmissionParkedBetweenItsCheckAndItsCountIsSeenByTheRetirement(t *testing.T) {
	seams := &ConsoleLANSeams{}
	r := newLANRig(t, newLANBox(t), seams, nil)
	entered, release := heldDoctor(r.srv)
	parked, unpark := make(chan struct{}), make(chan struct{})
	var once sync.Once
	seams.InAdmission = func() { once.Do(func() { close(parked); <-unpark }) }
	countAtMark := make(chan int, 4)
	p := r.policy()
	seams.RetirementMarked = func() {
		r.srv.consoleLAN.mu.Lock()
		n := p.inflight
		r.srv.consoleLAN.mu.Unlock()
		select {
		case countAtMark <- n:
		default:
		}
	}
	c := r.client("192.168.77.5")
	got := make(chan int, 1)
	go func() {
		resp, _, err := c.get("/api/doctor")
		if err != nil {
			got <- -1
			return
		}
		got <- resp.StatusCode
	}()
	<-parked
	// The retirement reads the box, then waits for the section the parked
	// request holds.
	readAsked := make(chan struct{})
	var rOnce sync.Once
	r.box.mu.Lock()
	r.box.ifs = []consolelan.Interface{enTest(9, "en-other", testLANAddr+"/24")}
	r.box.mu.Unlock()
	go func() {
		select {
		case r.tick <- time.Now():
			rOnce.Do(func() { close(readAsked) })
		case <-time.After(socketWait):
		}
	}()
	<-readAsked
	close(unpark)
	select {
	case n := <-countAtMark:
		if n != 1 {
			t.Fatalf("the retirement marked its policy with %d requests counted, want the parked one", n)
		}
	case <-time.After(socketWait):
		t.Fatal("the retirement never marked its policy")
	}
	select {
	case <-entered:
	case <-time.After(socketWait):
		t.Fatal("the parked request never ran its handler")
	}
	select {
	case <-r.ticked:
		t.Fatal("the retirement completed while the request it counted ran")
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	select {
	case <-r.ticked:
	case <-time.After(socketWait):
		t.Fatal("the retirement did not complete once its request ended")
	}
	<-got
}

// TestTheRebindTakesAFreshReadingAfterTheWait: the reading that finds the
// change says A moved to a second interface; while the retirement waits on
// a handler the test holds, the reading changes again, A gone in one case
// and on a third interface in the other; the rebind follows the fresh
// reading, never the second interface (F2).
func TestTheRebindTakesAFreshReadingAfterTheWait(t *testing.T) {
	for _, c := range []struct {
		name  string
		fresh []consolelan.Interface
		index int
	}{
		{"A gone", []consolelan.Interface{enTest(9, "en-second", "192.168.77.21/24")}, 0},
		{"A on a third interface", []consolelan.Interface{enTest(11, "en-third", testLANAddr+"/24")}, 11},
	} {
		t.Run(c.name, func(t *testing.T) {
			seams := &ConsoleLANSeams{}
			r := newLANRig(t, newLANBox(t), seams, nil)
			entered, release := heldDoctor(r.srv)
			closed := make(chan struct{})
			var closeOnce sync.Once
			seams.RetirementClosed = func() { closeOnce.Do(func() { close(closed) }) }
			go func() { _, _, _ = r.client("192.168.77.5").get("/api/doctor") }()
			<-entered
			r.box.set(enTest(9, "en-second", testLANAddr+"/24"))
			stepped := make(chan struct{})
			go func() { r.step(); close(stepped) }()
			<-closed
			r.box.set(c.fresh...)
			close(release)
			<-stepped
			st := r.state()
			r.box.mu.Lock()
			holds := append([]int(nil), r.box.holds...)
			r.box.mu.Unlock()
			if c.index == 0 {
				if st.State != ConsoleLANWaiting || len(holds) != 1 {
					t.Fatalf("state %+v, holds %v: want no socket", st, holds)
				}
				return
			}
			if st.State != ConsoleLANListening || holds[len(holds)-1] != c.index {
				t.Fatalf("state %+v, holds %v: want a socket held to %d", st, holds, c.index)
			}
			for _, h := range holds {
				if h == 9 {
					t.Fatal("a socket was held to the second interface, which the fresh reading no longer names")
				}
			}
		})
	}
}

// openStream opens GET /api/events on c, reading its headers.
func openStream(t *testing.T, c *lanClient) *bufio.Reader {
	t.Helper()
	req := "GET /api/events HTTP/1.1\r\nHost: " + testLANHost + "\r\n\r\n"
	_ = c.conn.SetDeadline(time.Now().Add(socketWait))
	if _, err := io.WriteString(c.conn, req); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(c.rd, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("the stream answered %d", resp.StatusCode)
	}
	return c.rd
}

// TestAnEventStreamEndsAtItsSocketsRetirement holds the rule the console
// keeps for an event stream (I2, I8): the retirement cancels the policy's
// context first, and a stream waiting on its context, or on a snapshot it
// waits for on that context, ends there, before the close; the close then
// ends a stream blocked in a write, which a cancellation cannot; and the
// retirement's wait ends once every handler has, never at its bound.
func TestAnEventStreamEndsAtItsSocketsRetirement(t *testing.T) {
	cases := []struct {
		name  string
		setup func(r *lanRig) (ready func())
		// atMark: whether the stream ends while the retirement is held
		// after its section and before its close.
		atMark bool
	}{
		{"a stream waiting on its context", func(r *lanRig) func() {
			return func() {}
		}, true},
		{"a stream waiting on a snapshot", func(r *lanRig) func() {
			in := make(chan struct{}, 4)
			hold := make(chan struct{})
			r.t.Cleanup(func() { close(hold) })
			r.srv.testCompositionFlight = func() { in <- struct{}{}; <-hold }
			return func() {
				select {
				case <-in:
				case <-time.After(socketWait):
					r.t.Fatal("the stream never waited on the composition snapshot")
				}
			}
		}, true},
		{"a stream blocked in a write", func(r *lanRig) func() {
			r.box.mu.Lock()
			r.box.stallNext = true
			r.box.mu.Unlock()
			return func() {
				waitFor(r.t, func() bool {
					r.box.mu.Lock()
					defer r.box.mu.Unlock()
					for _, c := range r.box.conns {
						c.mu.Lock()
						w := c.writes
						c.mu.Unlock()
						if c.stall && w > 1 {
							return true
						}
					}
					return false
				})
			}
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logs := loggingtest.Record(t)
			seams := &ConsoleLANSeams{}
			r := newLANRig(t, newLANBox(t), seams, nil)
			ready := tc.setup(r)
			released := make(chan int, 16)
			seams.Released = func(left int) { released <- left }
			c := r.client("192.168.77.5")
			_ = openStream(t, c)
			ready()
			marked, release := make(chan struct{}), make(chan struct{})
			closedSeen := make(chan struct{})
			var markOnce, closeOnce sync.Once
			seams.RetirementMarked = func() { markOnce.Do(func() { close(marked); <-release }) }
			seams.RetirementClosed = func() { closeOnce.Do(func() { close(closedSeen) }) }
			r.box.set(enTest(9, "en-other", testLANAddr+"/24"))
			stepped := make(chan struct{})
			go func() { r.step(); close(stepped) }()
			<-marked
			endedAtMark := false
			select {
			case left := <-released:
				endedAtMark = left == 0
			case <-time.After(2 * time.Second):
			}
			if endedAtMark != tc.atMark {
				t.Fatalf("the stream ended at the retirement's section: %v, want %v", endedAtMark, tc.atMark)
			}
			close(release)
			<-closedSeen
			if !tc.atMark {
				select {
				case left := <-released:
					if left != 0 {
						t.Fatalf("after the close %d requests are left", left)
					}
				case <-time.After(socketWait):
					t.Fatal("the close did not end the stream blocked in its write")
				}
			}
			<-stepped
			if lines := logs.Lines("console: a request of a retired home-network socket outlived its wait"); len(lines) != 0 {
				t.Fatalf("the retirement's wait ended at its bound: %v", lines)
			}
		})
	}
}

// TestAPairingQRMintedThroughTheLANSocketIsLoopbacks: a token minted through
// the LAN socket builds its link from the body and the bridge's endpoints,
// never the Host the request used (§7).
func TestAPairingQRMintedThroughTheLANSocketIsLoopbacks(t *testing.T) {
	r := newLANRig(t, newLANBox(t), nil, nil)
	c := r.client("192.168.77.5")
	body := `{"name":"phone","url":"https://bridge.local:7788"}`
	req := fmt.Sprintf("POST /api/tokens HTTP/1.1\r\nHost: %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", testLANHost, len(body), body)
	_ = c.conn.SetDeadline(time.Now().Add(socketWait))
	if _, err := io.WriteString(c.conn, req); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(c.rd, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		t.Fatalf("mint: %d %s", resp.StatusCode, b)
	}
	if strings.Contains(string(b), "192.168.77.20:7789") {
		t.Fatalf("the mint's answer carries the Host the request used: %s", b)
	}
}
