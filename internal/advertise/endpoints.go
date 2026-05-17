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
	// ClassTailscaleDNS is the host's Tailscale MagicDNS name (e.g.
	// `home-pc.tailfoo.ts.net`). Ranked BEFORE the IP-based Tailscale
	// classes because iOS 26.4+ Apple Transport Security (lower-layer
	// `Network.framework` path) rejects the bridge's self-signed cert
	// when accessed via a CGNAT IP literal — even with
	// `NSAllowsLocalNetworking=true` — but accepts `*.ts.net` magic-DNS
	// hostnames cleanly. See iOS-side `BridgeEndpointSkipReason
	// .tailscaleATS` and the bridge's TLS-SAN broadening
	// (PR feat/tls-broader-sans). Tailscale-IP entries remain in the
	// list as fallback for older iOS / non-ATS clients.
	ClassTailscaleDNS
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
	// ClassCustom is an operator-supplied URL from cfg.CustomEndpoints
	// — reverse proxies, port-forwarded WAN URLs, or non-default
	// Tailscale MagicDNS names that the auto-detector didn't pick up.
	// Ranked LAST so the auto-discovered classes (which are usually
	// faster and don't depend on operator config staying in sync with
	// reality) get tried first; custom URLs are fallbacks.
	ClassCustom
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
	case ClassTailscaleDNS:
		return "Tailscale DNS"
	case ClassTailscaleV4, ClassTailscaleV6:
		return "Tailscale"
	case ClassPublic:
		return "Public"
	case ClassCustom:
		return "Custom"
	default:
		return "Unknown"
	}
}

// Params bundles the inputs the advertiser needs: the port the bridge
// is listening on (from cfg.ListenAddress), an optional hostname
// override (falls back to os.Hostname when ""), and the optional
// operator-configured CustomEndpoints list (each element is a complete
// URL string). Tests pass all three explicitly so the function is pure.
//
// TailscaleMode controls whether HOST-Tailscale enumeration is included
// in the advertised set ("cli" or "" → include; "tsnet" or "disabled"
// → skip). The host's Tailscale identity (MagicDNS hostname + tailnet
// IPs) is only useful when the bridge's LAN HTTPS listener is fronted
// by an SNI cert switcher that can serve an LE cert for the host's
// `*.ts.net` SNI — that pre-condition only holds in `cli` mode. In
// `tsnet` mode the bridge has its own embedded tailnet node and its
// own ListenTLS-bound listener (different virtual interface, different
// LE cert); advertising the host's identity instead would route iOS
// clients to the LAN listener which serves the self-signed cert and
// fails ATS verification on the `.ts.net` SNI (PR fix/inspector-…).
type Params struct {
	Port            int
	HostOverride    string   // pass "" to use os.Hostname()
	CustomEndpoints []string // pass nil/empty to skip
	TailscaleMode   string   // "cli" / "" → include host Tailscale; "tsnet" / "disabled" → skip
}

