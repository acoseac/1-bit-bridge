package main

// A routed server's control URL is checked once, when the server is found,
// and dialled for as long as it is cached: every ingest walk sends its SOAP
// to it, and every byte fetch of the server's tracks goes to the host:port
// LiveHost derives from it (/v1/download, /dlna/file/{trackID} on the
// unauthenticated DLNA listener, the web player). A NAME in it resolves
// again at each of those dials (backlog B36). A peer that passed discovery
// with a name answering its own LAN address could answer 127.0.0.1 for it
// later, and both requests then reached the bridge's own console, whose
// answers the proxy relayed (measured on the unchanged code: "CONSOLE POST
// /ctl", "CONSOLE GET /api/stats", and a 200 carrying the console's body).
//
// These tests drive the real wiring end to end: discoveryServerResolver and
// upnpUpstreamSOAPHTTPClient under a real Ingester, and
// serverCacheHostResolver under upnpproxy, with a DNS server the test
// controls (internal/dnstest) answering the name first with this host's LAN
// address and then with 127.0.0.1.

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/dlna/discovery"
	"github.com/acoseac/1-bit-bridge/internal/dnstest"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/upnp"
	"github.com/acoseac/1-bit-bridge/internal/upnpingest"
	"github.com/acoseac/1-bit-bridge/internal/upnpproxy"
)

// rebindingName is the name a routed server's control URL names its host by.
const rebindingName = "upstream.rebind.test"

// rebindingSOAPUpdateID and rebindingSOAPBrowse are the answers a stand-in
// gives the ingest's two SOAP actions: a SystemUpdateID, and an empty root.
const (
	rebindingSOAPUpdateID = `<?xml version="1.0"?><s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body>` +
		`<u:GetSystemUpdateIDResponse xmlns:u="urn:schemas-upnp-org:service:ContentDirectory:1"><Id>1</Id>` +
		`</u:GetSystemUpdateIDResponse></s:Body></s:Envelope>`
	rebindingSOAPBrowse = `<?xml version="1.0"?><s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body>` +
		`<u:BrowseResponse xmlns:u="urn:schemas-upnp-org:service:ContentDirectory:1">` +
		`<Result>&lt;DIDL-Lite xmlns=&quot;urn:schemas-upnp-org:metadata-1-0/DIDL-Lite/&quot;&gt;&lt;/DIDL-Lite&gt;</Result>` +
		`<NumberReturned>0</NumberReturned><TotalMatches>0</TotalMatches><UpdateID>1</UpdateID>` +
		`</u:BrowseResponse></s:Body></s:Envelope>`
)

// rebindingStandIn stands in for a host the name can lead to: the server's
// own LAN address, or this machine (the bridge's console, in the attack).
// It answers the ingest's SOAP and a byte fetch, records what reached it,
// and closes every connection, as a rebinding server does so that each
// request resolves the name again.
type rebindingStandIn struct {
	srv  *httptest.Server
	mu   sync.Mutex
	seen []string
}

// newRebindingStandIn serves on ln until the test ends.
func newRebindingStandIn(t *testing.T, ln net.Listener) *rebindingStandIn {
	t.Helper()
	s := &rebindingStandIn{}
	s.srv = httptest.NewUnstartedServer(http.HandlerFunc(s.serve))
	_ = s.srv.Listener.Close() // the one NewUnstartedServer opened
	s.srv.Listener = ln
	s.srv.Start()
	t.Cleanup(s.srv.Close)
	return s
}

func (s *rebindingStandIn) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.seen = append(s.seen, r.Method+" "+r.URL.Path)
	s.mu.Unlock()
	w.Header().Set("Connection", "close")
	if r.Method != http.MethodPost {
		_, _ = io.WriteString(w, "fLaC")
		return
	}
	w.Header().Set("Content-Type", `text/xml; charset="utf-8"`)
	if strings.Contains(r.Header.Get("SOAPAction"), "#GetSystemUpdateID") {
		_, _ = io.WriteString(w, rebindingSOAPUpdateID)
		return
	}
	_, _ = io.WriteString(w, rebindingSOAPBrowse)
}

// take returns what reached the stand-in since the last take.
func (s *rebindingStandIn) take() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := s.seen
	s.seen = nil
	return seen
}

// port is the port the stand-in listens on.
func (s *rebindingStandIn) port(t *testing.T) string {
	t.Helper()
	_, port, err := net.SplitHostPort(s.srv.Listener.Addr().String())
	if err != nil {
		t.Fatalf("stand-in address: %v", err)
	}
	return port
}

