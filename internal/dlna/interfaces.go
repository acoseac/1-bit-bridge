package dlna

import (
	"errors"
	"net"
	"slices"
)

// EligibilityOpts customizes the per-interface LAN-eligibility check.
// TsnetIfaceName, when non-empty, opts the Tailscale tsnet interface
// in to DLNA binding — by default a CGNAT 100.64/10 address (Tailscale's
// range) is refused so an operator who hasn't opted in can't accidentally
// expose DLNA over their tailnet.
type EligibilityOpts struct {
	// TsnetIfaceName is the OS-level interface name of the Tailscale
	// tsnet socket (e.g. "utun7" on macOS, "tailscale0" on Linux).
	// Empty disables tsnet binding (the production default — operators
	// must explicitly flip `cfg.DLNA.AllowTsnet`).
	TsnetIfaceName string
}

// IsLANEligibleInterface reports whether the bridge's DLNA listener may
// bind to the given interface. This is the load-bearing safety invariant
// that keeps an unauthenticated DLNA endpoint off any non-LAN interface
// (a remote VPS public IP, for instance).
//
// Allowed:
//   - RFC1918 private ranges: 10/8, 172.16/12, 192.168/16
//   - Link-local IPv4 (169.254/16) and IPv6 (fe80::/10)
//   - The opted-in Tailscale tsnet interface (opts.TsnetIfaceName)
//
// Refused:
//   - Loopback (127.0.0.1, ::1) — DLNA on loopback is useless and is a
//     symptom of misconfiguration
//   - Public IPs (anything that's not in the allowed ranges)
//   - CGNAT (100.64/10) when NOT opted in via TsnetIfaceName — even
//     though Tailscale CGNAT addresses appear here, they're refused
//     unless explicitly opted in
//
// The helper signature accepts (iface, addrs) separately so tests can
// construct interface descriptors without making real OS-level calls
// (net.Interface.Addrs() is environment-dependent and expensive to mock).
func IsLANEligibleInterface(iface net.Interface, addrs []net.Addr, opts EligibilityOpts) bool {
	// Interface must be up and not loopback.
	if iface.Flags&net.FlagUp == 0 {
		return false
	}
	if iface.Flags&net.FlagLoopback != 0 {
		return false
	}

	// Tsnet opt-in — name match takes priority because the IP range
	// alone (CGNAT 100.64/10) would be ambiguous between Tailscale and
	// a generic carrier-grade NAT address.
	if opts.TsnetIfaceName != "" && iface.Name == opts.TsnetIfaceName {
		return true
	}

	// Scan ALL addresses before deciding. Returning true on the first
	// private/link-local address would wrongly ACCEPT a pure-WAN NIC — nearly
	// every interface (public gateways included) carries an fe80 link-local,
	// so an fe80 short-circuit classifies a public-only NIC as LAN. The
	// three-flag scan instead rejects a public-only NIC while keeping a
	// dual-stack home LAN eligible: modern home LAN interfaces routinely carry
	// a public IPv6 (2000::/3 via SLAAC) alongside their private IPv4, so
	// "disqualify on any public IP" would break DLNA on those networks.
	//
	// Eligible iff it has a private (LAN) address, OR it actually carries a
	// link-local address with no public unicast (mDNS-only / no-DHCP nets).
	// Requiring the link-local to be PRESENT (not merely "no public") keeps a
	// no-address / loopback-only interface ineligible. (Gemini-approved
	// predicate, refined for the empty-address edge the existing table pins.)
	hasPrivate, hasPublic, hasLinkLocal := false, false, false
	for _, addr := range addrs {
		ip := ipFromAddr(addr)
		if ip == nil || ip.IsLoopback() {
			continue
		}
		switch {
		case ip.IsPrivate():
			// RFC1918 IPv4 + RFC4193 IPv6 unique-local.
			hasPrivate = true
		case ip.IsLinkLocalUnicast():
			// fe80::/10 + 169.254/16 — neither Private nor GlobalUnicast.
			hasLinkLocal = true
		case ip.IsGlobalUnicast():
			// Public v4/v6, incl. CGNAT 100.64/10 (only LAN-eligible via the
			// TsnetIfaceName opt-in above).
			hasPublic = true
		}
	}
	return hasPrivate || (hasLinkLocal && !hasPublic)
}

// lanAddressClass is the best kind of address that makes an interface
// LAN-eligible, best first.
type lanAddressClass int

const (
	// lanPrivateIPv4 is an RFC 1918 IPv4 address: what a home LAN hands a
	// phone and a renderer, and what IPv4 multicast from this host needs.
	lanPrivateIPv4 lanAddressClass = iota
	// lanOtherUsable is no private IPv4, but an IPv6 ULA (fc00::/7), or the
	// opted-in tsnet interface.
	lanOtherUsable
	// lanLinkLocalOnly is fe80::/10 or 169.254/16 and nothing else: the
	// zero-config arm of IsLANEligibleInterface.
	lanLinkLocalOnly
)

