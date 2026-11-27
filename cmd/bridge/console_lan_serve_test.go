package main

import (
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/consolelan"
	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
	bridgemdns "github.com/acoseac/1-bit-bridge/internal/mdns"
	"github.com/acoseac/1-bit-bridge/internal/player/lan"
)

// Serve's home-network socket (backlog B989), the real serve with a handed
// loopback listener and a stand-in box for the LAN bind (the plan's §9.6):
// a reading of the test's, a hold that records the index it is given, and a
// bind on 127.0.0.1 that stands for A:P, whose connections report the source
// the test's client names.

const serveLANAddr = "192.168.88.20"

// serveBox is the stand-in box.
type serveBox struct {
	mu        sync.Mutex
	ifs       []consolelan.Interface
	holds     []int
	controls  []int
	holdErr   error
	listeners []net.Listener
	reals     []net.Listener
	sources   map[int]netip.Addr
	sourced   chan struct{}
	order     []string
	// gate, set before serve starts and never after, holds the bind in the
	// listen seam until it is let go (backlog B1047).
	gate *listenGate
}

// listenGate holds the box's bind at one point of its listen seam, outside
// the box's mutex, until it is let go (backlog B1047): before the bind is
// registered, so the box holds no listener and a client finds "no LAN
// listener", or once it is recorded (afterBind), so the box's order reads
// hold,control,bind while the state still says the bridge is starting. The
// bind's answer, and with it the state serve publishes and the banner after
// it, waits for the release: the gate holds open the window a loaded runner
// opens by chance between the banner startLANServe waits for and the
// home-network socket's first bind. startLANServe lets it go in a cleanup
// that runs before the drain, and a stop lets it go before it waits.
type listenGate struct {
	afterBind   bool
	reached     chan struct{} // closed once the seam has come to the hold
	released    chan struct{} // closed to let the seam go on
	reachOnce   sync.Once
	releaseOnce sync.Once
}

// newListenGate is a gate held before the bind's registration, or once the
// bind is recorded where afterBind.
func newListenGate(afterBind bool) *listenGate {
	return &listenGate{afterBind: afterBind, reached: make(chan struct{}), released: make(chan struct{})}
}

// hold marks the hold reached and waits for the release.
func (g *listenGate) hold() {
	g.reachOnce.Do(func() { close(g.reached) })
	<-g.released
}

// held reports whether the seam has come to the hold; a nil gate holds
// nothing.
func (g *listenGate) held() bool {
	if g == nil {
		return false
	}
	select {
	case <-g.reached:
		return true
	default:
		return false
	}
}

// release lets the seam go on, once, whoever calls it; a nil gate has
// nothing to let go.
func (g *listenGate) release() {
	if g != nil {
		g.releaseOnce.Do(func() { close(g.released) })
	}
}

func newServeBox(addrs ...string) *serveBox {
	b := &serveBox{sources: map[int]netip.Addr{}, sourced: make(chan struct{})}
	b.setAddrs(addrs...)
	return b
}

// setAddrs makes the box's one interface, index 41, hold addrs.
func (b *serveBox) setAddrs(addrs ...string) {
	in := consolelan.Interface{Name: "eth-test", Index: 41, Flags: net.FlagUp | net.FlagRunning | net.FlagBroadcast | net.FlagMulticast}
	for _, a := range addrs {
		in.Addrs = append(in.Addrs, netip.MustParsePrefix(a))
	}
	b.mu.Lock()
	b.ifs = []consolelan.Interface{in}
	b.mu.Unlock()
}

func (b *serveBox) seams(tick <-chan time.Time) consoleLANSeams {
	return consoleLANSeams{
		read: func() ([]consolelan.Interface, error) {
			b.mu.Lock()
			defer b.mu.Unlock()
			return append([]consolelan.Interface(nil), b.ifs...), nil
		},
		hold: func(index int, _ bool) func(string, string, syscall.RawConn) error {
			b.mu.Lock()
			b.holds = append(b.holds, index)
			b.order = append(b.order, "hold")
			err := b.holdErr
			b.mu.Unlock()
			return func(string, string, syscall.RawConn) error {
				b.mu.Lock()
				b.controls = append(b.controls, index)
				b.order = append(b.order, "control")
				b.mu.Unlock()
				return err
			}
		},
		listen: func(ctx context.Context, lc *net.ListenConfig, network, address string) (net.Listener, error) {
			real, err := lc.Listen(ctx, "tcp4", "127.0.0.1:0")
			if err != nil {
				return nil, err
			}
			l := &serveStandIn{box: b, real: real, lan: netip.MustParseAddrPort(address)}
			if b.gate != nil && !b.gate.afterBind {
				b.gate.hold()
			}
			b.mu.Lock()
			b.listeners = append(b.listeners, l)
			b.reals = append(b.reals, real)
			b.order = append(b.order, "bind")
			b.mu.Unlock()
			if b.gate != nil && b.gate.afterBind {
				b.gate.hold()
			}
			return l, nil
		},
		localName: func() string { return "" },
		goos:      "linux",
		tick:      tick,
	}
}

func (b *serveBox) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.listeners)
}

// client is an HTTP client whose connections reach the newest LAN listener
// from the source from, whatever the URL's host names.
func (b *serveBox) client(t *testing.T, from string) *http.Client {
	src := netip.MustParseAddr(from)
	tr := &http.Transport{DisableKeepAlives: true, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		b.mu.Lock()
		if len(b.reals) == 0 {
			b.mu.Unlock()
			return nil, errors.New("no LAN listener")
		}
		addr := b.reals[len(b.reals)-1].Addr().String()
		b.mu.Unlock()
		c, err := (&net.Dialer{}).DialContext(ctx, "tcp4", addr)
		if err != nil {
			return nil, err
		}
		b.mu.Lock()
		b.sources[c.LocalAddr().(*net.TCPAddr).Port] = src
		close(b.sourced)
		b.sourced = make(chan struct{})
		b.mu.Unlock()
		return c, nil
	}}
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr, Timeout: 10 * time.Second}
}

type serveStandIn struct {
	box  *serveBox
	real net.Listener
	lan  netip.AddrPort
}

