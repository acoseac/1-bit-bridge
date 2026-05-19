// Package metrics is the Prometheus exposition surface for the bridge.
//
// **Decoupled from internal/logging by design**: `internal/logging` is
// the absolute base of the bridge dependency tree — almost every
// other package imports it for component-loggers. If `internal/logging`
// imported `internal/metrics` to bump the per-level counter, the
// dependency graph would close into a cycle the moment `internal/metrics`
// reached for anything upstream (manifest, transcode, tsnet, …).
//
// We solve this by inverting the dependency: `internal/metrics`
// imports `internal/logging` and calls `logging.RegisterLogHook` from
// this package's `init()`. The hook is invoked from `Handle()` BEFORE
// the cache-hit early return so the steady-state path produces
// counter increments.
//
// **Endpoint binding**: `/metrics` is mounted on the admin HTTP mux,
// which is wrapped in `loopbackOnly` middleware. Prometheus scrapers
// must run on the same host (local Prometheus / Grafana Alloy /
// node_exporter sidecar). Remote scraping over Tailnet is deferred to
// a future config knob — the current binding matches the existing
// admin-trust model.
//
// **Dual-publish to sliding windows**: latency-sensitive observations
// (SQLite lock waits, transcode durations) push to BOTH the Prometheus
// histogram (for /metrics scrapers) AND a self-owned `SlidingHistogram`
// (for /v1/diagnostics's p50/p99 snapshot). Reading quantiles out of
// `client_golang`'s histogram internals at runtime is fragile —
// the library is optimized for write-concurrency and exposes no
// clean p50/p99 query path. The sliding window is the
// /v1/diagnostics source of truth; the Prometheus histogram is for
// long-horizon Grafana queries.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	dto "github.com/prometheus/client_model/go"

	"github.com/acoseac/1-bit-bridge/internal/logging"
)

// LogEventsCounter increments on every log emission, labeled by
// level (DEBUG / INFO / WARN / ERROR). Wired via logHook at init().
var LogEventsCounter = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "bridge",
		Subsystem: "log",
		Name:      "events_total",
		Help:      "Total log events observed, partitioned by slog level.",
	},
	[]string{"level"},
)

// SQLiteLockWaitHist is the per-op SQLite transaction lock-wait
// histogram. Default buckets (5ms–10s) cover the realistic spread on
// modest-to-large libraries; revisit if real-device traces show
// clustering off the default scale.
var SQLiteLockWaitHist = promauto.NewHistogramVec(
	prometheus.HistogramOpts{
		Namespace: "bridge",
		Subsystem: "sqlite",
		Name:      "lock_wait_seconds",
		Help:      "Time spent waiting for SQLite transaction locks, partitioned by operation.",
		Buckets:   prometheus.DefBuckets,
	},
	[]string{"op"},
)

// SQLiteLockWaitWindow is the sliding-window companion to
// SQLiteLockWaitHist. /v1/diagnostics reads p50/p99 from here.
var SQLiteLockWaitWindow = NewSlidingHistogram()

// UpscaleJobsCompletedTotal counts terminal job outcomes (success /
// failure). Mirrors the existing `pool.doneCnt` / `pool.failedCnt`
// atomics so Prometheus has a label-partitioned view that /v1/upscale/stats
// doesn't surface.
var UpscaleJobsCompletedTotal = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "bridge",
		Subsystem: "upscale",
		Name:      "jobs_completed_total",
		Help:      "Total upscale jobs that reached a terminal state, partitioned by outcome.",
	},
	[]string{"outcome"},
)

// UpscaleDurationHist captures wall-clock end-to-end upscale latency
// per successful job. Dual-publishes to the sliding window for
// /v1/diagnostics's `upscaleDurationP50/P99` fields.
var UpscaleDurationHist = promauto.NewHistogram(
	prometheus.HistogramOpts{
		Namespace: "bridge",
		Subsystem: "upscale",
		Name:      "duration_seconds",
		Help:      "End-to-end upscale wall-clock duration for successful jobs.",
		Buckets: []float64{
			1, 2.5, 5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000,
		},
	},
)

// UpscaleDurationWindow is the sliding-window counterpart for the
// /v1/diagnostics quantile read.
var UpscaleDurationWindow = NewSlidingHistogram()

