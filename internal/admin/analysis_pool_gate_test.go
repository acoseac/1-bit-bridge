package admin

import (
	"net/http"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/config"
)

// TestTheAnalysisPoolLineFollowsTheGate pins that GET /api/analysis/stats,
// the payload of the SSE `analysis` event and the source of the Jobs card's
// Queue line, carries the analysis pool's counters only while the live gate
// is open, as the upscale tile's stats do.
//
// cmd/bridge builds the pool on every bridge (#781) and wires
// Deps.AnalysisPoolStats to answer its counters whatever the gate says, as
// here. The handler set `pool` whenever that closure was wired, so with
// analysis off the card read "off" beside "0 queued · 0 in flight · 4 done ·
// 0 failed (6 workers)" (measured on a real serve after four analyses and a
// switch-off, backlog B113): counters beside an off badge read as a feature
// that is on and idle, which is why the upscale tile leaves its pool out by
// the gate.
//
// The steps move the config flag and the gate apart, because the gate is
// the flag AND a usable sox: the third step is a flag switched on over a
// missing sox, where the card says "degraded", and a handler that read the
// flag instead of the gate would put the counters back there.
func TestTheAnalysisPoolLineFollowsTheGate(t *testing.T) {
	srv, _, _ := newTestServer(t)
	h := srv.Handler()
	// Counters a pool that has done work reports: the line must be left
	// out whatever they say, not because they are zero.
	srv.deps.AnalysisPoolStats = func() *UpscalePoolStats {
		return &UpscalePoolStats{Workers: 6, QueueCap: 5000, Enqueued: 4, Done: 4}
	}
	gate := false
	srv.deps.AnalysisActive = func() bool { return gate }

	for step, st := range []struct {
		flag, gate bool
		state      string
	}{
		{false, false, "switched off"},
		{true, true, "active"},
		{true, false, "switched on without a usable sox"},
		{false, false, "switched off again"},
	} {
		next := config.Clone(srv.deps.CfgHolder.Load())
		next.Analysis.Enabled = st.flag
		srv.deps.CfgHolder.Store(next)
		gate = st.gate

		var got analysisStatsResponse
		if code := doJSON(t, h, "GET", "/api/analysis/stats", nil, &got); code != http.StatusOK {
			t.Fatalf("step %d (%s): /api/analysis/stats answered %d", step, st.state, code)
		}
		if got.Enabled != st.gate {
			t.Fatalf("step %d (%s): enabled=%v with the gate answering %v, so the fixture no "+
				"longer drives the gate and the check below measures nothing",
				step, st.state, got.Enabled, st.gate)
		}
		switch {
		case st.gate && got.Pool == nil:
			t.Errorf("step %d (%s): the gate is open and the payload carries no pool, so the "+
				"Jobs card's Queue line reads \"—\" while analysis runs", step, st.state)
		case !st.gate && got.Pool != nil:
			t.Errorf("step %d (%s): the gate is closed and the payload carries the pool's "+
				"counters (%+v), so the Jobs card shows a queue line beside its %s badge, "+
				"where the upscale tile leaves its pool out", step, st.state, *got.Pool,
				map[bool]string{true: "degraded", false: "off"}[st.flag])
		}
	}
}