func (l *serveStandIn) Accept() (net.Conn, error) {
	c, err := l.real.Accept()
	if err != nil {
		return nil, err
	}
	port := c.RemoteAddr().(*net.TCPAddr).Port
	deadline := time.Now().Add(10 * time.Second)
	var src netip.Addr
	for {
		l.box.mu.Lock()
		s, ok := l.box.sources[port]
		wait := l.box.sourced
		l.box.mu.Unlock()
		if ok || time.Now().After(deadline) {
			src = s
			break
		}
		select {
		case <-wait:
		case <-time.After(time.Until(deadline)):
		}
	}
	return &serveStandInConn{Conn: c, local: net.TCPAddrFromAddrPort(l.lan),
		remote: net.TCPAddrFromAddrPort(netip.AddrPortFrom(src, uint16(port)))}, nil
}

func (l *serveStandIn) Close() error   { return l.real.Close() }
func (l *serveStandIn) Addr() net.Addr { return net.TCPAddrFromAddrPort(l.lan) }

type serveStandInConn struct {
	net.Conn
	local, remote net.Addr
}

func (c *serveStandInConn) LocalAddr() net.Addr  { return c.local }
func (c *serveStandInConn) RemoteAddr() net.Addr { return c.remote }

// lanServe is a serve with the posture, its admin port handed in.
type lanServe struct {
	served   servedBridge
	admin    string
	cfgPath  string
	box      *serveBox
	tick     chan time.Time
	lanURL   string
	stopOnce sync.Once
}

// writeConsoleLANConfig writes a loopback config with lanLine (or none) whose
// admin address is admin.
func writeConsoleLANConfig(t *testing.T, dir, admin, lanLine string) string {
	t.Helper()
	lib := filepath.Join(dir, "Music")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "libraryRoots:\n  - " + lib + "\nlistenAddress: \"127.0.0.1:0\"\nadminAddress: \"" + admin + "\"\n" +
		"dataDir: " + filepath.Join(dir, "data") + "\nmdns:\n  enabled: false\n" + lanLine
	path := filepath.Join(dir, "bridge.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// startLANServe boots serve over cfg's directory with box, its admin socket
// handed in on admin, and returns once serve's banner names its API: before
// the home-network socket's first attempt, which serve makes after that
// banner, on its own goroutine. A box's gate is let go in a cleanup that
// runs before the drain (drainServeOnCleanup), whatever the boot did.
func startLANServe(t *testing.T, cfgPath string, adminLn net.Listener, box *serveBox) *lanServe {
	t.Helper()
	tick := make(chan time.Time)
	s := &lanServe{admin: adminLn.Addr().String(), cfgPath: cfgPath, box: box, tick: tick}
	opts := serveOpts{configPath: cfgPath, adminListener: adminLn, tailscaleCLI: noTailscaleCLI(), consoleLAN: box.seams(tick)}
	s.served = launchServe(t, func(ctx context.Context, stdout, stderr io.Writer) int {
		return runServe(ctx, opts, stdout, stderr)
	}, func(context.CancelFunc, <-chan struct{}) { t.Cleanup(box.gate.release) })
	return s
}

// stop lets go of a bind the box holds, which serve's exit waits for, then
// stops serve and waits for it to exit 0.
func (s *lanServe) stop(t *testing.T) {
	s.stopOnce.Do(func() {
		s.box.gate.release()
		stopServed(t, s.served)
	})
}

// console reads GET /api/console on the loopback socket, failing the test
// at a transport error: a test reads it once waitLANState has read the state
// it asserts, never at once after a start or a tick.
func (s *lanServe) console(t *testing.T) consolelan.State {
	t.Helper()
	var got struct {
		LAN consolelan.State `json:"lan"`
	}
	if err := fetchAdminJSON(context.Background(), s.admin, "/api/console", &got); err != nil {
		t.Fatal(err)
	}
	return got.LAN
}

// waitLANState waits until a GET /api/console of serve's loopback socket
// reads a state want holds for, and returns it (backlog B1047). Serve makes
// the home-network socket's first attempt, binds it and publishes its state
// on its own goroutine after the banner startLANServe waits for, and again
// at a tick, so a test that reads the socket or the state after a start or
// a change waits here first for the state it then asserts: a transition,
// never a state the start's own waiting, for starting, already answers, so
// a failed attempt's is named by its reason (lanWaitingFor). A listening
// state holds only once the box holds a listener too, so a client can
// reach it. Each read is a request of its own: a transport error, which a
// console that has not begun to answer gives, or an answer that does not
// decode, is tried again. A bind the box holds (listenGate) is let go once a
// read made while it held has not found want, so the bind answers after the
// test began to wait, as a late one did on CI. The wait gives up where
// serve's waits do (serveGiveUpTime), and at once once serve has exited,
// naming what it waited for, why it ended, the last state it read and the
// last error it met; its requests go on a context that ends at both
// (lanWaitContext), so neither waits out a request in flight, and past
// either it hands out no state, one it has just read included (backlog
// B1047's second round).
func waitLANState(t *testing.T, s *lanServe, what string, want func(consolelan.State) bool) consolelan.State {
	t.Helper()
	giveUp := serveGiveUpTime(t)
	ctx, cancel := lanWaitContext(s, giveUp)
	defer cancel()
	var last *consolelan.State
	var lastErr error
	failIfOver := func() {
		t.Helper()
		if why := lanWaitOver(s, giveUp); why != "" {
			t.Fatalf("GET /api/console: no state that is %s was taken: %s; the last state it read: %+v; "+
				"the last error: %v\nstdout=%s\nstderr=%s", what, why, last, lastErr, s.served.stdout.String(), s.served.stderr.String())
		}
	}
	for {
		failIfOver()
		held := s.box.gate.held()
		var got struct {
			LAN consolelan.State `json:"lan"`
		}
		if err := fetchAdminJSON(ctx, s.admin, "/api/console", &got); err != nil {
			lastErr = err
		} else {
			last = &got.LAN
			if want(got.LAN) && (got.LAN.State != consolelan.StateListening || s.box.count() > 0) {
				failIfOver()
				return got.LAN
			}
			if held {
				s.box.gate.release()
			}
		}
		lanWaitPause(ctx, s)
	}
}

// lanWaitContext is the context a console-LAN wait sends its requests on
// (backlog B1047's second round): it ends once serve has exited, so a request
// in flight then is cancelled, and at giveUp where the test binary has a
// deadline (giveUp is the zero time where it has none). Its cancel, which
// the wait defers, ends the goroutine that watches for serve's exit.
func lanWaitContext(s *lanServe, giveUp time.Time) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		select {
		case <-s.served.exited:
			cancel()
		case <-ctx.Done():
		}
	}()
	if giveUp.IsZero() {
		return ctx, cancel
	}
	capped, uncap := context.WithDeadline(ctx, giveUp)
	return capped, func() {
		uncap()
		cancel()
	}
}

