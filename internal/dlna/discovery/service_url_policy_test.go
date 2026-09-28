package discovery

// External audit 2026-09-23, finding M3: a discovered UPnP device's service
// URLs are its own say-so. Every test here pins one half of the rule the iOS
// app applies to its own SSDP path (UPnPURLPolicy, #1911) and to the renderers
// a bridge relays (#1977): a service URL must be http(s) with a host, and one
// in a DISCOVERED description must stay on the description URL's host (any
// port). A description the operator chose keeps a URL on another host, never
// one in another scheme.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// avtOnlyDescription is a renderer description whose one service is
// AVTransport with the given control URL.
func avtOnlyDescription(control string) []byte {
	return []byte(`<?xml version="1.0"?><root><device>` +
		`<friendlyName>Policy Renderer</friendlyName><UDN>uuid:policy</UDN><serviceList>` +
		`<service><serviceType>urn:schemas-upnp-org:service:AVTransport:1</serviceType>` +
		`<controlURL>` + control + `</controlURL></service>` +
		`</serviceList></device></root>`)
}

// TestParseDeviceDescription_RefusesAControlURLThatIsNotHTTPWithAHost pins
// the scheme and host half, for BOTH sources: the operator's choice vouches
// for another host, never for another scheme. A refused AVTransport control
// URL leaves the renderer with no AVTransport, which is the parse error the
// discovery client already treats as "not a renderer we can drive".
func TestParseDeviceDescription_RefusesAControlURLThatIsNotHTTPWithAHost(t *testing.T) {
	const base = "http://192.168.1.42:7790/d.xml"
	for _, control := range []string{
		"file:///etc/passwd",
		"ftp://192.168.1.42/avt",
		"http:///avt/control", // parses, and names no host
		"http://:8080/avt/control",
		"data:text/plain,x",
		"gopher://192.168.1.42/",
	} {
		for _, source := range []DescriptionSource{SourceDiscovered, SourceUserChosen} {
			desc, err := ParseDeviceDescriptionWithSource(avtOnlyDescription(control), base, source)
			if err == nil || !strings.Contains(err.Error(), "AVTransport") {
				t.Errorf("control %q (source %d): err = %v, want the no-AVTransport refusal", control, source, err)
			}
			if svc, ok := desc.Services[ServiceAVTransport]; ok {
				t.Errorf("control %q (source %d): AVTransport kept as %q", control, source, svc.ControlURL)
			}
		}
	}
}

// TestParseDeviceDescription_DiscoveredRefusesAControlURLOnAnotherHost is the
// SSRF half: a spoofed description must not steer the SOAP requests that
// follow (and, for an upstream, every byte fetch LiveHost derives from the
// control URL) to another machine. The strict source is also the DEFAULT:
// ParseDeviceDescription, with no source named, refuses too, and so does a
// source value this build does not know.
func TestParseDeviceDescription_DiscoveredRefusesAControlURLOnAnotherHost(t *testing.T) {
	const base = "http://192.168.1.42:7790/d.xml"
	for _, control := range []string{
		"http://other.host:7790/avt/control",
		"http://127.0.0.1:7789/api/stats", // the bridge's own no-auth console
		"//other.host/avt/control",        // a network-path reference is another host too
		"http://192.168.1.42@other.host/avt/control",
	} {
		for _, p := range []struct {
			name  string
			parse func(body []byte, baseURL string) (DeviceDescription, error)
		}{
			{"default", ParseDeviceDescription},
			{"discovered", parseWithSource(SourceDiscovered)},
			{"unknown source", parseWithSource(DescriptionSource(99))},
		} {
			desc, err := p.parse(avtOnlyDescription(control), base)
			if err == nil || !strings.Contains(err.Error(), "AVTransport") {
				t.Errorf("%s, control %q: err = %v, want the no-AVTransport refusal", p.name, control, err)
			}
			if svc, ok := desc.Services[ServiceAVTransport]; ok {
				t.Errorf("%s, control %q: AVTransport kept as %q", p.name, control, svc.ControlURL)
			}
		}
	}
}

