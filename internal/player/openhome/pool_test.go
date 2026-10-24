package openhome

import (
	"context"
	"io"
	"net/http"
	"net/http/httptrace"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/lan"
	"github.com/acoseac/1-bit-bridge/internal/player/lan/lantest"
)

// The write pool's tests (B470 PR 5a4): a write but Stop waits for the
// engine up to its budget, so it holds a slot of its own pool, smaller than
// the slots by two, and one source holds at most maxWritesPerSource of them.

// gateEngine is the test player's engine with every Play held until the
// test lets it go (one at a time, or all), its budget ending, counting the
// Plays held: a write that holds its slots as long as the test says.
type gateEngine struct {
	Engine
	one     chan struct{}
	all     chan struct{}
	mu      sync.Mutex
	held    int
	changed chan struct{}
	once    sync.Once
}

func newGateEngine(tb testing.TB, e Engine) *gateEngine {
	g := &gateEngine{Engine: e, one: make(chan struct{}), all: make(chan struct{}), changed: make(chan struct{})}
	tb.Cleanup(g.letGoAll)
	return g
}

func (g *gateEngine) Play(ctx context.Context, _ ...engine.Cond) error {
	g.note(1)
	defer g.note(-1)
	select {
	case <-g.one:
	case <-g.all:
	case <-ctx.Done():
	}
	return nil
}

func (g *gateEngine) note(d int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.held += d
	close(g.changed)
	g.changed = make(chan struct{})
}

// letGoOne lets one held Play go.
func (g *gateEngine) letGoOne(tb testing.TB) {
	tb.Helper()
	select {
	case g.one <- struct{}{}:
	case <-within(tb.(*testing.T), testWait):
		tb.Fatal("no Play was held to let go")
	}
}

func (g *gateEngine) letGoAll() { g.once.Do(func() { close(g.all) }) }

// waitHeld waits until n Plays are held.
func (g *gateEngine) waitHeld(tb testing.TB, n int) {
	tb.Helper()
	give := within(tb.(*testing.T), testWait)
	for {
		g.mu.Lock()
		got, ch := g.held, g.changed
		g.mu.Unlock()
		if got == n {
			return
		}
		select {
		case <-ch:
		case <-give:
			tb.Fatalf("waited for %d Plays held, %d are", n, got)
		}
	}
}

// from is s's action with args, its connection from src.
func from(src netip.Addr, s *service, action string, args ...string) *http.Request {
	r := actionRequest(s, action, args...)
	r.RemoteAddr = netip.AddrPortFrom(src, 40000).String()
	return r
}

// TestTheLimitsKeepTwoSlotsFromTheWrites pins the pool's sizes: the
// defaults, six writes of the eight slots and four a source, below the
// listener's eight connections a source; and limits that would leave the
// reads fewer than two slots, or a source every connection, are held to them.
func TestTheLimitsKeepTwoSlotsFromTheWrites(t *testing.T) {
	d := DefaultLimits.orDefaults()
	if d.Slots != 8 || d.WriteSlots != 6 || d.WritesPerSource != maxWritesPerSource || maxWritesPerSource != 4 || d.MaxConnsPerSource != 8 {
		t.Errorf("the defaults: %d slots, %d writes, %d a source of %d connections", d.Slots, d.WriteSlots, d.WritesPerSource, d.MaxConnsPerSource)
	}
	if got := d.writeBudget(); got != 9*time.Second {
		t.Errorf("the write budget %v, want the 10 s write deadline less a second", got)
	}
	for _, tc := range []struct {
		in             Limits
		writes, source int
	}{
		{Limits{Slots: 4, WriteSlots: 4}, 2, 4},
		{Limits{Slots: 2, WriteSlots: 6}, 1, 4},
		{Limits{Slots: 1}, 1, 4},
		{Limits{MaxConnsPerSource: 3, WritesPerSource: 4}, 6, 2},
		{Limits{MaxConnsPerSource: 1}, 6, 1},
	} {
		got := tc.in.orDefaults()
		if got.WriteSlots != tc.writes || got.WritesPerSource != tc.source {
			t.Errorf("%+v: %d writes, %d a source; want %d and %d", tc.in, got.WriteSlots, got.WritesPerSource, tc.writes, tc.source)
		}
	}
}

