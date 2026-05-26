package api

import (
	"net/http"
)

// renderers handles `GET /v1/renderers` — returns the live snapshot
// of the SSDP-discovered MediaRenderer cache. Bearer-authed (the
// route registry wraps with `s.authed`).
//
// Returns 404 when discovery isn't wired (operator hasn't enabled
// `dlna.discovery.enabled`). Returns 200 with
// `{"renderers": []}` when discovery IS wired but no renderers
// are cached (cold start before first M-SEARCH cycle / empty LAN).
//
// **Endpoint vs feature-flag**: the endpoint registration in
// `routeRegistry()` is unconditional (no per-feature mux mutation),
// so a config that wires DLNA but NOT discovery would 404 at this
// path. The `rendererDiscovery` flag in `/v1/health.features`
// advertises feature availability so iOS doesn't have to probe.
//
// **No protocol version bump** — the route + the response shape
// + the feature flag are all additive on top of v1.4. Old iOS
// clients without `BridgeRendererDiscovery` (PR 6) simply ignore
// the flag + never call the endpoint.
func (s *Server) renderers(w http.ResponseWriter, _ *http.Request) {
	if s.rendererDiscovery == nil {
		// API-shaped JSON 404 (NOT http.NotFound's plain-text
		// body) so iOS-side error parsing stays consistent with
		// every other /v1 endpoint. Per CodeRabbit MINOR
		// round-1 on PR #305 + Gemini MEDIUM on the same line.
		writeError(w, http.StatusNotFound, "not_found",
			"renderer discovery is not enabled on this bridge")
		return
	}
	// Snapshot is a stable-sorted copy of the cache; cheap (RLock
	// + slice copy of typically <20 entries). The cache itself
	// stays live; we return a defensive copy.
	resp := RenderersResponse{Renderers: s.rendererDiscovery.Snapshot()}
	if resp.Renderers == nil {
		resp.Renderers = []RendererInfo{}
	}
	writeJSON(w, http.StatusOK, resp)
}
