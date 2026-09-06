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
	"context"
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

	// Database file accounting. An operator cannot sensibly decide
	// whether to compact a file whose size they have never seen, which is
	// why this is here and not only behind the button.
	//
	// DatabaseFreePageBytes is a FLOOR on what a compaction would return,
	// not an estimate of it — see manifest.PageStats.FreePageBytes, where
	// the measurement lives. Scattered deletion can leave it at ZERO on a
	// database a VACUUM would halve, so the browser must never render a
	// zero here as "nothing to reclaim".
	DatabaseBytes         int64 `json:"databaseBytes"`
	DatabaseFreePageBytes int64 `json:"databaseFreePageBytes"`
	// DatabaseStatsAvailable distinguishes "the file is empty" from "the
	// PRAGMAs failed", exactly as RetentionCountsAvailable does below.
	// The flag landed on that block and not this one because that is the
	// block a review happened to look at; the argument is identical.
	DatabaseStatsAvailable bool `json:"databaseStatsAvailable"`

	// Retention. Both tables grow without a bound unless the operator
	// sets a window, and until now there was no way to see either size.
	// Showing the number is what makes "keep everything" a decision
	// rather than something inherited — and most operators, shown it,
	// will correctly choose to keep everything.
	PlaybackHistoryRows     int64  `json:"playbackHistoryRows"`
	DeviceRegistrationRows  int64  `json:"deviceRegistrationRows"`
	OldestPlaybackStartedAt string `json:"oldestPlaybackStartedAt,omitempty"` // RFC3339; omitted when the table is empty
	// RetentionCountsAvailable distinguishes "the tables are empty" from
	// "the query failed". Without it a failed read renders as a bridge
	// that has never recorded anything — a confident wrong answer, and
	// the worse of the two by far. (CodeRabbit, PR #829.)
	RetentionCountsAvailable bool `json:"retentionCountsAvailable"`

	ServerUptimeSeconds int64 `json:"serverUptime"`
}