// MBCacheLookups counts MusicBrainz cache lookups partitioned by
// (kind, result). kind ∈ {album, artist, release_group};
// result ∈ {hit, miss}. /v1/diagnostics derives the hit ratio as
//
//	hits / (hits + misses)
//
// across all three kinds combined.
var MBCacheLookups = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "bridge",
		Subsystem: "mb_cache",
		Name:      "lookups_total",
		Help:      "MusicBrainz cache lookups, partitioned by kind and result.",
	},
	[]string{"kind", "result"},
)

// HTTPRequestsTotal labels every HTTP response by path + status.
// Wired in the existing `requestLogging` middleware tail — no
// dedicated metrics middleware needed.
var HTTPRequestsTotal = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "bridge",
		Subsystem: "http",
		Name:      "requests_total",
		Help:      "Total HTTP requests, partitioned by path and status code.",
	},
	[]string{"path", "code"},
)

// HTTPRequestDurationHist captures wall-clock latency per HTTP
// request, partitioned by path.
var HTTPRequestDurationHist = promauto.NewHistogramVec(
	prometheus.HistogramOpts{
		Namespace: "bridge",
		Subsystem: "http",
		Name:      "request_duration_seconds",
		Help:      "Total time taken to handle an HTTP request, partitioned by path.",
		Buckets:   prometheus.DefBuckets,
	},
	[]string{"path"},
)

func init() {
	// Wire the log-level counter via the logging hook so we don't
	// create a package-cycle between internal/logging and us.
	logging.RegisterLogHook(func(level string) {
		LogEventsCounter.WithLabelValues(level).Inc()
	})
	// Register the tsnet collector. It reports zeros until
	// `RegisterTsnetProvider` is called from cmd/bridge/main.go
	// after the tsnet server has Start()ed (or stays at zeros
	// permanently when tailscale.mode=disabled).
	prometheus.MustRegister(newTsnetCollector())
}

// RecordMBCache is the single chokepoint for MusicBrainz-cache hit/
// miss accounting. Keeping the three call sites (album / artist /
// release_group) uniform makes it impossible to accidentally
// instrument one branch and miss another.
func RecordMBCache(kind string, hit bool) {
	result := "miss"
	if hit {
		result = "hit"
	}
	MBCacheLookups.WithLabelValues(kind, result).Inc()
}

// MBCacheLookupsTotals returns (hits, misses) across all kinds
// combined — used by /v1/diagnostics's hit-ratio derivation. Walks
// the labels via `Write(*dto.Metric)` rather than parsing the
// exposition format, which would be both slower and structurally
// dependent on prometheus's text rendering.
func MBCacheLookupsTotals() (hits, misses uint64) {
	for _, kind := range []string{"album", "artist", "release_group"} {
		hits += counterValue(MBCacheLookups.WithLabelValues(kind, "hit"))
		misses += counterValue(MBCacheLookups.WithLabelValues(kind, "miss"))
	}
	return
}

// counterValue is the shared helper for snapshotting current
// `prometheus.Counter` values without touching package internals.
// Returns 0 on any read error (defensive — the Write path is
// guaranteed to succeed for healthy counters; failure means the
// metric isn't registered or has been concurrently torn down,
// both of which we want to surface as "no data" rather than panicking).
//
// Uses the generated proto getters (`GetCounter().GetValue()`)
// rather than direct field access — golangci-lint's `protogetter`
// rule flags `m.Counter` / `m.Counter.Value` as a CI failure,
// and the getter form is nil-safe by construction (both
// `GetCounter` and `GetValue` handle their nil receivers).
func counterValue(c prometheus.Counter) uint64 {
	var m dto.Metric
	if err := c.Write(&m); err != nil || m.GetCounter() == nil {
		return 0
	}
	return uint64(m.GetCounter().GetValue())
}

// LogEventCountsSnapshot returns the current counter values for the
// closed set {DEBUG, INFO, WARN, ERROR}. Backs /v1/diagnostics's
// `logEventCounts` field. Any other label (e.g. a misconfigured slog
// handler emitting unexpected level strings) is silently dropped to
// keep the on-the-wire map bounded.
func LogEventCountsSnapshot() map[string]uint64 {
	whitelist := []string{"DEBUG", "INFO", "WARN", "ERROR"}
	out := make(map[string]uint64, len(whitelist))
	for _, level := range whitelist {
		out[level] = counterValue(LogEventsCounter.WithLabelValues(level))
	}
	return out
}