// lanWaitOver is why a console-LAN wait may take nothing more, or "" while it
// may: serve has exited, or giveUp, the wait's, has passed. It asks exited
// alone, never done: the exit code stays for stopServed or the drain to read
// and report.
func lanWaitOver(s *lanServe, giveUp time.Time) string {
	select {
	case <-s.served.exited:
		return "serve exited while the test waited (its exit code is left for the stop or the drain to report)"
	default:
	}
	if !giveUp.IsZero() && time.Now().After(giveUp) {
		return "the test's deadline came first"
	}
	return ""
}

// lanWaitPause is a console-LAN wait's 10 ms between two reads, cut short once
// serve exits or ctx, the wait's context, ends at its give-up.
func lanWaitPause(ctx context.Context, s *lanServe) {
	select {
	case <-s.served.exited:
	case <-ctx.Done():
	case <-time.After(10 * time.Millisecond):
	}
}

// lanListening holds for a home-network socket that listens.
func lanListening(st consolelan.State) bool { return st.State == consolelan.StateListening }

// lanWaitingFor holds for a socket that does not listen for reason, a failed
// attempt's state: never the state alone, which the start's own waiting,
// for starting, answers before any attempt.
func lanWaitingFor(reason string) func(consolelan.State) bool {
	return func(st consolelan.State) bool { return st.State == consolelan.StateWaiting && st.Reason == reason }
}

// waitLANBanner waits for serve's first home-network banner line, the line
// it writes once the first attempt has answered, after the state that
// attempt publishes (backlog B1047), and returns it: a test that checks the
// banner's words waits for the line itself first. It waits as waitLANStdout
// does.
func waitLANBanner(t *testing.T, s *lanServe) string {
	t.Helper()
	return waitLANStdout(t, s, "home-network banner line", func(out string) (string, bool) {
		_, rest, ok := strings.Cut(out, "Home network: ")
		if !ok {
			return "", false
		}
		line, _, ok := strings.Cut(rest, "\n")
		return "Home network: " + line, ok
	})
}

// waitLANStdout waits until find finds what it looks for in what serve has
// written on stdout, and returns what find found (backlog B1047). A bind the
// box holds is let go as waitLANState lets it go. It gives up where serve's
// waits do, and at once once serve has exited, as waitLANState does, and
// past either it hands out nothing, what a read has just found included.
func waitLANStdout(t *testing.T, s *lanServe, what string, find func(stdout string) (string, bool)) string {
	t.Helper()
	giveUp := serveGiveUpTime(t)
	ctx, cancel := lanWaitContext(s, giveUp)
	defer cancel()
	failIfOver := func() {
		t.Helper()
		if why := lanWaitOver(s, giveUp); why != "" {
			t.Fatalf("serve's stdout: no %s was taken: %s; stdout=%s\nstderr=%s",
				what, why, s.served.stdout.String(), s.served.stderr.String())
		}
	}
	for {
		failIfOver()
		held := s.box.gate.held()
		if found, ok := find(s.served.stdout.String()); ok {
			failIfOver()
			return found
		}
		if held {
			s.box.gate.release()
		}
		lanWaitPause(ctx, s)
	}
}

// holdAdmin binds a loopback admin socket for serve to adopt.
func holdAdmin(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return ln
}

// lanPortOf is the admin socket's port.
func lanPortOf(t *testing.T, ln net.Listener) string {
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	return port
}

// TestServeHoldsItsLANSocketThroughConsolelan: serve's bind calls the hold
// it is given with the reading's index, its Control runs before the bind,
// and a hold that errs leaves no LAN socket (F8, the package boundary).
func TestServeHoldsItsLANSocketThroughConsolelan(t *testing.T) {
	adminLn := holdAdmin(t)
	dir := t.TempDir()
	cfg := writeConsoleLANConfig(t, dir, adminLn.Addr().String(), "adminLANAddress: \""+serveLANAddr+"\"\n")
	box := newServeBox(serveLANAddr + "/24")
	s := startLANServe(t, cfg, adminLn, box)
	defer s.stop(t)
	waitLANState(t, s, "listening", lanListening)
	box.mu.Lock()
	holds, order := append([]int(nil), box.holds...), append([]string(nil), box.order...)
	box.mu.Unlock()
	if len(holds) != 1 || holds[0] != 41 || strings.Join(order, ",") != "hold,control,bind" {
		t.Fatalf("holds %v, order %v; want the index 41 held, its Control run, then the bind", holds, order)
	}
	if st := s.console(t); st.State != consolelan.StateListening || st.Interface != "eth-test" {
		t.Fatalf("state %+v", st)
	}
	s.stop(t)

	adminLn2 := holdAdmin(t)
	cfg2 := writeConsoleLANConfig(t, t.TempDir(), adminLn2.Addr().String(), "adminLANAddress: \""+serveLANAddr+"\"\n")
	box2 := newServeBox(serveLANAddr + "/24")
	box2.holdErr = &consolelan.RefusalError{Reason: consolelan.ReasonRestrictionRefused}
	s2 := startLANServe(t, cfg2, adminLn2, box2)
	defer s2.stop(t)
	// The box holds no listener before the attempt either: the count is read
	// once the hold's refusal is the state.
	waitLANState(t, s2, "waiting, the hold refused", lanWaitingFor(consolelan.ReasonRestrictionRefused))
	if box2.count() != 0 {
		t.Fatal("a LAN socket listens after its hold erred")
	}
	if st := s2.console(t); st.State != consolelan.StateWaiting || st.Reason != consolelan.ReasonRestrictionRefused {
		t.Fatalf("state %+v", st)
	}
}

