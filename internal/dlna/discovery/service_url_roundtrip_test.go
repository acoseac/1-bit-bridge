package discovery

// Backlog B233: the nightly fuzz found the device-description parser keeping
// a service URL that does not parse again. net/url takes a raw non-ASCII byte
// in an IPv6 zone, writes it back escaped (%B3), and refuses that escape in a
// zone, so the string the parser kept named no URL at all, and the policy had
// judged a host no later reader would see.

import (
	"errors"
	"net"
	"net/url"
	"strings"
	"testing"
)

// TestParseDeviceDescription_KeepsNoServiceURLThatDoesNotParseBack pins the
// round-trip rule: every later reader (the caches, LiveHost, the proxy,
// GetProtocolInfo's POST) parses the kept string again, so it must parse back
// to the scheme and host the policy judged, or it is refused. The refused rows
// are the fuzzer's input (a raw byte in the description URL's zone, a
// relative reference) and the same zone written in UTF-8 in the description
// itself, from both sources; the kept rows are the zones and hosts that do
// parse back, so the rule cannot turn into "refuse every zone".
func TestParseDeviceDescription_KeepsNoServiceURLThatDoesNotParseBack(t *testing.T) {
	for _, tc := range []struct {
		name, base, control string
		source              DescriptionSource
	}{
		{"the fuzzer's input: a raw byte in the description URL's zone",
			"http://[fe80::1%25en0\xb3]:8080/d.xml", "t", SourceDiscovered},
		{"a raw byte in the zone of a LOCATION an SSDP client keeps (a global address)",
			"http://[2001:db8::7%25en0\xb3]:8080/d.xml", "/avt", SourceDiscovered},
		{"UTF-8 in the zone, in the description and its URL",
			"http://[fe80::1%25en0³]:8080/d.xml", "http://[fe80::1%25en0³]:9000/avt", SourceDiscovered},
		{"the operator's link-local upstream naming such a zone",
			"http://[fe80::2%25en0]:8200/d.xml", "http://[fe80::1%25en0³]:8200/avt", SourceUserChosen},
	} {
		desc, err := ParseDeviceDescriptionWithSource(avtOnlyDescription(tc.control), tc.base, tc.source)
		if err == nil || !strings.Contains(err.Error(), "AVTransport") {
			t.Errorf("%s: err = %v, want the no-AVTransport refusal", tc.name, err)
		}
		if svc, ok := desc.Services[ServiceAVTransport]; ok {
			t.Errorf("%s: AVTransport kept as %q", tc.name, svc.ControlURL)
		}
		base, err := url.Parse(tc.base)
		if err != nil {
			t.Fatalf("%s: the base must parse for this row to mean anything: %v", tc.name, err)
		}
		if got, err := resolveServiceURL(base, tc.control, tc.source); !errors.Is(err, errServiceURLDoesNotParseBack) {
			t.Errorf("%s: resolveServiceURL = %q, %v; want errServiceURLDoesNotParseBack", tc.name, got, err)
		}
	}

	for _, tc := range []struct{ name, base, want string }{
		{"an ASCII zone", "http://[fe80::1%25en0]:8080/d.xml", "http://[fe80::1%25en0]:8080/avt"},
		{"a Windows zone holding a space", "http://[fe80::1%25Wi-Fi%204]:8080/d.xml",
			"http://[fe80::1%25Wi-Fi%204]:8080/avt"},
		{"non-ASCII in a host name, outside any zone", "http://renderer-é.local:8080/d.xml",
			"http://renderer-%C3%A9.local:8080/avt"},
	} {
		desc, err := ParseDeviceDescription(avtOnlyDescription("/avt"), tc.base)
		if err != nil {
			t.Errorf("%s: parse: %v", tc.name, err)
			continue
		}
		got := desc.Services[ServiceAVTransport].ControlURL
		if got != tc.want {
			t.Errorf("%s: AVTransport.ControlURL = %q, want %q", tc.name, got, tc.want)
		}
		if _, err := url.Parse(got); err != nil {
			t.Errorf("%s: kept %q, which does not parse: %v", tc.name, got, err)
		}
	}
}

// TestParseDeviceDescription_DropsAnOptionalURLThatDoesNotParseBackAlone pins
// what the refusal costs an optional URL: the URL alone, as every other
// refusal of one does. The renderer stays, since its control URL parses back.
func TestParseDeviceDescription_DropsAnOptionalURLThatDoesNotParseBackAlone(t *testing.T) {
	xml := `<?xml version="1.0"?><root><device>` +
		`<friendlyName>Zoned</friendlyName><UDN>uuid:zoned</UDN><serviceList>` +
		`<service><serviceType>urn:schemas-upnp-org:service:AVTransport:1</serviceType>` +
		`<controlURL>/avt</controlURL><eventSubURL>http://[fe80::1%25en0³]:8200/e</eventSubURL></service>` +
		`</serviceList></device></root>`
	desc, err := ParseDeviceDescriptionWithSource([]byte(xml), "http://[fe80::2%25en0]:8200/d.xml", SourceUserChosen)
	if err != nil {
		t.Fatalf("parse: %v (the renderer must stay: its control URL parses back)", err)
	}
	av := desc.Services[ServiceAVTransport]
	if av.ControlURL != "http://[fe80::2%25en0]:8200/avt" {
		t.Errorf("AVTransport.ControlURL = %q", av.ControlURL)
	}
	if av.EventSubURL != "" {
		t.Errorf("AVTransport.EventSubURL = %q, want a URL that does not parse back dropped on its own", av.EventSubURL)
	}
}

// TestALocationIsKeptAsItArrived pins why the LOCATION path needs no such
// rule: ParseSSDPHeaders and LocationPermittedBy keep the value byte for byte
// and never write it back from its parsed form, so every reader parses the
// string the policy parsed. A LOCATION whose zone would not survive a
// re-serialisation is kept, and a description fetched from it keeps no service
// (the test above). Returning u.String() from either would keep a string that
// does not parse, which is this pin's reason to exist.
func TestALocationIsKeptAsItArrived(t *testing.T) {
	const location = "http://[2001:db8::7%25en0\xb3]:8080/d.xml"
	h, err := ParseSSDPHeaders([]byte("HTTP/1.1 200 OK\r\nLOCATION: " + location +
		"\r\nUSN: uuid:zoned::upnp:rootdevice\r\nST: upnp:rootdevice\r\n\r\n"))
	if err != nil {
		t.Fatalf("ParseSSDPHeaders: %v", err)
	}
	if h.Location != location {
		t.Errorf("ParseSSDPHeaders kept LOCATION %q, want it as it arrived, %q", h.Location, location)
	}
	src := &net.UDPAddr{IP: net.ParseIP("192.168.1.42"), Port: 1900}
	if got := LocationFromSource(location, src); got != location {
		t.Errorf("LocationFromSource = %q, want it as it arrived, %q", got, location)
	}
	if _, err := url.Parse(location); err != nil {
		t.Fatalf("the LOCATION must parse for this pin to mean anything: %v", err)
	}
}
