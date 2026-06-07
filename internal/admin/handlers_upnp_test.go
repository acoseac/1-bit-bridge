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
	discovered    []UPnPDiscoveredServer
	rescanCalls   atomic.Int32
	rescanLastUDN atomic.Value // string
	rescanErr     error

	// CRUD spy fields — tests assert the last call's payload + can
	// inject the next return value for each verb.
	addCalls    atomic.Int32
	addLastReq  UPnPServerAddRequest
	addErr      error
	removeCalls atomic.Int32
	removeLast  atomic.Value // string (last UDN)
	removeErr   error
	updateCalls atomic.Int32
	updateLast  UPnPServerUpdateRequest
	updateLstID atomic.Value // string (last UDN)
	updateErr   error
}

func (s *stubUPnPProvider) ConfiguredServers() []UPnPUpstreamServerState {
	return s.servers
}

func (s *stubUPnPProvider) DiscoveredServers() []UPnPDiscoveredServer {
	return s.discovered
}

func (s *stubUPnPProvider) ForceRescan(_ context.Context, udn string) error {
	s.rescanCalls.Add(1)
	s.rescanLastUDN.Store(udn)
	return s.rescanErr
}

func (s *stubUPnPProvider) AddServer(_ context.Context, req UPnPServerAddRequest) error {
	s.addCalls.Add(1)
	s.addLastReq = req
	return s.addErr
}

func (s *stubUPnPProvider) RemoveServer(_ context.Context, udn string) error {
	s.removeCalls.Add(1)
	s.removeLast.Store(udn)
	return s.removeErr
}

func (s *stubUPnPProvider) UpdateServer(_ context.Context, udn string, req UPnPServerUpdateRequest) error {
	s.updateCalls.Add(1)
	s.updateLast = req
	s.updateLstID.Store(udn)
	return s.updateErr
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

// --- GET /api/upnp/discovered ---

func TestApiUPnPDiscovered_NotWiredReturnsEnabledFalseEmpty(t *testing.T) {
	s := newTestUPnPHandler(t, nil)
	w := httptest.NewRecorder()
	s.apiUPnPDiscovered(w, httptest.NewRequest(http.MethodGet, "/api/upnp/discovered", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", w.Code)
	}
	var resp upnpDiscoveredResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Enabled {
		t.Errorf("Enabled = true; want false (no provider wired)")
	}
	if len(resp.Servers) != 0 {
		t.Errorf("Servers len = %d; want 0", len(resp.Servers))
	}
}

func TestApiUPnPDiscovered_WiredEmitsRows(t *testing.T) {
	provider := &stubUPnPProvider{discovered: []UPnPDiscoveredServer{
		{UDN: "uuid:server-a", FriendlyName: "A", LastSeenAt: time.Unix(1700, 0)},
		{UDN: "uuid:server-b", FriendlyName: "B", LastSeenAt: time.Unix(1701, 0)},
	}}
	s := newTestUPnPHandler(t, provider)
	w := httptest.NewRecorder()
	s.apiUPnPDiscovered(w, httptest.NewRequest(http.MethodGet, "/api/upnp/discovered", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", w.Code)
	}
	var resp upnpDiscoveredResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !resp.Enabled || len(resp.Servers) != 2 {
		t.Errorf("Enabled/Servers mismatch: %+v", resp)
	}
}

// --- POST /api/upnp/servers ---

func TestApiUPnPServerAdd_NotWiredReturns404(t *testing.T) {
	s := newTestUPnPHandler(t, nil)
	w := httptest.NewRecorder()
	body := strings.NewReader(`{"name":"X","udn":"uuid:abc"}`)
	s.apiUPnPServerAdd(w, httptest.NewRequest(http.MethodPost, "/api/upnp/servers", body))
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d; want 404", w.Code)
	}
	if !strings.Contains(w.Body.String(), "upnp_disabled") {
		t.Errorf("body missing upnp_disabled: %q", w.Body.String())
	}
}

func TestApiUPnPServerAdd_HappyPath(t *testing.T) {
	provider := &stubUPnPProvider{}
	s := newTestUPnPHandler(t, provider)
	w := httptest.NewRecorder()
	body := strings.NewReader(`{"name":"Chord 2Go","udn":"uuid:abc","pathPrefix":"2go"}`)
	s.apiUPnPServerAdd(w, httptest.NewRequest(http.MethodPost, "/api/upnp/servers", body))
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d; want 201; body=%s", w.Code, w.Body.String())
	}
	if provider.addCalls.Load() != 1 {
		t.Errorf("AddServer called %d times; want 1", provider.addCalls.Load())
	}
	if provider.addLastReq.Name != "Chord 2Go" || provider.addLastReq.UDN != "uuid:abc" || provider.addLastReq.PathPrefix != "2go" {
		t.Errorf("payload mismatch: %+v", provider.addLastReq)
	}
	// Response carries restartRequired:true (load-bearing for the UI).
	if !strings.Contains(w.Body.String(), `"restartRequired":true`) {
		t.Errorf("response missing restartRequired:true: %q", w.Body.String())
	}
}

func TestApiUPnPServerAdd_DuplicateMapsTo409(t *testing.T) {
	provider := &stubUPnPProvider{addErr: ErrUPnPDuplicateUDN}
	s := newTestUPnPHandler(t, provider)
	w := httptest.NewRecorder()
	body := strings.NewReader(`{"name":"X","udn":"uuid:exists"}`)
	s.apiUPnPServerAdd(w, httptest.NewRequest(http.MethodPost, "/api/upnp/servers", body))
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d; want 409", w.Code)
	}
	if !strings.Contains(w.Body.String(), "duplicate") {
		t.Errorf("body missing duplicate code: %q", w.Body.String())
	}
}

