package upnp

// Which host an SSDP LOCATION may make the upstream client fetch, and which
// host a manual upstream's control URL may name (backlog B14, the follow-ups
// #1050 recorded). The upstream half matters most: LiveHost derives every
// routed byte fetch's host:port from the cached ContentDirectory control URL,
// and the unauthenticated DLNA listener relays what those fetches answer.

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/dlna/discovery"
)

// udpFrom is the source address ReadFromUDP reports for a packet from ip.
func udpFrom(ip string) *net.UDPAddr {
	return &net.UDPAddr{IP: net.ParseIP(ip), Port: 1900}
}

// TestServerHostLocalLocationFromAnotherAddressIsNeverFetched drives the real
// SSDP path for a first-time server from a LAN address whose LOCATION names
// this host or a link-local address: no request, no entry.
func TestServerHostLocalLocationFromAnotherAddressIsNeverFetched(t *testing.T) {
	disp := &controlURLByHost{ctrl: map[string]string{}}
	cache := NewServerCache()
	c := newServerDiscoveryTestClient(t, disp, cache)
	for i, location := range []string{
		"http://127.0.0.1:7789/api/stats", // the bridge's own no-auth console
		"http://[::1]:7789/api/stats",
		"http://0.0.0.0:7789/api/stats",
		"http://[::ffff:127.0.0.1]:7789/api/stats",
		"http://localhost:7789/api/stats",
		"http://127.1:7789/api/stats",
		"http://169.254.169.254/latest/meta-data/",
	} {
		udn := fmt.Sprintf("uuid:host-local-%d", i)
		c.handlePacket(context.Background(), alivePacket(udn, location), udpFrom("192.0.2.7"))
		c.wg.Wait()
		if info, ok := cache.Get(udn); ok {
			t.Errorf("LOCATION %q from 192.0.2.7: cached %+v", location, info)
		}
	}
	if reqs := disp.requests(); len(reqs) != 0 {
		t.Errorf("requests = %q, want none", reqs)
	}
}

// TestServerCloudMetadataLocationIsNeverFetched drives the real SSDP path
// for a server whose LOCATION names a cloud metadata address, announced from
// a LAN address and from that very address, which a peer on the link can
// spoof (CodeRabbit on #1074). The same-address exception fetched the
// second, and a metadata address that is not link-local was fetched from
// anywhere; had its description named a ContentDirectory, the ingest's SOAP
// and every byte fetch would have followed, relayed to the unauthenticated
// DLNA listener. No request, no entry. A server on a direct cable, beside
// the metadata addresses, is still fetched from its own address.
func TestServerCloudMetadataLocationIsNeverFetched(t *testing.T) {
	disp := &controlURLByHost{ctrl: map[string]string{"169.254.7.7:8200": "/ctl/ContentDir"}}
	cache := NewServerCache()
	c := newServerDiscoveryTestClient(t, disp, cache)
	for i, addr := range []string{"169.254.169.254", "169.254.170.2", "fe80::a9fe:a9fe", "fd00:ec2::254", "100.100.100.200"} {
		location := "http://" + net.JoinHostPort(addr, "80") + "/latest/meta-data/"
		for j, source := range []string{addr, "192.0.2.7"} {
			udn := fmt.Sprintf("uuid:metadata-%d-%d", i, j)
			c.handlePacket(context.Background(), alivePacket(udn, location), udpFrom(source))
			c.wg.Wait()
			if info, ok := cache.Get(udn); ok {
				t.Errorf("LOCATION %q from %s: cached %+v", location, source, info)
			}
		}
	}
	if reqs := disp.requests(); len(reqs) != 0 {
		t.Errorf("requests = %q, want none", reqs)
	}

	direct := newServerDiscoveryTestClientOn(t, disp, cache, zeroConfServerLink)
	direct.handlePacket(context.Background(), alivePacket("uuid:direct-cable", "http://169.254.7.7:8200/desc.xml"), udpFrom("169.254.7.7"))
	direct.wg.Wait()
	info, ok := cache.Get("uuid:direct-cable")
	if want := "http://169.254.7.7:8200/ctl/ContentDir"; !ok || info.ContentDirectoryControlURL != want {
		t.Errorf("a server on a direct cable: cached %+v (ok %v), want its control URL %s", info, ok, want)
	}
	if got, want := info.DialApproval, discovery.AnnouncedOn(udpFrom("169.254.7.7"), true); got != want {
		t.Errorf("a server on a direct cable: approval %v, want %v", got, want)
	}
}