// lanPreference is how the pickers rank an interface IsLANEligibleInterface
// accepted. Lower is better, and pointToPoint is compared before class.
//
// It orders; it never admits. Which interfaces the unauthenticated DLNA
// endpoint may bind at all is IsLANEligibleInterface's decision alone.
type lanPreference struct {
	// pointToPoint is Go's net.FlagPointToPoint: a link between two
	// endpoints, which carries no LAN multicast whatever address it has.
	// Go sets it from IFF_POINTOPOINT on macOS (every utun) and Linux (tun,
	// wg, ppp), and on Windows only for IF_TYPE_PPP and IF_TYPE_TUNNEL.
	// Wintun, the adapter Tailscale and WireGuard use there, is
	// IF_TYPE_PROP_VIRTUAL and gets no such flag, so on Windows it is the
	// address class that ranks it below the LAN.
	pointToPoint bool
	class        lanAddressClass
}

// lanPreferenceOf returns iface's lanPreference. It is meaningful only for
// an interface IsLANEligibleInterface accepted with the same addrs and opts.
func lanPreferenceOf(iface net.Interface, addrs []net.Addr, opts EligibilityOpts) lanPreference {
	p := lanPreference{pointToPoint: iface.Flags&net.FlagPointToPoint != 0, class: lanLinkLocalOnly}
	if opts.TsnetIfaceName != "" && iface.Name == opts.TsnetIfaceName {
		p.class = lanOtherUsable
	}
	for _, addr := range addrs {
		ip := ipFromAddr(addr)
		if ip == nil || !ip.IsPrivate() {
			continue
		}
		if ip.To4() != nil {
			p.class = lanPrivateIPv4
			break
		}
		p.class = min(p.class, lanOtherUsable)
	}
	return p
}

// better reports whether p ranks strictly above q.
func (p lanPreference) better(q lanPreference) bool {
	if p.pointToPoint != q.pointToPoint {
		return !p.pointToPoint
	}
	return p.class < q.class
}

// linkLocalTunnel reports whether p is a point-to-point interface with no
// address but a link-local one: a tunnel's own link, which reaches nothing
// on the LAN. The Mac this was measured on had six, each with only an fe80
// address, so an SSDP client bound to one cannot send IPv4 at all, and the
// first was enumerated ahead of en0.
func (p lanPreference) linkLocalTunnel() bool {
	return p.pointToPoint && p.class == lanLinkLocalOnly
}

// interfaceAddrs returns one interface's addresses.
type interfaceAddrs func(*net.Interface) ([]net.Addr, error)

// hostInterfaceAddrs is the interfaceAddrs the exported pickers use.
func hostInterfaceAddrs(ifi *net.Interface) ([]net.Addr, error) { return ifi.Addrs() }

// lanCandidate is one interface IsLANEligibleInterface accepted, with its
// lanPreference.
type lanCandidate struct {
	iface *net.Interface
	pref  lanPreference
}

// lanCandidates returns every interface in ifaces that IsLANEligibleInterface
// accepts, in the order given. An interface whose addresses cannot be read is
// left out, as it always was.
func lanCandidates(ifaces []net.Interface, addrsOf interfaceAddrs, opts EligibilityOpts) []lanCandidate {
	var out []lanCandidate
	for i := range ifaces {
		addrs, err := addrsOf(&ifaces[i])
		if err != nil {
			continue
		}
		if IsLANEligibleInterface(ifaces[i], addrs, opts) {
			// The element's address, not a loop-local copy's: no heap
			// escape per candidate, and ifaces outlives the call through
			// the pointers the multicast set returns (gemini-code-assist on
			// PR #328).
			out = append(out, lanCandidate{iface: &ifaces[i], pref: lanPreferenceOf(ifaces[i], addrs, opts)})
		}
	}
	return out
}

// pickLANInterface is PickLANEligibleInterface over a given enumeration: the
// candidate lanPreference ranks best, and the first in enumeration order
// among equals. On a host whose first eligible interface already ranks best
// that is the interface the picker always chose.
func pickLANInterface(ifaces []net.Interface, addrsOf interfaceAddrs, opts EligibilityOpts) (*net.Interface, error) {
	cands := lanCandidates(ifaces, addrsOf, opts)
	if len(cands) == 0 {
		return nil, errors.New("no LAN-eligible interface found")
	}
	best := cands[0]
	for _, c := range cands[1:] {
		if c.pref.better(best.pref) {
			best = c
		}
	}
	chosen := *best.iface
	return &chosen, nil
}

// pickAllLANInterfaces is PickAllLANEligibleInterfaces over a given
// enumeration: every candidate in enumeration order, except that a
// linkLocalTunnel is left out whenever anything else is eligible. When only
// such tunnels are, they are all kept, as they always were.
func pickAllLANInterfaces(ifaces []net.Interface, addrsOf interfaceAddrs, opts EligibilityOpts) []*net.Interface {
	cands := lanCandidates(ifaces, addrsOf, opts)
	dropTunnels := slices.ContainsFunc(cands, func(c lanCandidate) bool { return !c.pref.linkLocalTunnel() })
	var out []*net.Interface
	for _, c := range cands {
		if dropTunnels && c.pref.linkLocalTunnel() {
			continue
		}
		out = append(out, c.iface)
	}
	return out
}

func ipFromAddr(addr net.Addr) net.IP {
	switch v := addr.(type) {
	case *net.IPNet:
		return v.IP
	case *net.IPAddr:
		return v.IP
	}
	return nil
}
