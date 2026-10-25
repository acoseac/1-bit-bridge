package openhome

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
	"github.com/acoseac/1-bit-bridge/internal/player/lan"
	"github.com/acoseac/1-bit-bridge/internal/player/lan/lantest"
)

// The box the listener tests serve: eth0's address and a secondary one on
// it, eth1's, and a link-local one on a direct cable.
var (
	eth0  = lantest.Address("192.168.1.20/24", "eth0", 2)
	eth0b = lantest.Address("10.0.0.2/8", "eth0", 2)
	eth1  = lantest.Address("172.16.5.1/16", "eth1", 3)
	cable = lantest.Address("169.254.7.7/16", "eth2", 4)
)

// A source on eth0's link, and two more beside it.
var (
	phone  = netip.MustParseAddr("192.168.1.50")
	tablet = netip.MustParseAddr("192.168.1.51")
	laptop = netip.MustParseAddr("192.168.1.52")
)

// startOn starts a surface on box with lim and seams, stopped at the
// test's end.
func startOn(t *testing.T, box *lantest.Box, port int, lim Limits, seams *Seams) *Surface {
	t.Helper()
	s, err := Start(Config{Device: Device{UUID: testUUID, ModelNumber: "test"}, Port: port,
		Addresses: box.Addresses, Listen: box.Listen, Limits: lim, Seams: seams})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Stop() })
	return s
}

// within is a channel that fires after d, or at the test's deadline less a
// second, whichever is first.
func within(t *testing.T, d time.Duration) <-chan time.Time {
	if dl, ok := t.Deadline(); ok {
		if left := time.Until(dl) - time.Second; left < d {
			d = left
		}
	}
	return time.After(d)
}

// awaitStatus waits until cond holds of s's Status, at most bound.
func awaitStatus(t *testing.T, s *Surface, bound time.Duration, what string, cond func(Status) bool) Status {
	t.Helper()
	give := within(t, bound)
	for {
		ch := s.Changed()
		st := s.Status()
		if cond(st) {
			return st
		}
		select {
		case <-ch:
		case <-give:
			t.Fatalf("waiting %v for the surface: %s; it is %+v", bound, what, st)
		}
	}
}

// boundOn is the address and port a's socket is bound on in st.
func boundOn(st Status, a lan.Address) (netip.AddrPort, bool) {
	for _, l := range st.Listening {
		if l.Address == a {
			return l.Bound, true
		}
	}
	return netip.AddrPort{}, false
}

// listening waits until s listens on every address of want and on none
// other, and returns where each is bound.
func listening(t *testing.T, s *Surface, want ...lan.Address) map[lan.Address]netip.AddrPort {
	t.Helper()
	st := awaitStatus(t, s, 30*time.Second, fmt.Sprintf("listening on %v alone", want), func(st Status) bool {
		if len(st.Listening) != len(want) {
			return false
		}
		for _, a := range want {
			if _, ok := boundOn(st, a); !ok {
				return false
			}
		}
		return true
	})
	out := map[lan.Address]netip.AddrPort{}
	for _, a := range want {
		out[a], _ = boundOn(st, a)
	}
	return out
}

// get sends a GET of the device description on c with host, and reads the
// answer: its status, or 0 and the error when the connection gave none.
func get(c net.Conn, br *bufio.Reader, host string) (int, error) {
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	defer func() { _ = c.SetDeadline(time.Time{}) }()
	if _, err := io.WriteString(c, "GET /openhome/description.xml HTTP/1.1\r\nHost: "+host+"\r\n\r\n"); err != nil {
		return 0, err
	}
	res, err := http.ReadResponse(br, nil)
	if err != nil {
		return 0, err
	}
	_, _ = io.Copy(io.Discard, res.Body)
	_ = res.Body.Close()
	return res.StatusCode, nil
}