// TestServerLinkLocalSourceIsApprovedOnlyOnAZeroConfLink is backlog B49 on
// the upstream path, which matters most: a server cached here has its control
// URL dialled by the ingest's SOAP and by every byte fetch of its routed
// tracks, whose answers the proxy relays to the unauthenticated DLNA
// listener. A UDP source is not authenticated, so on a link where this host
// holds a routable address a packet "from" a link-local neighbour with a
// LOCATION on it is not fetched, and nothing is cached; and one whose
// LOCATION is a name, which the string check cannot place and which is
// fetched wherever it resolves, is cached with an approval that does not
// cover that address, so when the name later answers it the ingest's SOAP
// and the proxy's byte fetches are refused at the dial check (cmd/bridge's
// TestAPacketFromALinkLocalAddressOffAZeroConfLinkApprovesNoLaterDialThere
// drives them). On a zero-configuration link the same packets are a
// direct-cable server: fetched, and cached with the approval that covers its
// address.
func TestServerLinkLocalSourceIsApprovedOnlyOnAZeroConfLink(t *testing.T) {
	for _, tc := range []struct {
		link, location string
		addrs          func() ([]net.Addr, error)
		fetched        bool
	}{
		{"configured", "http://169.254.7.7:8200/desc.xml", configuredServerLink, false},
		{"zero-conf", "http://169.254.7.7:8200/desc.xml", zeroConfServerLink, true},
		{"configured", "http://server.rebind.test:8200/desc.xml", configuredServerLink, true},
		{"zero-conf", "http://server.rebind.test:8200/desc.xml", zeroConfServerLink, true},
	} {
		disp := &controlURLByHost{ctrl: map[string]string{
			"169.254.7.7:8200":        "/ctl/ContentDir",
			"server.rebind.test:8200": "/ctl/ContentDir",
		}}
		cache := NewServerCache()
		c := newServerDiscoveryTestClientOn(t, disp, cache, tc.addrs)
		c.handlePacket(context.Background(), alivePacket("uuid:link-local", tc.location), udpFrom("169.254.7.7"))
		c.wg.Wait()
		info, cached := cache.Get("uuid:link-local")
		reqs := disp.requests()
		if got := cached && len(reqs) == 1; got != tc.fetched {
			t.Errorf("%s at %s on a %s link: fetched = %v (requests %q, cached %+v), want %v",
				tc.location, "169.254.7.7", tc.link, got, reqs, info, tc.fetched)
			continue
		}
		if !tc.fetched {
			continue
		}
		covers := info.DialApproval.Permits(netip.MustParseAddr("169.254.7.7"))
		if want := tc.link == "zero-conf"; covers != want {
			t.Errorf("%s on a %s link: the cached approval (%v) covers 169.254.7.7 = %v, want %v",
				tc.location, tc.link, info.DialApproval, covers, want)
		}
	}
}

// TestAKnownServerCannotMoveOntoAHostLocalLocation is #1050's move attack one
// step earlier: a server known at one address is re-announced with a LOCATION
// the move detector must not follow, so the description there is never
// fetched and the cached control URL, and LiveHost's target with it, stays
// where it was. Two re-announcements: one on the bridge's own console from
// another LAN address (backlog B14), and one on a link-local address from
// that very address, on a link where this host holds a routable address
// (backlog B49).
func TestAKnownServerCannotMoveOntoAHostLocalLocation(t *testing.T) {
	for _, move := range []struct{ location, source string }{
		{"http://127.0.0.1:7789/desc.xml", "192.0.2.99"},
		{"http://169.254.7.7:8200/desc.xml", "169.254.7.7"},
	} {
		disp := &controlURLByHost{ctrl: map[string]string{
			"192.0.2.7:8200":   "/ctl/ContentDir",
			"127.0.0.1:7789":   "/api/stats",
			"169.254.7.7:8200": "/ctl/ContentDir",
		}}
		cache := NewServerCache()
		c := newServerDiscoveryTestClient(t, disp, cache)
		c.handlePacket(context.Background(), alivePacket("uuid:ms", "http://192.0.2.7:8200/desc.xml"), udpFrom("192.0.2.7"))
		c.wg.Wait()
		const honest = "http://192.0.2.7:8200/ctl/ContentDir"
		if info, _ := cache.Get("uuid:ms"); info.ContentDirectoryControlURL != honest {
			t.Fatalf("first discovery cached %q, want %q", info.ContentDirectoryControlURL, honest)
		}

		c.handlePacket(context.Background(), alivePacket("uuid:ms", move.location), udpFrom(move.source))
		c.wg.Wait()
		if reqs := disp.requests(); len(reqs) != 1 {
			t.Errorf("re-announced at %s from %s: requests = %q, want only the first description GET: "+
				"the move detector followed it", move.location, move.source, reqs)
		}
		if info, _ := cache.Get("uuid:ms"); info.ContentDirectoryControlURL != honest {
			t.Errorf("re-announced at %s from %s: ContentDirectoryControlURL = %q, want %q kept",
				move.location, move.source, info.ContentDirectoryControlURL, honest)
		}
	}
}