// rebindingLANAddr is an IPv4 address this host holds on a LAN, private
// first, or the zero Addr when it holds none.
func rebindingLANAddr(t *testing.T) netip.Addr {
	t.Helper()
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return netip.Addr{}
	}
	var global netip.Addr
	for _, a := range addrs {
		n, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip, ok := netip.AddrFromSlice(n.IP)
		if !ok {
			continue
		}
		ip = ip.Unmap()
		if !ip.Is4() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
			continue
		}
		if ip.IsPrivate() {
			return ip
		}
		if !global.IsValid() {
			global = ip
		}
	}
	return global
}

// rebindingHosts is the stage the tests share: the DNS server every device
// dial resolves through, a stand-in for this machine on 127.0.0.1, and one
// for the server on the LAN address at the same port when this host has
// one (nil otherwise).
type rebindingHosts struct {
	dns     *dnstest.Server
	lan     netip.Addr
	console *rebindingStandIn
	lanHost *rebindingStandIn
	port    string
}

// newRebindingHosts sets the stage up until the test ends. The name answers
// 127.0.0.1 until the test says otherwise.
func newRebindingHosts(t *testing.T) *rebindingHosts {
	t.Helper()
	h := &rebindingHosts{lan: rebindingLANAddr(t)}
	h.dns = dnstest.Start(t, rebindingName, netip.MustParseAddr("127.0.0.1"))
	t.Cleanup(discovery.UseResolverForTest(h.dns.Resolver()))
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen on 127.0.0.1: %v", err)
	}
	h.console = newRebindingStandIn(t, ln)
	h.port = h.console.port(t)
	if h.lan.IsValid() {
		if ln, err := net.Listen("tcp4", net.JoinHostPort(h.lan.String(), h.port)); err == nil {
			h.lanHost = newRebindingStandIn(t, ln)
		} else {
			t.Logf("no stand-in on the LAN address (%v); only the rebinding half runs", err)
		}
	} else {
		t.Log("this host holds no LAN address; only the rebinding half runs")
	}
	return h
}

// routedServer is one configured upstream as the bridge serves it, over the
// production wiring: its entry in a discovery cache, an Ingester that
// resolves it through discoveryServerResolver and sends its SOAP with
// upnpUpstreamSOAPHTTPClient, and a proxy over serverCacheHostResolver.
type routedServer struct {
	key   string
	ing   *upnpingest.Ingester
	proxy *upnpproxy.Proxy
}

// newRoutedServer caches srv with controlURL, as discovery leaves it under
// approval.
func newRoutedServer(t *testing.T, srv config.UPnPUpstreamServerConfig, controlURL string, approval discovery.DialApproval) *routedServer {
	t.Helper()
	key := upnpingest.StableServerKey(srv)
	cache := upnp.NewServerCache()
	cache.Upsert(upnp.ServerInfo{UDN: key, ContentDirectoryControlURL: controlURL, DialApproval: approval, LastSeenAt: time.Now()})
	cds := upnp.NewContentDirectoryClient(&discovery.HTTPClientDispatcher{Client: upnpUpstreamSOAPHTTPClient(3 * time.Second)})
	ing, err := upnpingest.NewIngester(config.UPnPUpstreamConfig{Enabled: true, Servers: []config.UPnPUpstreamServerConfig{srv}},
		cds, &discoveryServerResolver{cache: cache}, openServeCancelStore(t), nil)
	if err != nil {
		t.Fatalf("NewIngester: %v", err)
	}
	return &routedServer{key: key, ing: ing, proxy: upnpproxy.New(&serverCacheHostResolver{cache: cache}, nil)}
}

// fetchBoth sends what the bridge sends a routed server: one ingest walk
// (its GetSystemUpdateID and Browse SOAP) and one byte fetch through the
// proxy. It returns the ingest's per-server error and the proxy's
// pre-stream error.
func (r *routedServer) fetchBoth(t *testing.T) (ingestErr error, proxyErr *upnpproxy.PreStreamError) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := r.ing.Run(ctx, upnpingest.Options{ForceWalk: true})
	if err != nil || len(res.PerServer) != 1 {
		t.Fatalf("ingest Run = (%+v, %v), want one server's result", res, err)
	}
	rt := &manifest.UPnPRouting{ServerUDN: r.key, ResURL: "/MediaItems/1.flac"}
	return res.PerServer[0].Err, r.proxy.Serve(ctx, httptest.NewRecorder(), http.MethodGet, http.Header{}, rt)
}

