package dlna

import (
	"bytes"
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"
)

// macWithTailscale is the shape of the Mac B48 was measured on, in example
// addresses: a system tunnel with one fe80, Wi-Fi with a private IPv4 and a
// SLAAC address, awdl0 (fe80 only), Tailscale's utun (its 100.64/10 address
// and its ULA), and a self-assigned direct link.
func macWithTailscale() []fakeIface {
	return []fakeIface{
		tunnel("utun0", "fe80::a1"),
		lan("en0", "fe80::e0", "192.168.1.20", "2001:db8::20"),
		{name: "awdl0", flags: lanFlags, addrs: []string{"fe80::a0"}},
		tunnel("utun7", "fe80::d7", "100.64.0.7", "fd7a:115c:a1e0::7"),
		lan("en13", "169.254.7.8", "fe80::13"),
	}
}

// testHost is a host a servedAddrs reads through its seams: a test can change
// its interfaces or make a listing fail, and it counts the listings.
type testHost struct {
	mu       sync.Mutex
	ifs      []fakeIface
	fail     bool
	listings int
	addrsOf  interfaceAddrs
}

func newTestHost(ifs ...fakeIface) *testHost { return &testHost{ifs: ifs} }

// set replaces the host's interfaces.
func (h *testHost) set(ifs ...fakeIface) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ifs = ifs
}

// failListings makes every later listing fail, or succeed again.
func (h *testHost) failListings(fail bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.fail = fail
}

// listed reports how many times the host's interfaces were listed.
func (h *testHost) listed() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.listings
}

// served returns a servedAddrs that reads this host, on the clock now (nil:
// time.Now), for a listener pinned to the public address pinned names, at
// most one, as PinnedPublicAddress gives it (none: no pin).
func (h *testHost) served(now func() time.Time, pinned ...string) *servedAddrs {
	var pin netip.Addr
	if len(pinned) > 0 {
		pin = netip.MustParseAddr(pinned[0])
	}
	return &servedAddrs{
		interfaces: func() ([]net.Interface, error) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.listings++
			if h.fail {
				return nil, errors.New("listing failed")
			}
			ifaces, addrsOf := fakeHost(h.ifs)
			h.addrsOf = addrsOf
			return ifaces, nil
		},
		addrsOf: func(ifi *net.Interface) ([]net.Addr, error) {
			h.mu.Lock()
			defer h.mu.Unlock()
			return h.addrsOf(ifi)
		},
		now:    now,
		pinned: pin,
	}
}

