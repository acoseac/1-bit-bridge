package main

import (
	"context"
	"errors"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/admin"
	"github.com/acoseac/1-bit-bridge/internal/updater"
	"github.com/acoseac/1-bit-bridge/internal/version"
)

// updateChecksOffSave is serve's admin.Deps.UpdateChecksOffSave (backlog B761's
// round 3): for a console save that turns update checks off, the install
// marker's lock in dataDir (updater.LockForChecksOff, first of the locks the
// save takes) and, holding it, the decision whether the off may be saved
// (updater.ChecksOffRefused, over the binary serve runs as, binaryPath, and
// the settings it loaded, configPath, its backup's checks within within, 0
// being updater.ChecksOffWithin). The release it answers is the lock's, which
// the console lets go once the save is published and the updater told; a
// refusal lets it go at once and wraps the console's sentinel for it. On a
// filesystem that keeps no locks the off is saved without the decision: no
// install commits there and no crash rollback acts there either.
func updateChecksOffSave(dataDir, binaryPath, configPath string, within time.Duration) func(context.Context) (func(), error) {
	return func(ctx context.Context) (func(), error) {
		lock, err := updater.LockForChecksOff(ctx, dataDir)
		if err != nil {
			return nil, consoleChecksOffError(err)
		}
		if !lock.Held() {
			return lock.Release, nil
		}
		err = updater.ChecksOffRefused(ctx, updater.ChecksOffSave{
			DataDir: dataDir, BinaryPath: binaryPath, ConfigPath: configPath,
			Current: version.ServerVersion, Within: within,
		})
		if err != nil {
			lock.Release()
			// What failed, for the log alone, by the refusal's kind: the
			// backup's check may name the bridge's own files, which a console
			// body does not; the install marker's read error names no path
			// even there (B761's round 4, B613's round 5 for the marker).
			var unchecked *updater.ChecksOffUncheckedError
			var unread *updater.ChecksOffMarkerError
			switch {
			case errors.As(err, &unread):
				logger.Warn("an off of update checks was refused: the install marker could not be read", "detail", unread.Detail)
			case errors.As(err, &unchecked):
				logger.Warn("an off of update checks was refused: the backup did not answer its check", "detail", unchecked.Detail)
			}
			return nil, consoleChecksOffError(err)
		}
		return lock.Release, nil
	}
}

// consoleRefusal is a refusal of the updater's, err, as the console takes it:
// its words, err's own, and its sentinel, which the console's answer is
// chosen by (admin's sentinels, which the console reads without importing
// the updater).
type consoleRefusal struct {
	sentinel, err error
}

func (e *consoleRefusal) Error() string { return e.err.Error() }

func (e *consoleRefusal) Is(target error) bool { return target == e.sentinel }

func (e *consoleRefusal) Unwrap() error { return e.err }

// consoleChecksOffError is an off-save's refusal by the updater, err, wrapping
// the console's sentinel for it; a stop (ctx's error) and anything else are
// passed on as they are.
func consoleChecksOffError(err error) error {
	switch {
	case errors.Is(err, updater.ErrChecksOffLocked):
		return &consoleRefusal{sentinel: admin.ErrUpdateChecksOffLocked, err: err}
	case errors.Is(err, updater.ErrChecksOffKeepsTheBackup):
		return &consoleRefusal{sentinel: admin.ErrUpdateChecksOffRefused, err: err}
	case errors.Is(err, updater.ErrChecksOffUnchecked):
		return &consoleRefusal{sentinel: admin.ErrUpdateChecksOffUnchecked, err: err}
	}
	return err
}
