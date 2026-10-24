package openhome

import (
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"testing"
)

// TestAuthorityIsHowALocationNamesItsSocket pins the one form both the
// Source's LOCATION and GET /v1/outputs' openHome.endpoints are built from
// (B470 PR 5b): an IPv4 address as it is, an IPv4-mapped one as its IPv4
// address, an IPv6 one bracketed with its zone percent-encoded as net/url
// writes a URL's host, and the port; the LOCATION parses back to the
// socket's address and port.
func TestAuthorityIsHowALocationNamesItsSocket(t *testing.T) {
	for _, tc := range []struct{ bound, want string }{
		{"192.168.1.42:7792", "192.168.1.42:7792"},
		{"[::ffff:192.168.1.42]:7792", "192.168.1.42:7792"},
		{"[fe80::1%eth0]:7792", "[fe80::1%25eth0]:7792"},
		{"[2001:db8::1]:49152", "[2001:db8::1]:49152"},
	} {
		ap := netip.MustParseAddrPort(tc.bound)
		if got := Authority(ap); got != tc.want {
			t.Errorf("Authority(%s) = %q, want %q", tc.bound, got, tc.want)
		}
		loc := sourceLocation(ap)
		if loc != "http://"+tc.want+"/openhome/description.xml" {
			t.Errorf("the LOCATION of %s is %q, not built from its Authority", tc.bound, loc)
		}
		u, err := url.Parse(loc)
		if err != nil || u.Scheme != "http" || u.Path != "/openhome/description.xml" {
			t.Fatalf("the LOCATION of %s, %q, does not parse as one: %v", tc.bound, loc, err)
		}
		back, err := netip.ParseAddrPort(u.Host)
		if err != nil || back != netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port()) {
			t.Errorf("the LOCATION of %s names %q (%v), not its socket", tc.bound, u.Host, err)
		}
	}
	if got := UDN(testUUID); got != "uuid:"+testUUID {
		t.Errorf("UDN(%s) = %q", testUUID, got)
	}
}

// TestEveryLocationTheSourceAdvertisesIsOneOfItsEndpoints: GET /v1/outputs
// lists the Source's endpoints so a client can tell this bridge's Source
// from a replay of its UDN, which SSDP carries in clear (B470 PR 5b): every
// LOCATION the shared SSDP subsystem advertises for the surface, in its
// alive notifications and its replies to a search, names one of
// Status.Endpoints; the endpoints are one per bound address, each with the
// port the kernel picked for it, in address order; the UDN advertised is
// UDN's; and a surface that has stopped lists none.
func TestEveryLocationTheSourceAdvertisesIsOneOfItsEndpoints(t *testing.T) {
	s, _, n := discoveryRig(t, eth0, eth0b)
	st := s.Status()
	eps := st.Endpoints()
	if len(st.Listening) != 2 || len(eps) != len(st.Listening) {
		t.Fatalf("endpoints %q of %d sockets, want one per bound address", eps, len(st.Listening))
	}
	for _, l := range st.Listening {
		if !slices.Contains(eps, Authority(l.Bound)) {
			t.Errorf("the socket bound at %s is not among the endpoints %q", l.Bound, eps)
		}
	}
	if st.Listening[0].Bound.Port() == st.Listening[1].Bound.Port() {
		t.Fatal("the fixture must prove a port the kernel picks per address")
	}
	if !slices.IsSortedFunc(eps, func(a, b string) int {
		return netip.MustParseAddrPort(a).Compare(netip.MustParseAddrPort(b))
	}) {
		t.Errorf("the endpoints %q are not in address order", eps)
	}
	searchSource(n, t, "ssdp:all", "192.168.1.50:50000", sourceGroup())
	awaitPackets(t, n, "ST", "upnp:rootdevice", 1)
	locations := 0
	for _, w := range n.Snapshot() {
		loc := packetHeader(w.Pkt, "LOCATION")
		if loc == "" {
			continue
		}
		locations++
		rest, ok := strings.CutPrefix(loc, "http://")
		authority, _, _ := strings.Cut(rest, "/")
		if !ok || !slices.Contains(eps, authority) {
			t.Errorf("an advertised LOCATION %q names %q, none of the endpoints %q", loc, authority, eps)
		}
		if usn := packetHeader(w.Pkt, "USN"); !strings.HasPrefix(usn, UDN(testUUID)) {
			t.Errorf("an advertisement's USN %q is not under UDN %q", usn, UDN(testUUID))
		}
	}
	if locations < 14 {
		t.Fatalf("%d LOCATIONs advertised, want the alive notifications and the search's replies", locations)
	}
	s.Stop()
	if got := s.Status().Endpoints(); len(got) != 0 {
		t.Errorf("a stopped surface lists endpoints %q", got)
	}
}
