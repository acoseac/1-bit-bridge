package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestAutocertStatusReturnsDisabledShapeWhenNotConfigured: when
// no AutocertStatus closure is wired (loopback installs, public
// installs without autocert), the endpoint returns an empty
// snapshot so the tile renders "not configured" without a 503.
// Mirrors the apiTailscaleStatus convention.
func TestAutocertStatusReturnsDisabledShapeWhenNotConfigured(t *testing.T) {
	srv, _, _ := newTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/autocert/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status %d, want 200", resp.StatusCode)
	}
	var snap AutocertStatusSnapshot
	if err := json.NewDecoder(resp.Body).Decode(&snap); err != nil {
		t.Fatal(err)
	}
	if snap.Domain != "" {
		t.Errorf("disabled state: Domain = %q, want empty", snap.Domain)
	}
	if snap.CertPresent {
		t.Error("disabled state: CertPresent should be false")
	}
}

// TestAutocertStatusReturnsConfiguredSnapshot: when the closure
// is wired, its return is JSON-marshalled and surfaced verbatim.
func TestAutocertStatusReturnsConfiguredSnapshot(t *testing.T) {
	srv, _, _ := newTestServer(t)
	notAfter := time.Date(2027, 5, 1, 0, 0, 0, 0, time.UTC)
	lastCheck := time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)
	srv.deps.AutocertStatus = func() AutocertStatusSnapshot {
		return AutocertStatusSnapshot{
			Domain:      "bridge.example.com",
			CertPresent: true,
			NotAfter:    notAfter,
			LastError:   "",
			LastCheck:   lastCheck,
		}
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/autocert/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status %d, want 200", resp.StatusCode)
	}
	var snap AutocertStatusSnapshot
	if err := json.NewDecoder(resp.Body).Decode(&snap); err != nil {
		t.Fatal(err)
	}
	if snap.Domain != "bridge.example.com" {
		t.Errorf("Domain = %q, want bridge.example.com", snap.Domain)
	}
	if !snap.CertPresent {
		t.Error("CertPresent should be true")
	}
	if !snap.NotAfter.Equal(notAfter) {
		t.Errorf("NotAfter = %v, want %v", snap.NotAfter, notAfter)
	}
}

// TestAutocertStatusSurfacesLastError: a recent LE failure
// (DNS not propagated, rate-limited, etc.) reaches the wire so the
// admin tile can render a meaningful "last error" badge.
func TestAutocertStatusSurfacesLastError(t *testing.T) {
	srv, _, _ := newTestServer(t)
	srv.deps.AutocertStatus = func() AutocertStatusSnapshot {
		return AutocertStatusSnapshot{
			Domain:    "bridge.example.com",
			LastError: "acme: rate limited",
			LastCheck: time.Now().UTC(),
		}
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/autocert/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
	var snap AutocertStatusSnapshot
	if err := json.NewDecoder(resp.Body).Decode(&snap); err != nil {
		t.Fatal(err)
	}
	if snap.LastError == "" {
		t.Error("LastError should be populated")
	}
}