// parseWithSource returns ParseDeviceDescriptionWithSource bound to source.
func parseWithSource(source DescriptionSource) func([]byte, string) (DeviceDescription, error) {
	return func(body []byte, baseURL string) (DeviceDescription, error) {
		return ParseDeviceDescriptionWithSource(body, baseURL, source)
	}
}

// TestParseDeviceDescription_UserChosenKeepsAControlURLOnAnotherHost is the
// other side of the same rule: a description URL the operator chose is the
// approval the same-host rule stands in for.
func TestParseDeviceDescription_UserChosenKeepsAControlURLOnAnotherHost(t *testing.T) {
	desc, err := ParseDeviceDescriptionWithSource(
		avtOnlyDescription("http://other.host:7790/avt/control"),
		"http://192.168.1.42:7790/d.xml", SourceUserChosen)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := desc.Services[ServiceAVTransport].ControlURL; got != "http://other.host:7790/avt/control" {
		t.Errorf("ControlURL = %q, want the absolute URL on the other host kept", got)
	}
}

// TestParseDeviceDescription_AHostLocalServiceURLNeedsAHostLocalDescription
// pins the bound on the operator's approval (backlog B14, #1050's follow-up).
// A description the operator chose may name a service on another host, but
// not on THIS host (loopback, the unspecified address, a localhost name) or
// on a link-local address, unless the description URL itself names an
// address of that same kind: the operator pointing at a server on this host,
// or at a zero-configuration device, on purpose. Otherwise the server a
// manual URL names chooses where LiveHost sends every byte fetch of its
// tracks, the bridge's own console included, and the unauthenticated DLNA
// listener relays the answers. A discovered description gets the same rule,
// where #1050's same-host rule already implies it.
func TestParseDeviceDescription_AHostLocalServiceURLNeedsAHostLocalDescription(t *testing.T) {
	for _, tc := range []struct {
		base, control string
		kept          bool
	}{
		{"http://192.168.1.42:8200/d.xml", "http://127.0.0.1:7789/api/stats", false},
		{"http://192.168.1.42:8200/d.xml", "http://127.53.0.1:7789/ctl", false},
		{"http://192.168.1.42:8200/d.xml", "http://localhost:7789/ctl", false},
		{"http://192.168.1.42:8200/d.xml", "http://LocalHost.:7789/ctl", false},
		{"http://192.168.1.42:8200/d.xml", "http://console.localhost:7789/ctl", false},
		{"http://192.168.1.42:8200/d.xml", "http://[::1]:7789/ctl", false},
		{"http://192.168.1.42:8200/d.xml", "http://0.0.0.0:7789/ctl", false},
		{"http://192.168.1.42:8200/d.xml", "http://[::]:7789/ctl", false},
		{"http://192.168.1.42:8200/d.xml", "http://[::ffff:127.0.0.1]:7789/ctl", false},
		{"http://192.168.1.42:8200/d.xml", "http://169.254.169.254/latest/meta-data/", false},
		{"http://192.168.1.42:8200/d.xml", "http://[fe80::1%25en0]:8200/ctl", false},
		{"http://nas.local:8200/d.xml", "http://127.0.0.1:8200/ctl", false},
		{"http://127.0.0.1:8200/d.xml", "http://169.254.169.254/latest/meta-data/", false},
		{"http://169.254.10.20:8200/d.xml", "http://127.0.0.1:7789/ctl", false},
		// The kept rows: a description on this host naming a service on this
		// host by any spelling, a zero-configuration device naming another
		// link-local address, and #1050's escape hatch, another LAN host.
		{"http://127.0.0.1:8200/d.xml", "http://127.0.0.1:8200/ctl", true},
		{"http://127.0.0.1:8200/d.xml", "http://localhost:8200/ctl", true},
		{"http://localhost:8200/d.xml", "http://[::1]:8200/ctl", true},
		{"http://[::1]:8200/d.xml", "http://127.0.0.1:8200/ctl", true},
		{"http://169.254.10.20:8200/d.xml", "http://169.254.10.21:8200/ctl", true},
		{"http://192.168.1.42:8200/d.xml", "http://192.0.2.50:8200/ctl", true},
		{"http://192.168.1.42:8200/d.xml", "/ctl", true},
	} {
		desc, err := ParseDeviceDescriptionWithSource(avtOnlyDescription(tc.control), tc.base, SourceUserChosen)
		svc, kept := desc.Services[ServiceAVTransport]
		if kept != tc.kept || (err == nil) != tc.kept {
			t.Errorf("description %s, control %q: kept = %v as %q (err %v), want kept = %v",
				tc.base, tc.control, kept, svc.ControlURL, err, tc.kept)
		}
	}
}

