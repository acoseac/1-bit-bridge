package admin

import (
	"bufio"
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/consolelan"
	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
)

// The home-network socket's lifecycle (backlog B989, its second round): every
// way out of serve retires the socket first; no socket binds while a
// retirement is under way, and the stop joins every retirement under way
// within its one deadline; the state says the socket is not listening from a
// retirement's first section, and nothing a retirement stores after it
// replaces what a later attempt said; and every snapshot read a request
// waits for ends with that request's context.

// failNextAccept makes the newest stand-in's next accept fail, and makes the
// connection it fails on: the socket's Serve returns that error.
func failNextAccept(t *testing.T, box *lanBox) {
	t.Helper()
	box.mu.Lock()
	box.failNext = true
	box.mu.Unlock()
	if c, err := box.dial(netip.MustParseAddr("192.168.77.6")); err == nil {
		_ = c.Close()
	}
}

// waitServed waits until p's Serve goroutine has returned, the failure's
// retirement it ran included.
func waitServed(t *testing.T, p *lanPolicy) {
	t.Helper()
	select {
	case <-p.served:
	case <-time.After(socketWait):
		t.Fatal("the socket's Serve, and the failure's retirement it runs, never returned")
	}
}

// TestALoopbackBindThatFailsRetiresTheLANSocket: Serve's bind of the loopback
// socket fails once the plan's first attempt has bound the home-network
// socket. Serve returns the bind's error with the socket retired, its state
// no longer listening and its listener taking no connection, within the
// console's one shutdown window though a handler it admitted never returns.
func TestALoopbackBindThatFailsRetiresTheLANSocket(t *testing.T) {
	const window = 500 * time.Millisecond
	srv, _, _ := newTestServer(t)
	box := newLANBox(t)
	held, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = held.Close() })
	next := srv.deps.CfgHolder.Clone()
	next.AdminAddress = held.Addr().String()
	srv.deps.CfgHolder.Store(next)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	plan := testLANPlan(box, make(chan time.Time), &ConsoleLANSeams{})
	plan.ShutdownWindow, plan.RetireWait = window, socketWait
	if st := srv.OpenConsoleLAN(ctx, plan); st.State != ConsoleLANListening {
		t.Fatalf("precondition: the plan's first attempt left %+v", st)
	}
	entered, release := heldDoctor(srv)
	letGoOnce(t, release)
	lis := box.current()
	conn, err := box.dial(netip.MustParseAddr("192.168.77.5"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	c := &lanClient{t: t, conn: conn, rd: bufio.NewReader(conn)}
	go func() { _, _, _ = c.get("/api/doctor") }()
	select {
	case <-entered:
	case <-time.After(socketWait):
		t.Fatal("the held handler never ran")
	}
	start := time.Now()
	serveErr := srv.Serve(ctx)
	took := time.Since(start)
	if serveErr == nil || !strings.Contains(serveErr.Error(), "admin listen") {
		t.Fatalf("Serve answered %v, want the loopback bind's failure", serveErr)
	}
	if st := srv.ConsoleLAN(); st.State == ConsoleLANListening {
		t.Fatalf("Serve returned its bind's failure with the home-network socket still %+v", st)
	}
	if again, err := net.DialTimeout("tcp4", lis.real.Addr().String(), time.Second); err == nil {
		_ = again.Close()
		t.Fatal("the home-network socket takes connections after Serve returned its bind's failure")
	}
	if took > window+window/2 {
		t.Fatalf("Serve returned after %v, past the console's one shutdown window of %v", took, window)
	}
}

// TestNoSocketBindsWhileAFailuresRetirementIsUnderWay: a socket whose Serve
// failed is retired on its Serve's goroutine; a tick while that retirement is
// held after its section binds nothing, and the first tick after it has ended
// (its Serve returned) binds the next socket.
func TestNoSocketBindsWhileAFailuresRetirementIsUnderWay(t *testing.T) {
	seams := &ConsoleLANSeams{}
	r := newLANRig(t, newLANBox(t), seams, nil)
	p := r.policy()
	marked, release := make(chan struct{}), make(chan struct{})
	letGo := letGoOnce(t, release)
	var once sync.Once
	seams.RetirementMarked = func() { once.Do(func() { close(marked); <-release }) }
	failNextAccept(t, r.box)
	select {
	case <-marked:
	case <-time.After(socketWait):
		t.Fatal("the failure's retirement never marked its policy")
	}
	r.step()
	if n := r.box.count(); n != 1 {
		t.Fatalf("%d sockets bound while the failure's retirement was under way, want the first alone", n)
	}
	letGo()
	waitServed(t, p)
	r.step()
	if st := r.state(); st.State != ConsoleLANListening || r.box.count() != 2 {
		t.Fatalf("the first tick after the retirement ended left %+v with %d binds, want listening on a second", st, r.box.count())
	}
}

// TestTheStateSaysNotListeningFromTheRetirementsFirstSection: while a
// retirement is held once its server's Close has returned, the state reads
// waiting with the retirement's own reason, whichever way out it is: a
// reading that changed (reopening), a socket that stopped serving
// (stopped_serving) and the stop (stopping).
func TestTheStateSaysNotListeningFromTheRetirementsFirstSection(t *testing.T) {
	for _, c := range []struct {
		name   string
		reason string
		begin  func(r *lanRig) (end func())
	}{
		{"a reading that changed", consolelan.ReasonReopening, func(r *lanRig) func() {
			r.box.set(enTest(9, "en-other", testLANAddr+"/24"))
			stepped := r.stepAsync()
			return func() { r.waitStep(stepped) }
		}},
		{"a socket that stopped serving", consolelan.ReasonStoppedServing, func(r *lanRig) func() {
			p := r.policy()
			failNextAccept(r.t, r.box)
			return func() { waitServed(r.t, p) }
		}},
		{"the stop", consolelan.ReasonStopping, func(r *lanRig) func() {
			r.cancel()
			return r.stop
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			seams := &ConsoleLANSeams{}
			r := newLANRig(t, newLANBox(t), seams, nil)
			closed, release := make(chan struct{}), make(chan struct{})
			letGo := letGoOnce(t, release)
			var once sync.Once
			seams.RetirementClosed = func() { once.Do(func() { close(closed); <-release }) }
			end := c.begin(r)
			select {
			case <-closed:
			case <-time.After(socketWait):
				t.Fatal("the retirement never closed its server")
			}
			st := r.state()
			letGo()
			end()
			if st.State != ConsoleLANWaiting || st.Reason != c.reason {
				t.Fatalf("while the retirement ran the state read %+v, want waiting %s", st, c.reason)
			}
		})
	}
}

// TestARetryAfterAFailuresRetirementKeepsItsOwnReason: a failure's
// retirement held once its wait has ended, and a tick that retries meanwhile
// and fails for a reason of its own (the address gone); once the retirement
// has returned, the state still says the retry's reason, never the failure's
// stopped_serving over it.
func TestARetryAfterAFailuresRetirementKeepsItsOwnReason(t *testing.T) {
	seams := &ConsoleLANSeams{}
	r := newLANRig(t, newLANBox(t), seams, nil)
	p := r.policy()
	ended, release := make(chan struct{}), make(chan struct{})
	letGo := letGoOnce(t, release)
	var once sync.Once
	seams.RetirementEnded = func() { once.Do(func() { close(ended); <-release }) }
	failNextAccept(t, r.box)
	select {
	case <-ended:
	case <-time.After(socketWait):
		t.Fatal("the failure's retirement never ended its wait")
	}
	r.box.set(enTest(7, "en-test", "192.168.77.21/24"))
	r.step()
	if st := r.state(); st.Reason != consolelan.ReasonAddressNotOnBox {
		t.Fatalf("precondition: the retry left %+v, want waiting %s", st, consolelan.ReasonAddressNotOnBox)
	}
	letGo()
	waitServed(t, p)
	if st := r.state(); st.State != ConsoleLANWaiting || st.Reason != consolelan.ReasonAddressNotOnBox {
		t.Fatalf("once the failure's retirement returned the state read %+v, want the retry's %s", st, consolelan.ReasonAddressNotOnBox)
	}
}

// TestAShutdownJoinsAFailuresRetirementUnderWay: a failure's retirement held
// at its section, its server not yet closed, a handler it admitted that never
// returns; the stop does not finish while the retirement is held, and once it
// is let go the stop finishes with the retirement's connections closed and
// its wait ended at the stop's one deadline.
func TestAShutdownJoinsAFailuresRetirementUnderWay(t *testing.T) {
	const window = 1500 * time.Millisecond
	logs := loggingtest.Record(t)
	seams := &ConsoleLANSeams{}
	r := newLANRig(t, newLANBox(t), seams, func(p *ConsoleLANPlan) { p.ShutdownWindow, p.RetireWait = window, socketWait })
	entered, releaseDoctor := heldDoctor(r.srv)
	letGoOnce(t, releaseDoctor)
	kept := r.client("192.168.77.5")
	go func() { _, _, _ = kept.get("/api/doctor") }()
	select {
	case <-entered:
	case <-time.After(socketWait):
		t.Fatal("the held handler never ran")
	}
	marked, release := make(chan struct{}), make(chan struct{})
	letGo := letGoOnce(t, release)
	var once sync.Once
	seams.RetirementMarked = func() { once.Do(func() { close(marked); <-release }) }
	failNextAccept(t, r.box)
	select {
	case <-marked:
	case <-time.After(socketWait):
		t.Fatal("the failure's retirement never marked its policy")
	}
	r.box.mu.Lock()
	r.box.events = nil
	r.box.mu.Unlock()
	start := time.Now()
	r.cancel()
	select {
	case <-r.served:
		r.stopOnce.Do(func() { r.stopped = true })
		t.Fatal("the stop finished while a failure's retirement was under way, its connections open")
	case <-time.After(300 * time.Millisecond):
	}
	letGo()
	select {
	case <-r.served:
		r.stopOnce.Do(func() { r.stopped = true })
	case <-time.After(socketWait):
		t.Fatal("the stop never finished")
	}
	took := time.Since(start)
	r.box.mu.Lock()
	events := append([]string(nil), r.box.events...)
	r.box.mu.Unlock()
	closed := false
	for _, e := range events {
		closed = closed || e == "conn closed"
	}
	if !closed {
		t.Fatalf("the stop finished with the failure's connections open: events %v", events)
	}
	if n := len(logs.Lines("console: a request of a retired home-network socket outlived its wait")); n != 1 {
		t.Fatalf("%d lines saying the retirement's wait ended at its bound, want one: the stop finished before the wait", n)
	}
	if took > window+window/2 {
		t.Fatalf("the stop took %v, past its one window of %v", took, window)
	}
}

// heldRead holds one of the console's reads until its context ends or the
// case lets it go, and records the context's error it ended on; the skip
// reads before it are refused at once.
type heldRead struct {
	in   chan struct{}
	hold chan struct{}
	skip int

	mu  sync.Mutex
	n   int
	err error
}

func newHeldRead(skip int) *heldRead {
	return &heldRead{in: make(chan struct{}, 8), hold: make(chan struct{}), skip: skip}
}

// wait is the read: a read before the one held is refused, and the held one
// waits for its context or for the case.
func (h *heldRead) wait(ctx context.Context) error {
	h.mu.Lock()
	h.n++
	n := h.n
	h.mu.Unlock()
	if n <= h.skip {
		return errors.New("refused before the read the case holds")
	}
	h.in <- struct{}{}
	select {
	case <-ctx.Done():
		h.mu.Lock()
		h.err = ctx.Err()
		h.mu.Unlock()
		return ctx.Err()
	case <-h.hold:
		return errors.New("let go by the case")
	}
}

func (h *heldRead) endedOn() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.err
}