// TestServeOpensTheConsoleOnItsLANAddress: a client from inside the network
// gets the console, one from outside 403, and the banner says where it
// answers; and all of it holds where the bind answers only after the test
// began to wait for it: held before the box registers it, where a client
// that does not wait finds no LAN listener, or once the box has recorded
// it, where the state still says starting. The rows with a held bind are
// the waits' regression guard (backlog B1047): the wait lets the bind go
// once it has read the state the held bind keeps back.
func TestServeOpensTheConsoleOnItsLANAddress(t *testing.T) {
	for _, c := range []struct {
		name string
		gate func() *listenGate
	}{
		{"as serve binds it", func() *listenGate { return nil }},
		{"its bind held before the box registers it", func() *listenGate { return newListenGate(false) }},
		{"its bind held once the box has recorded it", func() *listenGate { return newListenGate(true) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			adminLn := holdAdmin(t)
			port := lanPortOf(t, adminLn)
			cfg := writeConsoleLANConfig(t, t.TempDir(), adminLn.Addr().String(), "adminLANAddress: \""+serveLANAddr+"\"\n")
			box := newServeBox(serveLANAddr + "/24")
			box.gate = c.gate()
			s := startLANServe(t, cfg, adminLn, box)
			defer s.stop(t)
			waitLANState(t, s, "listening", lanListening)
			url := "http://" + serveLANAddr + ":" + port + "/api/console"
			resp, err := box.client(t, "192.168.88.5").Get(url)
			if err != nil || resp.StatusCode != 200 {
				t.Fatalf("from inside: %v %v", resp, err)
			}
			_ = resp.Body.Close()
			resp, err = box.client(t, "10.0.0.5").Get(url)
			if err != nil || resp.StatusCode != 403 {
				t.Fatalf("from outside: %v %v", resp, err)
			}
			_ = resp.Body.Close()
			waitLANBanner(t, s)
			want := "Home network: http://" + serveLANAddr + ":" + port + "/ (no password: anyone on your network can use it), set in bridge.yaml (adminLANAddress)"
			if !strings.Contains(s.served.stdout.String(), want) {
				t.Fatalf("the banner does not say %q:\n%s", want, s.served.stdout.String())
			}
		})
	}
}

