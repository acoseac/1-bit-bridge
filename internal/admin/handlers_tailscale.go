package admin

import (
	"context"
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
	writeJSON(w, http.StatusOK, s.getTailscaleSnapshot(r.Context()))
}

// tailscaleStatusContext is a Tailscale provider whose status read waits
// on its caller's context too (serve's, which asks the embedded node).
type tailscaleStatusContext interface {
	StatusContext(ctx context.Context) TailscaleStatus
}

// getTailscaleSnapshot returns the latest auto-pilot status, or the
// empty-zero-value when no Tailscale provider is wired (test
// deployments, builds without the auto-pilot). Shared by
// apiTailscaleStatus (REST) and apiEvents (SSE), each with its request's
// context, which a provider that reads on a context waits on: an event
// stream's ends at its socket's retirement and at the shutdown (backlog
// B989). Never RefreshNow, which mints.
func (s *Server) getTailscaleSnapshot(ctx context.Context) TailscaleStatus {
	if s.deps.Tailscale == nil {
		return TailscaleStatus{}
	}
	if p, ok := s.deps.Tailscale.(tailscaleStatusContext); ok {
		return p.StatusContext(ctx)
	}
	return s.deps.Tailscale.Status()
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
