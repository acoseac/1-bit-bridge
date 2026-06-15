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
func runSmartPlaylistRegenerator(ctx context.Context, store *manifest.Store, analysisEnabled bool, interval time.Duration) {
	regen := func() {
		opts := smartplaylistgen.DefaultOptions(time.Now().UnixNano(), analysisEnabled)
		n, err := smartplaylistgen.Regenerate(ctx, store, opts)
		if err != nil {
			logger.Warn("smart-playlist regeneration failed", "err", err)
			return
		}
		logger.Info("smart-playlist regeneration", "families", n)
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
			regen()
		}
	}
}
