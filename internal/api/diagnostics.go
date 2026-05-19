package api

import (
	"net/http"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/metrics"
)

// DiagnosticsResponse is the wire shape for `GET /v1/diagnostics`.
// Consumed by the iOS `BridgeHealthSection` in `DACDiagnosticsView`.
//
// **Counters + structured state only** — by deliberate design, no log
// text bodies. Including recent log lines would force per-line privacy
// redaction (paths / hostnames / track titles are all PII in some
// deployment) AND would land cross-system text in iOS-side diagnostic
// snapshots that operators might paste into a public issue without
// realizing it. The numbers convey "is the bridge healthy" without
// any of that risk; operators tail `bridge logs` separately if they
// need text.
//
// **No slow operations in the handler**: every field below reads from
// either a Prometheus atomic counter snapshot OR a sliding-window
// quantile snapshot. The handler returns in <1 ms in steady state —
// safe to share resource space with the playback API tree, which is
// the same listener.
type DiagnosticsResponse struct {
	SQLiteLockWaitP50Seconds  float64           `json:"sqliteLockWaitP50"`
	SQLiteLockWaitP99Seconds  float64           `json:"sqliteLockWaitP99"`
	MBCacheHitRatio           float64           `json:"mbCacheHitRatio"`
	UpscaleJobsInFlight       int               `json:"upscaleJobsInFlight"`
	UpscaleJobsCompletedTotal uint64            `json:"upscaleJobsCompletedTotal"`
	UpscaleDurationP50Seconds float64           `json:"upscaleDurationP50"`
	UpscaleDurationP99Seconds float64           `json:"upscaleDurationP99"`
	TailscaleNodeState        string            `json:"tailscaleNodeState"`
	TailscalePeersOnline      int               `json:"tailscalePeersOnline"`
	LogEventCounts            map[string]uint64 `json:"logEventCounts"`
	ServerUptimeSeconds       int64             `json:"serverUptime"`
}

// diagnostics renders the diagnostics summary. Wired as
// `GET /v1/diagnostics` in route_classification.go.
//
// **Uptime source**: reads `s.startedAt` — the same instance-level
// timestamp `/v1/health` returns — so the two surfaces stay
// consistent. A package-level `var serverStartedAt = time.Now()`
// would diverge when tests spin up multiple Server instances and
// would silently make the wire shape contradict itself.
func (s *Server) diagnostics(w http.ResponseWriter, r *http.Request) {
	resp := DiagnosticsResponse{
		LogEventCounts:      metrics.LogEventCountsSnapshot(),
		ServerUptimeSeconds: int64(time.Since(s.startedAt).Seconds()),
	}

	resp.SQLiteLockWaitP50Seconds, resp.SQLiteLockWaitP99Seconds =
		metrics.SQLiteLockWaitWindow.Snapshot()
	resp.UpscaleDurationP50Seconds, resp.UpscaleDurationP99Seconds =
		metrics.UpscaleDurationWindow.Snapshot()
	resp.MBCacheHitRatio = mbCacheHitRatio()

	// Upscale in-flight + completed: read through the existing
	// `UpscaleStatsProvider` interface so we don't replicate the
	// pool-snapshot indirection the /v1/upscale/stats handler
	// already routes through. The provider is nil on bridges that
	// disabled upscale; degrade to zeros rather than 5xxing —
	// diagnostics MUST stay reliable even when individual subsystems
	// are off.
	if s.upscaleStatsProvider != nil {
		if snap, err := s.upscaleStatsProvider.UpscaleStatsSnapshot(r.Context()); err == nil && snap.Pool != nil {
			resp.UpscaleJobsInFlight = snap.Pool.Inflight
			resp.UpscaleJobsCompletedTotal = snap.Pool.Done + snap.Pool.Failed
		}
	}

	resp.TailscaleNodeState = tailscaleStateString(metrics.TsnetNodeStateSnapshot())
	resp.TailscalePeersOnline = metrics.TsnetPeersOnlineSnapshot()

	writeJSON(w, http.StatusOK, resp)
}

// mbCacheHitRatio derives the hit ratio across all MusicBrainz cache
// kinds (album / artist / release_group). Returns 0 when no lookups
// have happened yet — distinguishable from "all misses" by the
// `UpscaleJobsCompletedTotal` field, which reveals whether any work
// has happened at all.
func mbCacheHitRatio() float64 {
	hits, misses := metrics.MBCacheLookupsTotals()
	denom := hits + misses
	if denom == 0 {
		return 0
	}
	return float64(hits) / float64(denom)
}

// tailscaleStateString maps the integer state from the tsnet
// provider to the wire-stable string the iOS client switches on.
// Distinct from `metrics.TsnetNodeStateSnapshot`'s integer surface
// because the iOS-side decoder reads strings, not magic numbers.
func tailscaleStateString(state int) string {
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