// refusedUnread reports whether c was closed by the surface with nothing
// sent: its read ends, at once, with no byte.
func refusedUnread(t *testing.T, c net.Conn) bool {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	_, _ = io.WriteString(c, "GET /openhome/description.xml HTTP/1.1\r\nHost: x\r\n\r\n")
	b, err := io.ReadAll(c)
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return false
	}
	return len(b) == 0
}

// TestTheSurfaceBindsEachServedAddressAndFollowsThem: a socket for each
// served address, on the configured port; an address that appears is
// bound; one that goes away is closed, its connections with it; one whose
// prefix changes, or whose interface does, is closed and bound again; a
// listing that fails keeps what is bound.
func TestTheSurfaceBindsEachServedAddressAndFollowsThem(t *testing.T) {
	box := lantest.New(t)
	box.Set([]lan.Address{eth0, eth0b})
	s := startOn(t, box, 7792, Limits{}, nil)
	bound := listening(t, s, eth0, eth0b)
	for _, want := range []string{"192.168.1.20:7792", "10.0.0.2:7792"} {
		if !slices.Contains(box.Listens(), netip.MustParseAddrPort(want)) {
			t.Errorf("no socket was asked for %s: %v", want, box.Listens())
		}
	}

	box.Set([]lan.Address{eth0, eth0b, cable})
	s.Rescan()
	bound = listening(t, s, eth0, eth0b, cable)

	// A connection held on the secondary address, kept alive.
	kept, err := box.Dial(bound[eth0b], netip.MustParseAddr("10.0.0.9"))
	if err != nil {
		t.Fatal(err)
	}
	defer kept.Close()
	br := bufio.NewReader(kept)
	if code, err := get(kept, br, bound[eth0b].String()); code != 200 {
		t.Fatalf("a source on the secondary address's link: %d, %v", code, err)
	}
	box.Set([]lan.Address{eth0, cable})
	s.Rescan()
	listening(t, s, eth0, cable)
	if slices.Contains(box.Open(), bound[eth0b]) {
		t.Errorf("the socket of an address that went away is open: %v", box.Open())
	}
	if code, err := get(kept, br, bound[eth0b].String()); err == nil {
		t.Errorf("a connection on an address that went away was answered %d", code)
	}

	// The prefix changes: a socket for the new one, and the old one closed.
	eth0wide := lantest.Address("192.168.1.20/16", "eth0", 2)
	box.Set([]lan.Address{eth0wide, cable})
	s.Rescan()
	now := listening(t, s, eth0wide, cable)
	if slices.Contains(box.Open(), bound[eth0]) || !slices.Contains(box.Open(), now[eth0wide]) {
		t.Errorf("after the prefix changed, the open sockets are %v (was %s, now %s)", box.Open(), bound[eth0], now[eth0wide])
	}
	// The interface is made again under another index.
	eth0again := lantest.Address("192.168.1.20/16", "eth0", 9)
	box.Set([]lan.Address{eth0again, cable})
	s.Rescan()
	again := listening(t, s, eth0again, cable)
	if slices.Contains(box.Open(), now[eth0wide]) {
		t.Errorf("the socket of the interface's old index is open: %v", box.Open())
	}

	box.FailListing(errors.New("netlink refused"))
	s.Rescan()
	st := awaitStatus(t, s, 30*time.Second, "the listing's failure", func(st Status) bool { return st.ListFailure != "" })
	if b, ok := boundOn(st, eth0again); !ok || b != again[eth0again] || len(st.Listening) != 2 {
		t.Errorf("a listing that failed changed what is bound: %+v", st.Listening)
	}
	box.Set([]lan.Address{eth0again, cable})
	s.Rescan()
	awaitStatus(t, s, 30*time.Second, "a listing that reads again", func(st Status) bool { return st.ListFailure == "" })
}