// TestParseDeviceDescription_KeepsServiceURLsOnTheSameHostAtAnotherPort pins
// that the rule is the HOST, not the origin: serving the description and the
// control endpoints on different ports of one address is ordinary UPnP, and
// the scheme and host compare case-insensitively.
func TestParseDeviceDescription_KeepsServiceURLsOnTheSameHostAtAnotherPort(t *testing.T) {
	xml := `<?xml version="1.0"?><root><device>` +
		`<friendlyName>Split Ports</friendlyName><UDN>uuid:split</UDN><serviceList>` +
		`<service><serviceType>urn:schemas-upnp-org:service:AVTransport:1</serviceType>` +
		`<controlURL>http://renderer.local:49153/avt/control</controlURL>` +
		`<eventSubURL>http://RENDERER.local:49154/avt/event</eventSubURL></service>` +
		`<service><serviceType>urn:schemas-upnp-org:service:RenderingControl:1</serviceType>` +
		`<controlURL>HTTP://Renderer.Local:9000/rc</controlURL></service>` +
		`</serviceList></device></root>`
	desc, err := ParseDeviceDescription([]byte(xml), "http://Renderer.Local:49152/d.xml")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	av := desc.Services[ServiceAVTransport]
	if av.ControlURL != "http://renderer.local:49153/avt/control" {
		t.Errorf("AVTransport.ControlURL = %q", av.ControlURL)
	}
	if av.EventSubURL != "http://RENDERER.local:49154/avt/event" {
		t.Errorf("AVTransport.EventSubURL = %q", av.EventSubURL)
	}
	if got := desc.Services[ServiceRenderingControl].ControlURL; got == "" {
		t.Error("RenderingControl on the same host (another port, upper-case scheme) was dropped")
	}
}

// TestParseDeviceDescription_DropsOnlyTheOffHostOptionalURLs pins the split:
// an optional service URL on another host is dropped on its own, and the
// renderer stays, since its AVTransport control URL is fine. The iOS twin is
// test_makeRenderer_discovered_dropsOnlyTheOffHostOptionalURLs.
func TestParseDeviceDescription_DropsOnlyTheOffHostOptionalURLs(t *testing.T) {
	xml := `<?xml version="1.0"?><root><device>` +
		`<friendlyName>Mixed</friendlyName><UDN>uuid:mixed</UDN><serviceList>` +
		`<service><serviceType>urn:schemas-upnp-org:service:AVTransport:1</serviceType>` +
		`<controlURL>/avt/control</controlURL>` +
		`<eventSubURL>http://evil.example/avt/event</eventSubURL></service>` +
		`<service><serviceType>urn:schemas-upnp-org:service:RenderingControl:1</serviceType>` +
		`<controlURL>http://evil.example/rc/control</controlURL></service>` +
		`<service><serviceType>urn:schemas-upnp-org:service:ConnectionManager:1</serviceType>` +
		`<controlURL>/cm/control</controlURL>` +
		`<eventSubURL>file:///tmp/cm-event</eventSubURL></service>` +
		`</serviceList></device></root>`
	desc, err := ParseDeviceDescription([]byte(xml), "http://192.168.1.42:7790/d.xml")
	if err != nil {
		t.Fatalf("parse: %v (the renderer must stay: its AVTransport control URL is on-host)", err)
	}
	av := desc.Services[ServiceAVTransport]
	if av.ControlURL != "http://192.168.1.42:7790/avt/control" {
		t.Errorf("AVTransport.ControlURL = %q", av.ControlURL)
	}
	if av.EventSubURL != "" {
		t.Errorf("AVTransport.EventSubURL = %q, want an off-host event URL dropped", av.EventSubURL)
	}
	if rc, ok := desc.Services[ServiceRenderingControl]; ok {
		t.Errorf("RenderingControl kept with an off-host control URL %q", rc.ControlURL)
	}
	cm, ok := desc.Services[ServiceConnectionManager]
	if !ok || cm.ControlURL != "http://192.168.1.42:7790/cm/control" {
		t.Errorf("ConnectionManager = %+v (present %v), want the on-host service untouched", cm, ok)
	}
	if cm.EventSubURL != "" {
		t.Errorf("ConnectionManager.EventSubURL = %q, want a non-http event URL dropped on its own", cm.EventSubURL)
	}
}