// requireLANServes asks the newest LAN socket, from a neighbour in addr's
// network, for the console under addr's own Host: serve must have bound it.
func requireLANServes(t *testing.T, box *serveBox, addr, port string) {
	t.Helper()
	from := netip.MustParseAddr(addr).As4()
	from[3] = 5
	resp, err := box.client(t, netip.AddrFrom4(from).String()).Get("http://" + addr + ":" + port + "/api/stats")
	if err != nil {
		t.Fatalf("the LAN socket at %s: %v", addr, err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the LAN socket at %s answered %d", addr, resp.StatusCode)
	}
}

// consoleDoctorLine is the console's own doctor's line name, through the
// loopback socket.
func consoleDoctorLine(t *testing.T, admin, name string) (status, summary string) {
	t.Helper()
	var resp struct {
		Report struct {
			Checks []struct {
				Name, Status, Summary, Hint string
			} `json:"checks"`
		} `json:"report"`
	}
	if err := fetchAdminJSON(context.Background(), admin, "/api/doctor", &resp); err != nil {
		t.Fatal(err)
	}
	for _, c := range resp.Report.Checks {
		if c.Name == name {
			return c.Status, c.Summary + " | " + c.Hint
		}
	}
	return "", ""
}

// TestALANAddressThatDoesNotBindLeavesTheConsoleOnLoopbackAndSaysSo: a bind
// that fails starts serve all the same, with one WARN, the console's doctor
// and /api/console saying this machine only, the loopback socket refusing
// the LAN authority; then a tick binds it and the state changes (R9).
func TestALANAddressThatDoesNotBindLeavesTheConsoleOnLoopbackAndSaysSo(t *testing.T) {
	logs := loggingtest.Record(t)
	adminLn := holdAdmin(t)
	port := lanPortOf(t, adminLn)
	cfg := writeConsoleLANConfig(t, t.TempDir(), adminLn.Addr().String(), "adminLANAddress: \""+serveLANAddr+"\"\n")
	box := newServeBox("192.168.88.21/24")
	s := startLANServe(t, cfg, adminLn, box)
	defer s.stop(t)
	// The attempt stores its state before it says its WARN: the wait is for
	// both, before the WARN's count is read.
	waitLANState(t, s, "waiting, the address not on the box, its WARN said", func(st consolelan.State) bool {
		return lanWaitingFor(consolelan.ReasonAddressNotOnBox)(st) && len(logs.Lines("console: not listening on the home network")) > 0
	})
	if st := s.console(t); st.State != consolelan.StateWaiting || st.Reason != consolelan.ReasonAddressNotOnBox {
		t.Fatalf("state %+v", st)
	}
	if n := len(logs.Lines("console: not listening on the home network")); n != 1 {
		t.Errorf("%d WARN lines, want one", n)
	}
	waitLANBanner(t, s)
	if !strings.Contains(s.served.stdout.String(), "Home network: not open yet: "+serveLANAddr+" is not on this box; tried again every 5 s") {
		t.Errorf("the banner:\n%s", s.served.stdout.String())
	}
	if status, line := consoleDoctorLine(t, s.admin, "console-lan"); status != "warn" || !strings.Contains(line, "this machine only") {
		t.Errorf("the console's doctor: %s %q", status, line)
	}
	if status, line := runDoctorJSON(t, cfg, "console-lan"); status != "warn" || !strings.Contains(line, "this machine only") {
		t.Errorf("bridge doctor: %s %q", status, line)
	}
	// The menu's line, on a page the loopback socket serves.
	page, err := http.Get("http://" + s.admin + "/stats")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(page.Body)
	_ = page.Body.Close()
	if !strings.Contains(string(body), "Until then this console answers on this machine only.") {
		t.Errorf("the menu does not say the console answers on this machine only")
	}
	req, _ := http.NewRequest(http.MethodGet, "http://"+s.admin+"/api/console", nil)
	req.Host = serveLANAddr + ":" + port
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != 421 {
		t.Errorf("the loopback socket answered Host A:P with %v %v", resp, err)
	} else {
		_ = resp.Body.Close()
	}
	post, _ := http.NewRequest(http.MethodPost, "http://"+s.admin+"/logout", nil)
	post.Header.Set("Origin", "http://"+serveLANAddr+":"+port)
	if resp, err := http.DefaultClient.Do(post); err != nil || resp.StatusCode != 403 {
		t.Errorf("the loopback socket answered Origin http://A:P with %v %v", resp, err)
	} else {
		_ = resp.Body.Close()
	}
	box.setAddrs(serveLANAddr + "/24")
	select {
	case s.tick <- time.Now():
	case <-s.served.exited:
		t.Fatalf("serve exited before the plan's loop took the tick (its exit code is left for the stop or the drain to report); stderr=%s",
			s.served.stderr.String())
	case <-serveGiveUp(t):
		t.Fatal("the plan's loop took no tick")
	}
	waitLANState(t, s, "listening, the tick's bind", lanListening)
}

// TestALANSocketThatDoesNotBindIsNoFailedStart: a LAN bind's failure never
// reaches adminErr: with the posture set and its hold refused, serve answers
// on loopback and stops cleanly, exit 0, as it does without the posture (a
// green control: a LAN bind's failure sent to adminErr turns it red).
func TestALANSocketThatDoesNotBindIsNoFailedStart(t *testing.T) {
	adminLn := holdAdmin(t)
	cfg := writeConsoleLANConfig(t, t.TempDir(), adminLn.Addr().String(), "adminLANAddress: \""+serveLANAddr+"\"\n")
	box := newServeBox(serveLANAddr + "/24")
	box.holdErr = &consolelan.RefusalError{Reason: consolelan.ReasonRestrictionRefused}
	s := startLANServe(t, cfg, adminLn, box)
	// The stop follows the failure it is about: the hold's refusal is the
	// state first.
	waitLANState(t, s, "waiting, the hold refused", lanWaitingFor(consolelan.ReasonRestrictionRefused))
	var stats map[string]any
	if err := fetchAdminJSON(context.Background(), s.admin, "/api/stats", &stats); err != nil {
		t.Fatalf("the loopback socket: %v", err)
	}
	s.stop(t)
}

// TestRemovingTheKeyLeavesTheVariablesListenerAndTheDoctorSaysSo: serve with
// the variable and a file without the key listens, source environment, and
// the console's doctor names the variable and what clears it (D7).
func TestRemovingTheKeyLeavesTheVariablesListenerAndTheDoctorSaysSo(t *testing.T) {
	t.Setenv("BRIDGE_ADMIN_LAN_ADDRESS", serveLANAddr)
	adminLn := holdAdmin(t)
	cfg := writeConsoleLANConfig(t, t.TempDir(), adminLn.Addr().String(), "")
	box := newServeBox(serveLANAddr + "/24")
	s := startLANServe(t, cfg, adminLn, box)
	defer s.stop(t)
	waitLANState(t, s, "listening", lanListening)
	requireLANServes(t, box, serveLANAddr, lanPortOf(t, adminLn))
	if st := s.console(t); st.State != consolelan.StateListening || st.Source != consolelan.SourceEnvironment {
		t.Fatalf("state %+v", st)
	}
	if _, line := consoleDoctorLine(t, s.admin, "console-lan"); !strings.Contains(line, "set by BRIDGE_ADMIN_LAN_ADDRESS in the bridge's environment") ||
		!strings.Contains(line, "unset BRIDGE_ADMIN_LAN_ADDRESS where the service is started") {
		t.Errorf("the console's doctor: %q", line)
	}
}

// runDoctorJSON runs bridge doctor --json over cfg and answers the line name.
func runDoctorJSON(t *testing.T, cfg, name string) (status, summary string) {
	t.Helper()
	var out, errOut strings.Builder
	code := run(context.Background(), []string{"doctor", "--json", "--config", cfg}, &out, &errOut)
	_ = code
	var rep struct {
		Checks []struct {
			Name, Status, Summary, Hint string
		} `json:"checks"`
	}
	if err := json.Unmarshal([]byte(out.String()), &rep); err != nil {
		t.Fatalf("doctor --json: %v\n%s\n%s", err, out.String(), errOut.String())
	}
	for _, c := range rep.Checks {
		if c.Name == name {
			return c.Status, c.Summary + " | " + c.Hint
		}
	}
	return "", ""
}

// TestTheDoctorReadsTheServingProcesssSourceNotItsOwnEnvironment: serve
// started with the variable and bridge doctor run without it, and the other
// way round: the line names the serving process's source, and says apart
// what this shell sets (D14).
func TestTheDoctorReadsTheServingProcesssSourceNotItsOwnEnvironment(t *testing.T) {
	t.Run("serve with the variable, the doctor without", func(t *testing.T) {
		t.Setenv("BRIDGE_ADMIN_LAN_ADDRESS", serveLANAddr)
		adminLn := holdAdmin(t)
		cfg := writeConsoleLANConfig(t, t.TempDir(), adminLn.Addr().String(), "")
		s := startLANServe(t, cfg, adminLn, newServeBox(serveLANAddr+"/24"))
		defer s.stop(t)
		waitLANState(t, s, "listening", lanListening)
		t.Setenv("BRIDGE_ADMIN_LAN_ADDRESS", "")
		_, line := runDoctorJSON(t, cfg, "console-lan")
		if !strings.Contains(line, "set by BRIDGE_ADMIN_LAN_ADDRESS in the bridge's environment") ||
			!strings.Contains(line, "this shell's BRIDGE_ADMIN_LAN_ADDRESS is not the running bridge's") {
			t.Fatalf("bridge doctor: %q", line)
		}
	})
	t.Run("serve without the variable, the doctor with", func(t *testing.T) {
		adminLn := holdAdmin(t)
		cfg := writeConsoleLANConfig(t, t.TempDir(), adminLn.Addr().String(), "adminLANAddress: \""+serveLANAddr+"\"\n")
		s := startLANServe(t, cfg, adminLn, newServeBox(serveLANAddr+"/24"))
		defer s.stop(t)
		waitLANState(t, s, "listening", lanListening)
		t.Setenv("BRIDGE_ADMIN_LAN_ADDRESS", "192.168.88.30")
		_, line := runDoctorJSON(t, cfg, "console-lan")
		if !strings.Contains(line, "set in bridge.yaml (adminLANAddress)") ||
			!strings.Contains(line, "this shell's BRIDGE_ADMIN_LAN_ADDRESS is not the running bridge's") {
			t.Fatalf("bridge doctor: %q", line)
		}
	})
}

// TestBridgeDoctorReadsTheRunningBridgesConsole: the CLI asks §5.2 and words
// its answer, and the rollback line from the file as it is now.
func TestBridgeDoctorReadsTheRunningBridgesConsole(t *testing.T) {
	adminLn := holdAdmin(t)
	port := lanPortOf(t, adminLn)
	cfg := writeConsoleLANConfig(t, t.TempDir(), adminLn.Addr().String(), "adminLANAddress: \""+serveLANAddr+"\"\n")
	s := startLANServe(t, cfg, adminLn, newServeBox(serveLANAddr+"/24"))
	defer s.stop(t)
	waitLANState(t, s, "listening", lanListening)
	status, line := runDoctorJSON(t, cfg, "console-lan")
	if status != "warn" || !strings.Contains(line, "http://"+serveLANAddr+":"+port) || !strings.Contains(line, "eth-test") {
		t.Fatalf("bridge doctor's console-lan: %s %q", status, line)
	}
	if status, line := runDoctorJSON(t, cfg, "console-lan-rollback"); status != "warn" || !strings.Contains(line, "0.2.3 and earlier refuse this file") {
		t.Fatalf("bridge doctor's console-lan-rollback: %s %q", status, line)
	}
}

// TestUnsettingTheVariableMakesTheFilesAddressTheOneServed: a file with A and
// a variable with A2 binds A2, source both; restarted without the variable
// it binds A, source file; the line said so before (D14).
func TestUnsettingTheVariableMakesTheFilesAddressTheOneServed(t *testing.T) {
	const a2 = "192.168.88.30"
	dir := t.TempDir()
	adminLn := holdAdmin(t)
	cfg := writeConsoleLANConfig(t, dir, adminLn.Addr().String(), "adminLANAddress: \""+serveLANAddr+"\"\n")
	t.Setenv("BRIDGE_ADMIN_LAN_ADDRESS", a2)
	box := newServeBox(serveLANAddr+"/24", a2+"/24")
	s := startLANServe(t, cfg, adminLn, box)
	waitLANState(t, s, "listening", lanListening)
	requireLANServes(t, box, a2, lanPortOf(t, adminLn))
	st := s.console(t)
	if st.Address != a2 || st.Source != consolelan.SourceBoth || st.FileAddressAtBoot != serveLANAddr {
		t.Fatalf("with the variable: %+v", st)
	}
	if _, line := consoleDoctorLine(t, s.admin, "console-lan"); !strings.Contains(line, "opens the console at bridge.yaml's address, "+serveLANAddr+",") {
		t.Errorf("the line did not say what unsetting the variable does: %q", line)
	}
	s.stop(t)
	t.Setenv("BRIDGE_ADMIN_LAN_ADDRESS", "")
	adminLn2, err := net.Listen("tcp4", adminLn.Addr().String())
	if err != nil {
		t.Skipf("the admin port could not be bound again: %v", err)
	}
	t.Cleanup(func() { _ = adminLn2.Close() })
	s2 := startLANServe(t, cfg, adminLn2, newServeBox(serveLANAddr+"/24", a2+"/24"))
	defer s2.stop(t)
	// The second boot's address and source are its plan's from the start:
	// the wait is for its bind.
	waitLANState(t, s2, "listening", lanListening)
	if st := s2.console(t); st.Address != serveLANAddr || st.Source != consolelan.SourceFile {
		t.Fatalf("without the variable: %+v", st)
	}
}

// TestAnEmptyKeyIsReportedAsCostingTheRollback: a file with an empty key
// opens no LAN socket, /api/console reports the key at boot, empty, and the
// console's doctor's rollback line says to remove the line (D16).
func TestAnEmptyKeyIsReportedAsCostingTheRollback(t *testing.T) {
	adminLn := holdAdmin(t)
	cfg := writeConsoleLANConfig(t, t.TempDir(), adminLn.Addr().String(), "adminLANAddress: \"\"\n")
	box := newServeBox(serveLANAddr + "/24")
	s := startLANServe(t, cfg, adminLn, box)
	defer s.stop(t)
	if st := s.console(t); st.State != consolelan.StateOff || st.FileKeyAtBoot != consolelan.FileKeyEmpty {
		t.Fatalf("state %+v", st)
	}
	if box.count() != 0 {
		t.Fatal("an empty key opened a LAN socket")
	}
	if status, line := consoleDoctorLine(t, s.admin, "console-lan-rollback"); status != "warn" || !strings.Contains(line, "holds adminLANAddress, empty") ||
		!strings.Contains(line, "remove the whole adminLANAddress line") {
		t.Fatalf("the console's doctor: %s %q", status, line)
	}
	if status, _ := runDoctorJSON(t, cfg, "console-lan-rollback"); status != "warn" {
		t.Fatalf("bridge doctor's rollback line: %q", status)
	}
}

// TestAVariableOverAnEmptyKeyIsBothAndCostsTheRollback: an empty key with
// the variable set binds the variable's address, source both, the file's key
// empty at boot; the line says unsetting the variable turns the console off
// the home network and that the line costs the rollback (F7).
func TestAVariableOverAnEmptyKeyIsBothAndCostsTheRollback(t *testing.T) {
	t.Setenv("BRIDGE_ADMIN_LAN_ADDRESS", serveLANAddr)
	adminLn := holdAdmin(t)
	cfg := writeConsoleLANConfig(t, t.TempDir(), adminLn.Addr().String(), "adminLANAddress: \"\"\n")
	box := newServeBox(serveLANAddr + "/24")
	s := startLANServe(t, cfg, adminLn, box)
	defer s.stop(t)
	waitLANState(t, s, "listening", lanListening)
	requireLANServes(t, box, serveLANAddr, lanPortOf(t, adminLn))
	st := s.console(t)
	if st.State != consolelan.StateListening || st.Source != consolelan.SourceBoth || st.FileKeyAtBoot != consolelan.FileKeyEmpty {
		t.Fatalf("state %+v", st)
	}
	if _, line := consoleDoctorLine(t, s.admin, "console-lan"); !strings.Contains(line, "turns the console off the home network") ||
		!strings.Contains(line, "still costs the rollback") {
		t.Errorf("the console's doctor: %q", line)
	}
	if status, _ := consoleDoctorLine(t, s.admin, "console-lan-rollback"); status != "warn" {
		t.Errorf("the rollback line: %q", status)
	}
}

// TestTheConsoleNamesTheBootFilesAddressBesideTheEffectiveOne: a variable's
// A2 over a file's A answers address A2 and fileAddressAtBoot A; over a file
// whose value is a secret the check refuses, not_an_address, the value in no
// answer, line or log (F7, R10).
func TestTheConsoleNamesTheBootFilesAddressBesideTheEffectiveOne(t *testing.T) {
	const a2 = "192.168.88.30"
	const secret = "hunter2SECRETserve"
	t.Setenv("BRIDGE_ADMIN_LAN_ADDRESS", a2)
	logs := loggingtest.Record(t)
	adminLn := holdAdmin(t)
	cfg := writeConsoleLANConfig(t, t.TempDir(), adminLn.Addr().String(), "adminLANAddress: \""+secret+"\"\n")
	s := startLANServe(t, cfg, adminLn, newServeBox(a2+"/24"))
	defer s.stop(t)
	// The fields asserted are the plan's from the start, but the bind's log
	// line and the banner, which the search below reads, come once the
	// attempt has answered: the wait is for the bind, then the banner.
	waitLANState(t, s, "listening", lanListening)
	waitLANBanner(t, s)
	st := s.console(t)
	if st.Address != a2 || st.FileKeyAtBoot != consolelan.FileKeyNotAnAddress || st.FileAddressAtBoot != "" {
		t.Fatalf("state %+v", st)
	}
	raw, _ := json.Marshal(st)
	_, line := consoleDoctorLine(t, s.admin, "console-lan")
	for where, text := range map[string]string{"the answer": string(raw), "the doctor's line": line,
		"stdout": s.served.stdout.String(), "stderr": s.served.stderr.String(), "the log": strings.Join(logs.All(), "\n")} {
		if strings.Contains(text, secret) {
			t.Errorf("%s names the file's value", where)
		}
	}
}

// TestStatusAndTheRunningInstanceGuardsFindTheBridgeAfterALANFallback: the
// local probes dial adminAddress, loopback, never the LAN address, so a
// serve whose LAN bind failed is found running (R3).
func TestStatusAndTheRunningInstanceGuardsFindTheBridgeAfterALANFallback(t *testing.T) {
	adminLn := holdAdmin(t)
	cfg := writeConsoleLANConfig(t, t.TempDir(), adminLn.Addr().String(), "adminLANAddress: \""+serveLANAddr+"\"\n")
	s := startLANServe(t, cfg, adminLn, newServeBox("192.168.88.21/24"))
	defer s.stop(t)
	// The fallback the probes are asked after: the first attempt's refusal.
	waitLANState(t, s, "waiting, the address not on the box", lanWaitingFor(consolelan.ReasonAddressNotOnBox))
	var out, errOut strings.Builder
	if code := run(context.Background(), []string{"status", "--config", cfg}, &out, &errOut); code != 0 {
		t.Fatalf("bridge status exited %d: %s", code, errOut.String())
	}
	if !probeAdminRunning(cfg) {
		t.Error("probeAdminRunning did not find the running bridge")
	}
}

// TestTheLibraryCommandsReachTheLiveAPIBesideALANSocket: the library
// commands' live API is adminAddress's, beside a LAN socket that listens
// (R3).
func TestTheLibraryCommandsReachTheLiveAPIBesideALANSocket(t *testing.T) {
	adminLn := holdAdmin(t)
	cfg := writeConsoleLANConfig(t, t.TempDir(), adminLn.Addr().String(), "adminLANAddress: \""+serveLANAddr+"\"\n")
	s := startLANServe(t, cfg, adminLn, newServeBox(serveLANAddr+"/24"))
	defer s.stop(t)
	waitLANState(t, s, "listening", lanListening)
	extra := filepath.Join(filepath.Dir(cfg), "More")
	if err := os.MkdirAll(extra, 0o755); err != nil {
		t.Fatal(err)
	}
	var out, errOut strings.Builder
	code := run(context.Background(), []string{"library", "add", "--config", cfg, extra}, &out, &errOut)
	if code != 0 {
		t.Fatalf("bridge library add exited %d: %s %s", code, out.String(), errOut.String())
	}
}

// TestOnlyTheBindAndItsChecksReadTheLANAddress: an AST walk of cmd/bridge
// holds the field's readers to the bind's plan, init and the doctor (§3.7,
// R3), so no local probe dials the LAN address.
func TestOnlyTheBindAndItsChecksReadTheLANAddress(t *testing.T) {
	allowed := map[string]bool{
		"serve_console_lan.go": true, "init.go": true, "init_console_lan.go": true, "init_rewrite.go": true,
		"doctor_console_lan.go": true,
	}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "AdminLANAddress" && !allowed[name] {
				t.Errorf("%s reads AdminLANAddress", name)
			}
			return true
		})
	}
}

