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

// TestAKnownServerCannotMoveOntoAHostLocalLocation is #1050's move attack one
// step earlier: a server known at one address is re-announced from another
// with a LOCATION on the bridge's own console. The move detector must not
// follow it, so the description there is never fetched and the cached control
// URL stays where it was.
func TestAKnownServerCannotMoveOntoAHostLocalLocation(t *testing.T) {
	disp := &controlURLByHost{ctrl: map[string]string{
		"192.0.2.7:8200": "/ctl/ContentDir",
		"127.0.0.1:7789": "/api/stats",
	}}
	cache := NewServerCache()
	c := newServerDiscoveryTestClient(t, disp, cache)
	c.handlePacket(context.Background(), alivePacket("uuid:ms", "http://192.0.2.7:8200/desc.xml"), udpFrom("192.0.2.7"))
	c.wg.Wait()
	const honest = "http://192.0.2.7:8200/ctl/ContentDir"
	if info, _ := cache.Get("uuid:ms"); info.ContentDirectoryControlURL != honest {
		t.Fatalf("first discovery cached %q, want %q", info.ContentDirectoryControlURL, honest)
	}

	c.handlePacket(context.Background(), alivePacket("uuid:ms", "http://127.0.0.1:7789/desc.xml"), udpFrom("192.0.2.99"))
	c.wg.Wait()
	if reqs := disp.requests(); len(reqs) != 1 {
		t.Errorf("requests = %q, want only the first description GET: "+
			"the move detector followed a LOCATION on this host", reqs)
	}
	if info, _ := cache.Get("uuid:ms"); info.ContentDirectoryControlURL != honest {
		t.Errorf("ContentDirectoryControlURL = %q, want %q kept", info.ContentDirectoryControlURL, honest)
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
