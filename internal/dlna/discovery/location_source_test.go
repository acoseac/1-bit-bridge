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
	"net/netip"
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
// side of the rule: a device on a zero-configuration link (a direct cable,
// no DHCP server) announces from its link-local address with a LOCATION on
// that address, and is fetched and served. A LOCATION on this host is
// fetched when the packet came from it too, on any link (a packet with a
// loopback source was sent on this host).
func TestHandlePacket_FetchesAHostLocalLocationFromThatSameAddress(t *testing.T) {
	for _, tc := range []struct {
		link                          string
		addrs                         func() ([]net.Addr, error)
		source, location, wantControl string
	}{
		{"zero-conf", zeroConfLink, "169.254.10.20", "http://169.254.10.20:8080/description.xml", "http://169.254.10.20:8080/avtransport/control"},
		// A direct-cable device, which the cloud metadata rule beside it
		// (cloudMetadataAddrs) must leave alone.
		{"zero-conf", zeroConfLink, "169.254.7.7", "http://169.254.7.7:8080/description.xml", "http://169.254.7.7:8080/avtransport/control"},
		{"configured", configuredLink, "127.0.0.1", "http://127.0.0.1:8080/description.xml", "http://127.0.0.1:8080/avtransport/control"},
		{"zero-conf", zeroConfLink, "127.0.0.1", "http://127.0.0.1:8080/description.xml", "http://127.0.0.1:8080/avtransport/control"},
	} {
		disp := &requestLog{handler: func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			if r.Method == http.MethodPost {
				_, _ = w.Write([]byte(chordGetProtocolInfoResponse))
				return
			}
			_, _ = w.Write([]byte(chordDeviceXML))
		}}
		c := newTestClientOn(t, disp, tc.addrs)
		c.handlePacket(context.Background(), rendererAnnouncement("uuid:same-address", tc.location), udpFrom(tc.source))
		c.wg.Wait()
		reqs := disp.requests()
		if len(reqs) == 0 || reqs[0] != "GET "+tc.location {
			t.Errorf("source %s on a %s link: requests = %q, want the description GET first", tc.source, tc.link, reqs)
		}
		served := c.cache.Snapshot()
		if len(served) != 1 || served[0].ControlURL != tc.wantControl {
			t.Errorf("source %s on a %s link: served %+v, want one renderer driven at %s", tc.source, tc.link, served, tc.wantControl)
		}
	}
}

// TestHandlePacket_NeverFetchesALinkLocalLocationOffAZeroConfLink is backlog
// B49 through the real announcement path. A UDP source is not authenticated,
// so on a link where this host holds a routable address a peer can answer an
// M-SEARCH "from" a link-local neighbour, with a LOCATION on it, and the
// same-address exception fetched that neighbour at the peer's port and path.
// On such a link nothing is requested and nothing is cached; on a
// zero-configuration link the same packet is a direct-cable device, fetched.
func TestHandlePacket_NeverFetchesALinkLocalLocationOffAZeroConfLink(t *testing.T) {
	const location = "http://169.254.7.7:8080/description.xml"
	for _, tc := range []struct {
		link     string
		addrs    func() ([]net.Addr, error)
		fetched  bool
		wantWarn int
	}{
		{"configured", configuredLink, false, 1},
		{"zero-conf", zeroConfLink, true, 0},
	} {
		t.Run(tc.link, func(t *testing.T) {
			logs := captureLogs(t)
			disp := &requestLog{handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				if r.Method == http.MethodPost {
					_, _ = w.Write([]byte(chordGetProtocolInfoResponse))
					return
				}
				_, _ = w.Write([]byte(chordDeviceXML))
			}}
			c := newTestClientOn(t, disp, tc.addrs)
			// Twice, as a device's answers to two searches arrive: one line.
			for range 2 {
				c.handlePacket(context.Background(), rendererAnnouncement("uuid:link-local", location), udpFrom("169.254.7.7"))
				c.wg.Wait()
			}
			reqs := disp.requests()
			_, cached := c.cache.Get("uuid:link-local")
			if got := len(reqs) > 0 && cached; got != tc.fetched {
				t.Errorf("fetched = %v (requests %q, cached %v), want %v", got, reqs, cached, tc.fetched)
			}
			if !tc.fetched && len(reqs) != 0 {
				t.Errorf("requests = %q, want none", reqs)
			}
			if got := strings.Count(logs.String(), "SSDP answer from a link-local address not followed"); got != tc.wantWarn {
				t.Errorf("link refusal warnings = %d, want %d; log:\n%s", got, tc.wantWarn, logs.String())
			}
		})
	}
}

