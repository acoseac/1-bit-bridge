package dlna

import (
	"net"
	"testing"
)

// mkIface constructs a synthetic interface descriptor with the given
// flags. The Name + Index fields are populated to non-zero defaults so
// the tsnet name-match path can be exercised when needed.
func mkIface(name string, flags net.Flags) net.Interface {
	return net.Interface{Index: 1, Name: name, Flags: flags}
}

func mkIPNet(ip string) net.Addr {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		panic("invalid test IP: " + ip)
	}
	return &net.IPNet{IP: parsed, Mask: net.CIDRMask(24, 32)}
}

func TestIsLANEligibleInterface(t *testing.T) {
	cases := []struct {
		name  string
		iface net.Interface
		addrs []net.Addr
		opts  EligibilityOpts
		want  bool
	}{
		// RFC1918 — all three blocks allowed
		{"rfc1918_10", mkIface("eth0", net.FlagUp), []net.Addr{mkIPNet("10.0.0.5")}, EligibilityOpts{}, true},
		{"rfc1918_172_16", mkIface("eth0", net.FlagUp), []net.Addr{mkIPNet("172.20.0.5")}, EligibilityOpts{}, true},
		{"rfc1918_192_168", mkIface("eth0", net.FlagUp), []net.Addr{mkIPNet("192.168.1.5")}, EligibilityOpts{}, true},

		// Link-local IPv4 allowed
		{"linklocal_ipv4", mkIface("eth0", net.FlagUp), []net.Addr{mkIPNet("169.254.1.2")}, EligibilityOpts{}, true},

		// Link-local IPv6 allowed (fe80::/10)
		{"linklocal_ipv6", mkIface("eth0", net.FlagUp), []net.Addr{mkIPNet("fe80::1")}, EligibilityOpts{}, true},

		// Loopback refused at interface level AND at address level
		{"loopback_iface_flag", mkIface("lo0", net.FlagUp|net.FlagLoopback), []net.Addr{mkIPNet("127.0.0.1")}, EligibilityOpts{}, false},
		{"loopback_addr_only", mkIface("eth0", net.FlagUp), []net.Addr{mkIPNet("127.0.0.1")}, EligibilityOpts{}, false},

		// Interface down refused
		{"iface_down", mkIface("eth0", 0), []net.Addr{mkIPNet("192.168.1.5")}, EligibilityOpts{}, false},

		// Public IP refused
		{"public_ipv4", mkIface("eth0", net.FlagUp), []net.Addr{mkIPNet("8.8.8.8")}, EligibilityOpts{}, false},
		{"public_ipv6", mkIface("eth0", net.FlagUp), []net.Addr{mkIPNet("2001:4860:4860::8888")}, EligibilityOpts{}, false},

		// CGNAT (100.64/10) refused without tsnet opt-in
		{"cgnat_no_optin", mkIface("utun7", net.FlagUp), []net.Addr{mkIPNet("100.64.0.5")}, EligibilityOpts{}, false},

		// CGNAT allowed when tsnet name matches
		{"cgnat_tsnet_optin", mkIface("utun7", net.FlagUp), []net.Addr{mkIPNet("100.64.0.5")}, EligibilityOpts{TsnetIfaceName: "utun7"}, true},

		// Tsnet opt-in by name wins even when the address would
		// otherwise be refused (CGNAT). The name match short-circuits
		// the address-walk.
		{"tsnet_name_match_no_addr_walk", mkIface("tailscale0", net.FlagUp), []net.Addr{mkIPNet("100.100.100.5")}, EligibilityOpts{TsnetIfaceName: "tailscale0"}, true},

		// Tsnet opt-in with WRONG iface name doesn't false-positive
		// (CGNAT address on a non-matching iface stays refused)
		{"tsnet_name_mismatch_refuses_cgnat", mkIface("utun7", net.FlagUp), []net.Addr{mkIPNet("100.64.0.5")}, EligibilityOpts{TsnetIfaceName: "tailscale0"}, false},

		// Multi-address interface: first eligible address wins
		{"multi_addr_first_private", mkIface("eth0", net.FlagUp), []net.Addr{mkIPNet("192.168.1.5"), mkIPNet("8.8.8.8")}, EligibilityOpts{}, true},

		// Multi-address with public first, private second — still
		// eligible because the walk continues
		{"multi_addr_public_then_private", mkIface("eth0", net.FlagUp), []net.Addr{mkIPNet("8.8.8.8"), mkIPNet("192.168.1.5")}, EligibilityOpts{}, true},

		// F5 fix: a pure-WAN NIC carries an fe80 link-local alongside its
		// public address. The old fe80 short-circuit wrongly classified it
		// LAN-eligible; now rejected (public present, no private).
		{"public_v4_with_linklocal", mkIface("eth0", net.FlagUp), []net.Addr{mkIPNet("8.8.8.8"), mkIPNet("fe80::1")}, EligibilityOpts{}, false},
		{"public_v6_with_linklocal", mkIface("eth0", net.FlagUp), []net.Addr{mkIPNet("2001:4860:4860::8888"), mkIPNet("fe80::1")}, EligibilityOpts{}, false},

		// Dual-stack home LAN: private IPv4 + public IPv6 (SLAAC) + fe80. Must
		// STAY eligible — "disqualify on any public IP" would break DLNA here.
		{"dualstack_private_v4_public_v6", mkIface("eth0", net.FlagUp), []net.Addr{mkIPNet("192.168.1.5"), mkIPNet("2001:4860:4860::8888"), mkIPNet("fe80::1")}, EligibilityOpts{}, true},

		// ULA (fc00::/7) is private per Go's IsPrivate — eligible.
		{"ula_ipv6", mkIface("eth0", net.FlagUp), []net.Addr{mkIPNet("fd12:3456::1")}, EligibilityOpts{}, true},

		// Link-local + private → eligible (private path).
		{"linklocal_and_private", mkIface("eth0", net.FlagUp), []net.Addr{mkIPNet("fe80::1"), mkIPNet("192.168.1.5")}, EligibilityOpts{}, true},

		// No addresses at all refused
		{"no_addrs", mkIface("eth0", net.FlagUp), nil, EligibilityOpts{}, false},

		// Loopback interface name + private addr — interface flag wins,
		// refused (defense in depth against the OS reporting a
		// misconfigured-looking interface)
		{"loopback_flag_with_private_addr", mkIface("lo0", net.FlagUp|net.FlagLoopback), []net.Addr{mkIPNet("192.168.1.5")}, EligibilityOpts{}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := IsLANEligibleInterface(tc.iface, tc.addrs, tc.opts)
			if got != tc.want {
				t.Errorf("IsLANEligibleInterface(%+v, %v, %+v) = %v, want %v",
					tc.iface, tc.addrs, tc.opts, got, tc.want)
			}
		})
	}
}
