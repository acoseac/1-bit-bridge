package openhome

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/lan"
	"github.com/acoseac/1-bit-bridge/internal/player/lan/lantest"
)

// The Stops' tests (B470 PR 5a4, round 2). Every Stop is the engine's own
// command, never a share of another Stop's wait, so each one's effect on the
// pending intent holds; a Stop holds its request's slot only until the engine
// has admitted it, then waits for the release holding its connection alone,
// so the Stops that wait are bounded by the listener's connection caps, never
// by the slots; and a Stop takes no slot of the write pool, so it is admitted
// with the pool full. Round 1 found eight Stops on one source's eight
// connections, during a pending release, holding every slot: every other
// source's reads, descriptions and Stops were 503 until the release ended.
//
// The engine's release here is the test backend's, held (holdReleases), and
// it is pending for the engine's ReleaseTimeout at most (2 s, the real
// clock), after which the engine stops waiting for it: each test makes its
// checks inside that window, and asks that its Stops still wait once they
// are made.

// waitingStops counts the Stops that have given their slot back and wait for
// the engine's answer (Seams.StopWaiting).
type waitingStops struct {
	mu      sync.Mutex
	n       int
	changed chan struct{}
}

func newWaitingStops() *waitingStops { return &waitingStops{changed: make(chan struct{})} }

func (w *waitingStops) seam(*http.Request) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.n++
	close(w.changed)
	w.changed = make(chan struct{})
}

// wait waits until n Stops wait without their slot.
func (w *waitingStops) wait(tb testing.TB, n int) {
	tb.Helper()
	give := time.After(testWait)
	for {
		w.mu.Lock()
		got, ch := w.n, w.changed
		w.mu.Unlock()
		if got >= n {
			return
		}
		select {
		case <-ch:
		case <-give:
			tb.Fatalf("waited %v for %d Stops to wait without their slot; %d do", testWait, n, got)
		}
	}
}

// playingWithReleasesHeld has p's engine play an entry, heard, and holds every
// Release from then on until the returned function lets them go: the next
// Stop starts a release that stays pending.
func playingWithReleasesHeld(t *testing.T, p *testPlayer) (letGo func()) {
	t.Helper()
	p.add(uriOf(goldenID), nil)
	must(t, "play", p.eng.Play(bg))
	s := p.be.session(t, 1)
	s.hear(s.first.ID, nil)
	p.heardNow(s.first.ID)
	letGo = p.be.holdReleases()
	t.Cleanup(letGo)
	return letGo
}

// stillWaiting fails the test unless every one of stops is unanswered and the
// engine still releasing: what the test checked, it checked while they
// waited, inside the release's window.
func stillWaiting(t *testing.T, p *testPlayer, stops []sent) {
	t.Helper()
	for i, c := range stops {
		if !c.pending() {
			t.Fatalf("Stop %d answered before the release ended: what was checked was not checked while it waited", i)
		}
	}
	if st := p.eng.Snapshot().State(); st != engine.Releasing {
		t.Fatalf("the engine is %s, not releasing: the release ended before the checks", st)
	}
}

// answeredAll requires each of stops to answer success, and the transport to
// be stopped then.
func answeredAll(t *testing.T, h *handler, stops []sent) {
	t.Helper()
	for i, c := range stops {
		if r := c.answer(t); r.status != http.StatusOK {
			t.Errorf("%s %d: %d, fault %d, want success", c.action, i, r.status, r.fault)
		}
	}
	if got := read(t, h, playlist, "TransportState")["Value"]; got != "Stopped" {
		t.Errorf("TransportState %s once the Stops answered, want Stopped", got)
	}
}

