package api

import (
	"context"
	"net/http"
)

// UpscaleStatsProvider is the interface GET /v1/upscale/stats reads.
// Mirrors the admin tile's data sources but lives here so the api
// package doesn't import internal/admin or internal/transcode.
//
// The cmd/bridge wiring constructs an adapter that returns the same
// pool snapshot the admin handler consumes — the two surfaces stay
// in lockstep.
//
// Nil-safe: when WithUpscaleStats wasn't called (test harness, or
// builds without the upscale wiring), the handler returns the
// zero-value UpscaleStats — `enabled=false, cachedVariants=0,
// cachedBytes=0, pool=nil, soxAvailable=nil`. iOS renders that as
// "feature off" without distinguishing a missing endpoint from a
// disabled feature, which matches the /v1/health.upscaleEnabled
// contract iOS already gates on.
type UpscaleStatsProvider interface {
	// UpscaleStatsSnapshot returns the live runtime+on-disk
	// snapshot for the upscale feature. The error return surfaces
	// transient/timeout failures (notably `context.DeadlineExceeded`
	// from the /v1/upscale/stats handler's 2s ctx-timeout) so the
	// handler can emit a 5xx instead of silently returning the
	// zero-value UpscaleStats. Pre-PR-218 the signature was
	// error-less and a wedged DB just produced an all-zeros body
	// the client misread as "feature off". Gemini HIGH on PR #218.
	UpscaleStatsSnapshot(ctx context.Context) (UpscaleStats, error)
}

// UpscaleStats is the wire shape GET /v1/upscale/stats returns.
//
// Field-for-field compatible with the admin /api/upscale/stats
// payload — the JSON shapes intentionally match so an operator
// inspecting the bridge with `curl -k https://…/v1/upscale/stats`
// (with bearer token) sees the same body the Settings tile is
// already showing them. iOS uses it for the "Upscaling" management
// section inside BridgeEditorView (counts of cached / in-flight /
// failed jobs).
//
//   - Enabled mirrors live runtime state, NOT the persisted
//     `cfg.Upscale.Enabled` flag. The two diverge in two real
//     cases the admin handler documents (see CLAUDE.md / PR #110):
//     (a) startup demoted the feature when sox-precheck failed
//     even though the flag was on; (b) the operator just PATCHed
//     the flag off but the long-lived Pool is still alive until
//     restart. Both surface as `pool == nil` from the wiring
//     closure, and we report `enabled = (pool != nil)` so the
//     iOS-facing /v1/health.upscaleEnabled and this endpoint
//     agree about what "active" means.
//   - Pool is omitted when the feature is off (no pool to query).
//   - SoxAvailable is omitted when the test harness didn't wire a
//     precheck closure.
type UpscaleStats struct {
	Enabled        bool              `json:"enabled"`
	SoxAvailable   *bool             `json:"soxAvailable,omitempty"`
	Pool           *UpscalePoolStats `json:"pool,omitempty"`
	CachedVariants int               `json:"cachedVariants"`
	CachedBytes    int64             `json:"cachedBytes"`
}

// UpscalePoolStats mirrors `transcode.PoolStats` field-for-field
// but lives here so the api package compiles without importing
// internal/transcode. The wiring closure in cmd/bridge/main.go
// translates between the two value types — same indirection the
// admin package already uses.
type UpscalePoolStats struct {
	Workers  int    `json:"workers"`
	QueueCap int    `json:"queueCap"`
	QueueLen int    `json:"queueLen"`
	Inflight int    `json:"inflight"`
	Enqueued uint64 `json:"enqueued"`
	Done     uint64 `json:"done"`
	Failed   uint64 `json:"failed"`
}

// upscaleStats: GET /v1/upscale/stats
//
// Authenticated read-only snapshot of the upscale feature's
// runtime + on-disk state. Mirrors the admin /api/upscale/stats
// tile but exposed on the public protocol so paired iOS clients
// can render an "Upscaling" management section without needing
// admin auth. The wire shape is documented in docs/BridgeProtocol.md.
//
// Cheap (single SQL COUNT + a mutex-protected pool snapshot + a
// TTL-cached sox precheck — the closure dedupes against the same
// admin-side cache by sharing the precheck function reference).
// iOS calls every 5 s while the management page is foregrounded.
func (s *Server) upscaleStats(w http.ResponseWriter, r *http.Request) {
	var resp UpscaleStats
	if s.upscaleStatsProvider != nil {
		snap, err := s.upscaleStatsProvider.UpscaleStatsSnapshot(r.Context())
		if err != nil {
			// Surface DB-wedge / timeout as 503 instead of a
			// silent-zero 200. The handler is wrapped with a 2s
			// ctx-timeout (see route_classification.go); when
			// the timeout fires the underlying CountVariants
			// returns `context.DeadlineExceeded` and this branch
			// routes it through writeErrorLog. iOS treats 5xx
			// the same way it treated the old silent zero
			// (renders "feature status unavailable") but the
			// signal in operator logs is now load-bearing.
			writeErrorLog(w, r, http.StatusServiceUnavailable, "stats_unavailable",
				"upscale stats are temporarily unavailable", err)
			return
		}
		resp = snap
	}
	writeJSON(w, http.StatusOK, resp)
}