// TestABindFailureFailsThatAddressAlone: an address whose bind fails (its
// port taken) is said once, the others served, and is bound once it can
// be.
func TestABindFailureFailsThatAddressAlone(t *testing.T) {
	rec := loggingtest.Record(t)
	box := lantest.New(t)
	box.FailListen(eth0b.Prefix.Addr(), errors.New("bind: address already in use"))
	box.Set([]lan.Address{eth0, eth0b})
	s := startOn(t, box, 7792, Limits{}, nil)
	st := awaitStatus(t, s, 30*time.Second, "eth0 bound and the secondary failed", func(st Status) bool {
		_, ok := boundOn(st, eth0)
		return ok && len(st.Listening) == 1 && len(st.Failed) == 1
	})
	if st.Failed[0].Address != eth0b || !strings.Contains(st.Failed[0].Err, "address already in use") {
		t.Errorf("failed %+v", st.Failed)
	}
	for i := 0; i < 3; i++ {
		n := s.Status().Reconciles
		s.Rescan()
		awaitStatus(t, s, 30*time.Second, "another listing", func(st Status) bool { return st.Reconciles > n })
	}
	const says = "player: the OpenHome surface could not listen on an address: the surface fails there alone, the player, the library and the rest of serve are unaffected, and it tries again at the next listing"
	if lines := rec.Lines(says); len(lines) != 1 || !strings.Contains(lines[0], "10.0.0.2:7792") {
		t.Errorf("the failure logged %q, want once, naming the address", lines)
	}
	box.FailListen(eth0b.Prefix.Addr(), nil)
	s.Rescan()
	listening(t, s, eth0, eth0b)
	if st := s.Status(); len(st.Failed) != 0 {
		t.Errorf("a bound address is still failed: %+v", st.Failed)
	}
}

// TestAdmissionRefusesAtAcceptAndCountsNothing: a connection from a
// source that is not approved is closed at accept, unread and unanswered,
// and counts against no cap; an approved source is served. Approved is an
// IPv4 address in the on-link prefix of the address the connection arrived
// on, of the LAN's classes, never loopback, never one of the box's own.
func TestAdmissionRefusesAtAcceptAndCountsNothing(t *testing.T) {
	box := lantest.New(t)
	// 192.168.9.1/8 is a mask a box was given by mistake: its prefix is
	// wider than its address's class, and holds public addresses.
	wide := lantest.Address("192.168.9.1/8", "eth3", 5)
	box.Set([]lan.Address{eth0, eth0b, eth1, wide}, netip.MustParseAddr("192.168.1.21"), netip.MustParseAddr("127.0.0.1"))
	s := startOn(t, box, 7792, Limits{}, nil)
	bound := listening(t, s, eth0, eth0b, eth1, wide)
	for _, tc := range []struct {
		name string
		to   lan.Address
		from string
		why  string
	}{
		{"a source in another prefix", eth0, "192.168.2.50", refusedOutsidePrefix},
		{"a source in the secondary address's prefix", eth0, "10.0.0.9", refusedOutsidePrefix},
		{"a source in another interface's prefix", eth0, "172.16.5.9", refusedOutsidePrefix},
		{"loopback", eth0, "127.0.0.1", refusedLoopback},
		{"another of the box's own addresses", eth0, "192.168.1.21", refusedOwnAddress},
		{"the address it arrived on", eth0, "192.168.1.20", refusedOwnAddress},
		{"the secondary address itself", eth0, "10.0.0.2", refusedOwnAddress},
		{"an IPv6 source", eth0, "fe80::1", refusedNotIPv4},
		{"a public source in a prefix wider than its class", wide, "192.169.0.5", refusedNotLAN},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := s.Status()
			c, err := box.Dial(bound[tc.to], netip.MustParseAddr(tc.from))
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			if !refusedUnread(t, c) {
				t.Fatalf("the connection from %s was answered", tc.from)
			}
			st := awaitStatus(t, s, 30*time.Second, "the refusal counted", func(st Status) bool { return st.Refused[tc.why] == before.Refused[tc.why]+1 })
			if st.Open != 0 || len(st.BySource) != 0 {
				t.Errorf("a refused connection was counted: %d open, %v", st.Open, st.BySource)
			}
		})
	}
	c, err := box.Dial(bound[eth0], phone)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	br := bufio.NewReader(c)
	if code, err := get(c, br, bound[eth0].String()); code != 200 {
		t.Fatalf("an approved source: %d, %v", code, err)
	}
	if st := s.Status(); st.Open != 1 || st.BySource[phone] != 1 {
		t.Errorf("an approved connection: %d open, %v", st.Open, st.BySource)
	}
	// Row 1's second half, on that admitted connection: a Host that names
	// another address of the box, or this one without its port, is 400.
	for _, host := range []string{bound[eth0b].String(), "192.168.1.20", "localhost:" + fmt.Sprint(bound[eth0].Port())} {
		if code, err := get(c, br, host); code != 400 {
			t.Errorf("Host %s: %d, %v; want 400", host, code, err)
		}
		c, err = box.Dial(bound[eth0], phone)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		br = bufio.NewReader(c)
	}
}

