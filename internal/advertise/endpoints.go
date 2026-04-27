// Package advertise enumerates every network endpoint a running bridge
// is reachable at — LAN IPv4 / IPv6, mDNS hostname, Tailscale interface
// — so /v1/health can self-report the full list to iOS clients, and the
// admin pairing QR can bake alternates into `bridge://pair?urls=...`.
//
// The enumeration is best-effort and additive: we return what's on the
// host right now, and never fail the whole function if one piece is
// unavailable (e.g. os.Hostname error). Callers that care about a
// specific interface class (Tailscale-only, LAN-only) filter the
// returned list themselves.
//
// Ordering matters — iOS picks the first candidate it can reach, so we
// rank LAN before Tailscale before `.local`, and within each class
// IPv4 before IPv6. See Endpoints() doc for the full heuristic.
package advertise

import (
	"fmt"
	"net"
	"os"
	"sort"
	"strings"
)

// Endpoint is one reachable URL for the bridge. Host is the literal
// string to put in a URL (IP, hostname); Class tells callers which
// bucket the endpoint came from so they can filter or stable-sort.
type Endpoint struct {
	URL   string
	Class Class
}

// Class ranks endpoints by expected reachability. Lower-numbered
// classes are tried first by the iOS selector.
type Class int

const (
	// ClassLANv4 is a private IPv4 address on an up, non-loopback,
	// non-Tailscale interface (10/8, 172.16/12, 192.168/16, 169.254/16
	// link-local). Usually the fastest path for same-LAN clients.
	ClassLANv4 Class = iota
	// ClassLANv6 is the IPv6 counterpart to ClassLANv4. We keep it in
	// its own bucket because iOS URLSession has been observed to
	// AAAA-only-fail on some cellular paths; IPv4 should be tried first.
	ClassLANv6
	// ClassMDNSHost is the `<shortHostname>.local:<port>` form. Useful
	// on same-LAN Bonjour clients but otherwise unreachable.
	ClassMDNSHost
	// ClassTailscaleV4 is a Tailscale CGNAT (100.64.0.0/10) IPv4. Only
	// reachable when both ends are on the same tailnet.
	ClassTailscaleV4
	// ClassTailscaleV6 is a Tailscale ULA IPv6 (fd7a:115c:a1e0::/48) —
	// Tailscale's unique local address space.
	ClassTailscaleV6
	// ClassPublic is anything non-private that isn't link-local or
	// Tailscale — an actual routable IP. Rare on a home machine; we
	// include it for completeness.
	ClassPublic
)

// String returns a stable user-facing label for the class. Used by
// the admin console's "Reachable endpoints" panel to tag each
// advertised URL ("LAN", "Tailscale", "mDNS", "Public"). Kept short
// because the admin UI renders these as inline tags next to the URL.
//
// Stable strings are part of the API for the `/admin/api/endpoints`
// JSON response — changing them is a wire-protocol-adjacent change
// (admin-only, not /v1/*). Bump the JS rendering at the same time
// if these ever change.
func (c Class) String() string {
	switch c {
	case ClassLANv4, ClassLANv6:
		return "LAN"
	case ClassMDNSHost:
		return "mDNS"
	case ClassTailscaleV4, ClassTailscaleV6:
		return "Tailscale"
	case ClassPublic:
		return "Public"
	default:
		return "Unknown"
	}
}

// Params bundles the two inputs we need: the port the bridge is
// listening on (from cfg.ListenAddress) and a hostname override
// (optional, falls back to os.Hostname). Tests pass these explicitly
// so the function is pure.
type Params struct {
	Port         int
	HostOverride string // pass "" to use os.Hostname()
}

