package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// stubUPnPProvider lets the handler tests pin every branch without
// standing up the real cmd/bridge lifecycle.
type stubUPnPProvider struct {
	servers       []UPnPUpstreamServerState
	rescanCalls   atomic.Int32
	rescanLastUDN atomic.Value // string
	rescanErr     error
}

func (s *stubUPnPProvider) ConfiguredServers() []UPnPUpstreamServerState {
	return s.servers
}

func (s *stubUPnPProvider) ForceRescan(_ context.Context, udn string) error {
	s.rescanCalls.Add(1)
	s.rescanLastUDN.Store(udn)
	return s.rescanErr
}

func newTestUPnPHandler(t *testing.T, provider UPnPUpstreamProvider) *Server {
	t.Helper()
	// Construct a bare Server — only the deps the handlers actually
	// touch need to be wired (UPnPUpstream + nothing else for the
	// happy path).
	return &Server{deps: Deps{UPnPUpstream: provider}}
}

// --- GET /api/upnp/servers ---

func TestApiUPnPServers_NotWiredReturnsEnabledFalseEmptyList(t *testing.T) {
	s := newTestUPnPHandler(t, nil)
	w := httptest.NewRecorder()
	s.apiUPnPServers(w, httptest.NewRequest(http.MethodGet, "/api/upnp/servers", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", w.Code)
	}
	var resp upnpServersResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Enabled {
		t.Errorf("Enabled = true; want false")
	}
	if len(resp.Servers) != 0 {
		t.Errorf("Servers len = %d; want 0", len(resp.Servers))
	}
}

func TestApiUPnPServers_WiredEmitsConfiguredAndDiscoveryState(t *testing.T) {
	servers := []UPnPUpstreamServerState{
		{
			Name:          "Chord 2Go",
			ConfiguredUDN: "uuid:4d696e69-444c-164e-9d41-00b78f5ae46a",
			Discovered:    true,
			ResolvedUDN:   "uuid:4d696e69-444c-164e-9d41-00b78f5ae46a",
			FriendlyName:  "Chord 2Go:2go-ars",
			ControlURL:    "http://192.168.0.62:8200/ctl/ContentDir",
			LastSeenAt:    time.Unix(1_700_000_000, 0).UTC(),
			LastWalked:    15283,
			LastReaped:    0,
			RoutedTracks:  15283,
		},
	}
	s := newTestUPnPHandler(t, &stubUPnPProvider{servers: servers})
	w := httptest.NewRecorder()
	s.apiUPnPServers(w, httptest.NewRequest(http.MethodGet, "/api/upnp/servers", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var resp upnpServersResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Enabled {
		t.Errorf("Enabled = false; want true")
	}
	if len(resp.Servers) != 1 {
		t.Fatalf("Servers len = %d; want 1", len(resp.Servers))
	}
	got := resp.Servers[0]
	if got.Name != "Chord 2Go" || got.FriendlyName != "Chord 2Go:2go-ars" {
		t.Errorf("server row = %+v", got)
	}
	if got.RoutedTracks != 15283 || got.LastWalked != 15283 {
		t.Errorf("counts = walked %d, routed %d", got.LastWalked, got.RoutedTracks)
	}
	if !got.Discovered {
		t.Errorf("Discovered must be true")
	}
}

// --- POST /api/upnp/rescan ---

func TestApiUPnPRescan_NotWiredReturns404(t *testing.T) {
	s := newTestUPnPHandler(t, nil)
	w := httptest.NewRecorder()
	s.apiUPnPRescan(w, httptest.NewRequest(http.MethodPost, "/api/upnp/rescan", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d; want 404", w.Code)
	}
	if !strings.Contains(w.Body.String(), "upnp_disabled") {
		t.Errorf("body missing upnp_disabled code: %q", w.Body.String())
	}
}

func TestApiUPnPRescan_ForwardsUDNAndReturns202(t *testing.T) {
	provider := &stubUPnPProvider{}
	s := newTestUPnPHandler(t, provider)
	w := httptest.NewRecorder()
	s.apiUPnPRescan(w, httptest.NewRequest(http.MethodPost, "/api/upnp/rescan?udn=uuid:abc", nil))
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d; want 202", w.Code)
	}
	if provider.rescanCalls.Load() != 1 {
		t.Errorf("rescan calls = %d; want 1", provider.rescanCalls.Load())
	}
	if got := provider.rescanLastUDN.Load().(string); got != "uuid:abc" {
		t.Errorf("forwarded UDN = %q; want %q", got, "uuid:abc")
	}
}

func TestApiUPnPRescan_NoUDNTriggersAllServers(t *testing.T) {
	provider := &stubUPnPProvider{}
	s := newTestUPnPHandler(t, provider)
	w := httptest.NewRecorder()
	s.apiUPnPRescan(w, httptest.NewRequest(http.MethodPost, "/api/upnp/rescan", nil))
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d", w.Code)
	}
	if got := provider.rescanLastUDN.Load().(string); got != "" {
		t.Errorf("forwarded UDN = %q; want empty (all-servers)", got)
	}
}

func TestApiUPnPRescan_NoSuchServerMapsTo404(t *testing.T) {
	provider := &stubUPnPProvider{rescanErr: ErrUPnPNoSuchServer}
	s := newTestUPnPHandler(t, provider)
	w := httptest.NewRecorder()
	s.apiUPnPRescan(w, httptest.NewRequest(http.MethodPost, "/api/upnp/rescan?udn=uuid:nope", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d; want 404", w.Code)
	}
	if !strings.Contains(w.Body.String(), "no_such_server") {
		t.Errorf("body missing no_such_server: %q", w.Body.String())
	}
}

func TestApiUPnPRescan_InFlightMapsTo409(t *testing.T) {
	provider := &stubUPnPProvider{rescanErr: ErrUPnPRescanInFlight}
	s := newTestUPnPHandler(t, provider)
	w := httptest.NewRecorder()
	s.apiUPnPRescan(w, httptest.NewRequest(http.MethodPost, "/api/upnp/rescan", nil))
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d; want 409", w.Code)
	}
	if !strings.Contains(w.Body.String(), "rescan_in_flight") {
		t.Errorf("body missing rescan_in_flight: %q", w.Body.String())
	}
}

func TestApiUPnPRescan_GenericErrorMapsTo500(t *testing.T) {
	provider := &stubUPnPProvider{rescanErr: errors.New("upstream timed out")}
	s := newTestUPnPHandler(t, provider)
	w := httptest.NewRecorder()
	s.apiUPnPRescan(w, httptest.NewRequest(http.MethodPost, "/api/upnp/rescan", nil))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d; want 500", w.Code)
	}
}
