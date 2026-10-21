package lan

import (
	"errors"
	"net"
	"net/netip"
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