// TestARebindingNameCannotTakeTheIngestOrAByteFetchToThisMachine caches a
// server the way discovery leaves it (a control URL naming its host by a
// name, and the approval it was found under), lets the name answer this
// host's LAN address, and then 127.0.0.1. The server's own requests reach
// the LAN stand-in; after the rebinding, a server whose approval does not
// cover this machine reaches nothing, and one whose approval does (a server
// announcing from 127.0.0.1, a manual URL on localhost) still reaches it,
// which is what shows the refusal is the check and that both the ingest and
// the proxy carry the approval to the connect.
func TestARebindingNameCannotTakeTheIngestOrAByteFetchToThisMachine(t *testing.T) {
	h := newRebindingHosts(t)
	controlURL := "http://" + rebindingName + ":" + h.port + "/ctl"
	manualByName := "http://" + rebindingName + ":8200/rootDesc.xml"
	manualOnThisMachine := "http://localhost:" + h.port + "/rootDesc.xml"
	fromLAN := &net.UDPAddr{IP: net.ParseIP("192.0.2.66"), Port: 1900}
	fromThisMachine := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1900}
	for _, tc := range []struct {
		name     string
		srv      config.UPnPUpstreamServerConfig
		approval discovery.DialApproval
		reaches  bool // whether the rebound requests may reach this machine
	}{
		{"a server announcing from a LAN address", config.UPnPUpstreamServerConfig{Name: "LAN", UDN: "uuid:rebind-lan"},
			discovery.AnnouncedFrom(fromLAN), false},
		{"a manual server named by a name", config.UPnPUpstreamServerConfig{Name: "Named", ManualDescriptionURL: manualByName},
			discovery.OperatorChose(manualByName), false},
		{"a server announcing from this machine", config.UPnPUpstreamServerConfig{Name: "Local", UDN: "uuid:rebind-local"},
			discovery.AnnouncedFrom(fromThisMachine), true},
		{"a manual server on localhost", config.UPnPUpstreamServerConfig{Name: "Localhost", ManualDescriptionURL: manualOnThisMachine},
			discovery.OperatorChose(manualOnThisMachine), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := newRoutedServer(t, tc.srv, controlURL, tc.approval)
			wantAll := []string{"POST /ctl", "POST /ctl", "GET /MediaItems/1.flac"}

			if h.lanHost != nil {
				h.dns.Answer(h.lan)
				server.fetchBoth(t)
				if got := h.lanHost.take(); fmt.Sprint(got) != fmt.Sprint(wantAll) {
					t.Errorf("with %s answering %s, the LAN stand-in saw %q, want %q", rebindingName, h.lan, got, wantAll)
				}
			}

			h.dns.Answer(netip.MustParseAddr("127.0.0.1"))
			ingestErr, proxyErr := server.fetchBoth(t)
			got := h.console.take()
			if tc.reaches {
				if fmt.Sprint(got) != fmt.Sprint(wantAll) || ingestErr != nil || proxyErr != nil {
					t.Errorf("with %s answering 127.0.0.1, this machine saw %q (ingest %v, proxy %v), want %q: "+
						"the approval covers it", rebindingName, got, ingestErr, proxyErr, wantAll)
				}
				return
			}
			if len(got) != 0 {
				t.Errorf("with %s answering 127.0.0.1, this machine saw %q, want nothing: "+
					"the approval the control URL came with does not cover it", rebindingName, got)
			}
			if ingestErr == nil {
				t.Error("the ingest reported no error for a walk that could not connect")
			}
			if proxyErr == nil || proxyErr.Code != "upnp_upstream_unreachable" {
				t.Errorf("proxy = %v, want upnp_upstream_unreachable", proxyErr)
			}
		})
	}
	if h.console.take() != nil || (h.lanHost != nil && h.lanHost.take() != nil) {
		t.Error("a stand-in saw a request no case accounted for")
	}
}