// TestTheProductionSeamsAreConsolelansAndLans: serve's default hold, Windows
// table and reading are consolelan.Hold, consolelan.HostModel and lan's
// reading, compared as functions, so consolelan's qualification is of the
// code serve runs (F8).
func TestTheProductionSeamsAreConsolelansAndLans(t *testing.T) {
	prod := consoleLANProduction(func() bool { return false })
	same := func(a, b any) bool { return reflect.ValueOf(a).Pointer() == reflect.ValueOf(b).Pointer() }
	if !same(prod.hold, consolelan.Hold) {
		t.Error("the production hold is not consolelan.Hold")
	}
	if !same(prod.hostTable, consolelan.HostModel) {
		t.Error("the production Windows table is not consolelan.HostModel")
	}
	if !same(prod.read, consoleLANReadInterfaces) {
		t.Error("the production reading is not consoleLANReadInterfaces")
	}
	if prod.listen != nil {
		t.Error("the production bind is not ListenConfig.Listen")
	}
	if prod.localName() != "" {
		t.Error("the .local name is offered while the responder does not run")
	}
	// The conversion passes a reading's failure on as it is.
	ifs, err := consoleLANInterfacesOf([]lan.Interface{{Name: "x"}}, errors.New("the listing"))
	if err == nil || ifs != nil {
		t.Fatalf("a failed reading converted to %v, %v", ifs, err)
	}
}

