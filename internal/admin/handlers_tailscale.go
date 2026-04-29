package admin

import (
	"net/http"
)

// --- GET /api/tailscale/status ---
//
// Returns the latest auto-pilot snapshot. Cheap (atomic load on the
// `cmd/bridge` side); the dashboard polls this every 30s, lower than
// the 3s general-stats poll because Tailscale state is slow-moving
// (mint runs once per process start + once every 24h, status doesn't
// change between).
//
// Returns the empty-zero-value status when `Tailscale` provider is
// nil so the tile can render a "not configured" pill without 503s.
// That's the same convention `apiUpdatesGet` follows for missing
// updater wiring.
func (s *Server) apiTailscaleStatus(w http.ResponseWriter, r *http.Request) {
	if s.deps.Tailscale == nil {
		writeJSON(w, http.StatusOK, TailscaleStatus{})
		return
	}
	writeJSON(w, http.StatusOK, s.deps.Tailscale.Status())
}

// --- POST /api/tailscale/refresh-cert ---
//
// Fires a detect+mint pass and returns the resulting snapshot. The
// auto-pilot rate-limits operator-triggered re-clicks via its
// internal `minMintInterval` so a panic-clicker can't blow through
// Let's Encrypt's per-domain quotas. The frontend's button-disabled
// "Minting…" state covers the common-case UX without needing a
// confirm dialog.
func (s *Server) apiTailscaleRefreshCert(w http.ResponseWriter, r *http.Request) {
	if s.deps.Tailscale == nil {
		writeError(w, http.StatusServiceUnavailable, "tailscale-not-configured",
			"Tailscale auto-pilot not wired in this build")
		return
	}
	writeJSON(w, http.StatusOK, s.deps.Tailscale.RefreshNow(r.Context()))
}