// fakeConn is a connection with the addresses the test gives it.
type fakeConn struct {
	net.Conn
	local, remote netip.AddrPort
}

func (c fakeConn) LocalAddr() net.Addr  { return net.TCPAddrFromAddrPort(c.local) }
func (c fakeConn) RemoteAddr() net.Addr { return net.TCPAddrFromAddrPort(c.remote) }

// TestAConnectionOnAnAddressNoLongerServedIsRefused: admission asks whether
// the address a connection arrived on is served now, and is the socket's:
// one taken in the moment between a listing that dropped the address and
// the socket's close is refused, and so is one whose local address is not
// the socket's.
func TestAConnectionOnAnAddressNoLongerServedIsRefused(t *testing.T) {
	g := newGate(DefaultLimits.orDefaults(), slog.New(slog.DiscardHandler), func() {})
	on := fakeConn{local: netip.MustParseAddrPort("192.168.1.20:7792"), remote: netip.MustParseAddrPort("192.168.1.50:40000")}
	g.set([]lan.Address{eth0}, nil)
	if src, why := g.admit(eth0, on); why != "" {
		t.Fatalf("a served address refused its source: %s", why)
	} else {
		g.release(src)
	}
	g.set(nil, nil)
	if _, why := g.admit(eth0, on); why != refusedDestination {
		t.Errorf("an address no longer served: %q, want %s", why, refusedDestination)
	}
	other := lantest.Address("192.168.1.21/24", "eth0", 2)
	g.set([]lan.Address{eth0, other}, nil)
	if _, why := g.admit(other, on); why != refusedDestination {
		t.Errorf("a connection whose local address is not its socket's: %q, want %s", why, refusedDestination)
	}
	if open, _, refused := g.counts(); open != 0 || refused[refusedDestination] != 2 {
		t.Errorf("%d open, refused %v", open, refused)
	}
}

// TestTheCapsCountOnlyAdmittedConnections: refused sources, more of them
// than the global cap, count against nothing, and an approved source is
// served after them; then the per-source cap refuses a source's third
// connection and the global one a third source's, until one closes.
func TestTheCapsCountOnlyAdmittedConnections(t *testing.T) {
	box := lantest.New(t)
	box.Set([]lan.Address{eth0})
	s := startOn(t, box, 7792, Limits{MaxConns: 3, MaxConnsPerSource: 2}, nil)
	at := listening(t, s, eth0)[eth0]
	for i := 0; i < 5; i++ {
		c, err := box.Dial(at, netip.MustParseAddr("192.168.2.50"))
		if err != nil {
			t.Fatal(err)
		}
		if !refusedUnread(t, c) {
			t.Fatalf("refused source %d was answered", i)
		}
		_ = c.Close()
	}
	awaitStatus(t, s, 30*time.Second, "five refusals, none counted", func(st Status) bool {
		return st.Refused[refusedOutsidePrefix] == 5 && st.Open == 0
	})
	served := func(from netip.Addr) net.Conn {
		t.Helper()
		c, err := box.Dial(at, from)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		if code, err := get(c, bufio.NewReader(c), at.String()); code != 200 {
			t.Fatalf("from %s: %d, %v; want served", from, code, err)
		}
		return c
	}
	refused := func(from netip.Addr, why string) {
		t.Helper()
		before := s.Status().Refused[why]
		c, err := box.Dial(at, from)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		if !refusedUnread(t, c) {
			t.Fatalf("from %s: answered past the cap", from)
		}
		awaitStatus(t, s, 30*time.Second, "the cap's refusal counted", func(st Status) bool { return st.Refused[why] == before+1 })
	}
	first := served(phone)
	served(phone)
	refused(phone, refusedSourceCap)
	served(tablet)
	refused(laptop, refusedGlobalCap)
	_ = first.Close()
	awaitStatus(t, s, 30*time.Second, "a closed connection released", func(st Status) bool { return st.Open == 2 })
	served(laptop)
}

