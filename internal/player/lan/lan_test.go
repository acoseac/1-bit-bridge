package lan

import (
	"errors"
	"net"
	"net/netip"
	"slices"
	"strings"
	"testing"
)

// iface is an Interface with flags and addresses.
func iface(name string, flags net.Flags, addrs ...string) Interface {
	in := Interface{Name: name, Flags: flags}
	for _, a := range addrs {
		in.Addrs = append(in.Addrs, netip.MustParsePrefix(a))
	}
	return in
}

// lanFlags are a LAN interface's: up, running, multicast.
const lanFlags = net.FlagUp | net.FlagRunning | net.FlagMulticast

// list is a Lister that returns ifaces.
func list(ifaces ...Interface) Lister {
	return func() ([]Interface, error) { return ifaces, nil }
}

// TestReadyOnAPrivateOrLinkLocalIPv4Interface: an interface that is up,
// running, multicast and holds an RFC 1918 or a link-local IPv4 address
// is the LAN the player serves.
func TestReadyOnAPrivateOrLinkLocalIPv4Interface(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   Interface
	}{
		{"home 192.168", iface("eth0", lanFlags, "192.168.1.20/24", "fe80::1/64")},
		{"10/8", iface("eth0", lanFlags, "10.0.0.7/8")},
		{"172.16/12", iface("wlan0", lanFlags, "172.20.1.2/16")},
		{"a direct cable", iface("eth0", lanFlags, "169.254.7.7/16")},
		{"dual stack with a public IPv6", iface("eth0", lanFlags, "192.168.1.20/24", "2001:db8::20/64")},
	} {
		r := Check(list(iface("lo", net.FlagUp|net.FlagLoopback|net.FlagRunning, "127.0.0.1/8"), tc.in))
		if r.State != StateReady || len(r.Interfaces) != 1 || r.Interfaces[0].Name != tc.in.Name {
			t.Errorf("%s: %+v", tc.name, r)
		}
	}
}

// TestLANUnavailableIsAStateNotARefusal: no eligible interface yet is
// lan_unavailable with a reason naming what was seen; each class the
// player does not serve is passed over.
func TestLANUnavailableIsAStateNotARefusal(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   Interface
		why  string
	}{
		{"loopback", iface("lo", net.FlagUp|net.FlagLoopback|net.FlagRunning|net.FlagMulticast, "127.0.0.1/8"), "loopback"},
		{"down", iface("eth0", net.FlagMulticast, "192.168.1.20/24"), "not up and running"},
		{"no carrier", iface("eth0", net.FlagUp|net.FlagMulticast, "192.168.1.20/24"), "not up and running"},
		{"no multicast", iface("eth0", net.FlagUp|net.FlagRunning, "192.168.1.20/24"), "multicast"},
		{"a tunnel", iface("wg0", lanFlags|net.FlagPointToPoint, "10.8.0.2/24"), "point-to-point"},
		{"tailscale v4", iface("tailscale0", lanFlags, "100.101.102.103/32", "192.168.1.20/24"), "Tailscale"},
		{"tailscale ULA", iface("utun9", lanFlags, "fd7a:115c:a1e0::1/128", "10.0.0.7/8"), "Tailscale"},
		{"DHCP not answered", iface("eth0", lanFlags, "fe80::1/64"), "no private or link-local IPv4"},
		{"public only", iface("eth0", lanFlags, "203.0.113.7/24"), "no private or link-local IPv4"},
		{"CGNAT", iface("eth0", lanFlags, "100.64.0.9/10"), "Tailscale"},
	} {
		r := Check(list(tc.in))
		if r.State != StateUnavailable || len(r.Interfaces) != 0 || !strings.Contains(r.Reason, tc.in.Name) || !strings.Contains(r.Reason, tc.why) {
			t.Errorf("%s: %+v; want lan_unavailable naming %s and %q", tc.name, r, tc.in.Name, tc.why)
		}
	}
	if r := Check(list()); r.State != StateUnavailable || !strings.Contains(r.Reason, "no network interface") {
		t.Errorf("no interface: %+v", r)
	}
	r := Check(func() ([]Interface, error) { return nil, errors.New("netlink refused") })
	if r.State != StateUnavailable || !strings.Contains(r.Reason, "netlink refused") {
		t.Errorf("a lister that fails: %+v", r)
	}
}