// TestHandlePacket_AKnownRendererCannotMoveOntoALinkLocalLocationOffAZeroConfLink
// is B49's move-detector route: a renderer known at a LAN address is
// re-announced from a link-local address, with a LOCATION on it, on a link
// where this host holds a routable address. The detector must not follow it.
func TestHandlePacket_AKnownRendererCannotMoveOntoALinkLocalLocationOffAZeroConfLink(t *testing.T) {
	disp := &requestLog{handler: func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodPost {
			_, _ = w.Write([]byte(chordGetProtocolInfoResponse))
			return
		}
		_, _ = w.Write([]byte(chordDeviceXML))
	}}
	c := newTestClient(t, disp)
	const udn = "uuid:known-lan-renderer"
	c.handlePacket(context.Background(), alivePacket(udn, "http://192.0.2.7:8080/description.xml"), udpFrom("192.0.2.7"))
	c.wg.Wait()
	const honest = "http://192.0.2.7:8080/avtransport/control"
	if info, _ := c.cache.Get(udn); info.ControlURL != honest {
		t.Fatalf("first discovery cached ControlURL %q, want %q", info.ControlURL, honest)
	}
	before := len(disp.requests())

	c.handlePacket(context.Background(), alivePacket(udn, "http://169.254.7.7:8080/description.xml"), udpFrom("169.254.7.7"))
	c.wg.Wait()
	if reqs := disp.requests(); len(reqs) != before {
		t.Errorf("requests after the re-announcement = %q, want nothing more", reqs[before:])
	}
	if info, _ := c.cache.Get(udn); info.ControlURL != honest {
		t.Errorf("ControlURL = %q after the re-announcement, want %q kept", info.ControlURL, honest)
	}
}

// approvalLog is a requestLog that also records the DialApproval each request
// was dispatched under, which the production client's dial check reads from
// the same context.
type approvalLog struct {
	requestLog
	amu  sync.Mutex
	seen []DialApproval
}

func (d *approvalLog) Do(ctx context.Context, req *http.Request) (*http.Response, error) {
	ap, _ := ctx.Value(dialApprovalKey{}).(DialApproval)
	d.amu.Lock()
	d.seen = append(d.seen, ap)
	d.amu.Unlock()
	return d.requestLog.Do(ctx, req)
}

func (d *approvalLog) approvals() []DialApproval {
	d.amu.Lock()
	defer d.amu.Unlock()
	return append([]DialApproval(nil), d.seen...)
}