// TestATrickledBodyMeetsItsDeadlineHoldingNoSlot: a client that sends its
// body a byte at a time is answered 408 at the body's own deadline and its
// connection closed, and while it trickles it holds no slot: with one slot,
// another client's request is answered (fault 501: the surface has no
// engine; 602 until B470 PR 5a4), never 503.
func TestATrickledBodyMeetsItsDeadlineHoldingNoSlot(t *testing.T) {
	box := lantest.New(t)
	box.Set([]lan.Address{eth0})
	s := startOn(t, box, 7792, Limits{Slots: 1, BodyRead: 400 * time.Millisecond}, nil)
	at := listening(t, s, eth0)[eth0]
	tr, err := box.Dial(at, phone)
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	pl := servicesByPath["playlist"]
	if _, err := io.WriteString(tr, "POST /openhome/playlist/control HTTP/1.1\r\nHost: "+at.String()+
		"\r\nContent-Type: text/xml\r\nSOAPAction: "+header(pl, "Play")+"\r\nContent-Length: 2000\r\n\r\n<"); err != nil {
		t.Fatal(err)
	}
	stopTrickle := make(chan struct{})
	defer close(stopTrickle)
	go func() {
		tick := time.NewTicker(40 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stopTrickle:
				return
			case <-tick.C:
				if _, err := io.WriteString(tr, " "); err != nil {
					return
				}
			}
		}
	}()
	awaitStatus(t, s, 30*time.Second, "the trickler admitted", func(st Status) bool { return st.Open == 1 })
	client := box.Client(tablet)
	req, err := http.NewRequest(http.MethodPost, "http://"+at.String()+"/openhome/playlist/control", strings.NewReader(envelope(call(pl, "Play", ""))))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "text/xml")
	req.Header.Set("SOAPAction", header(pl, "Play"))
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if res.StatusCode != 500 || !strings.Contains(string(body), "<errorCode>501</errorCode>") {
		t.Errorf("beside a trickler, with one slot: %d %s; want fault 501", res.StatusCode, body)
	}
	// Well inside the request's whole deadline (Limits.Read, 10 s), so the
	// answer is the body's own deadline's.
	_ = tr.SetReadDeadline(time.Now().Add(5 * time.Second))
	got, err := http.ReadResponse(bufio.NewReader(tr), nil)
	if err != nil {
		t.Fatalf("the trickler got no answer within 5 s: %v", err)
	}
	if got.StatusCode != http.StatusRequestTimeout {
		t.Errorf("the trickler got %d, want 408", got.StatusCode)
	}
	awaitStatus(t, s, 30*time.Second, "the trickler's connection closed", func(st Status) bool { return st.BySource[phone] == 0 })
}

