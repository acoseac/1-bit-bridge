package main

// Daily retention sweep for the two tables that grow without a bound.
//
// A daily tick, NOT a post-scan hook: none of this is related to scanning
// and firing it on every debounced watcher event would be pure noise.

import (
	"context"
	"errors"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// retentionSweepInterval is the tick. Retention is a slow-moving policy
// measured in months; a daily pass is already far more often than the
// state it acts on changes.
const retentionSweepInterval = 24 * time.Hour

// retentionSweepSettleDelay keeps the first pass off the startup critical
// path, which is already busy with the initial scan.
const retentionSweepSettleDelay = 5 * time.Minute

// liveTokenIDs is the set of auth-token IDs a registration may bind to.
// Supplied as a closure rather than an auth.Store handle so the sweeper
// has no opinion about where tokens live.
type liveTokenIDs func() ([]string, error)

// retentionReaper is the store surface one pass needs. An interface, not
// the concrete *manifest.Store, so a test can observe WHETHER the sweeper
// calls the reap at all — which is the only way to pin the fail-closed
// skip below. Asserting on surviving rows alone cannot: the store carries
// its own ErrNoLiveTokens guard, so the rows survive either way and a
// behavioural test goes green against a sweeper that skips nothing.
type retentionReaper interface {
	ReapOrphanDeviceRegistrations(ctx context.Context, liveTokenIDs []string) (int64, error)
	ReapStaleDeviceRegistrations(ctx context.Context, beforeNS int64) (int64, error)
	ReapPlaybackHistory(ctx context.Context, beforeNS int64) (int64, error)
}

// retentionSweeper holds what one pass needs.
type retentionSweeper struct {
	store     retentionReaper
	liveToken liveTokenIDs
	// cfg is read LIVE on every pass, so a settings change takes effect
	// on the next tick rather than at the next restart.
	cfg func() *config.Config
	now func() time.Time
}

// sweep runs one pass. Errors are logged and swallowed: a failed
// retention sweep costs nothing that the next tick does not recover.
func (r *retentionSweeper) sweep(ctx context.Context) {
	// A cancelled context is a shutdown, not a pass worth running. The
	// three reaps each take Store.mu before they discover the context is
	// dead, and each would log a Warn about a failure that is a clean
	// exit -- an operator would go looking for a fault on every restart
	// that lands inside a sweep.
	if ctx.Err() != nil {
		return
	}
	cfg := r.cfg()
	if cfg == nil {
		return
	}

	// 1. Orphaned registrations — ALWAYS ON. A row bound to a revoked
	//    token can never be used again; that is garbage, not policy.
	//
	//    FAIL CLOSED on a token-read failure. An empty live set means
	//    "delete every registration", and the realistic way to reach one
	//    is a failure to read the auth store rather than a genuinely
	//    token-less bridge. The store refuses an empty set outright
	//    (ErrNoLiveTokens); this skip means we never even ask.
	ids, err := r.liveToken()
	switch {
	case err != nil:
		logger.Warn("retention: skipping the orphan reap — could not read the live token set",
			"err", err)
	case len(ids) == 0:
		// Not an error: a bridge with no tokens minted yet is a normal
		// pre-pairing state. Reaping against it would delete nothing
		// legitimate only because there is nothing legitimate — but the
		// store refuses it either way, so say why we skipped.
		logger.Debug("retention: no auth tokens exist; nothing to reap against")
	default:
		n, err := r.store.ReapOrphanDeviceRegistrations(ctx, ids)
		switch {
		case errors.Is(err, manifest.ErrNoLiveTokens):
			// Unreachable given the len check above; kept so a future
			// caller change cannot turn this into a silent wipe.
			logger.Warn("retention: orphan reap refused an empty live-token set")
		case ctx.Err() != nil:
			// Shutdown, not a failure. The next tick reruns the pass.
		case err != nil:
			logger.Warn("retention: orphan device-registration reap failed", "err", err)
		case n > 0:
			logger.Info("retention: reaped device registrations bound to revoked tokens", "rows", n)
		}
	}

	now := r.now()

	// 2. Stale registrations — POLICY, default off.
	if days := cfg.Retention.DeviceRegistrationDays; days > 0 && ctx.Err() == nil {
		cutoff := now.AddDate(0, 0, -days).UnixNano()
		switch n, err := r.store.ReapStaleDeviceRegistrations(ctx, cutoff); {
		case ctx.Err() != nil:
			// Shutdown, not a failure.
		case errors.Is(err, manifest.ErrCutoffNotInThePast):
			// config.MaxRetentionDays makes this unreachable from a
			// validated config; kept because the store guard is the last
			// thing standing between an overflowed window and an empty
			// table, and a silent skip would hide that it fired.
			logger.Warn("retention: refusing a stale-registration cutoff that is not in the past",
				"days", days, "cutoff", cutoff)
		case err != nil:
			logger.Warn("retention: stale device-registration reap failed", "err", err)
		case n > 0:
			logger.Info("retention: reaped device registrations unseen past the window",
				"rows", n, "days", days)
		}
	}

	// 3. Playback history — POLICY, default off, and the config layer has
	//    already refused any non-zero value below the 90-day floor or
	//    above the ceiling that keeps the cutoff arithmetic from wrapping.
	if days := cfg.Retention.PlaybackHistoryDays; days > 0 && ctx.Err() == nil {
		cutoff := now.AddDate(0, 0, -days).UnixNano()
		switch n, err := r.store.ReapPlaybackHistory(ctx, cutoff); {
		case ctx.Err() != nil:
			// Shutdown, not a failure.
		case errors.Is(err, manifest.ErrCutoffNotInThePast):
			logger.Warn("retention: refusing a playback-history cutoff that is not in the past",
				"days", days, "cutoff", cutoff)
		case err != nil:
			logger.Warn("retention: playback-history reap failed", "err", err)
		case n > 0:
			logger.Info("retention: reaped playback history past the window",
				"rows", n, "days", days)
		}
	}
}

// runRetentionSweeper is the daily loop. Joined to bgWriters by the
// caller so shutdown waits for an in-flight pass rather than racing
// Store.Close.
func runRetentionSweeper(ctx context.Context, r *retentionSweeper) {
	// time.NewTimer + defer Stop, NOT time.After: an abandoned time.After
	// timer is not collected until it fires, so a cancelled ctx leaves a
	// 5-minute timer alive. runServe is re-entered from the launcher menu,
	// so those accumulate — the PR #290 convention, and this violated it.
	// (Gemini MEDIUM, PR #822.)
	settle := time.NewTimer(retentionSweepSettleDelay)
	defer settle.Stop()
	select {
	case <-ctx.Done():
		return
	case <-settle.C:
	}
	r.sweep(ctx)

	t := time.NewTicker(retentionSweepInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.sweep(ctx)
		}
	}
}