// boundFirstListener is a handed console listener whose address is read only
// once the box has bound its stand-in for the home-network socket, so serve's
// refusal of it comes after that bind.
type boundFirstListener struct {
	net.Listener
	bound <-chan struct{}
}

func (l boundFirstListener) Addr() net.Addr {
	select {
	case <-l.bound:
	case <-time.After(10 * time.Second):
	}
	return l.Listener.Addr()
}

// TestAHandedConsoleListenerThatIsRefusedRetiresTheLANSocket: serve refuses
// a console listener it was handed for an address other than the config's,
// and exits 1; the home-network socket, bound before the refusal (the handed
// listener's address is read only once the box's stand-in is bound), is
// retired on that way out, so once serve has returned its listener takes no
// connection (backlog B989, its second to fourth rounds). The refusal is this
// case's expected end, so it takes a path of its own, not the startup
// helpers': serve prints its banner before it reads the refusal, so the
// banner proves neither the retirement nor serve's end, and a helper reads an
// exit that follows the banner as a failed start. The case waits for serve's
// return, reads its exit code once, and only then asks the listener.
func TestAHandedConsoleListenerThatIsRefusedRetiresTheLANSocket(t *testing.T) {
	adminLn := holdAdmin(t)
	cfg := writeConsoleLANConfig(t, t.TempDir(), adminLn.Addr().String(), "adminLANAddress: \""+serveLANAddr+"\"\n")
	box := newServeBox(serveLANAddr + "/24")
	seams := box.seams(make(chan time.Time))
	bound := make(chan struct{})
	var boundOnce sync.Once
	listen := seams.listen
	seams.listen = func(ctx context.Context, lc *net.ListenConfig, network, address string) (net.Listener, error) {
		l, err := listen(ctx, lc, network, address)
		boundOnce.Do(func() { close(bound) })
		return l, err
	}
	other := boundFirstListener{Listener: holdAdmin(t), bound: bound}
	ctx, cancel := context.WithCancel(context.Background())
	stdout, stderr := &safeBuffer{}, &safeBuffer{}
	done := make(chan int, 1)
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		done <- runServe(ctx, serveOpts{configPath: cfg, adminListener: other, tailscaleCLI: noTailscaleCLI(),
			consoleLAN: seams}, stdout, stderr)
	}()
	drainServeOnCleanup(t, cancel, exited, done, stderr)
	select {
	case code := <-done:
		if code != 1 {
			t.Fatalf("serve exited %d with its console listener refused, want 1; stdout=%s\nstderr=%s",
				code, stdout.String(), stderr.String())
		}
	case <-serveGiveUp(t):
		t.Fatalf("serve did not exit with its console listener refused; stdout=%s\nstderr=%s", stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "admin server: admin listen") {
		t.Fatalf("serve did not say it refused the listener: %s", stderr.String())
	}
	box.mu.Lock()
	reals := append([]net.Listener(nil), box.reals...)
	box.mu.Unlock()
	if len(reals) != 1 {
		t.Fatalf("precondition: %d stand-ins bound before the refusal, want one", len(reals))
	}
	for _, real := range reals {
		if c, err := net.DialTimeout("tcp4", real.Addr().String(), time.Second); err == nil {
			_ = c.Close()
			t.Errorf("the home-network socket at %s takes connections after serve's way out", real.Addr())
		}
	}
}

