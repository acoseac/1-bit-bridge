package manifest

import (
	"fmt"
	"path/filepath"

	"github.com/acoseac/1-bit-bridge/internal/updater"
	"github.com/acoseac/1-bit-bridge/internal/version"
)

// disarmBeforeMigrating records, before the store's first migration step and
// under the ladder lock, in an armed install marker in the data dir that
// holds the database, that this version opened it (updater.DisarmEarlyRollback):
// a rollback to the backup an update kept would be a downgrade past what the
// migration writes, whichever command opened the store, serve, doctor,
// clear-missing, a scan or a backup (backlog B613's round 2, Codex's finding
// 1). migrate asks it only while a step is pending, so an open that migrates
// nothing leaves the marker alone. An error refuses the open before any step
// runs, as serve refuses to start over a record it cannot make.
func disarmBeforeMigrating(dbPath string, from int) error {
	res, err := updater.DisarmEarlyRollback(filepath.Dir(dbPath), version.ServerVersion, nil)
	if err != nil {
		return fmt.Errorf("record in the install marker that the store is migrated from version %d: %w", from, err)
	}
	if res.Unreadable != nil {
		logger.Warn("removed an install marker that could not be read before migrating the store", "err", res.Unreadable)
	}
	return nil
}
