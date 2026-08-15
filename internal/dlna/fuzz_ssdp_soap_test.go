// Fuzz coverage for the LAN-facing, UNAUTHENTICATED parsers.
//
// These sit at the bridge's widest attack surface: SSDP arrives as UDP
// datagrams from anything on the subnet, and the SOAP action header comes off
// an HTTP request the DLNA server answers before any identity is established.
// There is no bearer token in front of either, so unlike the /v1 handlers a
// panic here is reachable by any device on the LAN.
//
// A panic in the SSDP listener is worse than a normal handler panic, too: it
// runs on the read loop rather than under net/http's per-request recovery, so
// the blast radius is the discovery loop rather than one request.
package dlna

import "testing"

func FuzzParseMSearchST(f *testing.F) {
	f.Add([]byte("M-SEARCH * HTTP/1.1\r\nST: ssdp:all\r\nMX: 3\r\n\r\n"))
	f.Add([]byte(""))
	f.Fuzz(func(t *testing.T, b []byte) { _ = ParseMSearchST(b) })
}

func FuzzParseMSearchMX(f *testing.F) {
	f.Add([]byte("M-SEARCH * HTTP/1.1\r\nMX: 3\r\n\r\n"))
	f.Add([]byte("M-SEARCH * HTTP/1.1\r\nMX: 99999999999999999999\r\n\r\n"))
	f.Fuzz(func(t *testing.T, b []byte) { _ = ParseMSearchMX(b) })
}

func FuzzParseSOAPAction(f *testing.F) {
	f.Add(`"urn:schemas-upnp-org:service:ContentDirectory:1#Browse"`)
	f.Add(``)
	f.Fuzz(func(t *testing.T, s string) { _, _ = ParseSOAPAction(s) })
}
