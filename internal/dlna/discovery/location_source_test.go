package discovery

// Which host an SSDP LOCATION may make the bridge fetch (backlog B14, the
// follow-up #1050 recorded). #1050 bounded a discovered description's SERVICE
// URLs to the host that served it; nothing bounded which host that is. A LAN
// peer answering an M-SEARCH (or re-announcing a known UDN, which the move
// detector re-fetches) with LOCATION http://127.0.0.1:7789/<path> made the
// bridge GET its own no-auth console, and a body that parsed as a description
// there could name loopback service URLs that the same-host rule accepts.
//
// The rule: a LOCATION may lead the bridge to this host (loopback or the
// unspecified address) or to a link-local address only when the SSDP packet
// came from that same address. The handler refuses what the host STRING
// shows (an IP literal, a localhost name, a numeric spelling no device
// writes) before any fetch; the default client's dial check refuses the rest
// (a public DNS name that resolves to 127.0.0.1, say), because the connect
// sees the address a name resolved to and a string check cannot.

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// udpFrom is the source address ReadFromUDP reports for a packet from ip.
func udpFrom(ip string) *net.UDPAddr {
	return &net.UDPAddr{IP: net.ParseIP(ip), Port: 1900}
}

// hostLocalLocations are LOCATIONs whose host names this host or a
// link-local address in a way the host string shows, each spelled the way a
// hostile packet can spell it. Every one reached a listener on 127.0.0.1 (or
// the link) in a measurement on macOS, the numeric ones through the libc
// resolver, which accepts inet_aton's forms.
var hostLocalLocations = []string{
	"http://127.0.0.1:7789/api/stats", // the bridge's own no-auth console
	"http://127.53.0.1:7789/api/stats",
	"http://[::1]:7789/api/stats",
	"http://0.0.0.0:7789/api/stats", // a connect to it reaches this host
	"http://[::]:7789/api/stats",
	"http://[::ffff:127.0.0.1]:7789/api/stats",
	"http://127.0.0.1.:7789/api/stats",
	"http://localhost:7789/api/stats",
	"http://LocalHost.:7789/api/stats",
	"http://console.localhost:7789/api/stats",
	"http://127.1:7789/api/stats",
	"http://2130706433:7789/api/stats",
	"http://0x7f000001:7789/api/stats",
	"http://0:7789/api/stats",
	"http://169.254.169.254/latest/meta-data/", // link-local: a cloud metadata service
	"http://[fe80::1%25en0]:8080/description.xml",
}

// TestHandlePacket_NeverFetchesAHostLocalLocationFromAnotherAddress drives
// the real announcement path for a first-time renderer from a LAN address
// whose LOCATION names this host or a link-local address. No request is made
// and nothing is cached, so no fetch goroutine, stub or Location record is
// spent on it either.
func TestHandlePacket_NeverFetchesAHostLocalLocationFromAnotherAddress(t *testing.T) {
	disp := &requestLog{handler: func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(chordDeviceXML))
	}}
	c := newTestClient(t, disp)
	for i, location := range hostLocalLocations {
		udn := fmt.Sprintf("uuid:host-local-%d", i)
		c.handlePacket(context.Background(), rendererAnnouncement(udn, location), udpFrom("192.0.2.7"))
		c.wg.Wait() // the detail fetch, if any, is the only goroutine
		if _, cached := c.cache.Get(udn); cached {
			t.Errorf("LOCATION %q from 192.0.2.7 produced a cache entry", location)
		}
	}
	if reqs := disp.requests(); len(reqs) != 0 {
		t.Errorf("requests = %q, want none: each LOCATION named this host or a link-local "+
			"address, and no packet came from it", reqs)
	}
}

// TestHandlePacket_FetchesAHostLocalLocationFromThatSameAddress is the other
// side of the rule: a device on a zero-configuration LAN announces from its
// link-local address with a LOCATION on that address, and is fetched and
// served. A LOCATION on this host is fetched when the packet came from it too
// (a packet with a loopback source was sent on this host).
func TestHandlePacket_FetchesAHostLocalLocationFromThatSameAddress(t *testing.T) {
	for _, tc := range []struct{ source, location, wantControl string }{
		{"169.254.10.20", "http://169.254.10.20:8080/description.xml", "http://169.254.10.20:8080/avtransport/control"},
		{"127.0.0.1", "http://127.0.0.1:8080/description.xml", "http://127.0.0.1:8080/avtransport/control"},
	} {
		disp := &requestLog{handler: func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			if r.Method == http.MethodPost {
				_, _ = w.Write([]byte(chordGetProtocolInfoResponse))
				return
			}
			_, _ = w.Write([]byte(chordDeviceXML))
		}}
		c := newTestClient(t, disp)
		c.handlePacket(context.Background(), rendererAnnouncement("uuid:same-address", tc.location), udpFrom(tc.source))
		c.wg.Wait()
		reqs := disp.requests()
		if len(reqs) == 0 || reqs[0] != "GET "+tc.location {
			t.Errorf("source %s: requests = %q, want the description GET first", tc.source, reqs)
		}
		served := c.cache.Snapshot()
		if len(served) != 1 || served[0].ControlURL != tc.wantControl {
			t.Errorf("source %s: served %+v, want one renderer driven at %s", tc.source, served, tc.wantControl)
		}
	}
}

