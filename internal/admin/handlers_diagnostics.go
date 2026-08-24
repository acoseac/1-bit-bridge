// GET /api/diagnostics — the operational telemetry the bridge has always
// collected and only a paired iOS client could read.
//
// `GET /v1/diagnostics` is bearer-authed, so on a loopback install the
// operator sitting at the machine had no way to see SQLite lock waits,
// enrichment cache effectiveness, upscale durations or the tsnet peer
// count without pairing a phone to their own bridge.
//
// This reads the SAME sources as the v1 handler — package-level snapshots
// in internal/metrics — rather than calling that route. internal/admin
// must not import internal/api (the established direction; Deps carries
// ~25 closures precisely to avoid it), and metrics is a leaf that imports
// only internal/logging, so reading it directly introduces no cycle and
// no drift: there is one set of counters, not two.
package admin

import (
	"net/http"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/metrics"
)

// diagnosticsResponse is the admin wire DTO.
//
// Deliberately NOT api.DiagnosticsResponse: that type is the versioned v1
// contract an iOS client decodes, and the wire-type discipline says a
// handler must own its own shape so a change to one surface cannot
// silently alter the other. Field names differ where a clearer admin-side
// label exists; the underlying numbers are identical by construction.
type diagnosticsResponse struct {
	// Storage. The lock-wait quantiles are the single best signal for
	// "is SQLite the thing making this bridge feel slow".
	SQLiteLockWaitP50 float64 `json:"sqliteLockWaitP50"`
	SQLiteLockWaitP99 float64 `json:"sqliteLockWaitP99"`

	// Enrichment. Hit ratio across the album / artist / release-group
	// caches. Zero means "no lookups yet", which is NOT the same as "all
	// misses" — the UI says so rather than painting a 0% bar.
	MBCacheHitRatio float64 `json:"mbCacheHitRatio"`
	MBCacheLookups  uint64  `json:"mbCacheLookups"`

	// Upscale pool.
	UpscaleJobsInFlight       int     `json:"upscaleJobsInFlight"`
	UpscaleJobsCompletedTotal uint64  `json:"upscaleJobsCompletedTotal"`
	UpscaleDurationP50        float64 `json:"upscaleDurationP50"`
	UpscaleDurationP99        float64 `json:"upscaleDurationP99"`

	// Tailscale (tsnet mode only; "down" on CLI-mode and disabled
	// bridges, which is why the UI hides the row rather than reporting a
	// tailnet that isn't there).
	TailscaleNodeState   string `json:"tailscaleNodeState"`
	TailscalePeersOnline int    `json:"tailscalePeersOnline"`

	// Log events by level, since process start.
	LogEventCounts map[string]uint64 `json:"logEventCounts"`

	ServerUptimeSeconds int64 `json:"serverUptime"`
}

// apiDiagnostics handles GET /api/diagnostics.
//
// Every field reads either an atomic counter or a sliding-window quantile
// snapshot, so this returns in well under a millisecond and — unlike the
// composition and coverage snapshots on this server — needs no TTL cache.
// It touches no database.
func (s *Server) apiDiagnostics(w http.ResponseWriter, _ *http.Request) {
	resp := s.diagnosticsSnapshot()

	// No-store: these are point-in-time counters, and a browser cache hit
	// would show an operator stale numbers while they are actively
	// watching for a change.
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, resp)
}

// diagnosticsSnapshot builds the counter set.
//
// Split out of the handler so the bug-report bundle embeds the SAME numbers
// the page shows rather than reading the metrics package a second time. Two
// readers of one set of counters is fine; two assemblies of them is how the
// bundle and the page come to disagree about what the bridge reported.
func (s *Server) diagnosticsSnapshot() diagnosticsResponse {
	resp := diagnosticsResponse{
		LogEventCounts: metrics.LogEventCountsSnapshot(),
	}
	if !s.deps.StartedAt.IsZero() {
		resp.ServerUptimeSeconds = int64(time.Since(s.deps.StartedAt).Seconds())
	}

	resp.SQLiteLockWaitP50, resp.SQLiteLockWaitP99 = metrics.SQLiteLockWaitWindow.Snapshot()
	resp.UpscaleDurationP50, resp.UpscaleDurationP99 = metrics.UpscaleDurationWindow.Snapshot()

	hits, misses := metrics.MBCacheLookupsTotals()
	resp.MBCacheLookups = hits + misses
	if resp.MBCacheLookups > 0 {
		resp.MBCacheHitRatio = float64(hits) / float64(resp.MBCacheLookups)
	}

	// Upscale counters come from the pool closure this server already
	// carries, not from a second snapshot path. Nil on a bridge with
	// upscale off — degrade to zeros rather than erroring, because a
	// diagnostics surface that 5xxes when one subsystem is disabled is
	// useless exactly when someone is trying to work out what is wrong.
	if s.deps.UpscaleStats != nil {
		if snap := s.deps.UpscaleStats(); snap != nil {
			resp.UpscaleJobsInFlight = snap.Inflight
			resp.UpscaleJobsCompletedTotal = snap.Done + snap.Failed
		}
	}

	resp.TailscaleNodeState = tailscaleStateLabel(metrics.TsnetNodeStateSnapshot())
	resp.TailscalePeersOnline = metrics.TsnetPeersOnlineSnapshot()
	return resp
}

// tailscaleStateLabel maps the tsnet collector's integer state to a
// stable string.
//
// Mirrors api.tailscaleStateString rather than importing it — the two
// serve different consumers (an iOS decoder switches on the v1 strings;
// this feeds a template) and the import direction forbids sharing. The
// mapping is three lines and the values are pinned by a test on each
// side, so the duplication is cheaper than the coupling.
func tailscaleStateLabel(state int) string {
	switch state {
	case 1:
		return "starting"
	case 2:
		return "running"
	case 3:
		return "disabled"
	default:
		return "down"
	}
}

// pageDiagnostics renders the Diagnostics page shell. All values are
// filled in by app.js from /api/diagnostics — nothing is server-rendered,
// because every number here is point-in-time and a template-rendered one
// would be stale the moment the page painted.
func (s *Server) pageDiagnostics(w http.ResponseWriter, r *http.Request) {
	s.renderPage(w, r, "diagnostics", map[string]any{})
}