// testClock is a clock a test moves by hand.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func newTestClock() *testClock { return &testClock{t: time.Unix(1_800_000_000, 0)} }

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// TestServedAddrsAreTheLANAddressesOfTheMulticastSet pins what the DLNA
// listener and its advertisers serve (backlog B48): loopback, the LAN
// addresses of the multicast set's members, and the public address
// dlna.listenAddress pins, the operator's opt-in (B687). A tailnet address
// is refused on each platform's shape, and so are another interface's
// link-local address and a member's public address, whatever the server
// advertises: until B687 a member's address the server advertised was
// served whatever its kind (the row a_members_public_address, where it
// was). A link-local IPv6 address is served only with its own interface as
// its zone. listen is dlna.listenAddress, the pin what PinnedPublicAddress
// makes of it.
func TestServedAddrsAreTheLANAddressesOfTheMulticastSet(t *testing.T) {
	windowsTailscale := fakeIface{name: "Tailscale", flags: net.FlagUp | net.FlagRunning | net.FlagMulticast,
		addrs: []string{"fe80::d12", "100.64.0.7", "fd7a:115c:a1e0::7"}}
	windowsWireGuard := fakeIface{name: "WireGuard", flags: net.FlagUp | net.FlagRunning, addrs: []string{"10.8.0.2"}}
	cases := []struct {
		name    string
		host    []fakeIface
		listen  string
		served  []string
		refused []string
	}{
		{"macos_with_tailscale", macWithTailscale(), ":7790",
			[]string{"192.168.1.20", "fe80::e0%en0", "169.254.7.8", "fe80::13%en13", "::ffff:192.168.1.20"},
			[]string{
				"100.64.0.7", "fd7a:115c:a1e0::7", "fe80::d7%utun7", // Tailscale's interface
				"fe80::a0%awdl0", "fe80::a1%utun0", // left out of the set beside en0
				"2001:db8::20",               // en0's SLAAC address
				"fe80::e0", "fe80::e0%utun7", // en0's fe80, without its zone or with another
				"192.168.9.9", "0.0.0.0", "::", // no interface's, and the unspecified ones
			}},
		{"linux_with_docker_and_tailscale", []fakeIface{
			lan("enp1s0f0", "192.168.50.24", "fe80::24", "2001:db8:50::24"),
			lan("docker0", "172.17.0.1", "fe80::17"),
			lan("veth1a2b3c", "fe80::bb"),
			tunnel("tailscale0", "100.64.0.24", "fd7a:115c:a1e0::24", "fe80::7"),
		}, ":7790",
			[]string{"192.168.50.24", "fe80::24%enp1s0f0", "172.17.0.1", "fe80::17%docker0"},
			[]string{"100.64.0.24", "fd7a:115c:a1e0::24", "fe80::7%tailscale0", "fe80::bb%veth1a2b3c", "2001:db8:50::24"}},
		// Wintun has no point-to-point flag; Tailscale's adapter is refused by
		// its addresses. A WireGuard tunnel numbered in RFC 1918 space is in
		// the set, and served: eligibility cannot tell it from a LAN (the
		// residual the B48 bullet names).
		{"windows_with_tailscale_and_wireguard", []fakeIface{
			windowsTailscale,
			lan("Ethernet", "192.168.0.208", "fe80::208"),
			windowsWireGuard,
		}, ":7790",
			[]string{"192.168.0.208", "fe80::208%Ethernet", "10.8.0.2"},
			[]string{"100.64.0.7", "fd7a:115c:a1e0::7", "fe80::d12%Tailscale"}},
		// A container on a Docker bridge network: a connection to a published
		// port arrives on eth0's address after DNAT, wherever it came from.
		{"container_on_a_bridge_network", []fakeIface{
			{name: "lo", flags: net.FlagUp | net.FlagLoopback, addrs: []string{"127.0.0.1", "::1"}},
			lan("eth0", "172.18.0.5", "fe80::5"),
		}, ":7790",
			[]string{"172.18.0.5", "fe80::5%eth0", "127.0.0.1", "::1"},
			nil},
		// Eligible through its ULA, with a public IPv4 address, which the
		// wiring announced and so the listener served until B687.
		{"a_members_public_address", []fakeIface{
			lan("eth0", "203.0.113.5", "fd12:3456::5"),
		}, ":7790",
			[]string{"fd12:3456::5"},
			[]string{"203.0.113.5"}},
		// A private and a public IPv4 address on one member, either order:
		// the private one is served and the public one is not.
		{"a_mixed_members_public_address", []fakeIface{
			lan("eth0", "93.184.216.34", "192.168.1.20", "fe80::20"),
			lan("eth1", "10.0.0.5", "198.51.100.1"),
		}, "0.0.0.0:7790",
			[]string{"192.168.1.20", "fe80::20%eth0", "10.0.0.5"},
			[]string{"93.184.216.34", "198.51.100.1", "::ffff:93.184.216.34"}},
		// The operator's opt-in: the public address dlna.listenAddress names
		// is served, a member's or not, while an interface holds it.
		{"the_public_address_the_listen_address_pins", []fakeIface{
			lan("eth0", "203.0.113.5", "fd12:3456::5"),
		}, "203.0.113.5:7790",
			[]string{"203.0.113.5", "::ffff:203.0.113.5", "fd12:3456::5"},
			nil},
		{"a_pinned_public_address_off_the_set", []fakeIface{
			lan("eth0", "192.168.1.20"),
			lan("eth1", "203.0.113.9", "fe80::9"),
		}, "203.0.113.9:7790",
			[]string{"203.0.113.9", "192.168.1.20"},
			[]string{"fe80::9%eth1"}},
		{"a_pinned_public_address_no_interface_holds", []fakeIface{
			lan("eth0", "192.168.1.20"),
		}, "203.0.113.77:7790",
			[]string{"192.168.1.20"},
			[]string{"203.0.113.77"}},
		{"a_pinned_global_ipv6_address", macWithTailscale(), "[2001:db8::20]:7790",
			[]string{"2001:db8::20", "192.168.1.20"},
			[]string{"100.64.0.7", "fd7a:115c:a1e0::7"}},
		// A pin to the tailnet is no opt-in, so its interface not being a
		// member is what refuses it, as before B687.
		{"a_listen_address_pinned_to_the_tailnet", macWithTailscale(), "100.64.0.7:7790",
			[]string{"192.168.1.20"},
			[]string{"100.64.0.7", "fd7a:115c:a1e0::7"}},
		{"a_listen_address_pinned_to_tailscales_ula", macWithTailscale(), "[fd7a:115c:a1e0::7]:7790",
			[]string{"192.168.1.20"},
			[]string{"100.64.0.7", "fd7a:115c:a1e0::7"}},
		// A pin to a 100.64/10 address on a member is no opt-in either (a
		// tailnet's cannot be told from an ISP's): it was served until B687,
		// as an address the server advertised.
		{"a_listen_address_pinned_to_100_64_on_a_member", []fakeIface{
			lan("eth0", "192.168.1.20", "100.70.0.1"),
		}, "100.70.0.1:7790",
			[]string{"192.168.1.20"},
			[]string{"100.70.0.1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var pin []string
			if a := PinnedPublicAddress(tc.listen); a.IsValid() {
				pin = append(pin, a.String())
			}
			s := newTestHost(tc.host...).served(nil, pin...)
			for _, a := range tc.served {
				if !s.serves(netip.MustParseAddr(a)) {
					t.Errorf("%s is refused, want served", a)
				}
			}
			for _, a := range tc.refused {
				if s.serves(netip.MustParseAddr(a)) {
					t.Errorf("%s is served, want refused", a)
				}
			}
		})
	}
}