// TestParseDeviceDescription_ComparesIPv6LiteralsByHost pins that the host is
// read with url.URL.Hostname: an IPv6 literal (with or without a zone) carries
// brackets and a port in URL.Host, and only the address may be compared.
func TestParseDeviceDescription_ComparesIPv6LiteralsByHost(t *testing.T) {
	for _, tc := range []struct {
		base, wantAVT, sameHost, sameHostWant, otherHost string
	}{
		{
			base:         "http://[2001:db8::7]:8080/desc.xml",
			wantAVT:      "http://[2001:db8::7]:8080/avt/control",
			sameHost:     "http://[2001:DB8::7]:9000/rc",
			sameHostWant: "http://[2001:DB8::7]:9000/rc",
			otherHost:    "http://[2001:db8::8]:8080/cm",
		},
		{
			base:         "http://[fe80::1%25en0]:8080/desc.xml",
			wantAVT:      "http://[fe80::1%25en0]:8080/avt/control",
			sameHost:     "http://[fe80::1%25en0]:9000/rc",
			sameHostWant: "http://[fe80::1%25en0]:9000/rc",
			otherHost:    "http://[fe80::2%25en0]:8080/cm",
		},
	} {
		xml := `<?xml version="1.0"?><root><device>` +
			`<friendlyName>V6</friendlyName><UDN>uuid:v6</UDN><serviceList>` +
			`<service><serviceType>urn:schemas-upnp-org:service:AVTransport:1</serviceType>` +
			`<controlURL>/avt/control</controlURL></service>` +
			`<service><serviceType>urn:schemas-upnp-org:service:RenderingControl:1</serviceType>` +
			`<controlURL>` + tc.sameHost + `</controlURL></service>` +
			`<service><serviceType>urn:schemas-upnp-org:service:ConnectionManager:1</serviceType>` +
			`<controlURL>` + tc.otherHost + `</controlURL></service>` +
			`</serviceList></device></root>`
		desc, err := ParseDeviceDescription([]byte(xml), tc.base)
		if err != nil {
			t.Fatalf("%s: parse: %v", tc.base, err)
		}
		if got := desc.Services[ServiceAVTransport].ControlURL; got != tc.wantAVT {
			t.Errorf("%s: relative AVTransport resolved to %q, want %q", tc.base, got, tc.wantAVT)
		}
		if got := desc.Services[ServiceRenderingControl].ControlURL; got != tc.sameHostWant {
			t.Errorf("%s: same-address RenderingControl = %q, want %q", tc.base, got, tc.sameHostWant)
		}
		if cm, ok := desc.Services[ServiceConnectionManager]; ok {
			t.Errorf("%s: ConnectionManager on another address kept as %q", tc.base, cm.ControlURL)
		}
	}
}