// TestHandlePacket_AKnownRendererCannotMoveOntoAHostLocalLocation is the
// move-detector route: a renderer known at a LAN address is re-announced
// from another LAN address with a LOCATION on the bridge's console. Before
// this rule the detector read it as a move and fetched the console; now the
// LOCATION reads as absent, so the known entry is refreshed and kept.
func TestHandlePacket_AKnownRendererCannotMoveOntoAHostLocalLocation(t *testing.T) {
	disp := &requestLog{handler: func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodPost {
			_, _ = w.Write([]byte(chordGetProtocolInfoResponse))
			return
		}
		_, _ = w.Write([]byte(chordDeviceXML))
	}}
	c := newTestClient(t, disp)
	const udn = "uuid:known-renderer"
	c.handlePacket(context.Background(), alivePacket(udn, "http://192.0.2.7:8080/description.xml"), udpFrom("192.0.2.7"))
	c.wg.Wait()
	const honest = "http://192.0.2.7:8080/avtransport/control"
	if info, _ := c.cache.Get(udn); info.ControlURL != honest {
		t.Fatalf("first discovery cached ControlURL %q, want %q", info.ControlURL, honest)
	}
	before := len(disp.requests())

	c.handlePacket(context.Background(), alivePacket(udn, "http://127.0.0.1:7789/api/stats"), udpFrom("192.0.2.99"))
	c.wg.Wait()
	if reqs := disp.requests(); len(reqs) != before {
		t.Errorf("requests after the re-announcement = %q, want nothing more: "+
			"the move detector must not follow a LOCATION on this host", reqs[before:])
	}
	if info, _ := c.cache.Get(udn); info.ControlURL != honest {
		t.Errorf("ControlURL = %q after the re-announcement, want %q kept", info.ControlURL, honest)
	}
}

// loopbackDescriptionServer serves chordDeviceXML and its GetProtocolInfo
// answer on 127.0.0.1 and records each request, standing in for whatever
// listens on this host (the bridge's console, in the attack).
type loopbackDescriptionServer struct {
	*httptest.Server
	mu   sync.Mutex
	seen []string
}

func newLoopbackDescriptionServer(t *testing.T) *loopbackDescriptionServer {
	t.Helper()
	s := &loopbackDescriptionServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.seen = append(s.seen, r.Method+" "+r.URL.Path)
		s.mu.Unlock()
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodPost {
			_, _ = w.Write([]byte(chordGetProtocolInfoResponse))
			return
		}
		_, _ = w.Write([]byte(chordDeviceXML))
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *loopbackDescriptionServer) requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.seen...)
}

// port is the port the server listens on, for building a LOCATION that
// names it by a name rather than by its address.
func (s *loopbackDescriptionServer) port(t *testing.T) string {
	t.Helper()
	_, port, err := net.SplitHostPort(s.Listener.Addr().String())
	if err != nil {
		t.Fatalf("listener address: %v", err)
	}
	return port
}

// newDefaultDispatcherClient builds a client the way cmd/bridge does, with
// no Dispatcher, so its fetches go through the production client and its
// dial check.
func newDefaultDispatcherClient(t *testing.T) *SSDPDiscoveryClient {
	t.Helper()
	cfg := DefaultDiscoveryConfig()
	cfg.Interface = &net.Interface{}
	cfg.DetailFetchTimeout = 3 * time.Second
	cfg.NowFunc = func() time.Time { return time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC) }
	c, err := NewSSDPDiscoveryClient(cfg, NewRendererCache())
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	return c
}