// apiDiagnostics handles GET /api/diagnostics.
//
// MOST fields read an atomic counter or a sliding-window quantile
// snapshot and cost nothing. The database block does NOT: it runs three
// PRAGMAs (microseconds, genuinely free) plus
// `SELECT COUNT(*), COALESCE(MIN(started_at), 0) FROM playback_history`
// and a second COUNT. There is no index on `started_at` — deliberately,
// see the schema — so the MIN turns a covering-index count into a full
// table SCAN. Measured against the real driver and schema:
//
//	rows      COUNT(*)+MIN   COUNT(*) alone   3 PRAGMAs
//	18,000        1.02 ms          55 µs         7 µs
//	90,000        9.02 ms         748 µs         8 µs
//	500,000      39.55 ms        5.51 ms        12 µs
//
// The page polls this every 5 s while its tab is visible, so the block
// sits behind databaseStatsTTL — the rule this server already follows for
// its composition and coverage snapshots. This docblock said "It touches
// no database" for four days after that stopped being true, with the
// body's own "Three PRAGMAs" and "Two COUNTs and a MIN" comments directly
// beneath it, and app.js carried a second copy of the same false claim as
// the stated JUSTIFICATION for the 5 s poll. That is how the next
// database read gets added here.
func (s *Server) apiDiagnostics(w http.ResponseWriter, r *http.Request) {
	resp := s.diagnosticsSnapshot(r.Context())

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
func (s *Server) diagnosticsSnapshot(ctx context.Context) diagnosticsResponse {
	resp := diagnosticsResponse{
		LogEventCounts: metrics.LogEventCountsSnapshot(),
	}
	if !s.deps.StartedAt.IsZero() {
		resp.ServerUptimeSeconds = int64(time.Since(s.deps.StartedAt).Seconds())
	}

	resp.SQLiteLockWaitP50, resp.SQLiteLockWaitP99 = metrics.SQLiteLockWaitWindow.Snapshot()

	// The only database work on this handler, and the only part of it
	// that is not free — see the docblock's measurements. Behind a TTL,
	// which is what lets the 5 s poll stay a poll.
	db := s.databaseStats(ctx)
	resp.DatabaseStatsAvailable = db.statsOK
	resp.DatabaseBytes = db.fileBytes
	resp.DatabaseFreePageBytes = db.freePageBytes
	resp.RetentionCountsAvailable = db.countsOK
	resp.PlaybackHistoryRows = db.historyRows
	resp.DeviceRegistrationRows = db.registrationRows
	resp.OldestPlaybackStartedAt = db.oldestPlayback
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

// databaseStatsTTL bounds how stale the database block on
// GET /api/diagnostics may be.
//
// The page polls every 5 s, so this cuts the query rate by three while
// staying well inside "live" for numbers that move as slowly as a row
// count and a file size. It is not a guess at an acceptable cost: it is
// the rule this server already applies to its composition and coverage
// snapshots, applied to the block that turned this handler into a
// database reader.
//
// Compaction invalidates it explicitly, because that is the one moment an
// operator is watching for the number to change.
const databaseStatsTTL = 15 * time.Second

// databaseStatsSnapshot is the cached half of the diagnostics response.
// Unexported and unTAGGED: it is not a wire type, and the handler copies
// each field into the DTO it owns.
type databaseStatsSnapshot struct {
	statsOK          bool
	fileBytes        int64
	freePageBytes    int64
	countsOK         bool
	historyRows      int64
	registrationRows int64
	oldestPlayback   string
}

// databaseStats returns the cached database block, recomputing it at most
// once per databaseStatsTTL.
//
// The mutex is held ACROSS the recompute, which is the single-flight: a
// second caller arriving mid-query waits and then finds the fresh entry
// rather than issuing its own full table scan. That is the whole point on
// a 5 s poll with several tabs open. It costs the second caller the query
// duration, which is bounded by the same measurements the TTL is sized
// from.
//
// Every failure degrades to a zero value with its availability flag
// false, never to an error: a diagnostics surface that 5xxes when one
// subsystem is unavailable is useless exactly when someone needs it, and
// the flags are what stop a failed read from rendering as a bridge with
// an empty database that has never recorded anything.
func (s *Server) databaseStats(ctx context.Context) databaseStatsSnapshot {
	s.dbStatsMu.Lock()
	defer s.dbStatsMu.Unlock()
	if s.dbStats != nil && time.Since(s.dbStatsAt) < databaseStatsTTL {
		return *s.dbStats
	}
	var snap databaseStatsSnapshot
	if s.deps.Manifest != nil {
		if ps, err := s.deps.Manifest.PageStats(ctx); err == nil {
			snap.statsOK = true
			snap.fileBytes = ps.FileBytes
			snap.freePageBytes = ps.FreePageBytes
		}
		if rc, err := s.deps.Manifest.RetentionCounts(ctx); err == nil {
			snap.countsOK = true
			snap.historyRows = rc.PlaybackHistoryRows
			snap.registrationRows = rc.DeviceRegistrationRows
			// Gate on the ROW COUNT, not on the timestamp: the count is
			// the direct answer to "is this table empty", while a
			// timestamp of 0 would also be produced by a clock-skewed row
			// (the ingest validator refuses non-positive startedAt, so
			// that should be unreachable — but reading the count is both
			// more direct and unreachable-proof). The stamp guard stays so
			// a 0 can never render as 1970. (Gemini MEDIUM, PR #829.)
			if rc.PlaybackHistoryRows > 0 && rc.OldestPlaybackStartedAt > 0 {
				snap.oldestPlayback = time.Unix(0, rc.OldestPlaybackStartedAt).
					UTC().Format(time.RFC3339)
			}
		}
	}
	s.dbStats = &snap
	s.dbStatsAt = time.Now()
	return snap
}

// invalidateDatabaseStats drops the cached block so the next read is
// fresh. Called after a compaction: that is the one moment an operator is
// watching the number, and a TTL-stale "nothing changed" there would read
// as a button that did nothing.
func (s *Server) invalidateDatabaseStats() {
	s.dbStatsMu.Lock()
	s.dbStats = nil
	s.dbStatsMu.Unlock()
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
