//go:build linux || darwin

package dlna

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/ipv4"
)

// searchTheGroupOutOf sends one M-SEARCH for st to the SSDP group out of
// iface at TTL 0, with an MX header when mx is not empty, from a socket the
// test closes at its end, and returns that socket. The kernel loops the
// search back as a datagram that arrived on iface and transmits nothing (the
// B38 rule). Windows is left out of this file: ListenMulticastUDP turns
// IP_MULTICAST_LOOP off on the server's listener, which Winsock applies to
// what a socket receives, so the server hears no search sent from its own
// host (backlog B120).
func searchTheGroupOutOf(t *testing.T, iface *net.Interface, st, mx string) *net.UDPConn {
	t.Helper()
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero})
	if err != nil {
		t.Fatalf("open a searcher: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	p := ipv4.NewPacketConn(c)
	if err := p.SetMulticastInterface(iface); err != nil {
		t.Fatalf("pin the searcher to %s: %v", iface.Name, err)
	}
	if err := p.SetMulticastTTL(0); err != nil {
		t.Fatalf("set TTL 0: %v", err)
	}
	if err := p.SetMulticastLoopback(true); err != nil {
		t.Fatalf("turn loopback on: %v", err)
	}
	msg := "M-SEARCH * HTTP/1.1\r\nHOST: " + SSDPMulticastAddr + "\r\nMAN: \"ssdp:discover\"\r\n"
	if mx != "" {
		msg += "MX: " + mx + "\r\n"
	}
	msg += "ST: " + st + "\r\n\r\n"
	group, err := net.ResolveUDPAddr("udp4", SSDPMulticastAddr)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.WriteToUDP([]byte(msg), group); err != nil {
		t.Skipf("this host will not send a multicast datagram out of %s to loop it back: %v", iface.Name, err)
	}
	return c
}

// readResponses reads c until it holds want M-SEARCH responses that name
// udn, or until nothing has arrived for wait, and returns them in arrival
// order with their DATE normalized (normalizeDate).
func readResponses(t *testing.T, c *net.UDPConn, udn string, want int, wait time.Duration) []string {
	t.Helper()
	var got []string
	buf := make([]byte, 8192)
	for len(got) < want {
		_ = c.SetReadDeadline(time.Now().Add(wait))
		n, _, err := c.ReadFromUDP(buf)
		if err != nil {
			return got
		}
		s := string(buf[:n])
		if strings.HasPrefix(s, "HTTP/1.1 ") && strings.Contains(s, udn) {
			got = append(got, normalizeDate(t, s))
		}
	}
	return got
}

// TestTheMediaServerAnswersEachSearchTargetAsItAlwaysHas pins the MediaServer's
// answers to a multicast M-SEARCH, through the real Server on the loopback
// interface: ssdp:all gets one response per target, in the order the server
// announces them, each carrying its own NT as its ST; a search for one of its
// targets gets that one; a search for anything else gets nothing; and MX is
// honoured. Every byte is as written here but DATE, which is checked and then
// normalized.
func TestTheMediaServerAnswersEachSearchTargetAsItAlwaysHas(t *testing.T) {
	lo := loopbackInterface(t)
	udn := goldenUDN(t)
	s, addr := goldenServer(t, udn, ServerConfig{})
	s.cfg.AdvertiseEndpoints = []AdvertiseEndpoint{{Interface: lo, ServerURL: "http://" + addr}}
	if err := s.Start(context.Background()); err != nil {
		t.Skipf("the server did not start on %s: %v", lo.Name, err)
	}
	t.Cleanup(func() { _ = s.Stop(context.Background()) })
	location := "http://" + addr + "/dlna/description.xml"
	targets := goldenTargets(udn)
	response := func(i int) string { return goldenResponse(location, targets[i][0], targets[i][1]) }
	var all []string
	for i := range targets {
		all = append(all, response(i))
	}
	for _, tc := range []struct {
		name, st, mx string
		want         []string
	}{
		{"ssdp_all", "ssdp:all", "", all},
		{"rootdevice", "upnp:rootdevice", "", []string{response(0)}},
		{"its_uuid", udn, "", []string{response(1)}},
		{"mediaserver_device", "urn:schemas-upnp-org:device:MediaServer:1", "", []string{response(2)}},
		{"content_directory", "urn:schemas-upnp-org:service:ContentDirectory:1", "", []string{response(3)}},
		{"connection_manager", "urn:schemas-upnp-org:service:ConnectionManager:1", "", []string{response(4)}},
		{"rootdevice_with_mx", "upnp:rootdevice", "1", []string{response(0)}},
		{"another_device_type", "urn:schemas-upnp-org:device:MediaRenderer:1", "", nil},
		{"another_uuid", "uuid:00000000-0000-4000-8000-000000000000", "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := searchTheGroupOutOf(t, lo, tc.st, tc.mx)
			// One more than wanted, so a surplus answer shows; the quiet wait
			// is past the 100 ms window an MX-less search is answered in, and
			// past MX's second.
			wait := 600 * time.Millisecond
			if tc.mx != "" {
				wait = 1500 * time.Millisecond
			}
			sameDatagrams(t, "the answers to "+tc.st, readResponses(t, c, udn, len(tc.want)+1, wait), tc.want)
		})
	}
}
