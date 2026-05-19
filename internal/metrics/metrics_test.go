package metrics

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	dto "github.com/prometheus/client_model/go"

	"github.com/acoseac/1-bit-bridge/internal/logging"
)

// Test_LogEventsCounter_IncrementsViaLoggingHook verifies the
// decoupled hook wiring documented at the top of `metrics.go`.
// At init() the metrics package registers a hook on
// internal/logging; this test fires a slog.Default() emission
// at WARN level and confirms the counter ticked.
func Test_LogEventsCounter_IncrementsViaLoggingHook(t *testing.T) {
	// The hook fires inside `dynamicHandler.Handle` — which is what
	// `logging.Component(...)` returns. Plain `slog.Warn` goes
	// through `slog.Default()`, which `logging.Init` configures
	// as a stdlib text handler (NOT our dynamicHandler), so the
	// hook would never fire on the default path. Component loggers
	// are the production surface — every component-tagged logger
	// in the bridge codebase routes through dynamicHandler and
	// triggers the counter.
	prior := slog.Default()
	defer slog.SetDefault(prior)
	logging.Init(&bytes.Buffer{})

	component := logging.Component("metrics-test")
	before := readCounter(LogEventsCounter.WithLabelValues("WARN"))
	component.Warn("metrics-test: confirming hook wiring")
	after := readCounter(LogEventsCounter.WithLabelValues("WARN"))
	if after <= before {
		t.Fatalf("LogEventsCounter{level=WARN} did not advance: before=%v after=%v", before, after)
	}
}

// Test_LogEventCountsSnapshot_WhitelistsKnownLevels confirms the
// /v1/diagnostics-facing snapshot keeps the wire map bounded.
func Test_LogEventCountsSnapshot_WhitelistsKnownLevels(t *testing.T) {
	snap := LogEventCountsSnapshot()
	allowed := map[string]bool{"DEBUG": true, "INFO": true, "WARN": true, "ERROR": true}
	for k := range snap {
		if !allowed[k] {
			t.Errorf("unexpected key %q in LogEventCountsSnapshot — must be whitelisted to {DEBUG,INFO,WARN,ERROR}", k)
		}
	}
}

// Test_RecordMBCache_LabelsHitVsMiss confirms the helper threads
// the right `result` label per branch.
func Test_RecordMBCache_LabelsHitVsMiss(t *testing.T) {
	hitBefore := readCounter(MBCacheLookups.WithLabelValues("album", "hit"))
	missBefore := readCounter(MBCacheLookups.WithLabelValues("album", "miss"))
	RecordMBCache("album", true)
	RecordMBCache("album", false)
	hitAfter := readCounter(MBCacheLookups.WithLabelValues("album", "hit"))
	missAfter := readCounter(MBCacheLookups.WithLabelValues("album", "miss"))
	if hitAfter-hitBefore != 1 {
		t.Errorf("album/hit: want delta=1, got %v", hitAfter-hitBefore)
	}
	if missAfter-missBefore != 1 {
		t.Errorf("album/miss: want delta=1, got %v", missAfter-missBefore)
	}
}

// Test_MetricsEndpointExposes_BridgeMetricFamilies validates that
// `promhttp.Handler()` (the same handler mounted on the admin mux)
// renders our registered families with their declared names.
func Test_MetricsEndpointExposes_BridgeMetricFamilies(t *testing.T) {
	// Touch every label-vector family at least once — Prometheus
	// only renders rows that have been observed at least once, so
	// the registry would silently omit our families if no call
	// site has ticked them yet.
	LogEventsCounter.WithLabelValues("INFO").Inc()
	RecordMBCache("artist", true)
	SQLiteLockWaitHist.WithLabelValues("test_op").Observe(0.001)
	UpscaleJobsCompletedTotal.WithLabelValues("success").Inc()
	HTTPRequestsTotal.WithLabelValues("/test", "200").Inc()
	HTTPRequestDurationHist.WithLabelValues("/test").Observe(0.005)
	UpscaleDurationHist.Observe(1.0)

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	promhttp.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("/metrics returned %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, name := range []string{
		"bridge_log_events_total",
		"bridge_mb_cache_lookups_total",
		"bridge_sqlite_lock_wait_seconds",
		"bridge_tsnet_node_state",
		"bridge_upscale_jobs_completed_total",
		"bridge_http_requests_total",
	} {
		if !strings.Contains(body, name) {
			t.Errorf("/metrics missing expected family %q", name)
		}
	}
}

// readCounter is a small helper that extracts the current value of
// a Prometheus counter without importing the dto package at every
// test site.
func readCounter(c interface {
	Write(*dto.Metric) error
}) float64 {
	var m dto.Metric
	if err := c.Write(&m); err != nil || m.Counter == nil || m.Counter.Value == nil {
		return 0
	}
	return *m.Counter.Value
}
