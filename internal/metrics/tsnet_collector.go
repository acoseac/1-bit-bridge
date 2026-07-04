package metrics

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

// tsnetStatusProvider is the narrow interface metrics needs from
// internal/tsnet. Defined here (not imported from internal/tsnet)
// so the metrics package retains a strictly downstream position
// in the dependency graph — internal/tsnet may import internal/metrics
// in the future, but the reverse must NEVER happen.
//
// Wire-up: `cmd/bridge/main.go` calls `metrics.RegisterTsnetProvider`
// after `tsnet.Server.Start()` returns. Before registration, the
// gauges read as zero (state=0/disabled) — the absence of a
// registered provider is itself an honest signal.
// Method names match the implementing type's `Metrics*`-prefix
// convention (tsnet.Server already has overlapping non-metrics
// methods like `Status(ctx)`); the prefix prevents accidental
// confusion and makes the "this is for the observability surface"
// intent explicit.
type tsnetStatusProvider interface {
	// MetricsState returns 0=down, 1=starting, 2=running. Disabled mode
	// is represented by "no provider registered" — the collector /
	// snapshot map a nil provider to state 3 (disabled).
	MetricsState() int
	// MetricsPeersOnline returns the count of peers currently online.
	MetricsPeersOnline() int
	// MetricsDERPLatencies returns a snapshot of per-region DERP
	// latencies in seconds. Empty map when the node isn't running
	// OR (in v1) always — DERP wiring is a follow-up.
	MetricsDERPLatencies() map[string]float64
}

var (
	tsnetProviderMu sync.RWMutex
	tsnetProvider   tsnetStatusProvider
)

// RegisterTsnetProvider installs the tsnet observability surface.
// Safe to call once at boot; subsequent calls replace the prior
// provider (e.g. for tests). Pass nil to unregister.
func RegisterTsnetProvider(p tsnetStatusProvider) {
	tsnetProviderMu.Lock()
	tsnetProvider = p
	tsnetProviderMu.Unlock()
}

// TsnetNodeStateSnapshot returns the current state value, or 3
// (disabled) when no provider is registered. Used by /v1/diagnostics.
func TsnetNodeStateSnapshot() int {
	tsnetProviderMu.RLock()
	p := tsnetProvider
	tsnetProviderMu.RUnlock()
	if p == nil {
		return 3 // no provider registered == tailscale disabled
	}
	return p.MetricsState()
}

// TsnetPeersOnlineSnapshot returns the current online-peer count.
func TsnetPeersOnlineSnapshot() int {
	tsnetProviderMu.RLock()
	p := tsnetProvider
	tsnetProviderMu.RUnlock()
	if p == nil {
		return 0
	}
	return p.MetricsPeersOnline()
}

// tsnetCollector implements prometheus.Collector so the gauges are
// computed at scrape time rather than maintained by polling. This
// keeps the read path consistent with /v1/diagnostics (both end up
// calling the same provider methods).
type tsnetCollector struct {
	nodeState   *prometheus.Desc
	peersOnline *prometheus.Desc
	derpLatency *prometheus.Desc
}

func newTsnetCollector() *tsnetCollector {
	return &tsnetCollector{
		nodeState: prometheus.NewDesc(
			"bridge_tsnet_node_state",
			"tsnet node state: 0=down, 1=starting, 2=running, 3=disabled.",
			nil, nil,
		),
		peersOnline: prometheus.NewDesc(
			"bridge_tsnet_peers_online",
			"Count of peers reported online via tsnet Status().",
			nil, nil,
		),
		derpLatency: prometheus.NewDesc(
			"bridge_tsnet_derp_latency_seconds",
			"DERP region latency as reported by the tailnet, in seconds.",
			[]string{"region"}, nil,
		),
	}
}

func (c *tsnetCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.nodeState
	ch <- c.peersOnline
	ch <- c.derpLatency
}

func (c *tsnetCollector) Collect(ch chan<- prometheus.Metric) {
	tsnetProviderMu.RLock()
	p := tsnetProvider
	tsnetProviderMu.RUnlock()
	if p == nil {
		// No provider registered == tailscale disabled → state 3 (matches
		// the metric descriptor, and lets dashboards tell an intentionally-
		// disabled node apart from a genuinely-down one).
		ch <- prometheus.MustNewConstMetric(c.nodeState, prometheus.GaugeValue, 3)
		ch <- prometheus.MustNewConstMetric(c.peersOnline, prometheus.GaugeValue, 0)
		return
	}
	// Provider methods handle their own internal timeouts (see
	// `tsnet.Server.MetricsPeersOnline`'s 1 s Status() bound).
	// A wrapper `context.WithTimeout` here would have been dead
	// code: the resulting context wasn't passed to anything, so
	// it couldn't actually cancel a hung call.
	ch <- prometheus.MustNewConstMetric(c.nodeState, prometheus.GaugeValue, float64(p.MetricsState()))
	ch <- prometheus.MustNewConstMetric(c.peersOnline, prometheus.GaugeValue, float64(p.MetricsPeersOnline()))
	for region, latency := range p.MetricsDERPLatencies() {
		ch <- prometheus.MustNewConstMetric(c.derpLatency, prometheus.GaugeValue, latency, region)
	}
}