// TestParseSSDPHeaders_BlanksALocationThatIsNotHTTPWithAHost pins the rule
// for the SSDP LOCATION header: the discovery clients GET whatever it names,
// so a value that is not an http(s) URL with a host reads as absent, which
// both clients already treat as "nothing to fetch". The iOS app's
// SSDPResponseParser does the same.
func TestParseSSDPHeaders_BlanksALocationThatIsNotHTTPWithAHost(t *testing.T) {
	for _, tc := range []struct{ location, want string }{
		{"http://192.168.1.42:8080/description.xml", "http://192.168.1.42:8080/description.xml"},
		{"HTTP://Renderer.local/d.xml", "HTTP://Renderer.local/d.xml"},
		{"https://192.168.1.42/d.xml", "https://192.168.1.42/d.xml"},
		{"http://[2001:db8::7]:8080/d.xml", "http://[2001:db8::7]:8080/d.xml"},
		{"file:///etc/passwd", ""},
		{"ftp://192.168.1.42/d.xml", ""},
		{"gopher://192.168.1.42/", ""},
		{"http:///desc.xml", ""},
		{"http://:8080/d.xml", ""},
		{"192.168.1.42:8080/desc.xml", ""}, // no scheme: does not even parse
		{"/desc.xml", ""},
	} {
		pkt := []byte("HTTP/1.1 200 OK\r\n" +
			"LOCATION: " + tc.location + "\r\n" +
			"ST: urn:schemas-upnp-org:device:MediaRenderer:1\r\n" +
			"USN: uuid:loc::urn:schemas-upnp-org:device:MediaRenderer:1\r\n" +
			"\r\n")
		hdr, err := ParseSSDPHeaders(pkt)
		if err != nil {
			t.Fatalf("%q: parse: %v", tc.location, err)
		}
		if hdr.Location != tc.want {
			t.Errorf("LOCATION %q: Location = %q, want %q", tc.location, hdr.Location, tc.want)
		}
		if hdr.USN == "" {
			t.Errorf("LOCATION %q: the rest of the packet was lost with it", tc.location)
		}
	}
}

// requestLog is a SOAPDispatcher that records every request it is handed
// and answers from a handler, so a test can assert on what the discovery
// client sent and where.
type requestLog struct {
	mu      sync.Mutex
	seen    []string // "METHOD url"
	handler http.HandlerFunc
}

func (d *requestLog) Do(_ context.Context, req *http.Request) (*http.Response, error) {
	d.mu.Lock()
	d.seen = append(d.seen, req.Method+" "+req.URL.String())
	d.mu.Unlock()
	rec := httptest.NewRecorder()
	d.handler(rec, req)
	return rec.Result(), nil
}

func (d *requestLog) requests() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.seen...)
}

// rendererAnnouncement is an M-SEARCH response for a MediaRenderer.
func rendererAnnouncement(udn, location string) []byte {
	return []byte("HTTP/1.1 200 OK\r\n" +
		"LOCATION: " + location + "\r\n" +
		"ST: urn:schemas-upnp-org:device:MediaRenderer:1\r\n" +
		"USN: " + udn + "::urn:schemas-upnp-org:device:MediaRenderer:1\r\n" +
		"\r\n")
}

