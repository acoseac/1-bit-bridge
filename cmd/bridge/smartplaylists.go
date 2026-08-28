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
func runSmartPlaylistRegenerator(ctx context.Context, store *manifest.Store, analysisEnabled bool, enabled func() bool, interval func() time.Duration, rearm <-chan struct{}, status *sweepStatus[struct{}]) {
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
		opts := smartplaylistgen.DefaultOptions(time.Now().UnixNano(), analysisEnabled)
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
	const settleDelay = 120 * time.Second
	// The shared loop, rather than the hand-rolled ticker this used: it
	// already has the interval-provider + rearm + dormant-parks semantics
	// this needs, and a second copy would be a second place to get the
	// 0 → N transition wrong. No work-nudge — there is no "regenerate now"
	// button; the admin's per-family regenerate goes straight to the
	// engine.
	runSweepLoop(ctx, status, settleDelay, interval, nil, rearm, regen)
}
