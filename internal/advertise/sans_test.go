package advertise

import (
	"errors"
	"net"
	"testing"
)

// TestGatherCertSANIPs_FromCustomEndpoints verifies IP-literal hosts in
// CustomEndpoints land in the IP gather (not the DNS gather).
func TestGatherCertSANIPs_FromCustomEndpoints(t *testing.T) {
	withStubTailscaleStatus(t, tailscaleStatus{}, errors.New("not running"))

	got := GatherCertSANIPs(CertSANConfig{
		CustomEndpoints: []string{
			"https://192.168.50.10:7788",
			"https://[fe80::1]:7788",
			"https://my-bridge.example.com:7788", // hostname — should NOT appear
		},
	})

	want4 := net.ParseIP("192.168.50.10")
	want6 := net.ParseIP("fe80::1")
	have4, have6 := false, false
	for _, ip := range got {
		if ip.Equal(want4) {
			have4 = true
		}
		if ip.Equal(want6) {
			have6 = true
		}
	}
	if !have4 || !have6 {
		t.Errorf("missing IP-literal customs: 192.168.50.10=%v fe80::1=%v; got %v",
			have4, have6, got)
	}
}

// TestGatherCertSANIPs_TakesTailscaleIPs verifies Tailscale-supplied
// IPs from `tailscale status --json` land in the gather.
func TestGatherCertSANIPs_TakesTailscaleIPs(t *testing.T) {
	withStubTailscaleStatus(t, tailscaleStatus{
		Self: struct {
			DNSName      string   `json:"DNSName"`
			TailscaleIPs []string `json:"TailscaleIPs"`
		}{TailscaleIPs: []string{"100.91.73.88"}},
	}, nil)

	got := GatherCertSANIPs(CertSANConfig{})
	want := net.ParseIP("100.91.73.88")
	found := false
	for _, ip := range got {
		if ip.Equal(want) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("Tailscale IP missing from gather: got %v", got)
	}
}

// TestGatherCertSANIPs_DropsLoopbackAndLinkLocal verifies the gather
// skips loopback (TLS template adds them unconditionally) and link-
// local (not useful across-device).
func TestGatherCertSANIPs_DropsLoopbackAndLinkLocal(t *testing.T) {
	withStubTailscaleStatus(t, tailscaleStatus{}, errors.New("none"))

	got := GatherCertSANIPs(CertSANConfig{})
	for _, ip := range got {
		if ip.IsLoopback() {
			t.Errorf("loopback leaked into gather: %v", ip)
		}
		if ip.IsLinkLocalUnicast() {
			t.Errorf("link-local leaked into gather: %v", ip)
		}
	}
}

// TestGatherCertSANDNS_FromCustomEndpoints verifies hostname-shaped
// custom endpoints land in the DNS gather (and IP-literals do NOT).
func TestGatherCertSANDNS_FromCustomEndpoints(t *testing.T) {
	withStubTailscaleStatus(t, tailscaleStatus{}, errors.New("none"))

	got := GatherCertSANDNS(CertSANConfig{
		CustomEndpoints: []string{
			"https://my-bridge.example.com:7788",
			"https://reverse-proxy.acme.io",
			"https://192.168.50.10:7788", // IP literal — must be skipped
		},
	})

	wantHosts := map[string]bool{
		"my-bridge.example.com": false,
		"reverse-proxy.acme.io": false,
	}
	for _, h := range got {
		if _, ok := wantHosts[h]; ok {
			wantHosts[h] = true
		}
		if h == "192.168.50.10" {
			t.Errorf("IP literal leaked into DNS gather: %v", got)
		}
	}
	for host, found := range wantHosts {
		if !found {
			t.Errorf("DNS gather missing %q; got %v", host, got)
		}
	}
}

// TestGatherCertSANDNS_TakesTailscaleMagicDNS verifies Tailscale's
// MagicDNS name appears in the DNS gather.
func TestGatherCertSANDNS_TakesTailscaleMagicDNS(t *testing.T) {
	withStubTailscaleStatus(t, tailscaleStatus{
		Self: struct {
			DNSName      string   `json:"DNSName"`
			TailscaleIPs []string `json:"TailscaleIPs"`
		}{DNSName: "home-pc.tailfoo.ts.net"},
	}, nil)

	got := GatherCertSANDNS(CertSANConfig{})
	found := false
	for _, h := range got {
		if h == "home-pc.tailfoo.ts.net" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("Tailscale MagicDNS missing from gather: got %v", got)
	}
}

// TestGatherCertSANDNS_DedupesCaseInsensitive verifies the same name
// at different casings collapses to a single entry.
func TestGatherCertSANDNS_DedupesCaseInsensitive(t *testing.T) {
	withStubTailscaleStatus(t, tailscaleStatus{}, errors.New("none"))

	got := GatherCertSANDNS(CertSANConfig{
		CustomEndpoints: []string{
			"https://MyBridge.example.com",
			"https://mybridge.example.com",
		},
	})
	count := 0
	for _, h := range got {
		if h == "MyBridge.example.com" || h == "mybridge.example.com" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected case-insensitive dedupe, got %d entries: %v", count, got)
	}
}