// TestStopsWaitingForAReleaseHoldNoSlot pins the Stops that wait for one
// pending release, each sent once the one before it waits: one source's
// eight (its eight connections' worth), eight sources' one each, and, with
// the write pool full (six writes waiting from three sources), six from two
// sources more. Each gives its slot back once the engine has admitted it, so
// another source's TransportState and device description are answered, and
// another source's Stop is admitted and waits too; once the release is
// acknowledged every Stop answers success and the transport is Stopped.
func TestStopsWaitingForAReleaseHoldNoSlot(t *testing.T) {
	sources := func(n int) []netip.Addr {
		var out []netip.Addr
		for i := 0; i < n; i++ {
			out = append(out, netip.MustParseAddr(fmt.Sprintf("192.168.1.%d", 60+i)))
		}
		return out
	}
	for _, tc := range []struct {
		what  string
		stops []netip.Addr
		// writes are the sources of the writes held waiting first.
		writes []netip.Addr
	}{
		{"one source's eight", []netip.Addr{phone, phone, phone, phone, phone, phone, phone, phone}, nil},
		{"eight sources' one each", sources(8), nil},
		{"two sources' three each, the write pool full", append(slicesOf(netip.MustParseAddr("192.168.1.70"), 3), slicesOf(netip.MustParseAddr("192.168.1.71"), 3)...),
			[]netip.Addr{phone, tablet, laptop, phone, tablet, laptop}},
	} {
		t.Run(tc.what, func(t *testing.T) {
			p := newTestPlayer(t)
			letGo := playingWithReleasesHeld(t, p)
			g := newGateEngine(t, p.eng)
			waits := newWaitingStops()
			h := p.handlerWith("", g, DefaultLimits.orDefaults())
			h.seams = &Seams{StopWaiting: waits.seam}
			var writes []sent
			for i, src := range tc.writes {
				writes = append(writes, sendRequest(h, playlist, "Play", from(src, playlist, "Play")))
				g.waitHeld(t, i+1)
			}
			if len(tc.writes) > 0 {
				if got := serve(t, h, from(netip.MustParseAddr("192.168.1.99"), playlist, "Play")); got.status != http.StatusServiceUnavailable {
					t.Fatalf("a write past the write pool: %v, want it full (503)", got)
				}
			}
			var stops []sent
			for i, src := range tc.stops {
				stops = append(stops, sendRequest(h, playlist, "Stop", from(src, playlist, "Stop")))
				waits.wait(t, i+1)
			}
			other := netip.MustParseAddr("192.168.1.90")
			if got := serve(t, h, from(other, playlist, "TransportState")); got.status != http.StatusOK {
				t.Errorf("another source's TransportState while the Stops wait: %v, want it answered", got)
			}
			desc := request(http.MethodGet, "/openhome/description.xml", nil)
			desc.RemoteAddr = netip.AddrPortFrom(other, 40000).String()
			if got := serve(t, h, desc); got.status != http.StatusOK {
				t.Errorf("another source's device description while the Stops wait: %v, want it answered", got)
			}
			stops = append(stops, sendRequest(h, playlist, "Stop", from(other, playlist, "Stop")))
			waits.wait(t, len(stops))
			stillWaiting(t, p, stops)
			g.letGoAll()
			letGo()
			answeredAll(t, h, append(writes, stops...))
		})
	}
}

// slicesOf is n copies of a.
func slicesOf(a netip.Addr, n int) []netip.Addr {
	out := make([]netip.Addr, n)
	for i := range out {
		out[i] = a
	}
	return out
}

// TestEveryStopIsTheEnginesOwnCommand pins the decision round 2 took on
// Codex's interleaving: Stop A starts a release; a Play sent while it is
// pending sets the intent to play; Stop B, sent after the Play, is submitted
// to the engine like every Stop, never answered from A's wait, so it sets the
// intent back to stop. Once the release is acknowledged the transport is
// Stopped and no session starts; A, the Play (superseded) and B each answer
// success. Had B joined A's wait, it would have answered success while the
// Play's intent started playback.
func TestEveryStopIsTheEnginesOwnCommand(t *testing.T) {
	p := newTestPlayer(t)
	letGo := playingWithReleasesHeld(t, p)
	waits := newWaitingStops()
	h := p.handler("")
	h.seams = &Seams{StopWaiting: waits.seam}
	a := send(h, playlist, "Stop")
	waits.wait(t, 1)
	p.be.callsBegun(t, "Release", 1)
	play := send(h, playlist, "Play")
	p.waitSnap("the Play's intent, pending", func(s *engine.Snapshot) bool { return s.Pending() == engine.IntentPlay })
	b := send(h, playlist, "Stop")
	waits.wait(t, 2)
	p.waitSnap("Stop B's intent, pending", func(s *engine.Snapshot) bool { return s.Pending() == engine.IntentStop })
	stillWaiting(t, p, []sent{a, play, b})
	letGo()
	answeredAll(t, h, []sent{a, play, b})
	if n := p.be.sessionCount(); n != 1 {
		t.Errorf("%d sessions started, want the first alone: the Play between the Stops started playback", n)
	}
}

