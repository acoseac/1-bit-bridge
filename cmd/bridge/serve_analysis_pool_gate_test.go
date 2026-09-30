package main

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/api"
	"github.com/acoseac/1-bit-bridge/internal/transcode"
)

// TestServeAnalysisPoolLineFollowsTheLiveGate boots the real serve and asks
// the console's analysis stats, which the SSE `analysis` event carries and
// the Jobs card's Queue line is painted from, beside /v1/health (backlog
// B113): with analysis at its default, off, then switched on and off again
// through PATCH /api/settings; and switched on over a sox the probe does
// not find, where the card reads "degraded".
//
// The pool is built on every bridge (#781), and runServe wires
// admin.Deps.AnalysisPoolStats to answer its counters whatever the gate
// says. The console set `pool` whenever that closure was wired, so with
// analysis off the card read "off" beside "0 queued · 0 in flight · 4 done ·
// 0 failed (6 workers)" (measured on a real serve after four analyses and a
// switch-off), where the upscale tile leaves its pool out by the live gate.
// The counters must follow the gate /v1/health's `waveform` flag reads,
// which only a boot can show: every internal/admin test builds its own Deps.
//
// serve's sox probe is answered by the test, so the gate is decided by what
// the test says on any host; health must agree before each comparison.
func TestServeAnalysisPoolLineFollowsTheLiveGate(t *testing.T) {
	t.Run("switched on and off", func(t *testing.T) {
		b := startConsoleBridge(t, "", nil, withUsableSox)
		for step, on := range []bool{false, true, false} {
			if step > 0 {
				patchSwitchLive(t, b.console, b.adminBase, "analysisEnabled", on, b.stderr)
			}
			requireAnalysisPoolLineFollowsHealth(t, b, fmt.Sprintf("step %d", step), on)
		}
	})
	t.Run("switched on without a usable sox", func(t *testing.T) {
		b := startConsoleBridge(t, "analysis:\n  enabled: true\n", nil, func(o *serveOpts) {
			o.soxProbe = func(context.Context) (transcode.SoxInfo, error) {
				return transcode.SoxInfo{}, transcode.ErrSoxMissing
			}
		})
		var jobs struct {
			Analysis struct {
				Enabled bool `json:"enabled"`
			} `json:"analysis"`
		}
		getJSON(t, b.console, b.adminBase+"/api/jobs", "", &jobs)
		if !jobs.Analysis.Enabled {
			t.Fatalf("fixture broken: the Jobs card says analysis is switched off, so this case is " +
				"the first one's, not a switch left on over a missing sox")
		}
		requireAnalysisPoolLineFollowsHealth(t, b, "boot", false)
	})
}

// requireAnalysisPoolLineFollowsHealth requires /v1/health to advertise the
// waveform flag exactly when on, and then the console's analysis stats to
// say enabled=on and carry the pool's counters exactly when on.
func requireAnalysisPoolLineFollowsHealth(t *testing.T, b *consoleBridge, when string, on bool) {
	t.Helper()
	var health api.HealthResponse
	getJSON(t, b.phone, b.apiBase+"/v1/health", "", &health)
	if got := slices.Contains(health.Features, "waveform"); got != on {
		t.Fatalf("%s: /v1/health advertises waveform=%t where the test's flag and sox probe make "+
			"it %t, so the check below would not show what it is meant to; stderr=%s",
			when, got, on, b.stderr.String())
	}
	var stats struct {
		Enabled bool           `json:"enabled"`
		Pool    map[string]any `json:"pool"`
	}
	getJSON(t, b.console, b.adminBase+"/api/analysis/stats", "", &stats)
	if stats.Enabled != on {
		t.Errorf("%s: GET /api/analysis/stats says enabled=%t while /v1/health says analysis is %s",
			when, stats.Enabled, map[bool]string{true: "on", false: "off"}[on])
	}
	switch {
	case on && stats.Pool == nil:
		t.Errorf("%s: analysis is on and GET /api/analysis/stats carries no pool, so the Jobs "+
			"card's Queue line reads \"—\" while the feature runs", when)
	case !on && stats.Pool != nil:
		t.Errorf("%s: analysis is off and GET /api/analysis/stats carries the pool's counters %v, "+
			"so the Jobs card shows a queue line beside its off or degraded badge", when, stats.Pool)
	}
}
