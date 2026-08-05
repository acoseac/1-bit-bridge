package main

import (
	"context"
	"errors"

	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/dupes"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// dupePolicyFromConfig maps the validated config vocabulary onto
// dupes.FilterMode. The literals are identical by construction
// (lockstep-pinned by TestDuplicatesFilterVocabularyLockstep); the
// error branch is unreachable for a config that passed Validate /
// the settings PATCH, and fails toward the default rather than
// toward suppressing or unsuppressing anything surprising.
func dupePolicyFromConfig(cfg *config.Config) dupes.Policy {
	mode, err := cfg.Duplicates.EffectiveFilter()
	if err != nil {
		mode = config.DuplicatesFilterHighestQuality
	}
	return dupes.Policy{Mode: dupes.FilterMode(mode)}
}

// duplicatesSweepCounts is the recorder DTO for the duplicates stamping
// sweeper (the Jobs-card adapter arrives with the admin Duplicates page).
type duplicatesSweepCounts struct {
	Changed int
}

// runDuplicatesSweeper applies the duplicates policy ON DEMAND — it is
// the hot-apply half of the settings PATCH (Deps.TriggerDuplicatesPass →
// nudge). Unlike the analysis/fingerprint sweepers it has NO periodic
// tick and NO startup run: every full scan already re-stamps in its
// success tail, so the sweeper exists solely for the operator flipping
// duplicates.filter between scans.
//
// A nudge that lands while a scan is in flight is deliberately DROPPED,
// not deferred: the running scan's own tail pass reads the policy
// closure at its runtime and therefore already applies the new value —
// running here too would just double-stamp behind the scanner's back.
//
// Joined on bgWriters (the pass writes the store; it must drain before
// Store.Close), which is why the ctx.Canceled outcome logs at Info, not
// Error.
func runDuplicatesSweeper(ctx context.Context, scanner *manifest.Scanner, nudge <-chan struct{}, status *sweepStatus[duplicatesSweepCounts]) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-nudge:
		}
		if scanner.IsScanning() {
			logger.Info("duplicates restamp deferred: scan in flight (its tail applies the current policy)")
			continue
		}
		status.sweepStarted()
		n, err := scanner.RestampDuplicates(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				logger.Info("duplicates restamp stopped: shutdown")
			} else {
				logger.Error("duplicates restamp", "err", err)
			}
			status.sweepFinished(nil)
			continue
		}
		logger.Info("duplicates restamp applied", "changedRows", n)
		status.sweepFinished(&duplicatesSweepCounts{Changed: n})
	}
}
