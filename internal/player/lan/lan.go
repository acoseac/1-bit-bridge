// Package lan says whether the box has, yet, a LAN interface the player can
// serve its controllers on (B470 PR 4a). It is runtime state, never a
// config refusal: on a Raspberry Pi the interface and DHCP often come up
// after the service starts, so a player with no eligible interface yet is
// lan_unavailable, reported, and asked again, and nothing stops.
//
// An eligible interface is up and running (it has a carrier), multicast
// (discovery is SSDP), not loopback, not point-to-point (a VPN tunnel
// numbered in private space is not the LAN: private space alone is not
// enough), carries no Tailscale address (100.64.0.0/10, which is also
// carrier-grade NAT, or fd7a:115c:a1e0::/48), and holds an IPv4 address in
// RFC 1918 space or the link-local 169.254.0.0/16 of a direct cable. IPv6
// alone does not count: OpenHome's discovery is IPv4 SSDP.
//
// The player's listener (internal/player/openhome, B470 PR 5a2a) serves each
// eligible IPv4 address of each eligible interface, and answers a source
// inside that address's on-link prefix alone: Look gives each such address
// with its prefix and its interface's index, and every address the box
// holds, none of which is a controller's.
package lan

import (
	"net"
	"net/netip"
	"strconv"
	"strings"
)

// The player's LAN states.
const (
	// StateReady is at least one eligible interface.
	StateReady = "ready"
	// StateUnavailable is none yet.
	StateUnavailable = "lan_unavailable"
)

// Interface is one network interface as a Lister reports it.
type Interface struct {
	Name  string
	Index int
	Flags net.Flags
	Addrs []netip.Prefix
}

// Lister lists the host's interfaces; HostInterfaces is this host's, and
// a test passes its own.
type Lister func() ([]Interface, error)

// HostInterfaces lists this host's interfaces (net.Interfaces), each with
// its addresses. An interface whose addresses cannot be read is listed
// with none.
func HostInterfaces() ([]Interface, error) {
	ifs, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	out := make([]Interface, 0, len(ifs))
	for i := range ifs {
		in := Interface{Name: ifs[i].Name, Index: ifs[i].Index, Flags: ifs[i].Flags}
		addrs, _ := ifs[i].Addrs()
		for _, a := range addrs {
			n, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			if p, ok := prefixOf(n); ok {
				in.Addrs = append(in.Addrs, p)
			}
		}
		out = append(out, in)
	}
	return out, nil
}

// prefixOf is an interface address as the address and its on-link prefix:
// unmapped, so an IPv4 address in its 16-byte form is an IPv4 address, with
// a 4-byte mask or a 16-byte one. A mask that is not a prefix (no single run
// of ones, which net.IPMask.Size answers 0, 0), one of no ones, and one
// that does not fit the address are the address alone, which has no on-link
// neighbour: read as /0, as the conversion before it did, a mask that is not
// a prefix put every address on the link. ok is false for an address that is
// no IP.
func prefixOf(n *net.IPNet) (netip.Prefix, bool) {
	ip, ok := netip.AddrFromSlice(n.IP)
	if !ok {
		return netip.Prefix{}, false
	}
	ip = ip.Unmap()
	ones, bits := n.Mask.Size()
	if ip.Is4() && bits == 128 {
		ones -= 96
	}
	p := netip.PrefixFrom(ip, ones)
	if bits == 0 || ones <= 0 || !p.IsValid() {
		p = netip.PrefixFrom(ip, ip.BitLen())
	}
	return p, true
}

// Address is one address the player's listener serves (B470 PR 5a2a): an
// eligible IPv4 address of an eligible interface, its on-link prefix, and
// the interface that holds it, by name and by index, which is what a socket
// restricted to the interface names.
type Address struct {
	// Prefix is the address and its on-link prefix: Prefix.Addr() is the
	// address, and Prefix.Masked() the network on its link.
	Prefix    netip.Prefix
	Interface string
	Index     int
}

// Inventory is one listing of the box's interfaces as the player's listener
// reads it: the addresses it serves, and every address the box holds.
type Inventory struct {
	// Served are the eligible interfaces' eligible addresses, in the
	// listing's order, a secondary address its own entry.
	Served []Address
	// Own is every address of every interface listed, eligible or not, the
	// loopback's, a tunnel's and IPv6 ones included, unmapped and without a
	// zone: no connection from one of them is a controller's.
	Own []netip.Addr
}

// Look lists the interfaces once, and returns what the player's listener
// serves and every address the box holds. A listing that fails is that
// error and nothing else, so a caller keeps what it had.
func Look(list Lister) (Inventory, error) {
	ifs, err := list()
	if err != nil {
		return Inventory{}, err
	}
	var inv Inventory
	for _, in := range ifs {
		for _, p := range in.Addrs {
			inv.Own = append(inv.Own, p.Addr().Unmap().WithZone(""))
		}
		prefixes, reason := eligible(in)
		if reason != "" {
			continue
		}
		for _, p := range prefixes {
			inv.Served = append(inv.Served, Address{Prefix: p, Interface: in.Name, Index: in.Index})
		}
	}
	return inv, nil
}

