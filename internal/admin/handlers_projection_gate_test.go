package admin

import (
	"net/http"
	"testing"
)

// The live upscale gate on GET /api/library/browse-projection.
//
// cmd/bridge wires ProjectedSize and AvailableDiskSpace on every bridge,
// so their nil-ness says nothing about whether upscaling is on. Until
// 2026-09-28 it was the endpoint's whole gate, decided once at boot from
// `upscale.enabled`, and a Settings flip reached the endpoint only
// through a restart. These tests pin the gate that replaced it:
// Deps.UpscaleActive, the closure the batch submit and /v1/health read.

// projectionAnswer asks the projection endpoint about MusicA under one
// kind, counting the disk probes the request made, and returns the status,
// the error code of a refusal ("" for a success) and the probe count.
func projectionAnswer(t *testing.T, srv *Server, kind string) (status int, errCode string, probes int) {
	t.Helper()
	srv.deps.AvailableDiskSpace = func(string) (int64, error) {
		probes++
		return 1 << 40, nil
	}
	var body map[string]any
	status = doJSON(t, srv.Handler(), "GET", "/api/library/browse-projection?path=MusicA&kind="+kind, nil, &body)
	errCode, _ = body["error"].(string)
	return status, errCode, probes
}

// TestProjectionAnswersTheUpscaleGateLive: every kind the endpoint serves
// answers the gate on each request, and a refusal comes before the disk
// probe (and so before the target read and the projection walk, which
// precede it). Switching the gate off and on again moves the answer with
// it, which a value captured when the server was built could not do.
func TestProjectionAnswersTheUpscaleGateLive(t *testing.T) {
	srv, _, _ := newTestServer(t)
	browseTestSeed(t, srv)
	wireOptimizeTestDeps(t, srv)
	// The pcm kind needs the DSD-render caps and its two helpers; none of
	// the fixture's rows is DSD, so every one of them is at target.
	srv.deps.DSDRenderCaps = func() (bool, bool) { return true, false }
	srv.deps.DSDRenderEligible = func(_, _ string, isDSD bool, _ int, _ string) bool { return isDSD }
	srv.deps.TargetRateForPCMRender = func(int) int { return 176400 }
	on := true
	srv.deps.UpscaleActive = func() bool { return on }

	for _, kind := range []string{"upscale", "optimize", "pcm"} {
		t.Run(kind, func(t *testing.T) {
			for i, turnOn := range []bool{true, false, true} {
				on = turnOn
				status, errCode, probes := projectionAnswer(t, srv, kind)
				switch {
				case turnOn && (status != http.StatusOK || probes != 1):
					t.Errorf("request %d, gate on: %d %q with %d disk probes, want 200 and one probe",
						i+1, status, errCode, probes)
				case !turnOn && (status != http.StatusServiceUnavailable || errCode != errCodeUpscaleDisabled || probes != 0):
					t.Errorf("request %d, gate off: %d %q with %d disk probes, want 503 %q and none",
						i+1, status, errCode, probes, errCodeUpscaleDisabled)
				}
			}
		})
	}
}

// TestProjectionReadsANilUpscaleGateAsOff: a Deps built without the gate
// describes a bridge whose upscale state nobody wired, and the endpoint
// reads that as off, as the batch submit and /v1 do
// (TestBatchSubmitReadsANilUpscaleGateAsOff, TestNilFeatureGatesReadAsOff),
// with its helpers wired.
func TestProjectionReadsANilUpscaleGateAsOff(t *testing.T) {
	srv, _, _ := newTestServer(t)
	browseTestSeed(t, srv)
	wireOptimizeTestDeps(t, srv)
	srv.deps.UpscaleActive = nil

	status, errCode, probes := projectionAnswer(t, srv, "upscale")
	if status != http.StatusServiceUnavailable || errCode != errCodeUpscaleDisabled || probes != 0 {
		t.Errorf("nil gate: %d %q with %d disk probes, want 503 %q and none",
			status, errCode, probes, errCodeUpscaleDisabled)
	}
}