// TestHandlePacket_NeverPostsGetProtocolInfoToAnOffHostConnectionManager
// drives the real announcement path (handlePacket → fetchAndCacheDetails →
// FetchDeviceDescription) for a renderer whose ConnectionManager, event and
// RenderingControl URLs name another host. The bridge POSTs its own
// GetProtocolInfo only to a ConnectionManager URL that passed, so here it
// POSTs nothing; the renderer is still served, without the refused URLs.
func TestHandlePacket_NeverPostsGetProtocolInfoToAnOffHostConnectionManager(t *testing.T) {
	const offHost = "http://192.0.2.200:7789"
	disp := &requestLog{handler: func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(chordGetProtocolInfoResponse))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`<?xml version="1.0"?><root><device>` +
			`<friendlyName>Split Brain</friendlyName><UDN>uuid:split-brain</UDN><serviceList>` +
			`<service><serviceType>urn:schemas-upnp-org:service:AVTransport:1</serviceType>` +
			`<controlURL>/avt/control</controlURL>` +
			`<eventSubURL>` + offHost + `/avt/event</eventSubURL></service>` +
			`<service><serviceType>urn:schemas-upnp-org:service:ConnectionManager:1</serviceType>` +
			`<controlURL>` + offHost + `/api/cm</controlURL></service>` +
			`<service><serviceType>urn:schemas-upnp-org:service:RenderingControl:1</serviceType>` +
			`<controlURL>` + offHost + `/rc</controlURL></service>` +
			`</serviceList></device></root>`))
	}}
	c := newTestClient(t, disp)
	c.handlePacket(context.Background(),
		rendererAnnouncement("uuid:split-brain", "http://192.168.1.42:8080/description.xml"), nil)
	c.wg.Wait() // the detail fetch is the only goroutine: no run loops were started

	reqs := disp.requests()
	if len(reqs) != 1 || reqs[0] != "GET http://192.168.1.42:8080/description.xml" {
		t.Errorf("requests = %q, want the one description GET and nothing sent off-host", reqs)
	}
	for _, r := range reqs {
		if strings.Contains(r, "192.0.2.200") {
			t.Errorf("the bridge sent %q to a host the description does not live on", r)
		}
	}
	served := c.cache.Snapshot() // what GET /v1/renderers serializes
	if len(served) != 1 {
		t.Fatalf("served %d renderers, want 1 (its AVTransport is on-host): %+v", len(served), served)
	}
	got := served[0]
	if got.ControlURL != "http://192.168.1.42:8080/avt/control" {
		t.Errorf("ControlURL = %q", got.ControlURL)
	}
	if got.EventURL != "" || got.RenderingControlURL != "" {
		t.Errorf("EventURL = %q, RenderingControlURL = %q: want both off-host URLs dropped",
			got.EventURL, got.RenderingControlURL)
	}
	if len(got.SinkProtocolInfos) != 0 {
		t.Errorf("SinkProtocolInfos = %q, want none: no GetProtocolInfo may be sent off-host", got.SinkProtocolInfos)
	}
}

// TestHandlePacket_DoesNotServeARendererWhoseAVTransportIsOffHost pins the
// mandatory half: an AVTransport control URL on another host would have iOS
// dispatch SetAVTransportURI (a track URL and its metadata) to that host, so
// the renderer is not served at all.
func TestHandlePacket_DoesNotServeARendererWhoseAVTransportIsOffHost(t *testing.T) {
	disp := &requestLog{handler: func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(avtOnlyDescription("http://192.0.2.200:7789/avt/control"))
	}}
	c := newTestClient(t, disp)
	c.handlePacket(context.Background(),
		rendererAnnouncement("uuid:off-host-avt", "http://192.168.1.42:8080/description.xml"), nil)
	c.wg.Wait()

	if served := c.cache.Snapshot(); len(served) != 0 {
		t.Errorf("served %+v, want no renderer: its AVTransport control URL is on another host", served)
	}
	if reqs := disp.requests(); len(reqs) != 1 {
		t.Errorf("requests = %q, want only the description GET", reqs)
	}
}

// TestHandlePacket_NeverFetchesALocationThatIsNotHTTPWithAHost pins that a
// first-time renderer announcing a LOCATION the bridge must not fetch costs
// no request, no goroutine's worth of work and no cache entry.
func TestHandlePacket_NeverFetchesALocationThatIsNotHTTPWithAHost(t *testing.T) {
	disp := &requestLog{handler: func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(chordDeviceXML))
	}}
	c := newTestClient(t, disp)
	for i, location := range []string{"file:///etc/passwd", "ftp://192.168.1.42/d.xml", "http:///desc.xml"} {
		udn := "uuid:bad-location-" + string(rune('a'+i))
		c.handlePacket(context.Background(), rendererAnnouncement(udn, location), nil)
		c.wg.Wait()
		if _, cached := c.cache.Get(udn); cached {
			t.Errorf("LOCATION %q produced a cache entry", location)
		}
	}
	if reqs := disp.requests(); len(reqs) != 0 {
		t.Errorf("requests = %q, want none", reqs)
	}
}