// heldTailscale is a Tailscale provider whose status read is h's: Status
// waits for the case alone, StatusContext for its context too.
type heldTailscale struct{ h *heldRead }

func (p heldTailscale) Status() TailscaleStatus {
	_ = p.h.wait(context.Background())
	return TailscaleStatus{}
}

func (p heldTailscale) StatusContext(ctx context.Context) TailscaleStatus {
	_ = p.h.wait(ctx)
	return TailscaleStatus{}
}

func (heldTailscale) RefreshNow(context.Context) TailscaleStatus { return TailscaleStatus{} }

// TestEachSnapshotReadEndsWithItsRequestsContext: the stats reads, the
// sources' cold read and the tailnet's status, each held in an event stream
// and in its REST route, end with the request's context, which the
// retirement's section cancels: the request ends there, before its
// connection is closed, and never merely at the read's own timeout.
func TestEachSnapshotReadEndsWithItsRequestsContext(t *testing.T) {
	stats := func(srv *Server, h *heldRead) { srv.testStatsRead = h.wait }
	tailnet := func(srv *Server, h *heldRead) { srv.deps.Tailscale = heldTailscale{h} }
	for _, c := range []struct {
		name string
		path string // the REST route, or "" for the event stream
		skip int
		wire func(*Server, *heldRead)
	}{
		{"the stats read in the event stream", "", 0, stats},
		{"the stats read in GET /api/stats", "/api/stats", 0, stats},
		// The stream's stats read refused, so its sources read is the cold one.
		{"the sources' cold read in the event stream", "", 1, stats},
		{"the sources' cold read in GET /api/sources", "/api/sources", 0, stats},
		{"the tailnet's status in the event stream", "", 0, tailnet},
		{"the tailnet's status in GET /api/tailscale/status", "/api/tailscale/status", 0, tailnet},
	} {
		t.Run(c.name, func(t *testing.T) {
			seams := &ConsoleLANSeams{}
			r := newLANRig(t, newLANBox(t), seams, func(p *ConsoleLANPlan) { p.RetireWait = socketWait })
			h := newHeldRead(c.skip)
			letGoRead := letGoOnce(t, h.hold)
			c.wire(r.srv, h)
			released := make(chan int, 16)
			seams.Released = func(left int) { released <- left }
			client := r.client("192.168.77.5")
			if c.path == "" {
				_ = openStream(t, client)
			} else {
				go func() { _, _, _ = client.get(c.path) }()
			}
			select {
			case <-h.in:
			case <-time.After(socketWait):
				t.Fatal("the request never reached the read the case holds")
			}
			marked, release := make(chan struct{}), make(chan struct{})
			letGo := letGoOnce(t, release)
			var markOnce sync.Once
			seams.RetirementMarked = func() { markOnce.Do(func() { close(marked); <-release }) }
			r.box.set(enTest(9, "en-other", testLANAddr+"/24"))
			stepped := r.stepAsync()
			select {
			case <-marked:
			case <-time.After(socketWait):
				t.Fatal("the retirement never marked its policy")
			}
			endedAtMark := false
			select {
			case left := <-released:
				endedAtMark = left == 0
			case <-time.After(time.Second):
			}
			err := h.endedOn()
			letGoRead()
			letGo()
			r.waitStep(stepped)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("the held read ended on %v, want its request's context, which the retirement cancels", err)
			}
			if !endedAtMark {
				t.Fatal("the request did not end at the retirement's section, before its connection's close")
			}
		})
	}
}