// setMDNS rewrites the mDNS switch writeConsoleLANConfig writes.
func setMDNS(t *testing.T, cfg string, on bool) {
	t.Helper()
	raw, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Replace(string(raw), "mdns:\n  enabled: false\n", "mdns:\n  enabled: "+strconv.FormatBool(on)+"\n", 1)
	if err := os.WriteFile(cfg, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestTheConsolesFirstReadingFollowsTheResponderStart: serve makes the home-
// network socket's first attempt once the mDNS responder's start has
// answered, so the first reading holds the box's .local name exactly while
// the responder runs and the first tick of a clean start retires nothing:
// with a responder that starts, the socket answers the name from its first
// bind; with mDNS off, and with a responder that fails to start, it answers
// no name (backlog B989, its second round).
func TestTheConsolesFirstReadingFollowsTheResponderStart(t *testing.T) {
	for _, c := range []struct {
		name    string
		mdns    bool
		fails   bool
		answers bool
	}{
		{"a responder that starts", true, false, true},
		{"mDNS off", false, false, false},
		{"a responder that fails to start", true, true, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			reportHostName(t, "musicbox")
			logs := loggingtest.Record(t)
			adminLn := holdAdmin(t)
			port := lanPortOf(t, adminLn)
			cfg := writeConsoleLANConfig(t, t.TempDir(), adminLn.Addr().String(), "adminLANAddress: \""+serveLANAddr+"\"\n")
			setMDNS(t, cfg, c.mdns)
			box := newServeBox(serveLANAddr + "/24")
			tick := make(chan time.Time)
			seams := box.seams(tick)
			seams.localName = nil // production's: the box's .local name while the responder runs
			opts := serveOpts{configPath: cfg, adminListener: adminLn, tailscaleCLI: noTailscaleCLI(), consoleLAN: seams,
				mdnsPick: loopbackPick(t)}
			if c.fails {
				opts.mdnsStart = func(bridgemdns.Config) (*bridgemdns.Advertiser, error) {
					return nil, errors.New("the responder could not start")
				}
			}
			s := &lanServe{admin: adminLn.Addr().String(), cfgPath: cfg, box: box, tick: tick}
			s.served = serveWithOpts(t, opts)
			defer s.stop(t)
			// The first tick of a clean start comes once the responder's start
			// has answered, as production's comes five seconds into the start.
			if c.mdns {
				said, where := "mDNS: advertising as", s.served.stdout
				if c.fails {
					said, where = "mDNS advertise failed (non-fatal): the responder could not start", s.served.stderr
				}
				deadline := time.Now().Add(10 * time.Second)
				for !strings.Contains(where.String(), said) {
					if time.Now().After(deadline) {
						t.Fatalf("precondition: serve never said %q; stdout=%s stderr=%s", said, s.served.stdout.String(), s.served.stderr.String())
					}
					time.Sleep(10 * time.Millisecond)
				}
			}
			// Two ticks: the second is taken only once the first is done.
			for i := 0; i < 2; i++ {
				select {
				case tick <- time.Now():
				case <-time.After(10 * time.Second):
					t.Fatal("the plan's loop took no tick")
				}
			}
			if n := box.count(); n != 1 {
				t.Errorf("%d binds after the first tick of a clean start, want the first alone", n)
			}
			if lines := logs.Lines("console: the home-network socket is retired"); len(lines) != 0 {
				t.Errorf("the first tick of a clean start retired the socket: %v", lines)
			}
			st := s.console(t)
			nameURL := ""
			if c.answers {
				nameURL = "http://musicbox.local:" + port
			}
			if st.State != consolelan.StateListening || st.NameURL != nameURL {
				t.Fatalf("state %+v, want listening with the name's URL %q", st, nameURL)
			}
			resp, err := box.client(t, "192.168.88.5").Get("http://musicbox.local:" + port + "/api/stats")
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			want := http.StatusMisdirectedRequest
			if c.answers {
				want = http.StatusOK
			}
			if resp.StatusCode != want {
				t.Fatalf("the socket answered the .local name %d, want %d", resp.StatusCode, want)
			}
		})
	}
}
