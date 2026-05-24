package admin

import "net/http"

// --- GET /api/autocert/status ---
//
// Returns the latest autocert state for the dashboard tile:
// configured domain, whether a cert has been minted, expiry, and
// any recent error. Cheap on the hot path (the closure on the
// cmd-side reads its own RWMutex once) — the dashboard polls
// every ~30s, the same cadence used for the Tailscale tile (state
// is slow-moving: certs renew at ~60-day intervals, errors only
// surface on failed refresh attempts).
//
// Returns the empty-zero-value snapshot when no autocert provider
// is wired (loopback installs, reverse-proxy deployments, any
// build with `autocert.enabled: false`) so the tile renders a
// clean "not configured" pill without 503s. Same convention as
// `apiTailscaleStatus`.
func (s *Server) apiAutocertStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.getAutocertSnapshot())
}

// getAutocertSnapshot is the shared path between apiAutocertStatus
// (REST) and the future SSE event publisher (PR 3 only ships the
// REST endpoint; the dashboard's existing /api/events stream can
// be extended later to push autocert state changes without
// polling).
func (s *Server) getAutocertSnapshot() AutocertStatusSnapshot {
	if s.deps.AutocertStatus == nil {
		return AutocertStatusSnapshot{}
	}
	return s.deps.AutocertStatus()
}
