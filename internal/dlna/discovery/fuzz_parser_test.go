// Fuzz coverage for the renderer-discovery parsers.
//
// Every input here is supplied by an unauthenticated LAN device: the SSDP
// headers arrive as a multicast datagram, and the device description and
// GetProtocolInfo bodies are fetched FROM an address that datagram named. A
// rogue or merely broken device controls all three, and the discovery client
// parses them on its own goroutine, outside any request-scoped recovery.
//
// The device-description target is the one worth having most: it is XML from a
// remote host, decoded into a struct tree, and it feeds the version-tolerant
// service lookup (`canonicalServiceType`) that PR #470 reshaped. It carries
// PROPERTY assertions, the service-URL policy (external audit 2026-09-23, M3,
// #1050; and the host-local bound after it), stated here independently of the
// helpers that enforce it: every URL the parser keeps is http(s) with a host,
// re-parses to the host it was judged on, stays on the description's host
// when the description was discovered, and names this host or a link-local
// address only when the description URL does too.
package discovery

import (
	"net/netip"
	"net/url"
	"strings"
	"testing"
)

func FuzzParseSSDPHeaders(f *testing.F) {
	f.Add([]byte("HTTP/1.1 200 OK\r\nLOCATION: http://1.2.3.4/d.xml\r\nUSN: uuid:x::urn:y\r\n" +
		"NT: upnp:rootdevice\r\nCACHE-CONTROL: max-age=1800\r\n\r\n"))
	f.Add([]byte("NOTIFY * HTTP/1.1\r\nNTS: ssdp:byebye\r\n\r\n"))
	f.Add([]byte(""))
	f.Fuzz(func(t *testing.T, b []byte) { _, _ = ParseSSDPHeaders(b) })
}

func FuzzParseDeviceDescription(f *testing.F) {
	f.Add([]byte(`<root><device><friendlyName>x</friendlyName><serviceList><service>`+
		`<serviceType>urn:schemas-upnp-org:service:AVTransport:1</serviceType>`+
		`<controlURL>/c</controlURL></service></serviceList></device></root>`), "http://1.2.3.4/")
	// Version-tolerant lookup: :2 must fold onto the :1 map key.
	f.Add([]byte(`<root><device><serviceList><service>`+
		`<serviceType>urn:schemas-upnp-org:service:AVTransport:2</serviceType>`+
		`<controlURL>/c</controlURL></service></serviceList></device></root>`), "http://1.2.3.4/")
	// The shapes the policy refuses, each beside one it keeps, so the
	// fuzzer starts from both sides of every rule.
	f.Add([]byte(`<root><device><serviceList>`+
		`<service><serviceType>urn:schemas-upnp-org:service:AVTransport:1</serviceType>`+
		`<controlURL>http://127.0.0.1:7789/api/stats</controlURL>`+
		`<eventSubURL>http://Renderer.Local:49154/evt</eventSubURL></service>`+
		`<service><serviceType>urn:schemas-upnp-org:service:ConnectionManager:1</serviceType>`+
		`<controlURL>//other.host/cm</controlURL><eventSubURL>http://:8080/x</eventSubURL></service>`+
		`<service><serviceType>urn:schemas-upnp-org:service:RenderingControl:1</serviceType>`+
		`<controlURL>ftp://renderer.local/rc</controlURL><eventSubURL>HTTPS://[::1]/e</eventSubURL></service>`+
		`</serviceList></device></root>`), "http://renderer.local:49152/d.xml")
	f.Add([]byte(`<root><device><serviceList><service>`+
		`<serviceType>urn:schemas-upnp-org:service:ContentDirectory:1</serviceType>`+
		`<controlURL>http://localhost:8200/ctl</controlURL>`+
		`<eventSubURL>http://169.254.169.254/latest/meta-data/</eventSubURL></service>`+
		`</serviceList></device></root>`), "http://127.0.0.1:8200/rootDesc.xml")
	f.Add([]byte(`<root><device><serviceList><service>`+
		`<serviceType>urn:schemas-upnp-org:service:AVTransport:1</serviceType>`+
		`<controlURL>/avt</controlURL><eventSubURL>http://2130706433:7789/e</eventSubURL>`+
		`</service></serviceList></device></root>`), "http://[fe80::1%25en0]:8080/d.xml")
	f.Add([]byte(`<root><device><serviceList><service>`+
		`<serviceType>urn:schemas-upnp-org:service:AVTransport:1</serviceType>`+
		`<controlURL>/avt</controlURL></service></serviceList></device></root>`), "http://0x7f.1:8080/d.xml")
	f.Fuzz(func(t *testing.T, b []byte, baseURL string) {
		base, baseErr := url.Parse(baseURL)
		for _, source := range []DescriptionSource{SourceDiscovered, SourceUserChosen} {
			desc, _ := ParseDeviceDescriptionWithSource(b, baseURL, source)
			if baseErr != nil {
				if len(desc.Services) != 0 {
					t.Fatalf("base %q does not parse, yet the parser kept services %+v", baseURL, desc.Services)
				}
				continue
			}
			for stype, svc := range desc.Services {
				for _, kept := range []string{svc.ControlURL, svc.EventSubURL} {
					if kept != "" {
						checkKeptServiceURL(t, kept, base, source, stype)
					}
				}
			}
		}
	})
}