// IsLANAddress reports whether a is an IPv4 address of the classes an
// eligible interface's served address is: RFC 1918 or the link-local
// 169.254.0.0/16. The player's listener asks it of a connection's source
// too, so a prefix wider than its address's class (a mask a box was given
// by mistake) puts no public address on the link.
func IsLANAddress(a netip.Addr) bool {
	a = a.Unmap()
	return a.Is4() && (linkLocal.Contains(a) || inAny(private, a))
}

// Eligible is one eligible interface and its eligible IPv4 addresses.
type Eligible struct {
	Name  string
	Index int
	Addrs []netip.Addr
	// Prefixes are Addrs with their on-link prefixes, in the same order.
	Prefixes []netip.Prefix
}

// Readiness is the player's LAN state at one look.
type Readiness struct {
	State      string
	Interfaces []Eligible
	// Reason says, for lan_unavailable, why each interface seen is not
	// eligible, at most maxNamed of them.
	Reason string
}

// maxNamed bounds the interfaces a reason names.
const maxNamed = 8

// The address classes.
var (
	private = []netip.Prefix{
		netip.MustParsePrefix("10.0.0.0/8"),
		netip.MustParsePrefix("172.16.0.0/12"),
		netip.MustParsePrefix("192.168.0.0/16"),
	}
	linkLocal = netip.MustParsePrefix("169.254.0.0/16")
	// tailscale are Tailscale's ranges; the IPv4 one is carrier-grade NAT
	// space as well, which no LAN this serves is numbered in.
	tailscale = []netip.Prefix{
		netip.MustParsePrefix("100.64.0.0/10"),
		netip.MustParsePrefix("fd7a:115c:a1e0::/48"),
	}
)

// Check looks at the interfaces list gives, once.
func Check(list Lister) Readiness {
	ifs, err := list()
	if err != nil {
		return Readiness{State: StateUnavailable, Reason: "the interfaces could not be listed: " + err.Error()}
	}
	var r Readiness
	var why []string
	for _, in := range ifs {
		prefixes, reason := eligible(in)
		if reason != "" {
			why = append(why, in.Name+" "+reason)
			continue
		}
		e := Eligible{Name: in.Name, Index: in.Index, Prefixes: prefixes}
		for _, p := range prefixes {
			e.Addrs = append(e.Addrs, p.Addr())
		}
		r.Interfaces = append(r.Interfaces, e)
	}
	if len(r.Interfaces) > 0 {
		r.State = StateReady
		return r
	}
	r.State = StateUnavailable
	switch {
	case len(why) == 0:
		r.Reason = "no network interface is up"
	case len(why) > maxNamed:
		r.Reason = "no LAN interface yet: " + strings.Join(why[:maxNamed], "; ") + "; and " + strconv.Itoa(len(why)-maxNamed) + " more"
	default:
		r.Reason = "no LAN interface yet: " + strings.Join(why, "; ")
	}
	return r
}

// eligible returns in's eligible addresses, each with its on-link prefix,
// or why it is not eligible.
func eligible(in Interface) ([]netip.Prefix, string) {
	switch {
	case in.Flags&net.FlagLoopback != 0:
		return nil, "is loopback"
	case in.Flags&net.FlagUp == 0 || in.Flags&net.FlagRunning == 0:
		return nil, "is not up and running"
	case in.Flags&net.FlagMulticast == 0:
		return nil, "has no multicast"
	case in.Flags&net.FlagPointToPoint != 0:
		return nil, "is point-to-point (a tunnel)"
	}
	var out []netip.Prefix
	for _, p := range in.Addrs {
		p = unmapped(p)
		a := p.Addr()
		for _, ts := range tailscale {
			if ts.Contains(a) {
				return nil, "carries a Tailscale address (100.64.0.0/10 or fd7a:115c:a1e0::/48)"
			}
		}
		if IsLANAddress(a) {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil, "has no private or link-local IPv4 address yet (DHCP may not have answered)"
	}
	return out, ""
}

// unmapped is p with an IPv4-mapped IPv6 address unmapped and its length
// counted in IPv4 bits, and with no zone; a prefix whose length does not fit
// its address (an invalid one) is the address alone.
func unmapped(p netip.Prefix) netip.Prefix {
	a, bits := p.Addr(), p.Bits()
	if a.Is4In6() {
		a, bits = a.Unmap(), bits-96
	}
	a = a.WithZone("")
	if q := netip.PrefixFrom(a, bits); q.IsValid() && bits > 0 {
		return q
	}
	return netip.PrefixFrom(a, a.BitLen())
}

func inAny(ps []netip.Prefix, a netip.Addr) bool {
	for _, p := range ps {
		if p.Contains(a) {
			return true
		}
	}
	return false
}