func TestApiUPnPServerAdd_ValidationMapsTo400(t *testing.T) {
	provider := &stubUPnPProvider{addErr: ErrUPnPValidation}
	s := newTestUPnPHandler(t, provider)
	w := httptest.NewRecorder()
	body := strings.NewReader(`{"name":""}`)
	s.apiUPnPServerAdd(w, httptest.NewRequest(http.MethodPost, "/api/upnp/servers", body))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400", w.Code)
	}
}

func TestApiUPnPServerAdd_BadJSONMapsTo400(t *testing.T) {
	provider := &stubUPnPProvider{}
	s := newTestUPnPHandler(t, provider)
	w := httptest.NewRecorder()
	body := strings.NewReader(`{`) // invalid JSON
	s.apiUPnPServerAdd(w, httptest.NewRequest(http.MethodPost, "/api/upnp/servers", body))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400", w.Code)
	}
}

// --- DELETE /api/upnp/servers?udn=<UDN> ---

func TestApiUPnPServerRemove_HappyPath(t *testing.T) {
	provider := &stubUPnPProvider{}
	s := newTestUPnPHandler(t, provider)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/api/upnp/servers?udn=uuid%3Aabc", nil)
	s.apiUPnPServerRemove(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200; body=%s", w.Code, w.Body.String())
	}
	if provider.removeCalls.Load() != 1 {
		t.Errorf("RemoveServer not called")
	}
	if got, _ := provider.removeLast.Load().(string); got != "uuid:abc" {
		t.Errorf("removed UDN = %q; want uuid:abc", got)
	}
}

func TestApiUPnPServerRemove_MissingUDNMapsTo400(t *testing.T) {
	provider := &stubUPnPProvider{}
	s := newTestUPnPHandler(t, provider)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/api/upnp/servers", nil)
	s.apiUPnPServerRemove(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400", w.Code)
	}
}

func TestApiUPnPServerRemove_NoSuchMapsTo404(t *testing.T) {
	provider := &stubUPnPProvider{removeErr: ErrUPnPNoSuchServer}
	s := newTestUPnPHandler(t, provider)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/api/upnp/servers?udn=uuid%3Anope", nil)
	s.apiUPnPServerRemove(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d; want 404", w.Code)
	}
	if !strings.Contains(w.Body.String(), "no_such_server") {
		t.Errorf("body missing no_such_server: %q", w.Body.String())
	}
}

// TestApiUPnPServerRemove_ManualURLAsIdentity is the Gemini HIGH
// regression guard on PR #357 round-2: the adapter accepts UDN OR
// ManualDescriptionURL as identity (the SSDP-unreachable fallback).
// With a URL-shaped identity containing `/` and `:`, a single-segment
// `{udn}` path wildcard would NEVER match after Go's net/http
// `%2F`→`/` unescape + path-clean. Query strings bypass the cleaning.
func TestApiUPnPServerRemove_ManualURLAsIdentity(t *testing.T) {
	provider := &stubUPnPProvider{}
	s := newTestUPnPHandler(t, provider)
	w := httptest.NewRecorder()
	// The manual URL flavor — URL-encode the whole thing so the
	// `:` and `/` round-trip through the query parser cleanly.
	const manualURL = "http://192.168.0.62:8200/rootDesc.xml"
	req := httptest.NewRequest(http.MethodDelete, "/api/upnp/servers?udn=http%3A%2F%2F192.168.0.62%3A8200%2FrootDesc.xml", nil)
	s.apiUPnPServerRemove(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 (manual URL must work as identity); body=%s", w.Code, w.Body.String())
	}
	if got, _ := provider.removeLast.Load().(string); got != manualURL {
		t.Errorf("removed identity = %q; want %q", got, manualURL)
	}
}

// --- PATCH /api/upnp/servers?udn=<UDN> ---

func TestApiUPnPServerUpdate_HappyPath(t *testing.T) {
	provider := &stubUPnPProvider{}
	s := newTestUPnPHandler(t, provider)
	w := httptest.NewRecorder()
	body := strings.NewReader(`{"name":"Renamed","pathPrefix":"renamed"}`)
	req := httptest.NewRequest(http.MethodPatch, "/api/upnp/servers?udn=uuid%3Aabc", body)
	s.apiUPnPServerUpdate(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200; body=%s", w.Code, w.Body.String())
	}
	if provider.updateCalls.Load() != 1 {
		t.Errorf("UpdateServer not called")
	}
	if got, _ := provider.updateLstID.Load().(string); got != "uuid:abc" {
		t.Errorf("updated UDN = %q; want uuid:abc", got)
	}
	if provider.updateLast.Name == nil || *provider.updateLast.Name != "Renamed" {
		t.Errorf("Name payload mismatch: %+v", provider.updateLast.Name)
	}
}

func TestApiUPnPServerUpdate_ValidationMapsTo400(t *testing.T) {
	provider := &stubUPnPProvider{updateErr: ErrUPnPValidation}
	s := newTestUPnPHandler(t, provider)
	w := httptest.NewRecorder()
	body := strings.NewReader(`{"name":""}`)
	req := httptest.NewRequest(http.MethodPatch, "/api/upnp/servers?udn=uuid%3Aabc", body)
	s.apiUPnPServerUpdate(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400", w.Code)
	}
}

func TestApiUPnPServerUpdate_NoSuchMapsTo404(t *testing.T) {
	provider := &stubUPnPProvider{updateErr: ErrUPnPNoSuchServer}
	s := newTestUPnPHandler(t, provider)
	w := httptest.NewRecorder()
	body := strings.NewReader(`{"name":"X"}`)
	req := httptest.NewRequest(http.MethodPatch, "/api/upnp/servers?udn=uuid%3Anope", body)
	s.apiUPnPServerUpdate(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d; want 404", w.Code)
	}
}