// checkKeptServiceURL states the service-URL policy as a property of one URL
// the parser kept, re-parsed from the string it returned.
func checkKeptServiceURL(t *testing.T, kept string, base *url.URL, source DescriptionSource, stype string) {
	t.Helper()
	u, err := url.Parse(kept)
	if err != nil {
		t.Fatalf("%s: kept %q, which does not re-parse: %v", stype, kept, err)
	}
	if s := strings.ToLower(u.Scheme); s != "http" && s != "https" {
		t.Fatalf("%s: kept %q, whose scheme is not http(s)", stype, kept)
	}
	if u.Hostname() == "" {
		t.Fatalf("%s: kept %q, which names no host", stype, kept)
	}
	if source == SourceDiscovered && !strings.EqualFold(u.Hostname(), base.Hostname()) {
		t.Fatalf("%s: kept %q from a discovered description at %q, on another host", stype, kept, base)
	}
	k := fuzzHostKind(u.Hostname())
	if k == "a numeric spelling" {
		t.Fatalf("%s: kept %q, whose host ends in a number without being an IP address", stype, kept)
	}
	if k != "elsewhere" && k != fuzzHostKind(base.Hostname()) {
		t.Fatalf("%s: kept %q, which names %s, from a description at %q (source %d)",
			stype, kept, k, base, source)
	}
}

// fuzzHostKind says whether a host names this machine, a link-local address,
// a number that is no IP address, or anything else. Written apart from the
// classifier the parser uses, so the property does not borrow the code it
// checks.
func fuzzHostKind(host string) string {
	h := strings.TrimSuffix(host, ".")
	if a, err := netip.ParseAddr(h); err == nil {
		a = a.Unmap()
		if a.IsLoopback() || a.IsUnspecified() {
			return "this host"
		}
		if a.IsLinkLocalUnicast() {
			return "link-local"
		}
		return "elsewhere"
	}
	name := strings.ToLower(h)
	if name == "localhost" || strings.HasSuffix(name, ".localhost") {
		return "this host"
	}
	last := name[strings.LastIndexByte(name, '.')+1:]
	hex := strings.TrimPrefix(last, "0x")
	decimal := last != "" && strings.Trim(last, "0123456789") == ""
	hexadecimal := hex != last && strings.Trim(hex, "0123456789abcdef") == ""
	if decimal || hexadecimal {
		return "a numeric spelling"
	}
	return "elsewhere"
}

func FuzzParseGetProtocolInfoResponse(f *testing.F) {
	f.Add([]byte(`<s:Envelope><s:Body><u:GetProtocolInfoResponse>` +
		`<Sink>http-get:*:audio/flac:*</Sink></u:GetProtocolInfoResponse></s:Body></s:Envelope>`))
	// SOAP 1.1 faults arrive with HTTP 500 and must parse to ErrSOAPFault
	// rather than blowing up (PR #470).
	f.Add([]byte(`<s:Envelope><s:Body><s:Fault><faultcode>s:Client</faultcode></s:Fault></s:Body></s:Envelope>`))
	f.Fuzz(func(t *testing.T, b []byte) { _, _ = ParseGetProtocolInfoResponse(b) })
}