// loopbackServerDescription serves a MediaServer description on 127.0.0.1,
// with a relative ContentDirectory control URL, and counts requests.
type loopbackServerDescription struct {
	*httptest.Server
	mu   sync.Mutex
	hits int
}

func newLoopbackServerDescription(t *testing.T) *loopbackServerDescription {
	t.Helper()
	s := &loopbackServerDescription{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		s.hits++
		s.mu.Unlock()
		fmt.Fprint(w, descXML("uuid:local-ms", "Local MS", "/ctl/ContentDir"))
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *loopbackServerDescription) requestCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hits
}

// localhostLocation names s by `localhost` rather than by its address.
func (s *loopbackServerDescription) localhostLocation(t *testing.T) string {
	t.Helper()
	_, port, err := net.SplitHostPort(s.Listener.Addr().String())
	if err != nil {
		t.Fatalf("listener address: %v", err)
	}
	return "http://localhost:" + port + "/desc.xml"
}

// newDefaultServerDiscoveryClient builds the client as cmd/bridge does, with
// no Dispatcher, so the fetch goes through the production client.
func newDefaultServerDiscoveryClient(t *testing.T, cache *ServerCache) *MediaServerDiscoveryClient {
	t.Helper()
	c, err := NewMediaServerDiscoveryClient(DiscoveryConfig{
		Interface:          &net.Interface{},
		InterfaceAddrs:     configuredServerLink,
		DetailFetchTimeout: 3 * time.Second,
	}, cache)
	if err != nil {
		t.Fatalf("construct: %v", err)
	}
	return c
}

// TestServerDiscoveryDefaultClientNeverConnectsToThisHostOnAnotherAddressesSay
// pins the dial check on the upstream client's production client. The literal
// address is refused by the handler before any fetch; this drives the client
// itself, with the name the handler would also refuse, because a public DNS
// name pointed at 127.0.0.1 is the shape only the connect can see.
func TestServerDiscoveryDefaultClientNeverConnectsToThisHostOnAnotherAddressesSay(t *testing.T) {
	srv := newLoopbackServerDescription(t)
	c := newDefaultServerDiscoveryClient(t, NewServerCache())
	fromLAN := discovery.WithAnnouncementSource(context.Background(), udpFrom("192.0.2.7"))
	for _, url := range []string{srv.localhostLocation(t), srv.URL + "/desc.xml"} {
		if _, err := discovery.FetchDeviceDescription(fromLAN, c.dispatcher, url); err == nil {
			t.Errorf("the fetch of %s, announced from 192.0.2.7, succeeded", url)
		}
	}
	if n := srv.requestCount(); n != 0 {
		t.Errorf("the loopback listener saw %d requests on the say-so of a packet from 192.0.2.7", n)
	}
}

