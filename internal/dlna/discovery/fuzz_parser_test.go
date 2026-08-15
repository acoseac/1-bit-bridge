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
// service lookup (`canonicalServiceType`) that PR #470 reshaped.
package discovery

import "testing"

func FuzzParseSSDPHeaders(f *testing.F) {
	f.Add([]byte("HTTP/1.1 200 OK\r\nLOCATION: http://1.2.3.4/d.xml\r\nUSN: uuid:x::urn:y\r\n" +
		"NT: upnp:rootdevice\r\nCACHE-CONTROL: max-age=1800\r\n\r\n"))
	f.Add([]byte("NOTIFY * HTTP/1.1\r\nNTS: ssdp:byebye\r\n\r\n"))
	f.Add([]byte(""))
	f.Fuzz(func(t *testing.T, b []byte) { _, _ = ParseSSDPHeaders(b) })
}

func FuzzParseDeviceDescription(f *testing.F) {
	f.Add([]byte(`<root><device><friendlyName>x</friendlyName><serviceList><service>` +
		`<serviceType>urn:schemas-upnp-org:service:AVTransport:1</serviceType>` +
		`<controlURL>/c</controlURL></service></serviceList></device></root>`))
	// Version-tolerant lookup: :2 must fold onto the :1 map key.
	f.Add([]byte(`<root><device><serviceList><service>` +
		`<serviceType>urn:schemas-upnp-org:service:AVTransport:2</serviceType>` +
		`<controlURL>/c</controlURL></service></serviceList></device></root>`))
	f.Fuzz(func(t *testing.T, b []byte) { _, _ = ParseDeviceDescription(b, "http://1.2.3.4/") })
}

func FuzzParseGetProtocolInfoResponse(f *testing.F) {
	f.Add([]byte(`<s:Envelope><s:Body><u:GetProtocolInfoResponse>` +
		`<Sink>http-get:*:audio/flac:*</Sink></u:GetProtocolInfoResponse></s:Body></s:Envelope>`))
	// SOAP 1.1 faults arrive with HTTP 500 and must parse to ErrSOAPFault
	// rather than blowing up (PR #470).
	f.Add([]byte(`<s:Envelope><s:Body><s:Fault><faultcode>s:Client</faultcode></s:Fault></s:Body></s:Envelope>`))
	f.Fuzz(func(t *testing.T, b []byte) { _, _ = ParseGetProtocolInfoResponse(b) })
}