// TestAWindowsTableThatFailsOrMissesARowClosesTheSocketUntilAWholeReading:
// on the strong host model's hold, a table whose read fails and a table with
// no row for an interface that holds an IPv4 address each close the
// listening socket and its kept-alive connection at the next reading, and the
// next whole reading binds again; the loopback socket answers throughout.
func TestAWindowsTableThatFailsOrMissesARowClosesTheSocketUntilAWholeReading(t *testing.T) {
	box := newLANBox(t)
	box.table = consolelan.Table{{Index: 7}}
	r := newLANRig(t, box, nil, func(p *ConsoleLANPlan) { p.GOOS = "windows" })
	loopbackUp := func(when string) {
		t.Helper()
		resp, err := http.Get("http://" + r.loop.Addr().String() + "/api/console")
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("the loopback socket %s: %v %v", when, resp, err)
		}
		_ = resp.Body.Close()
	}
	answered := func(when string) *lanClient {
		t.Helper()
		if st := r.state(); st.State != ConsoleLANListening || st.Hold != consolelan.HoldStrongHost {
			t.Fatalf("%s: state %+v, want listening on the strong host model", when, st)
		}
		c := r.client("192.168.77.5")
		if resp, _, err := c.get("/api/console"); err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: %v %v", when, resp, err)
		}
		loopbackUp(when)
		return c
	}
	closedFor := func(kept *lanClient, reason, when string) {
		t.Helper()
		if st := r.state(); st.State != ConsoleLANWaiting || st.Reason != reason {
			t.Fatalf("%s: state %+v, want waiting %s", when, st, reason)
		}
		if resp, _, err := kept.get("/api/console"); err == nil {
			t.Fatalf("%s: the kept-alive connection was answered %d", when, resp.StatusCode)
		}
		loopbackUp(when)
	}
	kept := answered("before")
	box.mu.Lock()
	box.tableErr = errors.New("the stack did not answer")
	box.mu.Unlock()
	r.step()
	closedFor(kept, consolelan.ReasonHostModelUnreadable, "a table whose read fails")
	box.mu.Lock()
	box.tableErr = nil
	box.mu.Unlock()
	r.step()
	kept = answered("a whole reading after a table that failed")
	box.set(enTest(7, "en-test", testLANAddr+"/24"), enTest(12, "en-two", "10.1.2.3/24"))
	r.step()
	closedFor(kept, consolelan.ReasonInterfacesUnreadable, "an interface holding an IPv4 address with no row")
	box.mu.Lock()
	box.table = consolelan.Table{{Index: 7}, {Index: 12}}
	box.mu.Unlock()
	r.step()
	answered("a whole reading once the row is there")
}