// Endpoints returns every reachable `https://<host>:<port>` URL for
// the bridge, ordered by the Class ranking above. Deduplicates on URL
// string so a dual-addressed interface doesn't produce two identical
// entries.
//
// Never returns an error — iteration failures at any step are dropped
// and the function returns what it could enumerate. Empty return is
// possible (rare: no up non-loopback interfaces) but Callers should
// treat empty as "use the listen address directly".
func Endpoints(p Params) []Endpoint {
	port := p.Port
	if port <= 0 {
		port = 7788
	}
	out := make([]Endpoint, 0, 8)

	ifaces, err := net.Interfaces()
	if err == nil {
		for _, iface := range ifaces {
			if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
				continue
			}
			if isVirtualSwitchInterface(iface.Name) {
				// Windows Hyper-V / WSL / VirtualBox / VMware host
				// virtual switches enumerate as up interfaces with
				// 192.168.x.1 host-only IPs. iOS can't route to them,
				// and shipping them in /v1/health just produces red
				// "request timed out" rows in the device's endpoint
				// list. Drop at the source.
				continue
			}
			addrs, err := iface.Addrs()
			if err != nil {
				continue
			}
			for _, addr := range addrs {
				ip, ok := ipFromAddr(addr)
				if !ok || ip.IsLoopback() {
					continue
				}
				// Link-local addresses (IPv4 169.254/16, IPv6 fe80::/10)
				// are NOT useful across-device — IPv4 link-local only
				// appears when DHCP failed, and IPv6 fe80:: requires a
				// `%zone-id` the bridge can't emit for a remote client.
				// Shipping them in `/v1/health` just pollutes the iOS
				// selector with URLs that will always fail. mDNS still
				// advertises link-local on purpose for its own transport
				// — that's a different use case.
				if ip.IsLinkLocalUnicast() {
					continue
				}
				class := classify(iface, ip)
				out = append(out, Endpoint{
					URL:   fmt.Sprintf("https://%s:%d", ipHostForURL(ip), port),
					Class: class,
				})
			}
		}
	}

	// Hostname resolution for the mDNS `.local` form. Short-label only
	// — macOS/Linux hostnames may carry an FQDN that doesn't round-trip
	// through mDNS.
	host := p.HostOverride
	if host == "" {
		host, _ = os.Hostname()
	}
	if host != "" {
		if i := strings.IndexByte(host, '.'); i > 0 {
			host = host[:i]
		}
		out = append(out, Endpoint{
			URL:   fmt.Sprintf("https://%s.local:%d", host, port),
			Class: ClassMDNSHost,
		})
	}

	// Dedupe on URL string, preserving the first occurrence (which has
	// the lowest Class in the iteration order above).
	seen := make(map[string]bool, len(out))
	unique := make([]Endpoint, 0, len(out))
	for _, e := range out {
		if !seen[e.URL] {
			seen[e.URL] = true
			unique = append(unique, e)
		}
	}

	sort.SliceStable(unique, func(i, j int) bool {
		return unique[i].Class < unique[j].Class
	})
	return unique
}

// URLs is a tiny convenience so callers that only want the string form
// don't have to map over Endpoint themselves.
func URLs(p Params) []string {
	eps := Endpoints(p)
	out := make([]string, len(eps))
	for i, e := range eps {
		out[i] = e.URL
	}
	return out
}

// ipFromAddr peels the net.IP out of a net.Interface's Addr, tolerating
// either IPNet (most common) or IPAddr (rare).
func ipFromAddr(a net.Addr) (net.IP, bool) {
	switch v := a.(type) {
	case *net.IPNet:
		return v.IP, true
	case *net.IPAddr:
		return v.IP, true
	default:
		return nil, false
	}
}

// ipHostForURL formats an IP for embedding in a URL host-part. IPv6
// must be square-bracketed; IPv4 is just the dotted-quad. `%zone`
// suffixes on link-local IPv6 are preserved because iOS uses them.
func ipHostForURL(ip net.IP) string {
	if ip.To4() != nil {
		return ip.String()
	}
	return "[" + ip.String() + "]"
}

// classify assigns an endpoint to the right bucket. Tailscale is
// detected by a cheap two-part check: (1) the interface name starts
// with one of the known Tailscale prefixes, OR (2) the IP falls in
// Tailscale's documented address ranges (CGNAT 100.64/10 for IPv4,
// fd7a:115c:a1e0::/48 for IPv6). Either-or — some setups have
// non-standard interface names (macOS `utun<N>`), some have odd IP
// assignments (self-hosted Headscale).
func classify(iface net.Interface, ip net.IP) Class {
	if isTailscale(iface, ip) {
		if ip.To4() != nil {
			return ClassTailscaleV4
		}
		return ClassTailscaleV6
	}
	if ip.IsLinkLocalUnicast() || ip.IsPrivate() {
		if ip.To4() != nil {
			return ClassLANv4
		}
		return ClassLANv6
	}
	return ClassPublic
}

