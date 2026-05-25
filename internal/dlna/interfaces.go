package dlna

import "net"

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

	for _, addr := range addrs {
		ip := ipFromAddr(addr)
		if ip == nil {
			continue
		}
		// Defense in depth — re-check loopback at the address level too.
		if ip.IsLoopback() {
			continue
		}
		// Link-local always allowed (covers mDNS-only / no-DHCP networks).
		if ip.IsLinkLocalUnicast() {
			return true
		}
		// IsPrivate covers RFC1918 IPv4 + RFC4193 IPv6 unique-local.
		if ip.IsPrivate() {
			// CGNAT (100.64.0.0/10) is NOT RFC1918 — go's IsPrivate
			// returns false for it, so we don't need to filter it
			// explicitly here. CGNAT is only ever LAN-eligible via
			// the TsnetIfaceName opt-in above.
			return true
		}
	}
	return false
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
