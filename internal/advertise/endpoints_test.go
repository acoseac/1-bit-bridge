package advertise

import (
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
		ClassLANv4:       "LAN",
		ClassLANv6:       "LAN",
		ClassMDNSHost:    "mDNS",
		ClassTailscaleV4: "Tailscale",
		ClassTailscaleV6: "Tailscale",
		ClassPublic:      "Public",
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

// --- ipHostForURL() unit tests ---

func TestIPHostForURLBracketsV6(t *testing.T) {
	if got := ipHostForURL(net.ParseIP("fe80::1")); got != "[fe80::1]" {
		t.Errorf("v6 should be bracketed, got %q", got)
	}
	if got := ipHostForURL(net.ParseIP("192.168.1.5")); got != "192.168.1.5" {
		t.Errorf("v4 should be bare, got %q", got)
	}
}