// TestServerDiscoveryDefaultClientConnectsToThisHostWhenThePacketCameFromIt is
// the twin through the real SSDP path: the packet's source reaches the fetch,
// so a server on this host that announces from 127.0.0.1 is fetched and
// cached.
func TestServerDiscoveryDefaultClientConnectsToThisHostWhenThePacketCameFromIt(t *testing.T) {
	srv := newLoopbackServerDescription(t)
	cache := NewServerCache()
	c := newDefaultServerDiscoveryClient(t, cache)
	location := srv.localhostLocation(t)
	c.handlePacket(context.Background(), alivePacket("uuid:local-ms", location), udpFrom("127.0.0.1"))
	c.wg.Wait()
	if n := srv.requestCount(); n != 1 {
		t.Fatalf("the loopback listener saw %d requests, want the one description GET", n)
	}
	info, ok := cache.Get("uuid:local-ms")
	if !ok || info.DescriptionURL != location {
		t.Errorf("cached %+v (present %v), want the server described at %s", info, ok, location)
	}

	// The move path hands the source on too: the same server, announced at
	// its address rather than its name, reads as a move (the detector
	// compares host strings) and is re-fetched there.
	moved := srv.URL + "/desc.xml"
	c.handlePacket(context.Background(), alivePacket("uuid:local-ms", moved), udpFrom("127.0.0.1"))
	c.wg.Wait()
	if info, _ := cache.Get("uuid:local-ms"); info.DescriptionURL != moved {
		t.Errorf("after the move, DescriptionURL = %q, want %q", info.DescriptionURL, moved)
	}
}

// manualDescriptionAt serves descXML for a description URL on any host, so a
// manual upstream can be described from an address no test can listen on.
type manualDescriptionAt struct{ ctrl string }

func (d manualDescriptionAt) Do(_ context.Context, _ *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	rec.WriteHeader(http.StatusOK)
	_, _ = rec.WriteString(descXML("uuid:manual", "Manual", d.ctrl))
	return rec.Result(), nil
}

// TestManualPollerRefusesAHostLocalControlURLFromADescriptionElsewhere is the
// bound on the operator's approval. A manual description URL keeps a control
// URL on another host (#1050's escape hatch), but not one on THIS host or a
// link-local address when the description itself is elsewhere: the server
// the operator named would otherwise choose where LiveHost sends every byte
// fetch of its tracks, the bridge's own console included.
func TestManualPollerRefusesAHostLocalControlURLFromADescriptionElsewhere(t *testing.T) {
	for _, ctrl := range []string{
		"http://127.0.0.1:7789/api/stats",
		"http://localhost:7789/api/stats",
		"http://[::1]:7789/api/stats",
		"http://0.0.0.0:7789/api/stats",
		"http://127.1:7789/api/stats",
		"http://169.254.169.254/latest/meta-data/",
	} {
		cache := NewServerCache()
		var buf bytes.Buffer
		p := manualTestPoller(t, cache, []ManualServer{{
			Key: "manual:elsewhere", DescriptionURL: "http://192.0.2.50:8200/rootDesc.xml", Name: "Elsewhere",
		}}, nil, &buf)
		p.dispatcher = manualDescriptionAt{ctrl: ctrl}
		p.PollOnce(context.Background())
		if info, ok := cache.Get("manual:elsewhere"); ok {
			t.Errorf("control URL %q from a description on 192.0.2.50: cached %+v", ctrl, info)
		}
	}
}

// TestManualPollerKeepsAHostLocalControlURLFromAHostLocalDescription is the
// other side: an operator pointing a manual URL at a server on this host
// means it, and that server's control URL on this host is kept, by any
// spelling.
func TestManualPollerKeepsAHostLocalControlURLFromAHostLocalDescription(t *testing.T) {
	var mu sync.Mutex
	var ctrl string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		served := ctrl
		mu.Unlock()
		fmt.Fprint(w, descXML("uuid:local", "Local", served))
	}))
	defer srv.Close()
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	for _, c := range []string{"http://localhost:" + port + "/ctl", "http://127.0.0.1:" + port + "/ctl", "/ctl"} {
		mu.Lock()
		ctrl = c
		mu.Unlock()
		cache := NewServerCache()
		var buf bytes.Buffer
		p := manualTestPoller(t, cache, []ManualServer{{
			Key: "manual:local", DescriptionURL: srv.URL + "/rootDesc.xml", Name: "Local",
		}}, nil, &buf)
		p.PollOnce(context.Background())
		if info, ok := cache.Get("manual:local"); !ok || info.ContentDirectoryControlURL == "" {
			t.Errorf("control URL %q from a description on this host: not cached; log:\n%s", c, buf.String())
		}
	}
}
