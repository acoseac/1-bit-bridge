package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/dlna/discovery"
)

// TestUPnPUpstreamSOAPHTTPClientRefusesRedirects pins the blind-SSRF
// guard on the ContentDirectory SOAP client: the control URL is
// advertiser-supplied (SSDP description.xml on a LAN device, possibly
// rogue or spoofed), so a 3xx must be relayed verbatim — never
// followed toward loopback or link-local targets. Mirrors
// internal/upnpproxy's CheckRedirect guard.
func TestUPnPUpstreamSOAPHTTPClientRefusesRedirects(t *testing.T) {
	followed := false
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		followed = true
	}))
	defer target.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer redirector.Close()

	client := upnpUpstreamSOAPHTTPClient(2 * time.Second)
	// Both servers listen on 127.0.0.1, which the client's dial check
	// refuses unless the request's approval covers it; approve it the way a
	// server announcing from 127.0.0.1 is, so the redirect (which would be
	// followed to the same address) is what this test measures.
	req, err := http.NewRequestWithContext(
		discovery.WithAnnouncementSource(context.Background(), &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)}),
		http.MethodGet, redirector.URL, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Errorf("status = %d, want 302 relayed verbatim", resp.StatusCode)
	}
	if followed {
		t.Error("redirect target was hit — SOAP client followed a 3xx")
	}
}

// TestUpstreamDiscoveryConfigNamesTheConfiguredServers pins the wiring half
// of the upstream cache's bound: the servers the operator configured by UDN
// are the ones it never refuses, so the client on every interface must be
// built knowing them, in the spelling a device may announce (any case, and
// the config's own may carry stray whitespace). A manual-URL server needs no
// entry here: the manual poller writes past the bound itself.
func TestUpstreamDiscoveryConfigNamesTheConfiguredServers(t *testing.T) {
	upCfg := config.UPnPUpstreamConfig{Enabled: true, Servers: []config.UPnPUpstreamServerConfig{
		{Name: "Cellar", UDN: " uuid:Cellar-1 ", PathPrefix: "cellar"},
		{Name: "Attic", ManualDescriptionURL: "http://192.0.2.9:8200/rootDesc.xml", PathPrefix: "attic"},
	}}
	iface := &net.Interface{Index: 7, Name: "en7"}
	got := upstreamDiscoveryConfig(upCfg, iface)
	if got.Interface != iface || got.MSearchInterval != upCfg.EffectiveMSearchInterval() || got.ServerTTL != upCfg.EffectiveServerTTL() {
		t.Errorf("config = %+v, want the interface and the configured cadence and TTL", got)
	}
	if got.Configured == nil {
		t.Fatal("the client is built with no Configured, so the bound can refuse the operator's servers")
	}
	for udn, want := range map[string]bool{
		"uuid:cellar-1":   true,
		"UUID:CELLAR-1":   true,
		"uuid:Cellar-1":   true,
		"uuid:cellar-2":   false,
		"":                false,
		"uuid:attic":      false,
		"manual:whatever": false,
	} {
		if have := got.Configured(udn); have != want {
			t.Errorf("Configured(%q) = %v, want %v", udn, have, want)
		}
	}
}
