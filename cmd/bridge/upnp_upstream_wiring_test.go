package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
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
	resp, err := client.Get(redirector.URL)
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
