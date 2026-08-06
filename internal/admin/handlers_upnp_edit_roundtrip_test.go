package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestApiUPnPServers_WireCarriesEditableFields pins the JSON keys the edit
// modal reads. app.js builds its PATCH payload from these three fields —
// `s.pathPrefix` / `s.rootObjectID` / `s.skipTopLevelContainers` — and the
// submit handler sends all of them unconditionally, so a field missing from
// this response is a field the modal silently CLEARS on save. The names must
// match UPnPServerUpdateRequest exactly; that round-trip IS the contract, and
// decoding the GET response into the PATCH type is how this test states it.
func TestApiUPnPServers_WireCarriesEditableFields(t *testing.T) {
	provider := &stubUPnPProvider{servers: []UPnPUpstreamServerState{{
		Name:                   "2Go",
		ConfiguredUDN:          "uuid:2go",
		PathPrefix:             "chord-2go",
		RootObjectID:           "0",
		SkipTopLevelContainers: []string{"System Volume Information"},
	}}}
	s := newTestUPnPHandler(t, provider)
	w := httptest.NewRecorder()
	s.apiUPnPServers(w, httptest.NewRequest(http.MethodGet, "/api/upnp/servers", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", w.Code)
	}

	var body struct {
		Servers []UPnPServerUpdateRequest `json:"servers"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Servers) != 1 {
		t.Fatalf("got %d servers, want 1", len(body.Servers))
	}
	got := body.Servers[0]
	if got.PathPrefix == nil || *got.PathPrefix != "chord-2go" {
		t.Errorf("pathPrefix absent or wrong (%v) — the modal would PATCH a blank, "+
			"moving every routed track's manifest path into a new namespace", got.PathPrefix)
	}
	if got.RootObjectID == nil || *got.RootObjectID != "0" {
		t.Errorf("rootObjectID absent or wrong (%v) — the modal would PATCH a blank "+
			"and the next walk browses the MiniDLNA default subtree", got.RootObjectID)
	}
	if got.SkipTopLevelContainers == nil ||
		len(*got.SkipTopLevelContainers) != 1 ||
		(*got.SkipTopLevelContainers)[0] != "System Volume Information" {
		t.Errorf("skipTopLevelContainers absent or wrong (%v) — the modal would PATCH "+
			"an empty list and the junk folders get walked again",
			got.SkipTopLevelContainers)
	}
}

// TestApiUPnPServers_PropagatesRequestContext — ConfiguredServers issues one
// routed-track COUNT(*) per configured upstream, so the caller's ctx must
// reach it rather than a context.Background().
func TestApiUPnPServers_PropagatesRequestContext(t *testing.T) {
	provider := &stubUPnPProvider{}
	s := newTestUPnPHandler(t, provider)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodGet, "/api/upnp/servers", nil).WithContext(ctx)
	s.apiUPnPServers(httptest.NewRecorder(), req)

	got := provider.configuredCtx.Load()
	if got == nil {
		t.Fatal("ConfiguredServers was never called")
	}
	if (*got).Err() == nil {
		t.Error("handler passed a ctx that does not observe the request's " +
			"cancellation — a disconnected client's DB queries run to completion")
	}
}

// newSourcesCtxTestServer pre-seeds the cached track counts (as the sibling
// sources tests do) so getSourcesSnapshot's other half needs no live Store —
// these cases are only about the ctx handed to ConfiguredServers.
func newSourcesCtxTestServer(provider UPnPUpstreamProvider) *Server {
	s := &Server{deps: Deps{UPnPUpstream: provider}}
	s.statsDB = statsDBPart{tracks: 100, upnpRouted: 40}
	s.statsDBValid = true
	return s
}

// TestGetSourcesSnapshot_PropagatesCallerCancellation is the F3 pin.
//
// getSourcesSnapshot documented itself as doing "no new store query", but
// ConfiguredServers runs one `SELECT COUNT(*) FROM upnp_track_routing` per
// configured upstream — and it was handed context.Background(), so on the SSE
// slow tick a disconnected client's queries ran to completion with every other
// slow-tick publisher serialized behind them.
func TestGetSourcesSnapshot_PropagatesCallerCancellation(t *testing.T) {
	provider := &stubUPnPProvider{
		servers: []UPnPUpstreamServerState{{Name: "2Go", ConfiguredUDN: "uuid:a"}},
	}
	s := newSourcesCtxTestServer(provider)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s.getSourcesSnapshot(ctx)

	got := provider.configuredCtx.Load()
	if got == nil {
		t.Fatal("ConfiguredServers was never called")
	}
	if (*got).Err() == nil {
		t.Error("the per-upstream COUNT(*) runs on a ctx that ignores the caller's " +
			"cancellation — a hung-up SSE client still pays for its queries")
	}
}

// TestGetSourcesSnapshot_BoundsTheQueryDeadline — even under a live caller ctx
// a slow query must not pin the SSE publisher past snapshotDBTimeout, matching
// the treatment the sibling snapshots already get.
func TestGetSourcesSnapshot_BoundsTheQueryDeadline(t *testing.T) {
	provider := &stubUPnPProvider{
		servers: []UPnPUpstreamServerState{{Name: "2Go", ConfiguredUDN: "uuid:a"}},
	}
	s := newSourcesCtxTestServer(provider)
	s.getSourcesSnapshot(context.Background())

	got := provider.configuredCtx.Load()
	if got == nil {
		t.Fatal("ConfiguredServers was never called")
	}
	if _, ok := (*got).Deadline(); !ok {
		t.Error("no deadline on the ConfiguredServers ctx — a slow COUNT(*) can " +
			"pin the SSE slow tick indefinitely")
	}
}