// TestAClientThatStopsReadingIsCutOffByTheWriteDeadline: a client that
// pipelines requests and reads none of the answers, with small socket
// buffers, holds the server's writes; the write deadline ends them and the
// connection, which nothing else does (no read deadline runs while a
// response is written).
func TestAClientThatStopsReadingIsCutOffByTheWriteDeadline(t *testing.T) {
	box := lantest.New(t)
	box.Buffers(4096, 4096)
	box.Set([]lan.Address{eth0})
	s := startOn(t, box, 7792, Limits{Write: 300 * time.Millisecond}, nil)
	at := listening(t, s, eth0)[eth0]
	c, err := box.Dial(at, phone)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	go func() {
		req := "GET /openhome/description.xml HTTP/1.1\r\nHost: " + at.String() + "\r\n\r\n"
		for i := 0; i < 4000; i++ {
			if _, err := io.WriteString(c, req); err != nil {
				return
			}
		}
	}()
	awaitStatus(t, s, 30*time.Second, "the reader admitted", func(st Status) bool { return st.Open == 1 })
	awaitStatus(t, s, 15*time.Second, "the connection that stopped reading cut off", func(st Status) bool { return st.Open == 0 })
}

// TestTheSlotsAnswer503AtOncePastThem: with every slot held, a request is
// answered 503 at once; once one is free, requests are answered again.
func TestTheSlotsAnswer503AtOncePastThem(t *testing.T) {
	hold := make(chan struct{})
	entered := make(chan struct{}, 8)
	var once sync.Once
	release := func() { once.Do(func() { close(hold) }) }
	defer release()
	seams := &Seams{Handling: func(*http.Request) {
		entered <- struct{}{}
		<-hold
	}}
	box := lantest.New(t)
	box.Set([]lan.Address{eth0})
	s := startOn(t, box, 7792, Limits{Slots: 2}, seams)
	at := listening(t, s, eth0)[eth0]
	url := "http://" + at.String() + "/openhome/description.xml"
	results := make(chan int, 2)
	for _, from := range []netip.Addr{phone, tablet} {
		client := box.Client(from)
		go func() {
			res, err := client.Get(url)
			if err != nil {
				results <- 0
				return
			}
			_ = res.Body.Close()
			results <- res.StatusCode
		}()
	}
	for i := 0; i < 2; i++ {
		select {
		case <-entered:
		case <-within(t, 30*time.Second):
			t.Fatal("two requests did not take the slots")
		}
	}
	res, err := box.Client(laptop).Get(url)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("a request past the slots: %d, want 503", res.StatusCode)
	}
	release()
	for i := 0; i < 2; i++ {
		if code := <-results; code != 200 {
			t.Errorf("a request that held a slot: %d", code)
		}
	}
	res, err = box.Client(laptop).Get(url)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != 200 {
		t.Errorf("with the slots free: %d", res.StatusCode)
	}
}