// indexed is in with its interface index set.
func indexed(in Interface, index int) Interface {
	in.Index = index
	return in
}

// TestLookKeepsEachServedAddressWithItsPrefixAndInterface: Look is what the
// player's listener serves (B470 PR 5a2a): every eligible IPv4 address of
// every eligible interface, each with its on-link prefix and the index and
// name of the interface that holds it, a secondary address its own entry;
// and every address the box holds, eligible or not, its loopback, its
// tunnels' and its IPv6 addresses included, which the listener never takes
// a connection from.
func TestLookKeepsEachServedAddressWithItsPrefixAndInterface(t *testing.T) {
	inv, err := Look(list(
		indexed(iface("lo", net.FlagUp|net.FlagLoopback|net.FlagRunning, "127.0.0.1/8", "::1/128"), 1),
		indexed(iface("eth0", lanFlags, "192.168.1.20/24", "10.0.0.2/8", "fe80::1/64", "2001:db8::20/64"), 2),
		indexed(iface("wlan0", net.FlagUp|net.FlagMulticast, "192.168.7.3/24"), 3),
		indexed(iface("wg0", lanFlags|net.FlagPointToPoint, "10.8.0.2/24"), 4),
		indexed(iface("tailscale0", lanFlags, "100.101.102.103/32"), 5),
		indexed(iface("eth1", lanFlags, "169.254.7.7/16"), 6),
	))
	if err != nil {
		t.Fatal(err)
	}
	want := []Address{
		{Prefix: netip.MustParsePrefix("192.168.1.20/24"), Interface: "eth0", Index: 2},
		{Prefix: netip.MustParsePrefix("10.0.0.2/8"), Interface: "eth0", Index: 2},
		{Prefix: netip.MustParsePrefix("169.254.7.7/16"), Interface: "eth1", Index: 6},
	}
	if len(inv.Served) != len(want) {
		t.Fatalf("served %+v, want %+v", inv.Served, want)
	}
	for i := range want {
		if inv.Served[i] != want[i] {
			t.Errorf("served[%d] = %+v, want %+v", i, inv.Served[i], want[i])
		}
	}
	for _, a := range []string{"127.0.0.1", "::1", "192.168.1.20", "10.0.0.2", "fe80::1", "2001:db8::20", "192.168.7.3", "10.8.0.2", "100.101.102.103", "169.254.7.7"} {
		if !slices.Contains(inv.Own, netip.MustParseAddr(a)) {
			t.Errorf("own %v lacks %s", inv.Own, a)
		}
	}
	if _, err := Look(func() ([]Interface, error) { return nil, errors.New("netlink refused") }); err == nil || !strings.Contains(err.Error(), "netlink refused") {
		t.Errorf("a listing that fails: %v", err)
	}
	r := Check(list(indexed(iface("eth0", lanFlags, "192.168.1.20/24", "10.0.0.2/8"), 2)))
	if len(r.Interfaces) != 1 || r.Interfaces[0].Index != 2 || len(r.Interfaces[0].Prefixes) != 2 ||
		r.Interfaces[0].Prefixes[0] != netip.MustParsePrefix("192.168.1.20/24") || r.Interfaces[0].Addrs[1] != netip.MustParseAddr("10.0.0.2") {
		t.Errorf("Check's eligible interface %+v, want its index, its addresses and their prefixes", r.Interfaces)
	}
}