// TestEveryWaitingStopAnswersAsTheReleaseEnds pins the answers of the Stops
// that wait for one release, from two sources: each answers success once the
// release is acknowledged, the transport Stopped; and, with the release never
// acknowledged, each answers success once the engine's ReleaseTimeout has
// passed, the transport Stopped and the release unconfirmed.
func TestEveryWaitingStopAnswersAsTheReleaseEnds(t *testing.T) {
	for _, acked := range []bool{true, false} {
		t.Run(map[bool]string{true: "acknowledged", false: "past ReleaseTimeout"}[acked], func(t *testing.T) {
			t.Parallel()
			p := newTestPlayer(t)
			letGo := playingWithReleasesHeld(t, p)
			waits := newWaitingStops()
			h := p.handler("")
			h.seams = &Seams{StopWaiting: waits.seam}
			var stops []sent
			for i, src := range []netip.Addr{phone, tablet, phone, tablet} {
				stops = append(stops, sendRequest(h, playlist, "Stop", from(src, playlist, "Stop")))
				waits.wait(t, i+1)
			}
			stillWaiting(t, p, stops)
			if acked {
				letGo()
			}
			answeredAll(t, h, stops)
			if _, unconfirmed := p.eng.Snapshot().ReleaseUnconfirmed(); unconfirmed == acked {
				t.Errorf("the release unconfirmed %v, want %v", unconfirmed, !acked)
			}
		})
	}
}

// heldStops is the test player's engine with every Stop held before it
// reaches the engine, until the test lets them go, counting those held: a
// Stop the engine has not admitted yet.
type heldStops struct {
	Engine
	gate    chan struct{}
	mu      sync.Mutex
	held    int
	changed chan struct{}
}

func (e *heldStops) Stop(ctx context.Context, c ...engine.Cond) error {
	e.mu.Lock()
	e.held++
	close(e.changed)
	e.changed = make(chan struct{})
	e.mu.Unlock()
	select {
	case <-e.gate:
	case <-ctx.Done():
	}
	return e.Engine.Stop(ctx, c...)
}

// TestAStopTheEngineHasNotAdmittedHoldsItsSlot pins the other half of the
// rule: a Stop holds its request's slot until the engine has admitted it, so
// eight Stops the engine has not taken yet hold the eight slots, and a read is
// 503 at once; once they reach the engine, each answers, and the read is
// answered.
func TestAStopTheEngineHasNotAdmittedHoldsItsSlot(t *testing.T) {
	p := newTestPlayer(t)
	e := &heldStops{Engine: p.eng, gate: make(chan struct{}), changed: make(chan struct{})}
	h := p.handlerWith("", e, DefaultLimits.orDefaults())
	var stops []sent
	for i := 0; i < DefaultLimits.Slots; i++ {
		stops = append(stops, sendRequest(h, playlist, "Stop", from(phone, playlist, "Stop")))
	}
	give := time.After(testWait)
	for {
		e.mu.Lock()
		n, ch := e.held, e.changed
		e.mu.Unlock()
		if n == len(stops) {
			break
		}
		select {
		case <-ch:
		case <-give:
			t.Fatalf("waited %v for %d Stops held before the engine; %d are", testWait, len(stops), n)
		}
	}
	if got := serve(t, h, from(tablet, playlist, "TransportState")); got.status != http.StatusServiceUnavailable {
		t.Errorf("a read with eight Stops the engine has not admitted: %v, want 503", got)
	}
	close(e.gate)
	for i, c := range stops {
		if r := c.answer(t); r.status != http.StatusOK {
			t.Errorf("Stop %d: %d, fault %d", i, r.status, r.fault)
		}
	}
	if got := serve(t, h, from(tablet, playlist, "TransportState")); got.status != http.StatusOK {
		t.Errorf("a read once the Stops answered: %v", got)
	}
}

// stopOn sends a Stop over a new connection from src, and returns the
// connection and its reader, the answer still to come.
func stopOn(t *testing.T, box *lantest.Box, at netip.AddrPort, src netip.Addr) (net.Conn, *bufio.Reader) {
	t.Helper()
	c, err := box.Dial(at, src)
	must(t, "dial", err)
	t.Cleanup(func() { _ = c.Close() })
	body := envelope(call(playlist, "Stop", ""))
	_, err = io.WriteString(c, "POST /openhome/playlist/control HTTP/1.1\r\nHost: "+at.String()+
		"\r\nContent-Type: text/xml; charset=\"utf-8\"\r\nSOAPAction: "+header(playlist, "Stop")+
		"\r\nContent-Length: "+strconv.Itoa(len(body))+"\r\n\r\n"+body)
	must(t, "write a Stop", err)
	return c, bufio.NewReader(c)
}