// TestDefaultClient_NeverConnectsToThisHostOnAnotherAddressesSay pins the
// half the handler's string check cannot see. The production client (the one
// a client built with no Dispatcher fetches through, as cmd/bridge builds
// both) refuses a connect to this host or a link-local address unless the
// request's context says the SSDP packet came from that address. It is
// driven here with `localhost`, which resolves to 127.0.0.1 on every
// platform, standing in for the names the handler lets through: a public DNS
// name pointed at 127.0.0.1 reached the listener in the same measurement, on
// macOS and on Linux. The twin below connects with the same name when the
// packet came from this host, so the refusal is the check and not a lookup
// failure.
func TestDefaultClient_NeverConnectsToThisHostOnAnotherAddressesSay(t *testing.T) {
	srv := newLoopbackDescriptionServer(t)
	c := newDefaultDispatcherClient(t)
	fromLAN := WithAnnouncementSource(context.Background(), udpFrom("192.0.2.7"))
	for _, tc := range []struct {
		name string
		ctx  context.Context
		url  string
	}{
		{"a name, announced from a LAN address", fromLAN, "http://localhost:" + srv.port(t) + "/description.xml"},
		{"a literal, announced from a LAN address", fromLAN, srv.URL + "/description.xml"},
		{"a name, with no announcement at all", context.Background(), "http://localhost:" + srv.port(t) + "/description.xml"},
	} {
		if _, err := FetchDeviceDescription(tc.ctx, c.dispatcher, tc.url); err == nil {
			t.Errorf("%s: the fetch of %s succeeded", tc.name, tc.url)
		}
	}
	if reqs := srv.requests(); len(reqs) != 0 {
		t.Errorf("the loopback listener saw %q: the default client connected to this host "+
			"on the say-so of a packet from elsewhere", reqs)
	}
}

// TestDefaultClient_ChecksTheDevicesAddressNotAProxys pins the transport
// settings the dial check depends on. Through a proxy the connect goes to the
// proxy, so the check would judge the proxy's address (and refuse every fetch
// on a host whose HTTP_PROXY is on 127.0.0.1); a kept-alive connection could
// carry a later request no announcement vouched for; and a TLS dialer of its
// own would connect around the checked DialContext.
func TestDefaultClient_ChecksTheDevicesAddressNotAProxys(t *testing.T) {
	c := newDefaultDispatcherClient(t)
	disp, ok := c.dispatcher.(*HTTPClientDispatcher)
	if !ok {
		t.Fatalf("default dispatcher = %T, want *HTTPClientDispatcher", c.dispatcher)
	}
	tr, ok := disp.Client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("default client transport = %T, want *http.Transport", disp.Client.Transport)
	}
	if tr.Proxy != nil {
		t.Error("the default client uses a proxy; the dial check would judge the proxy's address")
	}
	if !tr.DisableKeepAlives {
		t.Error("the default client keeps connections alive; a later request could reuse one no announcement vouched for")
	}
	if tr.DialTLSContext != nil {
		t.Error("the default client dials TLS itself, around the checked DialContext")
	}
}

// TestHandlePacket_DefaultClientConnectsToThisHostWhenThePacketCameFromIt is
// the twin, through the real announcement path: a LOCATION naming this host,
// announced from 127.0.0.1, is fetched, and so is the GetProtocolInfo POST
// that follows it, which pins that the packet's source reaches both
// requests' contexts. A packet with a loopback source was sent on this host,
// whose processes can reach the console directly anyway.
func TestHandlePacket_DefaultClientConnectsToThisHostWhenThePacketCameFromIt(t *testing.T) {
	srv := newLoopbackDescriptionServer(t)
	c := newDefaultDispatcherClient(t)
	location := "http://localhost:" + srv.port(t) + "/description.xml"
	c.handlePacket(context.Background(), rendererAnnouncement("uuid:named-loopback", location), udpFrom("127.0.0.1"))
	c.wg.Wait()
	want := []string{"GET /description.xml", "POST /cm/control"}
	if reqs := srv.requests(); strings.Join(reqs, ",") != strings.Join(want, ",") {
		t.Errorf("the loopback listener saw %q, want %q", reqs, want)
	}
	served := c.cache.Snapshot()
	if len(served) != 1 || len(served[0].SinkProtocolInfos) != 3 {
		t.Errorf("served %+v, want the renderer with its three sinks", served)
	}

	// The move path hands the source on too: the same renderer, announced
	// at its address rather than its name, reads as a move (the detector
	// compares host strings) and is re-fetched there.
	moved := srv.URL + "/description.xml"
	c.handlePacket(context.Background(), rendererAnnouncement("uuid:named-loopback", moved), udpFrom("127.0.0.1"))
	c.wg.Wait()
	wantControl := srv.URL + "/avtransport/control"
	if served := c.cache.Snapshot(); len(served) != 1 || served[0].ControlURL != wantControl {
		t.Errorf("after the move, served %+v, want the renderer driven at %s", served, wantControl)
	}
}