// TestAnAddressHeldTwiceOnOneInterfaceIsSaidSo: A held twice on one
// interface (A/24 and A/32) binds nothing, address_on_two_interfaces, whose
// words and hint say the address is held more than once, twice on one
// interface or on two, never that two interfaces hold it.
func TestAnAddressHeldTwiceOnOneInterfaceIsSaidSo(t *testing.T) {
	box := newLANBox(t)
	in := enTest(7, "en-test", testLANAddr+"/24")
	in.Addrs = append(in.Addrs, netip.MustParsePrefix(testLANAddr+"/32"))
	box.set(in)
	r := newLANRigAnyPlan(t, box, nil)
	st := r.state()
	if st.State != ConsoleLANWaiting || st.Reason != consolelan.ReasonAddressOnTwo || r.box.count() != 0 {
		t.Fatalf("state %+v with %d binds, want waiting %s and none", st, r.box.count(), consolelan.ReasonAddressOnTwo)
	}
	words := consolelan.ReasonWords(st.Reason, st.Address, 7789, st.Interface)
	if !strings.Contains(words, "more than once") || !strings.Contains(words, "twice on one") ||
		!strings.Contains(words, "on two interfaces") {
		t.Fatalf("the words %q do not say the address is held more than once, twice on one interface or on two", words)
	}
	if hint := consolelan.ReasonHint(st.Reason, st.Interface); !strings.Contains(hint, "once") {
		t.Fatalf("the hint %q does not say to hold the address once", hint)
	}
}

