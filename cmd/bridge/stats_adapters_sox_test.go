package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/transcode"
)

// TestTheV1StatsAdaptersAnswerSoxFromTheirPrecheckEverySnapshot pins that
// /v1/upscale/stats and /v1/analysis/stats read `soxAvailable` from the
// precheck they are handed, for every snapshot, beside `enabled`.
//
// runServe hands both the shared probe (soxCache.precheck), which the gate
// behind `enabled` reads too (TestServeReadsSoxThroughTheSharedProbe pins
// that wiring). Here the gate and the precheck read one variable, as they
// read one cache there, and the variable moves between snapshots: an
// adapter that cached the precheck, as both did for 30 s until 2026-09-28,
// answers the second snapshot with `enabled: false` beside `soxAvailable:
// true`.
func TestTheV1StatsAdaptersAnswerSoxFromTheirPrecheckEverySnapshot(t *testing.T) {
	pool := transcode.NewPool(nil, 1, 1)
	pool.Stop()
	soxUsable := true
	gate := func() bool { return soxUsable }
	precheck := func() error {
		if soxUsable {
			return nil
		}
		return errors.New("sox not found")
	}
	upscale := &upscaleStatsAdapter{
		pool:        func() *transcode.Pool { return pool },
		enabled:     gate,
		soxPrecheck: precheck,
	}
	analysis := &analysisStatsAdapter{enabled: gate, soxPrecheck: precheck}

	for step, usable := range []bool{true, false, true} {
		soxUsable = usable
		up, err := upscale.UpscaleStatsSnapshot(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		an, err := analysis.AnalysisStatsSnapshot(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		for _, got := range []struct {
			endpoint string
			enabled  bool
			sox      *bool
		}{
			{"/v1/upscale/stats", up.Enabled, up.SoxAvailable},
			{"/v1/analysis/stats", an.Enabled, an.SoxAvailable},
		} {
			if got.enabled != usable {
				t.Fatalf("step %d: %s says enabled=%v with the gate answering %v, so the "+
					"fixture no longer drives the gate and this test measures nothing",
					step, got.endpoint, got.enabled, usable)
			}
			if got.sox == nil || *got.sox != usable {
				t.Errorf("step %d: %s says enabled=%v beside soxAvailable=%v, want both %v: "+
					"soxAvailable answered an older probe than the gate did",
					step, got.endpoint, got.enabled, soxAnswer(got.sox), usable)
			}
		}
	}

	// No precheck wired: the field is left out rather than guessed.
	bare := &analysisStatsAdapter{enabled: gate}
	an, err := bare.AnalysisStatsSnapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if an.SoxAvailable != nil {
		t.Errorf("with no precheck wired, soxAvailable is %v, want it left out", *an.SoxAvailable)
	}
}

// TestTheBootSoxLineSaysNoRestartIsNeeded pins serve's one boot-time line
// about a feature switched on without a usable sox.
//
// It said "— disabling", which reads as a demotion a restart undoes, while
// the gate is live: a sox installed later is picked up by the shared probe
// with no restart. The console's banners said "degrade to feature-off at
// startup" for the same gate until #1067 (backlog B45).
func TestTheBootSoxLineSaysNoRestartIsNeeded(t *testing.T) {
	for _, c := range []struct {
		name string
		info transcode.SoxInfo
		err  error
	}{
		{"no sox", transcode.SoxInfo{}, errors.New("sox binary not found on PATH")},
		{"sox without FLAC", transcode.SoxInfo{FormatsKnown: true}, nil},
	} {
		var out strings.Builder
		if soxUsable(c.info, c.err, "analysis", &out) {
			t.Fatalf("%s: soxUsable answered usable, so this case measures nothing", c.name)
		}
		line := out.String()
		if !strings.Contains(line, "no restart needed") || strings.Contains(line, "disabling") {
			t.Errorf("%s: the boot line reads %q; the gate is live, so it must say no restart is "+
				"needed and not read as a demotion", c.name, line)
		}
	}
}

// soxAnswer renders an optional soxAvailable for a failure message.
func soxAnswer(b *bool) any {
	if b == nil {
		return nil
	}
	return *b
}
