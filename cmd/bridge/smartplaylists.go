package main

import (
	"context"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/smartplaylistgen"
)

// runSmartPlaylistRegenerator is the serve-side smart-playlist loop. After an
// initial settle delay (let startup scan/analysis land) and then on every
// `interval` tick, it regenerates the populated playlist families into the
// `smart_playlists` cache that GET /v1/smart-playlists serves. analysisEnabled
// gates the harmonic Auto Mix + Daily Mix discovery (the listening families
// work from history alone). Honors ctx for clean shutdown.
// status (nil-safe) records last/next-run timestamps for the admin
// Jobs card — the on-demand POST /api/smart-playlists/regenerate stays
// the synchronous trigger and is not routed through this loop.
// smartPlaylistSettleDelay keeps the first regeneration off the startup
// critical path — slightly longer than the analysis sweeper's, since this reads
// the analysis it produces. A var (not const) purely as the test seam;
// production never mutates it. Same shape as analysisSweeperSettleDelay, and
// the reason this loop's live-gate behaviour had no test: at 120 s a unit test
// could not observe a second run.
var smartPlaylistSettleDelay = 120 * time.Second

// analysisActive is read LIVE per run, not captured.
//
// It used to be a plain bool — the VALUE of analysisActiveFn(), evaluated once
// at wiring time — sitting beside an `enabled` closure whose own docstring
// explains why a captured boot value is wrong here. Both halves gate the same
// cache, and ops/settings-apply-semantics.md classes analysisEnabled as live.
//
// The visible effect of freezing it: POST /api/smart-playlists/regenerate reads
// the gate live, so pressing "Regenerate now" after enabling analysis produced
// the harmonic families — and the daily ticker then rebuilt the same cache with
// the frozen false and dropped them again. The mixes flipped depending on which
// trigger last ran, with nothing logged. Installing sox at runtime had the same
// effect, since analysisActiveFn folds in the 30 s-TTL probe.
func runSmartPlaylistRegenerator(ctx context.Context, store *manifest.Store, analysisActive func() bool, enabled func() bool, interval func() time.Duration, rearm <-chan struct{}, status *sweepStatus[struct{}]) {
	on := func() bool { return enabled == nil || enabled() }
	regen := func() {
		// Checked per run, not once at startup: the toggle hot-applies,
		// and a regenerator that captured the boot value would keep
		// writing families to a store the API has stopped serving.
		if !on() {
			// Deliberately no status bookkeeping — a disabled run is not
			// a run, and recording one would put a "last regenerated"
			// timestamp on the Jobs card for work that never happened.
			return
		}
		status.sweepStarted()
		// Read INSIDE the run, beside enabled(), so a settings flip or a
		// freshly-installed sox reaches the very next regeneration.
		analysisOn := analysisActive != nil && analysisActive()
		opts := smartplaylistgen.DefaultOptions(time.Now().UnixNano(), analysisOn)
		n, err := smartplaylistgen.Regenerate(ctx, store, opts)
		if err != nil {
			logger.Warn("smart-playlist regeneration failed", "err", err)
			status.sweepFinished(nil)
			return
		}
		logger.Info("smart-playlist regeneration", "families", n)
		status.sweepFinished(&struct{}{})
	}

	// Settle delay so regeneration doesn't compete with startup work
	// (slightly longer than the analysis sweeper's, since this reads the
	// analysis it produces).
	// The shared loop, rather than the hand-rolled ticker this used: it
	// already has the interval-provider + rearm + dormant-parks semantics
	// this needs, and a second copy would be a second place to get the
	// 0 → N transition wrong. No work-nudge — there is no "regenerate now"
	// button; the admin's per-family regenerate goes straight to the
	// engine.
	runSweepLoop(ctx, status, smartPlaylistSettleDelay, interval, nil, rearm, regen)
}
