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
func runSmartPlaylistRegenerator(ctx context.Context, store *manifest.Store, analysisEnabled bool, interval time.Duration, status *sweepStatus[struct{}]) {
	regen := func() {
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
	select {
	case <-ctx.Done():
		return
	case <-time.After(settleDelay):
	}
	if interval > 0 {
		status.scheduleNext(time.Now().Add(interval))
	}
	regen()
	if interval <= 0 {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			status.scheduleNext(time.Now().Add(interval))
			regen()
		}
	}
}