// TestAnInterfaceAddressBecomesItsOwnPrefix: what net.Interface.Addrs
// gives becomes the address and its on-link prefix: an IPv4 address in its
// 16-byte form with a 4-byte or a 16-byte mask is the IPv4 address and its
// prefix, and a mask that is not a prefix (no run of ones) is the address
// alone, which has no on-link neighbour, never /0, which would put every
// address on the link.
func TestAnInterfaceAddressBecomesItsOwnPrefix(t *testing.T) {
	v4in16 := net.ParseIP("192.168.1.20")
	for _, tc := range []struct {
		name string
		in   *net.IPNet
		want string
	}{
		{"a 4-byte address and mask", &net.IPNet{IP: v4in16.To4(), Mask: net.CIDRMask(24, 32)}, "192.168.1.20/24"},
		{"a 16-byte address, a 4-byte mask", &net.IPNet{IP: v4in16, Mask: net.CIDRMask(24, 32)}, "192.168.1.20/24"},
		{"a 16-byte address, a 16-byte mask", &net.IPNet{IP: v4in16, Mask: net.CIDRMask(120, 128)}, "192.168.1.20/24"},
		{"a mask that is not a prefix", &net.IPNet{IP: v4in16.To4(), Mask: net.IPv4Mask(255, 0, 255, 0)}, "192.168.1.20/32"},
		{"an IPv6 address", &net.IPNet{IP: net.ParseIP("fe80::1"), Mask: net.CIDRMask(64, 128)}, "fe80::1/64"},
	} {
		got, ok := prefixOf(tc.in)
		if !ok || got != netip.MustParsePrefix(tc.want) {
			t.Errorf("%s: %v, %v; want %s", tc.name, got, ok, tc.want)
		}
	}
	if _, ok := prefixOf(&net.IPNet{IP: net.IP{1, 2, 3}, Mask: net.CIDRMask(24, 32)}); ok {
		t.Error("an address of three bytes became a prefix")
	}
}

// TestIsLANAddressIsTheEligibleClasses: the classes a served address is
// of, which the listener holds a source to as well: RFC 1918 and the
// link-local 169.254/16, IPv4 alone, an IPv4-mapped one unmapped.
func TestIsLANAddressIsTheEligibleClasses(t *testing.T) {
	for a, want := range map[string]bool{
		"10.1.2.3": true, "172.16.0.1": true, "172.31.255.254": true, "192.168.1.50": true, "169.254.7.7": true, "::ffff:192.168.1.50": true,
		"172.32.0.1": false, "192.169.0.1": false, "100.64.0.9": false, "203.0.113.7": false, "127.0.0.1": false,
		"0.0.0.0": false, "224.0.0.251": false, "fe80::1": false, "fd00::1": false,
	} {
		if got := IsLANAddress(netip.MustParseAddr(a)); got != want {
			t.Errorf("IsLANAddress(%s) = %v, want %v", a, got, want)
		}
	}
	inv, err := Look(list(indexed(iface("eth0", lanFlags, "::ffff:192.168.1.20/120"), 2)))
	if err != nil || len(inv.Served) != 1 || inv.Served[0].Prefix != netip.MustParsePrefix("192.168.1.20/24") {
		t.Errorf("a mapped address's prefix: %+v, %v; want 192.168.1.20/24", inv.Served, err)
	}
}

// TestTheReasonIsBounded: a box with many interfaces is named up to a
// bound, so the reason stays one line.
func TestTheReasonIsBounded(t *testing.T) {
	var many []Interface
	for i := 0; i < 40; i++ {
		many = append(many, iface("veth"+string(rune('a'+i%26))+string(rune('a'+i/26)), lanFlags, "fe80::1/64"))
	}
	r := Check(list(many...))
	if r.State != StateUnavailable || strings.Count(r.Reason, "veth") > maxNamed || !strings.Contains(r.Reason, "more") {
		t.Errorf("reason %q", r.Reason)
	}
}