// TestAPlanBetweenItsOpeningAndItsFirstAttemptSaysTheBridgeIsStarting: a
// plan opened and not yet begun (serve's mDNS start lies between the two)
// reads waiting, starting, in words of its own, and has bound nothing; the
// first attempt says what it found, and a second makes none; a plan whose
// stop came first binds nothing at its first attempt.
func TestAPlanBetweenItsOpeningAndItsFirstAttemptSaysTheBridgeIsStarting(t *testing.T) {
	t.Run("begun", func(t *testing.T) {
		srv, _, _ := newTestServer(t)
		box := newLANBox(t)
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		srv.PrepareConsoleLAN(ctx, testLANPlan(box, make(chan time.Time), &ConsoleLANSeams{}))
		t.Cleanup(srv.AbortConsoleLAN)
		if st := srv.ConsoleLAN(); st.State != ConsoleLANWaiting || st.Reason != consolelan.ReasonStarting || box.count() != 0 {
			t.Fatalf("a plan opened and not begun reads %+v with %d binds, want waiting %s and none", st, box.count(), consolelan.ReasonStarting)
		}
		if words := consolelan.ReasonWords(consolelan.ReasonStarting, testLANAddr, 7789, ""); words != "the bridge is starting" {
			t.Fatalf("the starting reason's words are %q", words)
		}
		if st := srv.BeginConsoleLAN(ctx); st.State != ConsoleLANListening || box.count() != 1 {
			t.Fatalf("the first attempt left %+v with %d binds, want listening on one", st, box.count())
		}
		srv.BeginConsoleLAN(ctx)
		if n := box.count(); n != 1 {
			t.Fatalf("a second BeginConsoleLAN bound again: %d binds", n)
		}
	})
	t.Run("stopped first", func(t *testing.T) {
		srv, _, _ := newTestServer(t)
		box := newLANBox(t)
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		srv.PrepareConsoleLAN(ctx, testLANPlan(box, make(chan time.Time), &ConsoleLANSeams{}))
		srv.AbortConsoleLAN()
		if st := srv.BeginConsoleLAN(ctx); st.State != ConsoleLANWaiting || st.Reason != consolelan.ReasonStopping || box.count() != 0 {
			t.Fatalf("a first attempt after the stop left %+v with %d binds, want waiting %s and none", st, box.count(), consolelan.ReasonStopping)
		}
	})
}
