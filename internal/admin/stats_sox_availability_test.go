package admin

import (
	"net/http"
	"testing"
)

// TestTheStatsSoxAvailableIsThePrecheckTheGateReads pins that the console's
// two stats endpoints answer `soxAvailable` from the precheck they are wired
// with, read for every snapshot, so the field moves with `enabled` and never
// behind it.
//
// cmd/bridge wires UpscalePrecheck, the upscale gate behind UpscaleStats and
// the analysis gate behind AnalysisActive to ONE TTL-cached sox probe. The
// console kept a 30 s cache of its own on top of it until 2026-09-28, so for up
// to 30 s after sox was installed or removed each endpoint answered `enabled`
// from the new probe and `soxAvailable` from an older one: measured on a live
// bridge, `enabled: false` beside `soxAvailable: true` for 14 s after sox left
// the PATH. Here the three closures read one variable, as they read one cache
// in production, and the variable moves between reads.
func TestTheStatsSoxAvailableIsThePrecheckTheGateReads(t *testing.T) {
	srv, _, _ := newTestServer(t)
	h := srv.Handler()
	soxUsable := true
	srv.deps.UpscalePrecheck = func() error {
		if soxUsable {
			return nil
		}
		return errNoSoxForTest
	}
	srv.deps.UpscaleStats = func() *UpscalePoolStats {
		if soxUsable {
			return &UpscalePoolStats{}
		}
		return nil
	}
	srv.deps.AnalysisActive = func() bool { return soxUsable }

	for step, usable := range []bool{true, false, true} {
		soxUsable = usable
		var up upscaleStatsResponse
		if code := doJSON(t, h, "GET", "/api/upscale/stats", nil, &up); code != http.StatusOK {
			t.Fatalf("step %d: /api/upscale/stats answered %d", step, code)
		}
		var an analysisStatsResponse
		if code := doJSON(t, h, "GET", "/api/analysis/stats", nil, &an); code != http.StatusOK {
			t.Fatalf("step %d: /api/analysis/stats answered %d", step, code)
		}
		for _, got := range []struct {
			endpoint string
			enabled  bool
			sox      *bool
		}{
			{"/api/upscale/stats", up.Enabled, up.SoxAvailable},
			{"/api/analysis/stats", an.Enabled, an.SoxAvailable},
		} {
			if got.enabled != usable {
				t.Fatalf("step %d: %s says enabled=%v with the gate answering %v, so the "+
					"fixture no longer drives the gate and this test measures nothing",
					step, got.endpoint, got.enabled, usable)
			}
			if got.sox == nil || *got.sox != usable {
				t.Errorf("step %d: %s says enabled=%v beside soxAvailable=%v, want both %v: "+
					"soxAvailable answered an older probe than the gate did",
					step, got.endpoint, got.enabled, boolOrNil(got.sox), usable)
			}
		}
	}
}

// boolOrNil renders an optional bool for a failure message.
func boolOrNil(b *bool) any {
	if b == nil {
		return nil
	}
	return *b
}
