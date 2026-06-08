package discovery

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// -----------------------------------------------------------------------------
// ParseSSDPHeaders
// -----------------------------------------------------------------------------

func TestParseSSDPHeaders_MSearchResponse(t *testing.T) {
	// Real-shape M-SEARCH response from Chord 2go (captured via
	// Phase 0 spike script 2026-05-26).
	raw := []byte("HTTP/1.1 200 OK\r\n" +
		"CACHE-CONTROL: max-age=1800\r\n" +
		"DATE: Wed, 26 May 2026 12:00:00 GMT\r\n" +
		"EXT:\r\n" +
		"LOCATION: http://192.168.1.42:8080/description.xml\r\n" +
		"SERVER: ChordElectronics/1.0 UPnP/1.0 ChordRenderer/2go\r\n" +
		"ST: urn:schemas-upnp-org:device:MediaRenderer:1\r\n" +
		"USN: uuid:abcd1234-5678-90ab-cdef-1234567890ab::urn:schemas-upnp-org:device:MediaRenderer:1\r\n" +
		"\r\n")
	hdr, err := ParseSSDPHeaders(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if hdr.Location != "http://192.168.1.42:8080/description.xml" {
		t.Errorf("Location = %q", hdr.Location)
	}
	if hdr.ST != "urn:schemas-upnp-org:device:MediaRenderer:1" {
		t.Errorf("ST = %q", hdr.ST)
	}
	if !strings.HasPrefix(hdr.USN, "uuid:abcd1234") {
		t.Errorf("USN = %q", hdr.USN)
	}
	if !strings.Contains(hdr.Server, "ChordElectronics") {
		t.Errorf("Server = %q", hdr.Server)
	}
}

func TestParseSSDPHeaders_NotifyAlive(t *testing.T) {
	raw := []byte("NOTIFY * HTTP/1.1\r\n" +
		"HOST: 239.255.255.250:1900\r\n" +
		"CACHE-CONTROL: max-age=1800\r\n" +
		"LOCATION: http://192.168.1.43:80/desc.xml\r\n" +
		"NT: urn:schemas-upnp-org:device:MediaRenderer:1\r\n" +
		"NTS: ssdp:alive\r\n" +
		"SERVER: SonyVendor/2.0\r\n" +
		"USN: uuid:zzz999::urn:schemas-upnp-org:device:MediaRenderer:1\r\n" +
		"\r\n")
	hdr, err := ParseSSDPHeaders(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if hdr.NTS != "ssdp:alive" {
		t.Errorf("NTS = %q", hdr.NTS)
	}
	if hdr.NT != "urn:schemas-upnp-org:device:MediaRenderer:1" {
		t.Errorf("NT = %q", hdr.NT)
	}
	if hdr.Location != "http://192.168.1.43:80/desc.xml" {
		t.Errorf("Location = %q", hdr.Location)
	}
}

func TestParseSSDPHeaders_NotifyByebye(t *testing.T) {
	raw := []byte("NOTIFY * HTTP/1.1\r\n" +
		"HOST: 239.255.255.250:1900\r\n" +
		"NT: urn:schemas-upnp-org:device:MediaRenderer:1\r\n" +
		"NTS: ssdp:byebye\r\n" +
		"USN: uuid:bye::urn:schemas-upnp-org:device:MediaRenderer:1\r\n" +
		"\r\n")
	hdr, err := ParseSSDPHeaders(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if hdr.NTS != "ssdp:byebye" {
		t.Errorf("NTS = %q", hdr.NTS)
	}
	if hdr.USN != "uuid:bye::urn:schemas-upnp-org:device:MediaRenderer:1" {
		t.Errorf("USN = %q", hdr.USN)
	}
}

func TestParseSSDPHeaders_CaseInsensitive(t *testing.T) {
	// Real renderers don't all canonicalize header case — some
	// emit "Location:" (Title-Case), others "LOCATION:" (UPPER).
	// The parser MUST handle both. Pin via mixed-case input.
	raw := []byte("HTTP/1.1 200 OK\r\n" +
		"Location: http://x.y/z\r\n" +
		"Usn: uuid:mixedcase\r\n" +
		"\r\n")
	hdr, err := ParseSSDPHeaders(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if hdr.Location != "http://x.y/z" {
		t.Errorf("Location = %q", hdr.Location)
	}
	if hdr.USN != "uuid:mixedcase" {
		t.Errorf("USN = %q", hdr.USN)
	}
}

func TestParseSSDPHeaders_RejectsMalformed(t *testing.T) {
	cases := []struct {
		name string
		raw  []byte
	}{
		{"empty", []byte{}},
		{"no_LF_in_first_line", []byte("HTTP/1.1 200 OK")},
		{"first_line_only", []byte("HTTP/1.1 200 OK\n")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseSSDPHeaders(tc.raw)
			if err == nil {
				t.Errorf("expected error for %s, got nil", tc.name)
			}
		})
	}
}

func TestParseSSDPHeaders_IgnoresUnknownHeaders(t *testing.T) {
	// Vendor extensions (`X-Vendor: foo`) MUST NOT trip the parser.
	raw := []byte("HTTP/1.1 200 OK\r\n" +
		"LOCATION: http://a/b\r\n" +
		"X-Sony-Custom: somevalue\r\n" +
		"USN: uuid:ext\r\n" +
		"\r\n")
	hdr, err := ParseSSDPHeaders(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if hdr.Location != "http://a/b" {
		t.Errorf("Location dropped due to extension header")
	}
}

// -----------------------------------------------------------------------------
// UDNFromUSN
// -----------------------------------------------------------------------------

func TestUDNFromUSN(t *testing.T) {
	cases := []struct {
		usn  string
		want string
	}{
		{
			usn:  "uuid:abc::urn:schemas-upnp-org:device:MediaRenderer:1",
			want: "uuid:abc",
		},
		{
			usn:  "uuid:abc::urn:schemas-upnp-org:service:AVTransport:1",
			want: "uuid:abc",
		},
		{
			// Some renderers emit just the UDN as USN for the
			// root-device announcement (no `::service`).
			usn:  "uuid:abc",
			want: "uuid:abc",
		},
		{
			usn:  "",
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.usn, func(t *testing.T) {
			got := UDNFromUSN(tc.usn)
			if got != tc.want {
				t.Errorf("UDNFromUSN(%q) = %q, want %q", tc.usn, got, tc.want)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// ParseDeviceDescription
// -----------------------------------------------------------------------------

const chordDeviceXML = `<?xml version="1.0" encoding="utf-8"?>
<root xmlns="urn:schemas-upnp-org:device-1-0">
  <specVersion><major>1</major><minor>0</minor></specVersion>
  <device>
    <deviceType>urn:schemas-upnp-org:device:MediaRenderer:1</deviceType>
    <friendlyName>Chord 2go</friendlyName>
    <manufacturer>Chord Electronics</manufacturer>
    <modelDescription>2go Network Music Streamer</modelDescription>
    <modelName>2go</modelName>
    <UDN>uuid:abcd1234-5678-90ab-cdef-1234567890ab</UDN>
    <serviceList>
      <service>
        <serviceType>urn:schemas-upnp-org:service:AVTransport:1</serviceType>
        <serviceId>urn:upnp-org:serviceId:AVTransport</serviceId>
        <SCPDURL>/avtransport/scpd.xml</SCPDURL>
        <controlURL>/avtransport/control</controlURL>
        <eventSubURL>/avtransport/event</eventSubURL>
      </service>
      <service>
        <serviceType>urn:schemas-upnp-org:service:ConnectionManager:1</serviceType>
        <controlURL>/cm/control</controlURL>
        <eventSubURL>/cm/event</eventSubURL>
      </service>
      <service>
        <serviceType>urn:schemas-upnp-org:service:RenderingControl:1</serviceType>
        <controlURL>/rc/control</controlURL>
        <eventSubURL>/rc/event</eventSubURL>
      </service>
    </serviceList>
  </device>
</root>`

func TestParseDeviceDescription_ChordShape(t *testing.T) {
	desc, err := ParseDeviceDescription(
		[]byte(chordDeviceXML),
		"http://192.168.1.42:8080/description.xml",
	)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if desc.FriendlyName != "Chord 2go" {
		t.Errorf("FriendlyName = %q", desc.FriendlyName)
	}
	if desc.Manufacturer != "Chord Electronics" {
		t.Errorf("Manufacturer = %q", desc.Manufacturer)
	}
	if desc.UDN != "uuid:abcd1234-5678-90ab-cdef-1234567890ab" {
		t.Errorf("UDN = %q", desc.UDN)
	}
	av, ok := desc.Services[ServiceAVTransport]
	if !ok {
		t.Fatalf("AVTransport service missing")
	}
	want := "http://192.168.1.42:8080/avtransport/control"
	if av.ControlURL != want {
		t.Errorf("AVTransport.ControlURL = %q, want %q", av.ControlURL, want)
	}
	if av.EventSubURL != "http://192.168.1.42:8080/avtransport/event" {
		t.Errorf("AVTransport.EventSubURL = %q", av.EventSubURL)
	}
	// RenderingControl controlURL must be parsed + resolved too — iOS
	// dispatches SetMute / SetVolume here, and the discovery DTO now
	// surfaces it (RendererInfo.RenderingControlURL) so the renderer-volume
	// slider AND the DSD-pause ring-suppression mute work over the
	// bridge-mediated path.
	rc, ok := desc.Services[ServiceRenderingControl]
	if !ok {
		t.Fatalf("RenderingControl service missing")
	}
	if rc.ControlURL != "http://192.168.1.42:8080/rc/control" {
		t.Errorf("RenderingControl.ControlURL = %q", rc.ControlURL)
	}
}

func TestParseDeviceDescription_RejectsMissingAVTransport(t *testing.T) {
	// A renderer without AVTransport can't be driven as an audio
	// target — surfacing it would mislead the user.
	xml := `<?xml version="1.0"?>
<root><device>
  <friendlyName>Speaker</friendlyName>
  <UDN>uuid:noav</UDN>
  <serviceList>
    <service>
      <serviceType>urn:schemas-upnp-org:service:ConnectionManager:1</serviceType>
      <controlURL>/cm/control</controlURL>
    </service>
  </serviceList>
</device></root>`
	desc, err := ParseDeviceDescription([]byte(xml), "http://x/y")
	if err == nil {
		t.Fatal("expected error for missing AVTransport, got nil")
	}
	if !strings.Contains(err.Error(), "AVTransport") {
		t.Errorf("error should mention AVTransport: %v", err)
	}
	// The PARTIAL desc MUST still carry the services it DID find — the
	// upstream MediaServer discovery tolerates this error and reads
	// desc.Services for the ContentDirectory URL. (CodeRabbit MAJOR / PR #361.)
	if _, ok := desc.Services["urn:schemas-upnp-org:service:ConnectionManager:1"]; !ok {
		t.Error("partial desc dropped its parsed services on the no-AVTransport error")
	}
}

func TestParseDeviceDescription_RejectsEmptyAVTransportControlURL(t *testing.T) {
	// AVTransport present but with no control URL is undrivable —
	// SetAVTransportURI has nowhere to go — so it must error (the
	// discovery layer then treats the renderer as structurally unusable
	// rather than caching a stub that retries forever). (Gemini HIGH / PR #361.)
	xml := `<?xml version="1.0"?>
<root><device>
  <friendlyName>Broken AVT</friendlyName>
  <UDN>uuid:emptyctrl</UDN>
  <serviceList>
    <service>
      <serviceType>urn:schemas-upnp-org:service:AVTransport:1</serviceType>
      <controlURL></controlURL>
    </service>
  </serviceList>
</device></root>`
	_, err := ParseDeviceDescription([]byte(xml), "http://x/y")
	if err == nil {
		t.Fatal("expected error for AVTransport with empty control URL, got nil")
	}
	if !strings.Contains(err.Error(), "control URL") {
		t.Errorf("error should mention the missing control URL: %v", err)
	}
}

func TestParseDeviceDescription_ResolvesRelativeURLs(t *testing.T) {
	// Absolute URLs in serviceList should pass through unchanged;
	// relative URLs should be resolved against the base.
	xml := `<?xml version="1.0"?>
<root><device>
  <friendlyName>Mix</friendlyName>
  <UDN>uuid:mix</UDN>
  <serviceList>
    <service>
      <serviceType>urn:schemas-upnp-org:service:AVTransport:1</serviceType>
      <controlURL>http://other.host:9000/abs/control</controlURL>
      <eventSubURL>/rel/event</eventSubURL>
    </service>
  </serviceList>
</device></root>`
	desc, err := ParseDeviceDescription([]byte(xml), "http://192.168.1.42:8080/desc.xml")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	av := desc.Services[ServiceAVTransport]
	if av.ControlURL != "http://other.host:9000/abs/control" {
		t.Errorf("absolute URL not preserved: %q", av.ControlURL)
	}
	if av.EventSubURL != "http://192.168.1.42:8080/rel/event" {
		t.Errorf("relative URL not resolved: %q", av.EventSubURL)
	}
}

func TestParseDeviceDescription_RejectsMalformedXML(t *testing.T) {
	_, err := ParseDeviceDescription([]byte("not xml at all"), "http://x")
	if err == nil {
		t.Fatal("expected error for malformed XML, got nil")
	}
}

// -----------------------------------------------------------------------------
// ParseGetProtocolInfoResponse
// -----------------------------------------------------------------------------

const chordGetProtocolInfoResponse = `<?xml version="1.0" encoding="utf-8"?>
<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/">
<s:Body>
<u:GetProtocolInfoResponse xmlns:u="urn:schemas-upnp-org:service:ConnectionManager:1">
<Source></Source>
<Sink>http-get:*:audio/x-dsf:*,http-get:*:audio/x-flac:*,http-get:*:audio/wav:*</Sink>
</u:GetProtocolInfoResponse>
</s:Body>
</s:Envelope>`

func TestParseGetProtocolInfoResponse_ChordShape(t *testing.T) {
	sinks, err := ParseGetProtocolInfoResponse([]byte(chordGetProtocolInfoResponse))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := []string{
		"http-get:*:audio/x-dsf:*",
		"http-get:*:audio/x-flac:*",
		"http-get:*:audio/wav:*",
	}
	if len(sinks) != len(want) {
		t.Fatalf("len = %d, want %d (got %v)", len(sinks), len(want), sinks)
	}
	for i, w := range want {
		if sinks[i] != w {
			t.Errorf("sinks[%d] = %q, want %q", i, sinks[i], w)
		}
	}
}

func TestParseGetProtocolInfoResponse_EmptySinkLegalEmpty(t *testing.T) {
	// Renderer responded with a recognisable Response element +
	// empty Sink — degenerate but legal per the UPnP spec.
	xml := `<?xml version="1.0"?>
<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/">
<s:Body>
<u:GetProtocolInfoResponse xmlns:u="urn:schemas-upnp-org:service:ConnectionManager:1">
<Source></Source>
<Sink></Sink>
</u:GetProtocolInfoResponse>
</s:Body>
</s:Envelope>`
	sinks, err := ParseGetProtocolInfoResponse([]byte(xml))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(sinks) != 0 {
		t.Errorf("empty Sink should yield empty slice, got %v", sinks)
	}
}

// CodeRabbit MAJOR round-1 on PR #305: SOAP fault must NOT
// silently mascarade as empty Sink list.
func TestParseGetProtocolInfoResponse_SOAPFaultReturnsError(t *testing.T) {
	xml := `<?xml version="1.0"?>
<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/">
<s:Body>
<s:Fault>
<faultcode>s:Client</faultcode>
<faultstring>UPnPError</faultstring>
</s:Fault>
</s:Body>
</s:Envelope>`
	_, err := ParseGetProtocolInfoResponse([]byte(xml))
	if err == nil {
		t.Fatal("expected SOAP fault to surface as error, got nil")
	}
	if !errors.Is(err, ErrSOAPFault) {
		t.Errorf("expected ErrSOAPFault, got %v", err)
	}
}

// CodeRabbit MAJOR round-1 on PR #305: missing Response element
// (bare `<Body/>` or unrelated element) must NOT silently
// mascarade as empty Sink list.
func TestParseGetProtocolInfoResponse_MissingResponseReturnsError(t *testing.T) {
	xml := `<?xml version="1.0"?>
<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/">
<s:Body></s:Body>
</s:Envelope>`
	_, err := ParseGetProtocolInfoResponse([]byte(xml))
	if err == nil {
		t.Fatal("expected missing-response-element error, got nil")
	}
	if !errors.Is(err, ErrMissingResponseElement) {
		t.Errorf("expected ErrMissingResponseElement, got %v", err)
	}
}

func TestParseGetProtocolInfoResponse_SkipsEmptyCSVEntries(t *testing.T) {
	// A renderer's Sink with adjacent commas / trailing comma must
	// NOT produce empty-string entries in the output.
	xml := `<?xml version="1.0"?>
<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/">
<s:Body>
<u:GetProtocolInfoResponse xmlns:u="urn:schemas-upnp-org:service:ConnectionManager:1">
<Sink>http-get:*:audio/x-flac:*,,http-get:*:audio/wav:*,</Sink>
</u:GetProtocolInfoResponse>
</s:Body>
</s:Envelope>`
	sinks, err := ParseGetProtocolInfoResponse([]byte(xml))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, s := range sinks {
		if s == "" {
			t.Error("empty Sink entries should be skipped")
		}
	}
	if len(sinks) != 2 {
		t.Errorf("len = %d, want 2 (got %v)", len(sinks), sinks)
	}
}

func TestParseGetProtocolInfoResponse_RejectsMalformedXML(t *testing.T) {
	_, err := ParseGetProtocolInfoResponse([]byte("not xml"))
	if err == nil {
		t.Fatal("expected error for malformed XML")
	}
}

// -----------------------------------------------------------------------------
// ReadResponseBodyCapped
// -----------------------------------------------------------------------------

func TestReadResponseBodyCapped_ReadsUnderCap(t *testing.T) {
	body := bytes.NewReader([]byte("hello world"))
	out, err := ReadResponseBodyCapped(body, 1024)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(out) != "hello world" {
		t.Errorf("body = %q", out)
	}
}

func TestReadResponseBodyCapped_ExactlyAtCap(t *testing.T) {
	body := bytes.NewReader([]byte("12345"))
	out, err := ReadResponseBodyCapped(body, 5)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(out) != "12345" {
		t.Errorf("body = %q", out)
	}
}

func TestReadResponseBodyCapped_ExceedsCap(t *testing.T) {
	body := bytes.NewReader([]byte("123456"))
	_, err := ReadResponseBodyCapped(body, 5)
	if err == nil {
		t.Fatal("expected error for body > cap")
	}
}

func TestReadResponseBodyCapped_RejectsZeroCap(t *testing.T) {
	body := bytes.NewReader([]byte("x"))
	_, err := ReadResponseBodyCapped(body, 0)
	if err == nil {
		t.Fatal("expected error for cap=0")
	}
}