// TestOneSourceHoldsAtMostItsShareOfTheWritePool pins the per-source share:
// a source holding its four waiting writes has a fifth refused, 503 at once,
// while a second source's write is taken and its TransportState read and its
// Stop are answered; once the writes end, every one of them answers success.
func TestOneSourceHoldsAtMostItsShareOfTheWritePool(t *testing.T) {
	p := newTestPlayer(t)
	g := newGateEngine(t, p.eng)
	h := p.handlerWith("", g, DefaultLimits.orDefaults())
	var held []sent
	for i := 0; i < maxWritesPerSource; i++ {
		held = append(held, sendRequest(h, playlist, "Play", from(phone, playlist, "Play")))
	}
	g.waitHeld(t, maxWritesPerSource)
	if got := serve(t, h, from(phone, playlist, "Play")); got.status != http.StatusServiceUnavailable {
		t.Errorf("a fifth write from one source: %v, want 503", got)
	}
	if got := serve(t, h, from(phone, playlist, "SetRepeat", "Value", "1")); got.status != http.StatusServiceUnavailable {
		t.Errorf("another write from that source: %v, want 503", got)
	}
	held = append(held, sendRequest(h, playlist, "Play", from(tablet, playlist, "Play")))
	g.waitHeld(t, maxWritesPerSource+1)
	if got := serve(t, h, from(tablet, playlist, "TransportState")); got.status != http.StatusOK {
		t.Errorf("the second source's TransportState: %v", got)
	}
	if got := serve(t, h, from(tablet, playlist, "Stop")); got.status != http.StatusOK {
		t.Errorf("the second source's Stop: %v", got)
	}
	if got := serve(t, h, from(phone, playlist, "Stop")); got.status != http.StatusOK {
		t.Errorf("the first source's Stop, its share full: %v", got)
	}
	g.letGoAll()
	for _, c := range held {
		if r := c.answer(t); r.status != http.StatusOK {
			t.Errorf("a held write: %d, fault %d", r.status, r.fault)
		}
	}
	if got := serve(t, h, from(phone, playlist, "SetRepeat", "Value", "1")); got.status != http.StatusOK {
		t.Errorf("a write once the share is free: %v", got)
	}
}

// TestTheWritePoolFullAcrossSourcesLeavesReadsAndStop pins the pool's own
// bound, a limit across controllers: six writes waiting from three sources
// fill it, and a fourth source's write is 503 at once; reads, the device
// description and a Stop are answered all the same, from the two slots the
// writes cannot take.
func TestTheWritePoolFullAcrossSourcesLeavesReadsAndStop(t *testing.T) {
	p := newTestPlayer(t)
	g := newGateEngine(t, p.eng)
	h := p.handlerWith("", g, DefaultLimits.orDefaults())
	lim := DefaultLimits.orDefaults()
	var held []sent
	sources := []netip.Addr{phone, tablet, laptop}
	for i := 0; i < lim.WriteSlots; i++ {
		held = append(held, sendRequest(h, playlist, "Play", from(sources[i%len(sources)], playlist, "Play")))
	}
	g.waitHeld(t, lim.WriteSlots)
	other := netip.MustParseAddr("192.168.1.53")
	if got := serve(t, h, from(other, playlist, "Play")); got.status != http.StatusServiceUnavailable {
		t.Errorf("a write past the pool: %v, want 503", got)
	}
	for _, c := range [][]string{{"TransportState"}, {"IdArray"}, {"Id"}, {"Stop"}} {
		if got := serve(t, h, from(other, playlist, c[0], c[1:]...)); got.status != http.StatusOK {
			t.Errorf("%s with the write pool full: %v", c[0], got)
		}
	}
	desc := request(http.MethodGet, "/openhome/description.xml", nil)
	desc.RemoteAddr = netip.AddrPortFrom(other, 40000).String()
	if got := serve(t, h, desc); got.status != http.StatusOK {
		t.Errorf("the description with the write pool full: %v", got)
	}
	g.letGoAll()
	for _, c := range held {
		if r := c.answer(t); r.status != http.StatusOK {
			t.Errorf("a held write: %d, fault %d", r.status, r.fault)
		}
	}
}