// TestServedAddrsServeLoopbackWithoutReadingTheInterfaces pins that a
// loopback address, the host itself, is served before any snapshot is
// read: the connections a host's own tools and the tests make cost nothing.
func TestServedAddrsServeLoopbackWithoutReadingTheInterfaces(t *testing.T) {
	h := newTestHost(macWithTailscale()...)
	s := h.served(nil)
	for _, a := range []string{"127.0.0.1", "127.0.0.2", "::1", "::ffff:127.0.0.1"} {
		if !s.serves(netip.MustParseAddr(a)) {
			t.Errorf("%s is refused, want served", a)
		}
	}
	if n := h.listed(); n != 0 {
		t.Errorf("the interfaces were listed %d times for loopback, want 0", n)
	}
	if s.serves(netip.Addr{}) {
		t.Error("an address the kernel did not report is served, want refused")
	}
}

// TestServedAddrsServeANewLANAddressASecondAfterItsFirstRefusal pins the miss
// rebuild: an address the snapshot does not hold rebuilds it once the
// snapshot is servedMissRebuild old, so a LAN address that comes up after
// Start is served a second after its first connection at most, and not
// before.
func TestServedAddrsServeANewLANAddressASecondAfterItsFirstRefusal(t *testing.T) {
	clock := newTestClock()
	h := newTestHost(lan("en0", "192.168.1.20"))
	s := h.served(clock.now)
	if !s.serves(netip.MustParseAddr("192.168.1.20")) {
		t.Fatal("en0's address is refused")
	}
	h.set(lan("en0", "192.168.1.20"), lan("en1", "192.168.2.5"))
	clock.advance(servedMissRebuild / 2)
	if s.serves(netip.MustParseAddr("192.168.2.5")) {
		t.Errorf("a new address was served from a snapshot %v old, before any rebuild", servedMissRebuild/2)
	}
	clock.advance(servedMissRebuild / 2)
	if !s.serves(netip.MustParseAddr("192.168.2.5")) {
		t.Errorf("a new LAN address is still refused once the snapshot is %v old", servedMissRebuild)
	}
	if n := h.listed(); n != 2 {
		t.Errorf("listed the interfaces %d times, want 2 (the first build and one miss rebuild)", n)
	}
}

// TestServedAddrsStopServingAnAddressWithinTheMaxAge pins the age rebuild: a
// served address whose interface has gone is still served from the snapshot
// until it is servedMaxAge old, and refused from then on.
func TestServedAddrsStopServingAnAddressWithinTheMaxAge(t *testing.T) {
	clock := newTestClock()
	h := newTestHost(lan("en0", "192.168.1.20"), lan("en1", "192.168.2.5"))
	s := h.served(clock.now)
	if !s.serves(netip.MustParseAddr("192.168.2.5")) {
		t.Fatal("en1's address is refused")
	}
	h.set(lan("en0", "192.168.1.20"))
	clock.advance(servedMaxAge - time.Millisecond)
	if !s.serves(netip.MustParseAddr("192.168.2.5")) {
		t.Error("an address was refused before the snapshot was rebuilt")
	}
	clock.advance(time.Millisecond)
	if s.serves(netip.MustParseAddr("192.168.2.5")) {
		t.Errorf("an address whose interface has gone is still served once the snapshot is %v old", servedMaxAge)
	}
}

