package main

import (
	"slices"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/api"
)

// TestServeAnalysisPoolLineFollowsTheLiveGate boots the real serve with
// analysis at its default, off, switches it on and off again through PATCH
// /api/settings, and after every step asks the console's analysis stats,
// which the SSE `analysis` event carries and the Jobs card's Queue line is
// painted from, beside /v1/health (backlog B113).
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
// serve's sox probe is answered by the test (withUsableSox), so the gate
// follows the flag on any host; health must agree before the comparison.
func TestServeAnalysisPoolLineFollowsTheLiveGate(t *testing.T) {
	b := startConsoleBridge(t, "", nil, withUsableSox)
	for step, on := range []bool{false, true, false} {
		if step > 0 {
			patchSwitchLive(t, b.console, b.adminBase, "analysisEnabled", on, b.stderr)
		}
		var health api.HealthResponse
		getJSON(t, b.phone, b.apiBase+"/v1/health", "", &health)
		if got := slices.Contains(health.Features, "waveform"); got != on {
			t.Fatalf("step %d: /v1/health advertises waveform=%t with analysis.enabled at %t and "+
				"serve's sox probe answering a usable sox, so the check below would not show what "+
				"it is meant to; stderr=%s", step, got, on, b.stderr.String())
		}
		var stats struct {
			Enabled bool           `json:"enabled"`
			Pool    map[string]any `json:"pool"`
		}
		getJSON(t, b.console, b.adminBase+"/api/analysis/stats", "", &stats)
		if stats.Enabled != on {
			t.Errorf("step %d: GET /api/analysis/stats says enabled=%t while /v1/health says "+
				"analysis is %s", step, stats.Enabled, map[bool]string{true: "on", false: "off"}[on])
		}
		switch {
		case on && stats.Pool == nil:
			t.Errorf("step %d: analysis is on and GET /api/analysis/stats carries no pool, so "+
				"the Jobs card's Queue line reads \"—\" while the feature runs", step)
		case !on && stats.Pool != nil:
			t.Errorf("step %d: analysis is off and GET /api/analysis/stats carries the pool's "+
				"counters %v, so the Jobs card shows a queue line beside its off badge", step, stats.Pool)
		}
	}
}