// TestAStopAndAWriteAfterASlotIsReleasedOverTheListener pins the pool over
// the listener's own connections: a source holding its four writes is refused
// a fifth, 503 at once, the connection kept, and its Stop on a connection it
// holds already is answered; once one of its writes ends, a write is taken
// again, on an existing connection and on a new one, and so is a Stop on a
// new connection. The listener's own limits stand beside these (the long
// form): a source's ninth connection is refused at accept, unanswered, and a
// kept-alive connection counts after its request.
func TestAStopAndAWriteAfterASlotIsReleasedOverTheListener(t *testing.T) {
	p := newTestPlayer(t)
	g := newGateEngine(t, p.eng)
	box := lantest.New(t)
	box.Set([]lan.Address{eth0})
	s, err := Start(Config{Device: Device{UUID: testUUID, ModelNumber: "test"}, Engine: g, Library: p.lib, Hosts: testHosts,
		Addresses: box.Addresses, Listen: box.Listen})
	must(t, "start", err)
	t.Cleanup(func() { s.Stop() })
	at := listening(t, s, eth0)[eth0]
	client := box.Client(phone)
	// do sends action on client and reads its answer, and whether it went on
	// a connection the client held already.
	do := func(c *http.Client, action string, args ...string) (int, bool) {
		t.Helper()
		var b strings.Builder
		for i := 0; i+1 < len(args); i += 2 {
			b.WriteString("<" + args[i] + ">" + escape(args[i+1]) + "</" + args[i] + ">")
		}
		reused := false
		ctx := httptrace.WithClientTrace(context.Background(), &httptrace.ClientTrace{
			GotConn: func(info httptrace.GotConnInfo) { reused = info.Reused },
		})
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+at.String()+"/openhome/playlist/control",
			strings.NewReader(envelope(call(playlist, action, b.String()))))
		must(t, "request", err)
		req.Header.Set("Content-Type", `text/xml; charset="utf-8"`)
		req.Header.Set("SOAPAction", header(playlist, action))
		res, err := c.Do(req)
		must(t, action, err)
		_, _ = io.Copy(io.Discard, res.Body)
		_ = res.Body.Close()
		return res.StatusCode, reused
	}
	done := make(chan int, maxWritesPerSource)
	for i := 0; i < maxWritesPerSource; i++ {
		go func() {
			code, _ := do(client, "Play")
			done <- code
		}()
	}
	g.waitHeld(t, maxWritesPerSource)
	if code, _ := do(client, "Play"); code != http.StatusServiceUnavailable {
		t.Fatalf("a fifth write from the source: %d, want 503", code)
	}
	if code, reused := do(client, "Stop"); code != http.StatusOK || !reused {
		t.Errorf("a Stop with the source's share full: %d, on a connection held already %v; want 200 on one", code, reused)
	}
	g.letGoOne(t)
	if code := <-done; code != http.StatusOK {
		t.Errorf("the write let go: %d", code)
	}
	if code, reused := do(client, "SetRepeat", "Value", "1"); code != http.StatusOK || !reused {
		t.Errorf("a write once a slot is free: %d, on a connection held already %v; want 200 on one", code, reused)
	}
	fresh := box.Client(phone)
	if code, reused := do(fresh, "SetRepeat", "Value", "0"); code != http.StatusOK || reused {
		t.Errorf("a write on a new connection: %d, reused %v; want 200 on a new one", code, reused)
	}
	if code, reused := do(box.Client(phone), "Stop"); code != http.StatusOK || reused {
		t.Errorf("a Stop on a new connection: %d, reused %v; want 200 on a new one", code, reused)
	}
	g.letGoAll()
	for i := 1; i < maxWritesPerSource; i++ {
		if code := <-done; code != http.StatusOK {
			t.Errorf("a held write: %d", code)
		}
	}
}