// TestAPacketFromAMetadataAddressApprovesNoLaterDialThere is the chain the
// cloud metadata rule closes (CodeRabbit on #1074), on a cloud VM. A peer on
// the link spoofs an SSDP packet from 169.254.169.254 whose LOCATION names
// its host by a name, which it answers with its own address while discovery
// fetches the description, so the server is cached under
// AnnouncedFrom(169.254.169.254). Then the name answers 169.254.169.254, and
// the same-address exception approved that connect for the ingest's SOAP and
// for every byte fetch, whose answer the proxy relays to the unauthenticated
// DLNA listener: the instance's credentials, on AWS's IMDSv1. Neither may
// connect there now, and both are refused at the dial check, before any
// packet leaves.
func TestAPacketFromAMetadataAddressApprovesNoLaterDialThere(t *testing.T) {
	h := newRebindingHosts(t)
	spoofed := &net.UDPAddr{IP: net.ParseIP("169.254.169.254"), Port: 1900}
	server := newRoutedServer(t, config.UPnPUpstreamServerConfig{Name: "Spoofed", UDN: "uuid:rebind-metadata"},
		"http://"+rebindingName+":"+h.port+"/ctl", discovery.AnnouncedFrom(spoofed))
	h.dns.Answer(netip.MustParseAddr("169.254.169.254"))
	ingestErr, proxyErr := server.fetchBoth(t)
	const refusal = "refusing to connect to a cloud metadata address"
	if ingestErr == nil || !strings.Contains(ingestErr.Error(), refusal) {
		t.Errorf("ingest error = %v, want the dial check's refusal (%q)", ingestErr, refusal)
	}
	if proxyErr == nil || proxyErr.Code != "upnp_upstream_unreachable" || !strings.Contains(proxyErr.Error(), refusal) {
		t.Errorf("proxy error = %v, want upnp_upstream_unreachable with the dial check's refusal (%q)", proxyErr, refusal)
	}
	if h.console.take() != nil || (h.lanHost != nil && h.lanHost.take() != nil) {
		t.Error("a stand-in saw a request, which no case here sends")
	}
}

// TestAPacketFromALinkLocalAddressOffAZeroConfLinkApprovesNoLaterDialThere
// is the same chain for a link-local address that is not a metadata one
// (backlog B49), on a link where this host holds a routable address. A
// packet "from" a link-local neighbour, whose LOCATION names a host that
// resolves elsewhere while discovery fetches the description, is cached
// under the approval a client on such a link gives it
// (discovery.AnnouncedOn(src, false), which is AnnouncedFrom). Then the name
// answers the neighbour's address: the same-address exception approved that
// connect for the ingest's SOAP and for every byte fetch, whose answers the
// proxy relays to the unauthenticated DLNA listener. On a link that is not a
// zero-configuration one it approves nothing, and both are refused at the
// dial check, before any packet leaves. (On a zero-configuration link the
// packet's own address stays approved: that is the direct-cable device the
// exception is for, and internal/upnp's tests pin it.)
func TestAPacketFromALinkLocalAddressOffAZeroConfLinkApprovesNoLaterDialThere(t *testing.T) {
	h := newRebindingHosts(t)
	from := &net.UDPAddr{IP: net.ParseIP("169.254.7.7"), Port: 1900}
	approval := discovery.AnnouncedOn(from, false)
	if approval != discovery.AnnouncedFrom(from) {
		t.Fatalf("AnnouncedOn(src, false) = %v, AnnouncedFrom(src) = %v: the two spellings of a configured link's approval differ", approval, discovery.AnnouncedFrom(from))
	}
	server := newRoutedServer(t, config.UPnPUpstreamServerConfig{Name: "Neighbour", UDN: "uuid:rebind-link-local"},
		"http://"+rebindingName+":"+h.port+"/ctl", approval)
	h.dns.Answer(netip.MustParseAddr("169.254.7.7"))
	ingestErr, proxyErr := server.fetchBoth(t)
	const refusal = "refusing to connect to this machine or a link-local address"
	if ingestErr == nil || !strings.Contains(ingestErr.Error(), refusal) {
		t.Errorf("ingest error = %v, want the dial check's refusal (%q)", ingestErr, refusal)
	}
	if proxyErr == nil || proxyErr.Code != "upnp_upstream_unreachable" || !strings.Contains(proxyErr.Error(), refusal) {
		t.Errorf("proxy error = %v, want upnp_upstream_unreachable with the dial check's refusal (%q)", proxyErr, refusal)
	}
	if h.console.take() != nil || (h.lanHost != nil && h.lanHost.take() != nil) {
		t.Error("a stand-in saw a request, which no case here sends")
	}
}
