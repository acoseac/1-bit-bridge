package manifest

import (
	"fmt"
	"path/filepath"

	"github.com/acoseac/1-bit-bridge/internal/updater"
	"github.com/acoseac/1-bit-bridge/internal/version"
)

// disarmBeforeMigrating records, before the store's first migration step and
// under the ladder lock, in an armed install marker in the data dir that
// holds the database, that this version opened it
// (updater.DisarmBeforeMigrating, which takes the install marker's lock
// inside the ladder's): a rollback to the backup an update kept would be a
// downgrade past what the migration writes, whichever command opened the
// store, serve, doctor, clear-missing, a scan or a backup (backlog B613's
// round 2, Codex's finding 1). migrate asks it only while a step is pending,
// so an open that migrates nothing leaves the marker alone. An error refuses
// the open before any step runs, as serve refuses to start over a record it
// cannot make, and so does the early rollback's record of an install of this
// version that no start has gone on past (updater.ErrMigrationVetoed; round
// 3): the backup may be back over this binary already.
//
// The data dir is the database's own directory. That is the boundary this
// rests on, since OpenStore is never given the data dir and cannot check it:
// every store the bridge opens is <dataDir>/bridge.db, by DefaultDBPath, and
// TestEveryStoreTheBridgeOpensIsItsDataDirsDatabase (cmd/bridge) holds every
// production open of the store to that path. A store opened anywhere else
// would record in a marker no rollback reads.
func disarmBeforeMigrating(dbPath string, from int) error {
	res, err := updater.DisarmBeforeMigrating(filepath.Dir(dbPath), version.ServerVersion)
	if err != nil {
		return fmt.Errorf("record in the install marker that the store is migrated from version %d: %w", from, err)
	}
	if res.Unreadable != nil {
		logger.Warn("removed an install marker that could not be read before migrating the store", "err", res.Unreadable)
	}
	return nil
}
