package main

import (
	"context"
	"errors"
	"time"

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

// duplicatesRestamper is the sweeper's view of the scanner: whether a
// scan is in flight, and the stamping pass itself. An interface rather
// than *manifest.Scanner because the deferral branch below has to be
// drivable from a test, and Scanner's scanning flag is an unexported
// atomic with no setter (nor should it have one).
type duplicatesRestamper interface {
	IsScanning() bool
	RestampDuplicates(context.Context) (int, error)
}

var _ duplicatesRestamper = (*manifest.Scanner)(nil)

// duplicatesDeferRetry is how long the sweeper waits before re-checking
// a scan it deferred behind. Long enough that a multi-minute scan costs
// a handful of atomic loads, short enough that the operator's flip lands
// promptly once the scan ends.
const duplicatesDeferRetry = 5 * time.Second

// runDuplicatesSweeper applies the duplicates policy ON DEMAND — it is
// the hot-apply half of the settings PATCH (Deps.TriggerDuplicatesPass →
// nudge). Unlike the analysis/fingerprint sweepers it has NO periodic
// tick and NO startup run: every full scan already re-stamps in its
// success tail, so the sweeper exists solely for the operator flipping
// duplicates.filter between scans.
//
// A nudge that lands while a scan is in flight is RE-ARMED, not dropped.
// The former behaviour rested on "the running scan's tail already
// applies the new value", which is not something the sweeper can know:
// RestampDuplicates snapshots the policy ONCE at the top of the pass,
// before two full-library streaming walks, so a PATCH that commits after
// that read and before the scan ends is simply lost — and the tail may
// not run at all, because Scan early-returns when the routed-exclusion
// fetch fails. Either way nothing would re-stamp until the next periodic
// scan (6h by default) while the admin UI claims the new policy is being
// applied. Re-arming costs one redundant pass in the common case, which
// is idempotent and diff-guarded to zero writes.
//
// Joined on bgWriters (the pass writes the store; it must drain before
// Store.Close), which is why the ctx.Canceled outcome logs at Info, not
// Error.
func runDuplicatesSweeper(ctx context.Context, scanner duplicatesRestamper, nudge chan struct{}, status *sweepStatus[duplicatesSweepCounts], deferRetry time.Duration) {
	// A non-positive retry makes time.After fire immediately, which turns
	// the re-arm branch below into a tight spin for the whole duration of
	// a scan — the nudge is put back and received again with no wait in
	// between. No production caller can pass one today (serve passes the
	// const), but this is exactly the loop where a spin would be
	// invisible: it logs at Info once per iteration and burns a core
	// silently until the scan ends. Clamp rather than trust the caller.
	if deferRetry <= 0 {
		deferRetry = duplicatesDeferRetry
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-nudge:
		}
		if scanner.IsScanning() {
			logger.Info("duplicates restamp deferred: scan in flight — re-arming")
			// Put the intent back. The channel is buffered-1, so a full
			// buffer means another nudge is already pending and carries
			// the same intent (the pass reads the LIVE policy when it
			// runs) — dropping ours there is correct, not a loss.
			select {
			case nudge <- struct{}{}:
			default:
			}
			// Back off before looping, or the re-armed nudge would be
			// received immediately and spin this goroutine for the whole
			// duration of the scan.
			select {
			case <-ctx.Done():
				return
			case <-time.After(deferRetry):
			}
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
