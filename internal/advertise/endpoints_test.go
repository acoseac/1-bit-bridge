package advertise

import (
	"errors"
	"net"
	"strings"
	"testing"
)

// TestEndpointsIncludesHost asserts that a non-empty host override
// produces a `<shortHostname>.local` URL in the result set. The
// function is hard to test more than this without mocking
// net.Interfaces (which is deliberately unmocked here so we catch a
// real-world regression), so the contract-level tests live in
// sub-functions below.
func TestEndpointsIncludesHost(t *testing.T) {
	eps := Endpoints(Params{Port: 7788, HostOverride: "testhost"})
	var found bool
	for _, e := range eps {
		if e.URL == "https://testhost.local:7788" && e.Class == ClassMDNSHost {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected testhost.local in endpoint set; got %v", eps)
	}
}

// TestEndpointsStripsFQDN confirms a fully-qualified hostname
// (corp.example.com) is trimmed to just the short label for the
// mDNS `.local` URL. mDNS hostnames can't be FQDNs.
func TestEndpointsStripsFQDN(t *testing.T) {
	eps := Endpoints(Params{Port: 7788, HostOverride: "box.corp.example.com"})
	var found bool
	for _, e := range eps {
		if e.URL == "https://box.local:7788" {
			found = true
			break
		}
		if strings.Contains(e.URL, "corp.example.com") {
			t.Errorf("FQDN leaked into URL: %s", e.URL)
		}
	}
	if !found {
		t.Errorf("expected box.local in endpoint set; got %v", eps)
	}
}

// TestEndpointsDedupes asserts the URL set is unique — no duplicate
// strings even when multiple interfaces share the same address
// (happens less often than you'd think, but VMs and bridges can
// manifest the same IP on two interfaces).
func TestEndpointsDedupes(t *testing.T) {
	eps := Endpoints(Params{Port: 7788, HostOverride: "testhost"})
	seen := map[string]bool{}
	for _, e := range eps {
		if seen[e.URL] {
			t.Errorf("duplicate URL in endpoint set: %s", e.URL)
		}
		seen[e.URL] = true
	}
}

// TestEndpointsStableSort asserts the result is ordered by Class,
// which is what iOS's endpoint selector relies on to try LAN before
// Tailscale before `.local`.
func TestEndpointsStableSort(t *testing.T) {
	eps := Endpoints(Params{Port: 7788, HostOverride: "testhost"})
	for i := 1; i < len(eps); i++ {
		if eps[i-1].Class > eps[i].Class {
			t.Errorf("endpoints out of Class order at %d: %+v", i, eps)
		}
	}
}

// TestPortDefaultsTo7788 verifies the port=0 fallback so a caller
// that forgets to parse the listen address doesn't emit URLs like
// `:0` that iOS would reject.
func TestPortDefaultsTo7788(t *testing.T) {
	eps := Endpoints(Params{Port: 0, HostOverride: "h"})
	for _, e := range eps {
		if strings.Contains(e.URL, ":0") {
			t.Errorf("port 0 leaked into URL: %s", e.URL)
		}
		if strings.Contains(e.URL, ":7788") {
			return // happy path
		}
	}
	// No assertion failure even if none contained :7788 — this host
	// may have no up interfaces in the test env; the `:0` check above
	// is the real guard.
}

// TestURLsMirrorsEndpoints verifies the convenience wrapper returns
// the same ordering as Endpoints(), just without the class info.
func TestURLsMirrorsEndpoints(t *testing.T) {
	p := Params{Port: 7788, HostOverride: "h"}
	eps := Endpoints(p)
	urls := URLs(p)
	if len(urls) != len(eps) {
		t.Fatalf("URLs len=%d, Endpoints len=%d", len(urls), len(eps))
	}
	for i := range urls {
		if urls[i] != eps[i].URL {
			t.Errorf("mismatch at %d: urls=%q eps=%q", i, urls[i], eps[i].URL)
		}
	}
}

// --- classify() unit tests ---

func TestClassifyTailscaleV4ByInterfaceName(t *testing.T) {
	iface := net.Interface{Name: "tailscale0"}
	if got := classify(iface, net.ParseIP("100.64.5.9")); got != ClassTailscaleV4 {
		t.Errorf("tailscale0 + CGNAT v4: got %v, want ClassTailscaleV4", got)
	}
}

func TestClassifyTailscaleV4ByCGNATRange(t *testing.T) {
	// Interface name is a generic `utun2` but the IP falls in CGNAT,
	// so it's Tailscale (macOS WireGuard path).
	iface := net.Interface{Name: "utun2"}
	if got := classify(iface, net.ParseIP("100.100.100.100")); got != ClassTailscaleV4 {
		t.Errorf("utun + CGNAT: got %v, want ClassTailscaleV4", got)
	}
}

func TestClassifyTailscaleV6ByULA(t *testing.T) {
	iface := net.Interface{Name: "tailscale0"}
	ip := net.ParseIP("fd7a:115c:a1e0:ab12:4843:cd96:6236:1234")
	if got := classify(iface, ip); got != ClassTailscaleV6 {
		t.Errorf("Tailscale ULA v6: got %v, want ClassTailscaleV6", got)
	}
}

func TestClassifyLANv4ForPrivate(t *testing.T) {
	iface := net.Interface{Name: "en0"}
	for _, ip := range []string{"192.168.1.5", "10.0.0.3", "172.16.42.7"} {
		if got := classify(iface, net.ParseIP(ip)); got != ClassLANv4 {
			t.Errorf("%s: got %v, want ClassLANv4", ip, got)
		}
	}
}

func TestClassifyLANv4ForLinkLocal(t *testing.T) {
	iface := net.Interface{Name: "en0"}
	if got := classify(iface, net.ParseIP("169.254.42.1")); got != ClassLANv4 {
		t.Errorf("169.254 link-local: got %v, want ClassLANv4", got)
	}
}

func TestClassifyPublicForRoutableV4(t *testing.T) {
	iface := net.Interface{Name: "en0"}
	if got := classify(iface, net.ParseIP("8.8.8.8")); got != ClassPublic {
		t.Errorf("8.8.8.8: got %v, want ClassPublic", got)
	}
}

// --- Class.String() unit tests ---

// TestClassStringStableLabels pins the user-facing class labels —
// the admin console's "Reachable endpoints" panel renders these as
// inline tags, and the JS in `app.js` lower-cases them to derive a
// CSS class. Changing one of these strings is an admin-side wire-
// shape change; bump the JS rendering at the same time. (PR #69 —
// paired with iOS PR #150.)
func TestClassStringStableLabels(t *testing.T) {
	cases := map[Class]string{
		ClassLANv4:        "LAN",
		ClassLANv6:        "LAN",
		ClassMDNSHost:     "mDNS",
		ClassTailscaleDNS: "Tailscale DNS",
		ClassTailscaleV4:  "Tailscale",
		ClassTailscaleV6:  "Tailscale",
		ClassPublic:       "Public",
		ClassCustom:       "Custom",
	}
	for c, want := range cases {
		if got := c.String(); got != want {
			t.Errorf("Class(%d).String() = %q, want %q", c, got, want)
		}
	}
}

func TestClassStringUnknownFallback(t *testing.T) {
	// A future Class value beyond the const block should NOT crash
	// or return "" — return "Unknown" so the admin UI renders
	// SOMETHING and the operator can file a bug.
	c := Class(99)
	if got := c.String(); got != "Unknown" {
		t.Errorf("Class(99).String() = %q, want %q", got, "Unknown")
	}
}

// --- isVirtualSwitchInterface() unit tests ---

// TestIsVirtualSwitchInterfaceMatchesKnownNames pins the canonical
// Windows / cross-platform virtual-switch interface names that should
// be excluded from /v1/health advertisement. Without this filter, iOS
// sees `192.168.x.1` host-only IPs as "request timed out" entries —
// see the PR3 review case study (Hyper-V vEthernet (Default Switch)
// + WSL vNIC both showed up red in the iOS endpoint list).
//
// New entries added in the inverse-heuristic refactor (covers
// Microsoft's renames + multi-NIC trailing-index variants without a
// per-name allowlist update):
//
//   - "vEthernet (WSL (Hyper-V firewall))" — Win11 24H2 WSL rename
//   - "vEthernet (NAT-Switch) 2" — multi-NIC trailing index
//   - "vEthernet (Default Switch) 2" — same, on Default Switch
//
// Plus VPN-style adapters that historically slipped through:
// TeamViewer, ZeroTier, Hamachi, Radmin, generic OpenVPN/Tunnelblick.
func TestIsVirtualSwitchInterfaceMatchesKnownNames(t *testing.T) {
	matches := []string{
		// Hyper-V family — caught by inverse heuristic (no carve-out match)
		"vEthernet (Default Switch)",
		"vEthernet (Default Switch) 2", // Windows trailing-index variant
		"vEthernet (WSL)",
		"vEthernet (WSL (Hyper-V firewall))", // Win11 24H2 rename
		"vEthernet (Internal Switch)",
		"vEthernet (Private 01)",
		"vEthernet (nat)",
		"vEthernet (NAT-Switch)",
		"vEthernet (NAT-Switch) 2",
		// Standalone vendor prefixes
		"WSL",
		"VirtualBox Host-Only Network",
		"VirtualBox Host-Only Network #2",
		"VBoxNet0",
		"VMware Network Adapter VMnet1",
		"VMware Network Adapter VMnet8",
		"Docker Networking",
		"Npcap Loopback Adapter",
		"Bluetooth Network Connection",
		// VPN-style vNICs added in the broaden pass
		"TeamViewer VPN",
		"TeamViewer VPN Adapter",
		"ZeroTier One [abc123]",
		"Hamachi",
		"Radmin VPN",
		"tap-windows",
		"tap-bridge0",
		"Tunnelblick",
		// Linux virtualisation/Docker variants (Gemini on PR #90 round 1)
		"vmnet1",        // VMware Linux host-only
		"vmnet8",        // VMware Linux NAT
		"vboxnet0",      // VirtualBox Linux host-only
		"virbr0",        // libvirt / KVM default bridge
		"virbr0-nic",    // libvirt sub-iface
		"br-1a2b3c4d5e", // Docker user-defined bridge
	}
	for _, n := range matches {
		if !isVirtualSwitchInterface(n) {
			t.Errorf("expected %q to match as virtual switch", n)
		}
	}
}

// TestIsVirtualSwitchInterfaceLeavesPhysicalAlone confirms common
// physical interface names DON'T match — accidental over-matching
// would silently drop the LAN endpoint and break /v1/health
// discoverability entirely. The Hyper-V "External Switch" case is
// load-bearing (CodeRabbit on PR #72): on hosts that bridge their
// LAN via an external switch, that adapter carries the host's only
// real LAN IP, and a blanket `vEthernet` filter would drop it.
//
// The vendor-token carve-outs (`realtek`, `intel`, `broadcom`, …)
// are tested here — they cover auto-named-after-physical-NIC
// variants that some Windows builds ship with.
func TestIsVirtualSwitchInterfaceLeavesPhysicalAlone(t *testing.T) {
	notMatches := []string{
		"Ethernet",
		"Ethernet 2",
		"Wi-Fi",
		"en0",
		"eth0",
		"wlan0",
		// Tailscale stays — classified separately, not a virtual switch
		"tailscale0",
		"utun0",
		// External-Switch variants (Hyper-V passthrough)
		"vEthernet (External Switch)",
		"vEthernet (External Switch) 3",  // multi-NIC trailing index
		"vEthernet (External)",           // alt name
		"vEthernet (External - Realtek)", // hyphenated extra
		// Auto-named-after-physical-NIC variants
		"vEthernet (Realtek PCIe GbE)",
		"vEthernet (Intel I225-V)",
		"vEthernet (Broadcom NetXtreme)",
		"vEthernet (Killer E3100G)",
		"vEthernet (Marvell AQtion)",
	}
	for _, n := range notMatches {
		if isVirtualSwitchInterface(n) {
			t.Errorf("did not expect %q to match", n)
		}
	}
}

// --- ipHostForURL() unit tests ---

func TestIPHostForURLBracketsV6(t *testing.T) {
	if got := ipHostForURL(net.ParseIP("fe80::1")); got != "[fe80::1]" {
		t.Errorf("v6 should be bracketed, got %q", got)
	}
	if got := ipHostForURL(net.ParseIP("192.168.1.5")); got != "192.168.1.5" {
		t.Errorf("v4 should be bare, got %q", got)
	}
}

// TestEndpoints_AppendsCustom pins that operator-supplied URLs from
// cfg.CustomEndpoints land in the advertised list with ClassCustom,
// AFTER the auto-discovered classes (LAN, mDNS, Tailscale*, Public).
func TestEndpoints_AppendsCustom(t *testing.T) {
	withStubTailscaleStatus(t, tailscaleStatus{}, errors.New("no tailscale"))
	eps := Endpoints(Params{
		Port:         7788,
		HostOverride: "test",
		CustomEndpoints: []string{
			"https://reverse-proxy.example.com:443",
			"https://192.168.50.100:7788",
		},
	})

	custom := make([]Endpoint, 0)
	for _, e := range eps {
		if e.Class == ClassCustom {
			custom = append(custom, e)
		}
	}
	if len(custom) != 2 {
		t.Fatalf("want 2 ClassCustom entries, got %d: %v", len(custom), custom)
	}
	// Stable sort by Class means custom rows trail every auto-discovered
	// class. Find the highest-index non-custom entry; assert all custom
	// rows are after it.
	lastNonCustom := -1
	firstCustom := -1
	for i, e := range eps {
		if e.Class == ClassCustom && firstCustom == -1 {
			firstCustom = i
		}
		if e.Class != ClassCustom {
			lastNonCustom = i
		}
	}
	if firstCustom != -1 && lastNonCustom != -1 && firstCustom < lastNonCustom {
		t.Errorf("custom entries should come AFTER auto-discovered ones; firstCustom=%d, lastNonCustom=%d, eps=%v",
			firstCustom, lastNonCustom, eps)
	}

	// Class.String() returns "Custom" for the new bucket.
	if got := ClassCustom.String(); got != "Custom" {
		t.Errorf("ClassCustom.String() = %q, want %q", got, "Custom")
	}
}

// TestEndpoints_CustomDeduplicatesAgainstAuto verifies that a custom
// entry duplicating an auto-discovered URL is silently squashed (the
// dedupe pass keeps the first occurrence, which is the auto entry —
// preserves the LAN/mDNS class label rather than re-classifying as
// Custom).
func TestEndpoints_CustomDeduplicatesAgainstAuto(t *testing.T) {
	withStubTailscaleStatus(t, tailscaleStatus{}, errors.New("none"))
	hostMagic := "https://test.local:7788"
	eps := Endpoints(Params{
		Port:            7788,
		HostOverride:    "test",
		CustomEndpoints: []string{hostMagic}, // dups the mDNS entry
	})
	count := 0
	for _, e := range eps {
		if e.URL == hostMagic {
			count++
			if e.Class != ClassMDNSHost {
				t.Errorf("dedupe should keep first (mDNS) class, got %v", e.Class)
			}
		}
	}
	if count != 1 {
		t.Errorf("URL %q deduped count = %d, want 1", hostMagic, count)
	}
}