// TestDefaultClientDialCheck pins the classification the dial check makes,
// on the address a connect actually targets, one row per shape. A source is
// the SSDP packet's address, carried in the request's context; "" is a
// request with none.
func TestDefaultClientDialCheck(t *testing.T) {
	for _, tc := range []struct {
		source, address string
		allowed         bool
	}{
		{"", "192.168.1.42:8080", true},
		{"192.0.2.7", "192.168.1.42:8080", true}, // another LAN host: #1050's same-host rule bounds it
		{"192.0.2.7", "203.0.113.9:443", true},
		{"", "127.0.0.1:7789", false},
		{"192.0.2.7", "127.0.0.1:7789", false},
		{"127.0.0.1", "127.0.0.1:7789", true},
		{"127.0.0.1", "127.0.0.2:7789", false},
		{"127.0.0.1", "[::1]:7789", false},
		{"::ffff:127.0.0.1", "127.0.0.1:7789", true}, // a mapped source is its IPv4 address
		{"192.0.2.7", "[::ffff:127.0.0.1]:7789", false},
		{"192.0.2.7", "0.0.0.0:7789", false},
		{"0.0.0.0", "0.0.0.0:7789", false}, // unspecified is never a peer's address
		{"192.0.2.7", "[::]:7789", false},
		{"192.0.2.7", "169.254.169.254:80", false},
		{"169.254.10.20", "169.254.10.20:8080", true},
		{"169.254.10.20", "169.254.10.21:8080", false},
		{"192.0.2.7", "[fe80::1%en0]:8080", false},
		{"192.0.2.7", "not-an-address", false}, // fail closed on anything unparseable
	} {
		ctx := context.Background()
		if tc.source != "" {
			ctx = WithAnnouncementSource(ctx, udpFrom(tc.source))
		}
		err := refuseUnannouncedHostLocal(ctx, "tcp4", tc.address, nil)
		if got := err == nil; got != tc.allowed {
			t.Errorf("source %q, connect to %s: allowed = %v (err %v), want %v",
				tc.source, tc.address, got, err, tc.allowed)
		}
	}
}

// TestLocationFromSource pins the handler's string check, one row per shape.
// A name it cannot place passes it: that one is the dial check's.
func TestLocationFromSource(t *testing.T) {
	for _, tc := range []struct {
		location, source string
		kept             bool
	}{
		{"http://192.168.1.42:8080/d.xml", "192.168.1.42", true},
		{"http://192.168.1.42:8080/d.xml", "192.0.2.7", true}, // another LAN address: not this rule's business
		{"http://nas.local:8200/d.xml", "192.0.2.7", true},
		{"http://localtest.me:7789/d.xml", "192.0.2.7", true}, // a public name for 127.0.0.1: the dial check's
		{"http://127.0.0.1:7789/api/stats", "192.0.2.7", false},
		{"http://127.0.0.1:7789/api/stats", "", false},
		{"http://127.0.0.1:7789/d.xml", "127.0.0.1", true},
		{"http://127.0.0.1.:7789/d.xml", "127.0.0.1", true},
		{"http://127.0.0.2:7789/d.xml", "127.0.0.1", false},
		{"http://[::ffff:127.0.0.1]:7789/d.xml", "127.0.0.1", true},
		{"http://localhost:7789/d.xml", "192.0.2.7", false},
		{"http://localhost:7789/d.xml", "127.0.0.1", true},
		{"http://Console.LOCALHOST.:7789/d.xml", "192.0.2.7", false},
		{"http://127.1:7789/d.xml", "192.0.2.7", false},
		{"http://127.1:7789/d.xml", "127.0.0.1", false}, // a spelling no device writes, from anywhere
		{"http://2130706433:7789/d.xml", "192.0.2.7", false},
		{"http://0x7f.1:7789/d.xml", "192.0.2.7", false},
		{"http://0:7789/d.xml", "192.0.2.7", false},
		{"http://device.123:7789/d.xml", "192.0.2.7", false},
		{"http://0.0.0.0:7789/d.xml", "0.0.0.0", false},
		{"http://169.254.10.20:8080/d.xml", "169.254.10.20", true},
		{"http://169.254.10.20:8080/d.xml", "169.254.10.21", false},
		{"http://[fe80::1%25en0]:8080/d.xml", "192.0.2.7", false},
		{"", "192.0.2.7", false},
	} {
		var src *net.UDPAddr
		if tc.source != "" {
			src = udpFrom(tc.source)
		}
		got := LocationFromSource(tc.location, src)
		if kept := got != ""; kept != tc.kept || (kept && got != tc.location) {
			t.Errorf("LOCATION %q from %q: got %q, want kept = %v", tc.location, tc.source, got, tc.kept)
		}
	}
}