// shouldAdvertiseHostTailscale reports whether host-Tailscale identity
// (host MagicDNS hostname + host tailnet IPs from `net.Interfaces()`)
// should be in the advertised endpoint set. Empty mode preserves the
// pre-tsnet-aware behaviour (include) for back-compat with callers that
// don't pass the field yet.
func shouldAdvertiseHostTailscale(mode string) bool {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "tsnet", "disabled":
		return false
	default:
		return true
	}
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
				if !shouldAdvertiseHostTailscale(p.TailscaleMode) &&
					(class == ClassTailscaleV4 || class == ClassTailscaleV6) {
					// Host's tailnet IPs route to the LAN listener (which
					// is bound to `*:port` so it accepts on every interface
					// including the host's `utun*` Tailscale virtual one).
					// The LAN listener serves the self-signed bridge cert
					// with `<machine>.local` SANs — wrong cert for the
					// `.ts.net` SNI the iOS client sends, ATS rejects,
					// iOS surfaces "TLS error". Skip.
					continue
				}
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

	// Tailscale MagicDNS — append a `https://<host>.<tailnet>.ts.net`
	// entry when the local Tailscale CLI surfaces one. Best-effort:
	// `GetTailscaleDNSName` returns "" on every error path (Tailscale
	// not installed, not running, JSON parse failure, CLI timeout).
	// The resulting URL passes Apple ATS without re-pairing because
	// iOS allowlists `*.ts.net`; the IP-based Tailscale entries stay
	// in the list as fallback for non-ATS clients.
	//
	// Skipped in `tsnet` / `disabled` mode: the LAN listener doesn't
	// have an LE cert for the host's MagicDNS hostname, and the
	// embedded tsnet node (when configured) has its OWN identity
	// served by its OWN ListenTLS listener — not by this LAN-listener
	// advertise pass. Advertising the host MagicDNS in those modes
	// would route iOS to the LAN listener with the self-signed cert
	// and trip an ATS error on the `.ts.net` SNI.
	if shouldAdvertiseHostTailscale(p.TailscaleMode) {
		if magic := GetTailscaleDNSName(); magic != "" {
			out = append(out, Endpoint{
				URL:   fmt.Sprintf("https://%s:%d", magic, port),
				Class: ClassTailscaleDNS,
			})
		}
	}

	// Custom operator-supplied endpoints — already validated by
	// cfg.Validate (HTTPS-only, parseable). We append in input order;
	// the dedupe pass below keeps the first occurrence so a custom
	// URL that happens to match an auto-discovered one (rare) keeps
	// the auto-class label. Custom URLs are last in the Class
	// ranking — they're fallbacks, not primaries.
	for _, raw := range p.CustomEndpoints {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		out = append(out, Endpoint{
			URL:   raw,
			Class: ClassCustom,
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

// virtualSwitchPrefixes are Windows / Linux / macOS interface-name
// prefixes for host-only or VPN-style virtual adapters that iOS will
// never route to. Host-virtual switches expose `192.168.x.1` /
// `172.x.x.1` IPs that aren't reachable from off-host; commercial VPN
// vNICs (TeamViewer, ZeroTier, Hamachi) advertise IPs reachable only
// through their own coordination layer, never via direct TCP. Both
// surface as "request timed out" rows in the iOS endpoint list.
//
// Hyper-V `vEthernet (...)` adapters are NOT in this list — they're
// handled by a separate inverse heuristic in `isVirtualSwitchInterface`
// because the External-switch variant DOES carry the host's real LAN IP.
//
// Match is case-insensitive prefix, lenient on purpose to catch the
// common shapes without enumerating every install variant.
//
// References for the canonical names:
//   - "WSL" — Windows Subsystem for Linux standalone vNIC (WSL1 era)
//   - "VirtualBox Host-Only Network" — VirtualBox
//   - "VMware Network Adapter VMnet*" — VMware Workstation (Windows)
//   - "vmnet1" / "vmnet8" — VMware Workstation/Fusion on Linux/macOS
//   - "vboxnet0" — VirtualBox host-only (Linux)
//   - "virbr0" / "virbr0-nic" — libvirt / KVM bridge (Linux)
//   - "Docker ..." — Docker for Windows
//   - "br-<hash>" — Docker user-defined bridge networks (Linux)
//   - "Bluetooth Network Connection" — Bluetooth PAN
//   - "Npcap Loopback Adapter" — Wireshark
//   - "TeamViewer VPN" — TeamViewer's VPN adapter
//   - "ZeroTier One" — ZeroTier
//   - "Hamachi" — LogMeIn Hamachi
//   - "Radmin VPN" — Famatech Radmin
//   - "tap-" — generic OpenVPN/WireGuard TAP (when not Tailscale-classified)
//   - "Tunnelblick" — macOS OpenVPN GUI virtual adapter
//
// **Tailscale is intentionally absent** — Tailscale interfaces (`tailscale0`,
// `ts0`, `utun*` carrying a 100.x IP) get classified by `isTailscale()`
// as `ClassTailscale*`, NOT dropped. The bridge advertises Tailscale URLs
// because they ARE reachable when both ends are on the same tailnet.
var virtualSwitchPrefixes = []string{
	"wsl",            // WSL2 standalone vNIC (matches "WSL" and "WSL2")
	"virtualbox",     // VirtualBox host-only
	"vmware",         // VMware Workstation/Player (Windows)
	"vmnet",          // VMware Linux host-only/NAT: vmnet1, vmnet8 (Gemini on PR #90)
	"vbox",           // VirtualBox alt naming
	"vboxnet",        // VirtualBox Linux host-only: vboxnet0, vboxnet1, …
	"virbr",          // libvirt / KVM bridges: virbr0, virbr0-nic (Gemini on PR #90)
	"docker",         // Docker for Windows / Docker bridge
	"br-",            // Docker user-defined bridge networks (Linux): br-<hash>
	"npcap loopback", // Wireshark Npcap loopback adapter
	"bluetooth",      // Bluetooth PAN connection
	"teamviewer",     // TeamViewer VPN
	"zerotier",       // ZeroTier One
	"hamachi",        // LogMeIn Hamachi
	"radmin",         // Radmin VPN
	"tap-",           // OpenVPN/WireGuard TAP (when not Tailscale-classified)
	"tunnelblick",    // macOS OpenVPN GUI
}

// vEthernetPhysicalCarveOuts are case-insensitive substrings that, when
// present inside a `vEthernet (...)` interface label, indicate the
// adapter wraps a physical NIC and therefore carries the host's real
// LAN IP. Anything matched here is KEPT rather than dropped.
//
// Without this list, the inverse heuristic in `isVirtualSwitchInterface`
// would over-filter: legitimate Hyper-V external switches auto-named
// after the underlying chipset (e.g. "vEthernet (Realtek PCIe GbE)")
// would lose their LAN IP from /v1/health and break iOS discoverability
// on hosts that bridge their LAN through an external Hyper-V switch.
//
// "external" covers the canonical "External Switch" / "External (...)"
// variants. The vendor tokens cover the auto-named-after-NIC variants
// that some Windows builds ship with. The list is non-exhaustive on
// purpose — adding more chipset brands as they're observed in the wild
// is the maintenance pattern; over-broad tokens (single letters, common
// English words) are the failure mode to avoid.
var vEthernetPhysicalCarveOuts = []string{
	"external",
	"realtek",
	"intel",
	"broadcom",
	"marvell",
	"killer",
	"qualcomm",
	"atheros",
	"mediatek",
	"aquantia",
	"mellanox",
}

// isVirtualSwitchInterface reports whether name looks like a host-only
// virtual-switch / VPN-vNIC adapter that iOS will never route to.
//
// Two-stage match:
//
//  1. Hyper-V family: the inverse heuristic. Names starting with
//     `vEthernet (` are host-only by default UNLESS the parenthesised
//     label contains a token from `vEthernetPhysicalCarveOuts` (the
//     external-switch variants). This catches every `vEthernet (X)`
//     name Microsoft ships now or in the future without a per-name
//     allowlist update — including indexed variants like
//     `vEthernet (Default Switch) 2` on multi-NIC hosts. We use
//     `strings.Contains` for the carve-out check so the carve-out
//     tolerates trailing indices, parenthesised inner labels, and
//     hyphenated extras.
//
//  2. Non-Hyper-V virtual NICs: prefix-allowlist via
//     `virtualSwitchPrefixes` (TeamViewer, VMware, VirtualBox, …).
//
// Why two stages: Hyper-V's naming surface area is open-ended, but the
// "External" / vendor-named variants are a small, stable carve-out;
// inverse-with-carve-outs scales with Microsoft's ongoing renames.
// The non-Hyper-V vendors have stable product names that prefix-match
// cleanly without an inverse rule.
func isVirtualSwitchInterface(name string) bool {
	lower := strings.ToLower(name)
	if strings.HasPrefix(lower, "vethernet (") {
		for _, keep := range vEthernetPhysicalCarveOuts {
			if strings.Contains(lower, keep) {
				return false
			}
		}
		return true
	}
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