// Tailscale interface name prefixes seen in the wild:
//   - `tailscale0` — Linux default, also Windows
//   - `utun<N>` — macOS (Tailscale uses an unnumbered utun; any utun
//     with a 100.x IP is almost certainly Tailscale)
var tailscaleIfacePrefixes = []string{"tailscale", "ts"}

// cgnatV4 is the 100.64.0.0/10 block Tailscale uses for IPv4. RFC 6598.
var cgnatV4 = &net.IPNet{
	IP:   net.IPv4(100, 64, 0, 0),
	Mask: net.CIDRMask(10, 32),
}

// tsULAv6 is Tailscale's documented IPv6 ULA prefix. See
// https://tailscale.com/kb/1201/ipv6 .
var tsULAv6 = &net.IPNet{
	IP:   net.ParseIP("fd7a:115c:a1e0::"),
	Mask: net.CIDRMask(48, 128),
}

// virtualSwitchPrefixes are Windows interface-name prefixes for host
// host-only virtual switches (Hyper-V default + WSL, VirtualBox,
// VMware host-only, Docker, Npcap loopback, Bluetooth PAN). These
// are always up but their 192.168.x.1 / 172.x.x.1 IPs aren't routable
// from off-host — iOS sees them as "request timed out" rows in the
// bridge endpoint list. Match is case-insensitive prefix.
//
// **Hyper-V external switches are deliberately NOT filtered.** On
// Windows hosts that bridge their LAN through an external Hyper-V
// switch, the only physical-LAN-carrying adapter is named
// `vEthernet (External Switch)` (or similar) — a blanket `vethernet`
// prefix would drop the host's real LAN IP. We only filter the
// canonical host-only variants by their parenthesised type label
// (CodeRabbit on PR #72).
//
// References for the canonical names:
//   - "vEthernet (Default Switch)" — Hyper-V default host-only switch
//   - "vEthernet (WSL)" / "WSL" — Windows Subsystem for Linux vNIC
//   - "vEthernet (Internal)" / "vEthernet (Private)" — non-external switches
//   - "VirtualBox Host-Only Network" — VirtualBox
//   - "VMware Network Adapter VMnet*" — VMware Workstation
//   - "Docker ..." — Docker for Windows
//   - "Bluetooth Network Connection" — Bluetooth PAN
//   - "Npcap Loopback Adapter" — Wireshark
var virtualSwitchPrefixes = []string{
	"vethernet (default switch", // Hyper-V default switch
	"vethernet (wsl",            // WSL vNIC
	"vethernet (internal",       // Hyper-V internal switch
	"vethernet (private",        // Hyper-V private switch
	"vethernet (nat",            // Docker Desktop / Podman vSwitch
	"wsl",                       // WSL2 standalone vNIC
	"virtualbox",                // VirtualBox host-only
	"vmware",                    // VMware Workstation/Player
	"vbox",                      // VirtualBox alt naming
	"docker",                    // Docker for Windows
	"npcap loopback",            // Wireshark Npcap loopback adapter
	"bluetooth",                 // Bluetooth PAN connection
}

// isVirtualSwitchInterface reports whether name looks like a Windows
// host-only virtual-switch adapter that iOS will never route to. The
// match is case-insensitive prefix, lenient on purpose to catch the
// common shapes without enumerating every install variant.
func isVirtualSwitchInterface(name string) bool {
	lower := strings.ToLower(name)
	for _, p := range virtualSwitchPrefixes {
		if strings.HasPrefix(lower, p) {
			return true
		}
	}
	return false
}

func isTailscale(iface net.Interface, ip net.IP) bool {
	name := strings.ToLower(iface.Name)
	for _, p := range tailscaleIfacePrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	// macOS: utun interfaces carrying a 100.x IP are almost always
	// Tailscale (WireGuard, Cloudflare WARP also use utun but on
	// different IP ranges, so the combined check is specific).
	if strings.HasPrefix(name, "utun") && cgnatV4.Contains(ip) {
		return true
	}
	if cgnatV4.Contains(ip) {
		return true
	}
	if tsULAv6.Contains(ip) {
		return true
	}
	return false
}
