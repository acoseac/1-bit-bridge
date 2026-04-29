package admin

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

// fakeTailscale is a TailscaleProvider stub for handler tests. Records
// RefreshNow invocations so we can assert the operator's "Re-mint cert"
// click reaches the auto-pilot.
type fakeTailscale struct {
	status        TailscaleStatus
	refreshCount  atomic.Int32
	refreshResult TailscaleStatus
}

func (f *fakeTailscale) Status() TailscaleStatus { return f.status }
func (f *fakeTailscale) RefreshNow(_ context.Context) TailscaleStatus {
	f.refreshCount.Add(1)
	return f.refreshResult
}

// --- GET /api/tailscale/status ---

func TestApiTailscaleStatus_NotConfiguredReturnsZero(t *testing.T) {
	// `Tailscale` Deps field is nil — the handler returns the
	// zero-value TailscaleStatus rather than 503 so the dashboard's
	// JS tile can render "not configured" / hidden without a network
	// error. Same convention apiUpdatesGet follows for missing
	// updater wiring.
	srv, _, _ := newTestServer(t)
	var s TailscaleStatus
	code := doJSON(t, srv.Handler(), "GET", "/api/tailscale/status", nil, &s)
	if code != http.StatusOK {
		t.Fatalf("status: %d, want 200 (not-configured = empty 200, not 503)", code)
	}
	if s.CLIAvailable {
		t.Errorf("CLIAvailable = true on a server without Tailscale wiring")
	}
}

func TestApiTailscaleStatus_HappyPathReturnsSnapshot(t *testing.T) {
	srv, _, _ := newTestServer(t)
	stub := &fakeTailscale{
		status: TailscaleStatus{
			CLIAvailable:      true,
			NodeName:          "home-pc",
			MagicDNSName:      "home-pc.sable-eagle.ts.net",
			HTTPSCertsEnabled: true,
			CertPresent:       true,
			CertNotAfter:      time.Now().Add(60 * 24 * time.Hour),
			CertPath:          "/data/tls/tailscale.crt",
			LastChecked:       time.Now(),
		},
	}
	srv.deps.Tailscale = stub

	var got TailscaleStatus
	code := doJSON(t, srv.Handler(), "GET", "/api/tailscale/status", nil, &got)
	if code != http.StatusOK {
		t.Fatalf("status: %d, want 200", code)
	}
	if got.MagicDNSName != "home-pc.sable-eagle.ts.net" {
		t.Errorf("MagicDNSName = %q, want round-trip from stub", got.MagicDNSName)
	}
	if !got.CertPresent {
		t.Errorf("CertPresent = false, want true (stub said so)")
	}
	if got.CertPath != "/data/tls/tailscale.crt" {
		t.Errorf("CertPath = %q, want round-trip (admin tile reads this for the tooltip)", got.CertPath)
	}
}

// --- POST /api/tailscale/refresh-cert ---

func TestApiTailscaleRefresh_NotConfiguredReturns503(t *testing.T) {
	// Re-mint without an auto-pilot wired is operator error — surface
	// 503 so the JS alert("Re-mint failed: …") shows a clear
	// not-configured hint rather than silently no-op.
	srv, _, _ := newTestServer(t)
	code := doJSON(t, srv.Handler(), "POST", "/api/tailscale/refresh-cert", nil, nil)
	if code != http.StatusServiceUnavailable {
		t.Errorf("not-configured POST = %d, want 503", code)
	}
}

func TestApiTailscaleRefresh_RoutesThroughProvider(t *testing.T) {
	srv, _, _ := newTestServer(t)
	stub := &fakeTailscale{
		refreshResult: TailscaleStatus{
			CLIAvailable: true,
			MagicDNSName: "home-pc.sable-eagle.ts.net",
			CertPresent:  true,
			CertNotAfter: time.Now().Add(89 * 24 * time.Hour),
		},
	}
	srv.deps.Tailscale = stub

	var got TailscaleStatus
	code := doJSON(t, srv.Handler(), "POST", "/api/tailscale/refresh-cert", nil, &got)
	if code != http.StatusOK {
		t.Fatalf("refresh: %d, want 200", code)
	}
	if stub.refreshCount.Load() != 1 {
		t.Errorf("RefreshNow called %d times, want 1", stub.refreshCount.Load())
	}
	if !got.CertPresent {
		t.Errorf("CertPresent = false, want true (stub returned the freshly-minted cert)")
	}
}