// TestStopRefusesThenShutsDownThenCloses: Stop refuses every request after
// it begins (503, the connection closed), then shuts the sockets down,
// waiting at most its grace for a request under way, then closes that
// request's connection, then joins what it started, abandoning a handler
// that does not return within its bound and saying so. Afterwards no
// socket is open, and once the handler returns nothing of the surface
// runs.
func TestStopRefusesThenShutsDownThenCloses(t *testing.T) {
	var mu sync.Mutex
	var order []string
	note := func(s string) {
		mu.Lock()
		defer mu.Unlock()
		order = append(order, s)
	}
	held := make(chan struct{})
	entered := make(chan struct{})
	var holdOnce sync.Once
	box := lantest.New(t)
	var kept net.Conn
	var keptReader *bufio.Reader
	var at netip.AddrPort
	seams := &Seams{
		Handling: func(r *http.Request) {
			if r.Header.Get("X-Hold") != "" {
				holdOnce.Do(func() { close(entered) })
				<-held
			}
		},
		Stopping: func() {
			note("refusing")
			_, _ = io.WriteString(kept, "GET /openhome/description.xml HTTP/1.1\r\nHost: "+at.String()+"\r\n\r\n")
			res, err := http.ReadResponse(keptReader, nil)
			if err != nil {
				note("no answer: " + err.Error())
				return
			}
			_ = res.Body.Close()
			// ReadResponse takes Connection: close out of the header into Close.
			note(fmt.Sprintf("answered %d, closing %v", res.StatusCode, res.Close))
		},
		Forcing: func() { note("closing what is open") },
	}
	box.Set([]lan.Address{eth0})
	s := startOn(t, box, 7792, Limits{ShutdownGrace: 300 * time.Millisecond, Join: 300 * time.Millisecond}, seams)
	at = listening(t, s, eth0)[eth0]
	var err error
	kept, err = box.Dial(at, phone)
	if err != nil {
		t.Fatal(err)
	}
	defer kept.Close()
	keptReader = bufio.NewReader(kept)
	if code, err := get(kept, keptReader, at.String()); code != 200 {
		t.Fatalf("the kept connection: %d, %v", code, err)
	}
	hc, err := box.Dial(at, tablet)
	if err != nil {
		t.Fatal(err)
	}
	defer hc.Close()
	if _, err := io.WriteString(hc, "GET /openhome/description.xml HTTP/1.1\r\nHost: "+at.String()+"\r\nX-Hold: 1\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-within(t, 30*time.Second):
		t.Fatal("the held request never reached its handler")
	}
	rep := s.Stop()
	mu.Lock()
	got := slices.Clone(order)
	mu.Unlock()
	want := []string{"refusing", "answered 503, closing true", "closing what is open"}
	if !slices.Equal(got, want) {
		t.Errorf("the stop went %q, want %q", got, want)
	}
	if !rep.Forced || rep.Abandoned != 1 {
		t.Errorf("the report %+v, want forced and one handler abandoned", rep)
	}
	_ = hc.SetReadDeadline(time.Now().Add(10 * time.Second))
	if b, err := io.ReadAll(hc); len(b) != 0 || isTimeout(err) {
		t.Errorf("the held request's connection: %q, %v; want it closed with nothing sent", b, err)
	}
	if open := box.Open(); len(open) != 0 {
		t.Errorf("sockets open after the stop: %v", open)
	}
	if _, err := box.Dial(at, phone); err == nil {
		t.Errorf("a socket of the stopped surface is open")
	}
	close(held)
	for s.h.flight.running() != 0 {
		select {
		case <-within(t, 10*time.Second):
			t.Fatal("the held handler never returned")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if st := s.Status(); !st.Stopped || st.Open != 0 {
		t.Errorf("after the stop: %+v", st)
	}
}

// TestAStopWithNothingUnderWayIsQuick: nothing to wait for, nothing
// forced or abandoned, and a second Stop answers the first's report.
func TestAStopWithNothingUnderWayIsQuick(t *testing.T) {
	box := lantest.New(t)
	box.Set([]lan.Address{eth0, cable})
	s := startOn(t, box, 7792, Limits{}, nil)
	listening(t, s, eth0, cable)
	rep := s.Stop()
	if rep.Forced || rep.Abandoned != 0 || rep.Took >= DefaultLimits.ShutdownGrace {
		t.Errorf("the report %+v", rep)
	}
	if again := s.Stop(); again != rep {
		t.Errorf("a second Stop: %+v, want the first's %+v", again, rep)
	}
	if open := box.Open(); len(open) != 0 {
		t.Errorf("sockets open after the stop: %v", open)
	}
}

// TestEveryURLCarriesThePortTheRequestArrivedOn: with port 0, each socket
// is asked for a port the kernel picks, and the description a request
// gets names the address and the port it arrived on, the bound one, in
// every URL.
func TestEveryURLCarriesThePortTheRequestArrivedOn(t *testing.T) {
	box := lantest.New(t)
	box.Set([]lan.Address{eth0, cable})
	s := startOn(t, box, 0, Limits{}, nil)
	bound := listening(t, s, eth0, cable)
	for _, at := range box.Listens() {
		if at.Port() != 0 {
			t.Errorf("a socket was asked for port %d, want 0", at.Port())
		}
	}
	for a, at := range bound {
		from := phone
		if a == cable {
			from = netip.MustParseAddr("169.254.9.9")
		}
		res, err := box.Client(from).Get("http://" + at.String() + "/openhome/description.xml")
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(res.Body)
		_ = res.Body.Close()
		d := readDevice(t, raw)
		for _, svc := range d.Device.Services {
			for _, u := range []string{svc.SCPDURL, svc.Control, svc.EventSub} {
				if !strings.HasPrefix(u, "http://"+at.String()+"/openhome/") {
					t.Errorf("on %s a URL is %s", at, u)
				}
			}
		}
	}
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// TestIdleSocketsNeverMakeACleanStopForced: repeated stops with completed
// requests must not mistake an already-closed listener for an expired grace.
func TestIdleSocketsNeverMakeACleanStopForced(t *testing.T) {
	for i := 0; i < 200; i++ {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			box := lantest.New(t)
			box.Set([]lan.Address{eth0, eth0b})
			forced := false
			s := startOn(t, box, 0, Limits{}, &Seams{Forcing: func() { forced = true }})
			bound := listening(t, s, eth0, eth0b)
			for _, peer := range []struct {
				address lan.Address
				source  netip.Addr
			}{
				{eth0, phone}, {eth0b, netip.MustParseAddr("10.0.0.9")},
			} {
				c, err := box.Dial(bound[peer.address], peer.source)
				if err != nil {
					t.Fatal(err)
				}
				defer c.Close()
				if code, err := get(c, bufio.NewReader(c), bound[peer.address].String()); code != 200 || err != nil {
					t.Fatalf("idle connection: %d, %v", code, err)
				}
			}
			if rep := s.Stop(); rep.Forced || rep.Abandoned != 0 || forced {
				t.Fatalf("clean stop reported %+v; forcing hook=%v", rep, forced)
			}
		})
		if t.Failed() {
			return
		}
	}
}

// TestOptionsStarPassesThroughTheSurface: net/http must apply the surface's
// Host check, routes, stop refusal and EXT header even to OPTIONS *.
func TestOptionsStarPassesThroughTheSurface(t *testing.T) {
	for _, tc := range []struct {
		name              string
		foreign, stopping bool
		want              int
	}{
		{"own Host", false, false, 404},
		{"foreign Host", true, false, 400},
		{"stopping own Host", false, true, 503},
		{"stopping foreign Host", true, true, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			box := lantest.New(t)
			box.Set([]lan.Address{eth0})
			seams := &Seams{}
			s := startOn(t, box, 0, Limits{}, seams)
			at := listening(t, s, eth0)[eth0]
			c, err := box.Dial(at, phone)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			br := bufio.NewReader(c)
			if code, err := get(c, br, at.String()); code != 200 || err != nil {
				t.Fatalf("kept connection: %d, %v", code, err)
			}
			host := at.String()
			if tc.foreign {
				host = "foreign.invalid:7792"
			}
			var res *http.Response
			exchange := func() {
				_ = c.SetDeadline(time.Now().Add(10 * time.Second))
				_, err = io.WriteString(c, "OPTIONS * HTTP/1.1\r\nHost: "+host+"\r\n\r\n")
				if err == nil {
					res, err = http.ReadResponse(br, nil)
				}
				if err == nil {
					_, err = io.Copy(io.Discard, res.Body)
					_ = res.Body.Close()
				}
			}
			if tc.stopping {
				seams.Stopping = exchange
				s.Stop()
			} else {
				exchange()
			}
			if err != nil {
				t.Fatal(err)
			}
			if res.StatusCode != tc.want {
				t.Errorf("OPTIONS *: %d, want %d", res.StatusCode, tc.want)
			}
			if ext, ok := res.Header["Ext"]; !ok || len(ext) != 1 || ext[0] != "" {
				t.Errorf("OPTIONS * lost EXT: %v", res.Header)
			}
			if tc.stopping && !res.Close {
				t.Error("stop refusal kept the connection alive")
			}
		})
	}
}