// TestRefusedAddressesRebuildTheSnapshotAtMostOnceASecond pins the cost of a
// flood of refused connections: one listing per servedMissRebuild.
func TestRefusedAddressesRebuildTheSnapshotAtMostOnceASecond(t *testing.T) {
	clock := newTestClock()
	h := newTestHost(macWithTailscale()...)
	s := h.served(clock.now)
	tailnet := netip.MustParseAddr("100.64.0.7")
	for range 1000 {
		if s.serves(tailnet) {
			t.Fatal("the tailnet address is served")
		}
	}
	if n := h.listed(); n != 1 {
		t.Errorf("1000 refusals in one instant listed the interfaces %d times, want 1", n)
	}
	clock.advance(servedMissRebuild)
	for range 1000 {
		s.serves(tailnet)
	}
	if n := h.listed(); n != 2 {
		t.Errorf("listed the interfaces %d times after a second, want 2", n)
	}
}

// TestAFailedListingKeepsTheSnapshotItWouldReplace pins that one listing
// that fails refuses nothing the last one served.
func TestAFailedListingKeepsTheSnapshotItWouldReplace(t *testing.T) {
	clock := newTestClock()
	h := newTestHost(lan("en0", "192.168.1.20"))
	s := h.served(clock.now)
	if !s.serves(netip.MustParseAddr("192.168.1.20")) {
		t.Fatal("en0's address is refused")
	}
	h.failListings(true)
	clock.advance(servedMaxAge)
	if !s.serves(netip.MustParseAddr("192.168.1.20")) {
		t.Error("a failed listing refused an address the last one served")
	}
	if n := h.listed(); n != 2 {
		t.Errorf("listed the interfaces %d times, want 2", n)
	}
}

// TestARefusalIsLoggedOncePerKindAndAddress pins the refusal log: one Warn
// for each kind (a connection, an M-SEARCH) and refused address, naming the
// address and the interface that holds it, however often the peer returns.
// The peer is not handed to the refusal at all (B604): the listener's and the
// M-SEARCH admission's own tests show it reaches no line
// (served_peer_log_test.go).
func TestARefusalIsLoggedOncePerKindAndAddress(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	s := newTestHost(macWithTailscale()...).served(nil)
	for range 3 {
		if s.admit(log, "a connection", netip.MustParseAddr("100.64.0.7")) {
			t.Fatal("a connection to the tailnet address was admitted")
		}
	}
	// A dual-stack listener reports an IPv4 connection's local address
	// mapped into IPv6 (measured end to end: "::ffff:" and the tailnet
	// address); it is the same address, logged once and as IPv4.
	s.admit(log, "a connection", netip.MustParseAddr("::ffff:100.64.0.7"))
	s.admit(log, "a connection", netip.MustParseAddr("fd7a:115c:a1e0::7"))
	s.admit(log, "an M-SEARCH", netip.MustParseAddr("100.64.0.7"))
	s.admit(log, "an M-SEARCH", netip.Addr{})
	if !s.admit(log, "a connection", netip.MustParseAddr("192.168.1.20")) {
		t.Fatal("a connection to the LAN address was refused")
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	const why = "it answers only on the private and link-local addresses of the LAN-eligible interfaces"
	want := []string{
		`refused a connection to an address it does not serve: ` + why + `" address=100.64.0.7 interface=utun7`,
		`refused a connection to an address it does not serve: ` + why + `" address=fd7a:115c:a1e0::7 interface=utun7`,
		`refused an M-SEARCH to an address it does not serve: ` + why + `" address=100.64.0.7 interface=utun7`,
		`refused an M-SEARCH to an address it does not serve: ` + why + `" address="(not reported)" interface=(none)`,
	}
	if len(lines) != len(want) {
		t.Fatalf("logged %d lines, want %d:\n%s", len(lines), len(want), buf.String())
	}
	for i, w := range want {
		if !strings.Contains(lines[i], "level=WARN") || !strings.HasSuffix(lines[i], w) {
			t.Errorf("line %d is\n  %s\nwant a WARN ending\n  %s", i, lines[i], w)
		}
	}
}

// TestTheRefusalLogIsBounded pins servedRefusedSeenCap: a host whose
// addresses churn logs that many refusals and then no more.
func TestTheRefusalLogIsBounded(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	s := newTestHost(macWithTailscale()...).served(nil)
	for i := range servedRefusedSeenCap + 10 {
		s.noteRefused(log, "a connection", netip.AddrFrom4([4]byte{100, 64, 1, byte(i)}))
	}
	if n := strings.Count(buf.String(), "level=WARN"); n != servedRefusedSeenCap {
		t.Errorf("logged %d refusals of %d addresses, want %d", n, servedRefusedSeenCap+10, servedRefusedSeenCap)
	}
}