// statusOn reads the answer on c: its status, 0 when there is none.
func statusOn(t *testing.T, c net.Conn, br *bufio.Reader) int {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(testWait))
	res, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Errorf("no answer: %v", err)
		return 0
	}
	_, _ = io.Copy(io.Discard, res.Body)
	_ = res.Body.Close()
	return res.StatusCode
}

// TestStopsWaitingOverTheListenerAreBoundedByItsConnections pins the bound on
// the Stops that wait, over the listener's own connections: one source's
// eight connections each send a Stop during a pending release, and each gives
// its slot back once the engine has admitted it, so another source's device
// description and TransportState are answered, and its Stop admitted; the
// source's ninth connection is refused at accept, unanswered
// (MaxConnsPerSource is what bounds one source's waiting Stops); one of its
// connections closing mid-wait ends that Stop alone, and the connection is
// counted no more; once the release is acknowledged every other Stop answers
// success, the transport Stopped, and the surface stops with nothing left
// running.
func TestStopsWaitingOverTheListenerAreBoundedByItsConnections(t *testing.T) {
	p := newTestPlayer(t)
	letGo := playingWithReleasesHeld(t, p)
	waits := newWaitingStops()
	box := lantest.New(t)
	box.Set([]lan.Address{eth0})
	s, err := Start(Config{Device: Device{UUID: testUUID, ModelNumber: "test"}, Engine: p.eng, Library: p.lib, Hosts: testHosts,
		Addresses: box.Addresses, Listen: box.Listen, Seams: &Seams{StopWaiting: waits.seam}})
	must(t, "start", err)
	stopped := false
	t.Cleanup(func() {
		if !stopped {
			s.Stop()
		}
	})
	at := listening(t, s, eth0)[eth0]
	lim := DefaultLimits.orDefaults()
	type pending struct {
		c  net.Conn
		br *bufio.Reader
	}
	var phones []pending
	for i := 0; i < lim.MaxConnsPerSource; i++ {
		c, br := stopOn(t, box, at, phone)
		phones = append(phones, pending{c, br})
		waits.wait(t, i+1)
	}
	client := box.Client(tablet)
	res, err := client.Get("http://" + at.String() + "/openhome/description.xml")
	must(t, "another source's description", err)
	_, _ = io.Copy(io.Discard, res.Body)
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Errorf("another source's device description while one source's eight Stops wait: %d", res.StatusCode)
	}
	req, err := http.NewRequest(http.MethodPost, "http://"+at.String()+"/openhome/playlist/control",
		strings.NewReader(envelope(call(playlist, "TransportState", ""))))
	must(t, "request", err)
	req.Header.Set("Content-Type", `text/xml; charset="utf-8"`)
	req.Header.Set("SOAPAction", header(playlist, "TransportState"))
	res, err = client.Do(req)
	must(t, "another source's TransportState", err)
	_, _ = io.Copy(io.Discard, res.Body)
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Errorf("another source's TransportState while one source's eight Stops wait: %d", res.StatusCode)
	}
	tc, tbr := stopOn(t, box, at, tablet)
	waits.wait(t, lim.MaxConnsPerSource+1)
	ninth, err := box.Dial(at, phone)
	must(t, "dial", err)
	if !refusedUnread(t, ninth) {
		t.Errorf("the source's ninth connection, while its eight Stops wait, was answered")
	}
	_ = ninth.Close()
	if st := p.eng.Snapshot().State(); st != engine.Releasing {
		t.Fatalf("the engine is %s, not releasing: the release ended before the checks", st)
	}
	_ = phones[0].c.Close()
	awaitStatus(t, s, testWait, "the closed connection's Stop ended and its connection counted no more",
		func(st Status) bool { return st.BySource[phone] == lim.MaxConnsPerSource-1 })
	if st := p.eng.Snapshot().State(); st != engine.Releasing {
		t.Fatalf("the engine is %s once one Stop's connection closed, want it still releasing", st)
	}
	letGo()
	for i, w := range phones[1:] {
		if code := statusOn(t, w.c, w.br); code != http.StatusOK {
			t.Errorf("the source's Stop %d once the release is acknowledged: %d", i+1, code)
		}
	}
	if code := statusOn(t, tc, tbr); code != http.StatusOK {
		t.Errorf("the other source's Stop once the release is acknowledged: %d", code)
	}
	if st := p.eng.Snapshot().State(); st != engine.Stopped {
		t.Errorf("the transport is %s once the Stops answered, want Stopped", st)
	}
	stopped = true
	if rep := s.Stop(); rep.Abandoned != 0 || rep.Forced {
		t.Errorf("the surface's stop: %+v, want nothing left running", rep)
	}
}