// TestHandlePacket_TheFetchRunsUnderTheLinksApproval pins that the approval
// a packet gives on the client's link reaches the fetch, as the dial check
// reads it: on a configured link a LOCATION the string check cannot place
// (a name) is fetched under an approval that does not cover the packet's
// link-local address, so a name that answers it is refused at the connect;
// on a zero-configuration link the approval covers it.
func TestHandlePacket_TheFetchRunsUnderTheLinksApproval(t *testing.T) {
	for _, tc := range []struct {
		link   string
		addrs  func() ([]net.Addr, error)
		covers bool
	}{
		{"configured", configuredLink, false},
		{"zero-conf", zeroConfLink, true},
	} {
		disp := &approvalLog{requestLog: requestLog{handler: func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			if r.Method == http.MethodPost {
				_, _ = w.Write([]byte(chordGetProtocolInfoResponse))
				return
			}
			_, _ = w.Write([]byte(chordDeviceXML))
		}}}
		c := newTestClientOn(t, disp, tc.addrs)
		c.handlePacket(context.Background(), rendererAnnouncement("uuid:named", "http://renderer.local:8080/description.xml"), udpFrom("169.254.7.7"))
		c.wg.Wait()
		got := disp.approvals()
		if len(got) != 2 {
			t.Fatalf("%s link: %d requests, want the description GET and the GetProtocolInfo POST", tc.link, len(got))
		}
		for i, ap := range got {
			if covers := ap.Permits(netip.MustParseAddr("169.254.7.7")); covers != tc.covers {
				t.Errorf("%s link, request %d: its approval (%v) covers 169.254.7.7 = %v, want %v", tc.link, i, ap, covers, tc.covers)
			}
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
// dial check. Its link is a configured one (configuredLink).
func newDefaultDispatcherClient(t *testing.T) *SSDPDiscoveryClient {
	t.Helper()
	cfg := DefaultDiscoveryConfig()
	cfg.Interface = &net.Interface{}
	cfg.InterfaceAddrs = configuredLink
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
// on the address a connect actually targets, one row per shape, for a packet
// read on a configured link (allowed) and on a zero-configuration one
// (onZeroConf). A source is the SSDP packet's address, carried in the
// request's context as the link's approval (AnnouncedOn); "" is a request
// with none. The two columns differ only where a link-local source names its
// own address (backlog B49).
func TestDefaultClientDialCheck(t *testing.T) {
	for _, tc := range []struct {
		source, address     string
		allowed, onZeroConf bool
	}{
		{"", "192.168.1.42:8080", true, true},
		{"192.0.2.7", "192.168.1.42:8080", true, true}, // another LAN host: #1050's same-host rule bounds it
		{"192.0.2.7", "203.0.113.9:443", true, true},
		{"", "127.0.0.1:7789", false, false},
		{"192.0.2.7", "127.0.0.1:7789", false, false},
		{"127.0.0.1", "127.0.0.1:7789", true, true},
		{"127.0.0.1", "127.0.0.2:7789", false, false},
		{"127.0.0.1", "[::1]:7789", false, false},
		{"::ffff:127.0.0.1", "127.0.0.1:7789", true, true}, // a mapped source is its IPv4 address
		{"192.0.2.7", "[::ffff:127.0.0.1]:7789", false, false},
		{"192.0.2.7", "0.0.0.0:7789", false, false},
		{"0.0.0.0", "0.0.0.0:7789", false, false}, // unspecified is never a peer's address
		{"192.0.2.7", "[::]:7789", false, false},
		{"192.0.2.7", "169.254.169.254:80", false, false},
		// A link-local source names its own address only on a
		// zero-configuration link: elsewhere a UDP source is a claim.
		{"169.254.10.20", "169.254.10.20:8080", false, true},
		{"169.254.10.20", "169.254.10.21:8080", false, false},
		{"169.254.7.7", "169.254.7.7:8080", false, true}, // a direct-cable device beside the metadata addresses
		{"::ffff:169.254.7.7", "169.254.7.7:8080", false, true},
		{"169.254.7.7", "[::ffff:169.254.7.7]:8080", false, true},
		// An IPv6 link-local source approves nothing: no SSDP client here
		// reads one (both are udp4), and fe80 is where IPv6 devices announce
		// from on every link.
		{"fe80::7", "[fe80::7%en0]:8080", false, false},
		{"192.0.2.7", "[fe80::1%en0]:8080", false, false},
		// A cloud metadata address is approved by nothing, not even a
		// packet from that very address (a peer on the link can spoof one),
		// and the ones that are not link-local by no announcement at all.
		{"169.254.169.254", "169.254.169.254:80", false, false},
		{"169.254.170.2", "169.254.170.2:80", false, false},
		{"::ffff:169.254.169.254", "[::ffff:169.254.169.254]:80", false, false},
		{"fe80::a9fe:a9fe", "[fe80::a9fe:a9fe%en0]:80", false, false},
		{"fd00:ec2::254", "[fd00:ec2::254]:80", false, false},
		{"", "[fd20:ce::254]:80", false, false},
		{"192.0.2.7", "100.100.100.200:80", false, false},
		{"100.100.100.200", "100.100.100.200:80", false, false},
		{"", "168.63.129.16:80", false, false},
		{"192.0.2.7", "not-an-address", false, false}, // fail closed on anything unparseable
	} {
		for _, link := range []struct {
			name     string
			zeroConf bool
			want     bool
		}{{"a configured link", false, tc.allowed}, {"a zero-configuration link", true, tc.onZeroConf}} {
			ctx := context.Background()
			if tc.source != "" {
				ctx = WithDialApproval(ctx, AnnouncedOn(udpFrom(tc.source), link.zeroConf))
			}
			err := refuseUnapprovedHostLocal(ctx, "tcp4", tc.address, nil)
			if got := err == nil; got != link.want {
				t.Errorf("source %q on %s, connect to %s: allowed = %v (err %v), want %v",
					tc.source, link.name, tc.address, got, err, link.want)
			}
		}
		// WithAnnouncementSource is the approval on a link not known to be
		// a zero-configuration one.
		if tc.source != "" {
			err := refuseUnapprovedHostLocal(WithAnnouncementSource(context.Background(), udpFrom(tc.source)), "tcp4", tc.address, nil)
			if got := err == nil; got != tc.allowed {
				t.Errorf("source %q through WithAnnouncementSource, connect to %s: allowed = %v, want %v (a configured link's)",
					tc.source, tc.address, got, tc.allowed)
			}
		}
	}
}

// TestLocationPermittedBy pins the handler's string check, one row per shape,
// for a packet read on a configured link (kept) and on a zero-configuration
// one (keptOnZeroConf). A name it cannot place passes it: that one is the
// dial check's. LocationFromSource is the configured link's answer.
func TestLocationPermittedBy(t *testing.T) {
	for _, tc := range []struct {
		location, source     string
		kept, keptOnZeroConf bool
	}{
		{"http://192.168.1.42:8080/d.xml", "192.168.1.42", true, true},
		{"http://192.168.1.42:8080/d.xml", "192.0.2.7", true, true}, // another LAN address: not this rule's business
		{"http://nas.local:8200/d.xml", "192.0.2.7", true, true},
		{"http://nas.local:8200/d.xml", "169.254.7.7", true, true},  // a name: the dial check's, on the approval
		{"http://localtest.me:7789/d.xml", "192.0.2.7", true, true}, // a public name for 127.0.0.1: the dial check's
		{"http://127.0.0.1:7789/api/stats", "192.0.2.7", false, false},
		{"http://127.0.0.1:7789/api/stats", "", false, false},
		{"http://127.0.0.1:7789/d.xml", "127.0.0.1", true, true},
		{"http://127.0.0.1.:7789/d.xml", "127.0.0.1", true, true},
		{"http://127.0.0.2:7789/d.xml", "127.0.0.1", false, false},
		{"http://[::ffff:127.0.0.1]:7789/d.xml", "127.0.0.1", true, true},
		{"http://localhost:7789/d.xml", "192.0.2.7", false, false},
		{"http://localhost:7789/d.xml", "127.0.0.1", true, true},
		{"http://localhost:7789/d.xml", "169.254.7.7", false, false},
		{"http://Console.LOCALHOST.:7789/d.xml", "192.0.2.7", false, false},
		{"http://127.1:7789/d.xml", "192.0.2.7", false, false},
		{"http://127.1:7789/d.xml", "127.0.0.1", false, false}, // a spelling no device writes, from anywhere
		{"http://2130706433:7789/d.xml", "192.0.2.7", false, false},
		{"http://0x7f.1:7789/d.xml", "192.0.2.7", false, false},
		{"http://0:7789/d.xml", "192.0.2.7", false, false},
		{"http://device.123:7789/d.xml", "192.0.2.7", false, false},
		{"http://0.0.0.0:7789/d.xml", "0.0.0.0", false, false},
		// A link-local LOCATION from that address: a zero-configuration
		// device, and on any other link a claim a UDP source cannot back.
		{"http://169.254.10.20:8080/d.xml", "169.254.10.20", false, true},
		{"http://169.254.10.20:8080/d.xml", "169.254.10.21", false, false},
		{"http://169.254.7.7:8080/d.xml", "169.254.7.7", false, true}, // a direct-cable device
		{"http://169.254.7.7:8080/d.xml", "192.0.2.7", false, false},
		{"http://[fe80::7%25en0]:8080/d.xml", "fe80::7", false, false}, // no IPv6 SSDP here
		{"http://[fe80::1%25en0]:8080/d.xml", "192.0.2.7", false, false},
		// A cloud metadata address, from any source, that address included:
		// the metadata service sends no SSDP, so such a packet was spoofed.
		{"http://169.254.169.254/latest/meta-data/", "169.254.169.254", false, false},
		{"http://169.254.170.2/v2/credentials/x", "169.254.170.2", false, false},
		{"http://[::ffff:169.254.169.254]/latest/meta-data/", "169.254.169.254", false, false},
		{"http://[fe80::a9fe:a9fe%25en0]/latest/", "fe80::a9fe:a9fe", false, false},
		{"http://[fd00:ec2::254]/latest/meta-data/", "fd00:ec2::254", false, false},
		{"http://[fd00:ec2::254]/latest/meta-data/", "192.0.2.7", false, false},
		{"http://100.100.100.200/latest/meta-data/", "192.0.2.7", false, false},
		{"http://100.100.100.200/latest/meta-data/", "100.100.100.200", false, false},
		{"http://168.63.129.16/machine/", "192.0.2.7", false, false},
		{"", "192.0.2.7", false, false},
	} {
		var src *net.UDPAddr
		if tc.source != "" {
			src = udpFrom(tc.source)
		}
		for _, link := range []struct {
			name     string
			zeroConf bool
			want     bool
		}{{"a configured link", false, tc.kept}, {"a zero-configuration link", true, tc.keptOnZeroConf}} {
			got := LocationPermittedBy(tc.location, AnnouncedOn(src, link.zeroConf))
			if kept := got != ""; kept != link.want || (kept && got != tc.location) {
				t.Errorf("LOCATION %q from %q on %s: got %q, want kept = %v", tc.location, tc.source, link.name, got, link.want)
			}
		}
		if got, want := LocationFromSource(tc.location, src), LocationPermittedBy(tc.location, AnnouncedOn(src, false)); got != want {
			t.Errorf("LocationFromSource(%q, %q) = %q, want the configured link's %q", tc.location, tc.source, got, want)
		}
	}
}
