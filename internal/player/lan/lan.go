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
// alone does not count: OpenHome's discovery is IPv4 SSDP. Which of an
// eligible interface's addresses the player's listener serves, and whom
// it answers, are PR 5a's.
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
		in := Interface{Name: ifs[i].Name, Flags: ifs[i].Flags}
		addrs, _ := ifs[i].Addrs()
		for _, a := range addrs {
			n, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip, ok := netip.AddrFromSlice(n.IP)
			if !ok {
				continue
			}
			bits, _ := n.Mask.Size()
			in.Addrs = append(in.Addrs, netip.PrefixFrom(ip.Unmap(), bits))
		}
		out = append(out, in)
	}
	return out, nil
}

// Eligible is one eligible interface and its eligible IPv4 addresses.
type Eligible struct {
	Name  string
	Addrs []netip.Addr
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
		addrs, reason := eligible(in)
		if reason != "" {
			why = append(why, in.Name+" "+reason)
			continue
		}
		r.Interfaces = append(r.Interfaces, Eligible{Name: in.Name, Addrs: addrs})
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

// eligible returns in's eligible addresses, or why it is not eligible.
func eligible(in Interface) ([]netip.Addr, string) {
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
	var out []netip.Addr
	for _, p := range in.Addrs {
		a := p.Addr().Unmap()
		for _, ts := range tailscale {
			if ts.Contains(a) {
				return nil, "carries a Tailscale address (100.64.0.0/10 or fd7a:115c:a1e0::/48)"
			}
		}
		if !a.Is4() {
			continue
		}
		if linkLocal.Contains(a) || inAny(private, a) {
			out = append(out, a)
		}
	}
	if len(out) == 0 {
		return nil, "has no private or link-local IPv4 address yet (DHCP may not have answered)"
	}
	return out, ""
}

func inAny(ps []netip.Prefix, a netip.Addr) bool {
	for _, p := range ps {
		if p.Contains(a) {
			return true
		}
	}
	return false
}
