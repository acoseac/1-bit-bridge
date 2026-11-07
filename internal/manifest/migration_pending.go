package manifest

import (
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/acoseac/1-bit-bridge/internal/dsn"
)

// PendingMigrations is how many steps of this build's migration ladder the
// store at path has not run, read without running one (backlog B660's
// second round): `bridge doctor` says it where a command may not migrate the
// store (updater.MigrationHeld), rather than open the store and refuse. The
// database is opened read-only, as a live backup's snapshot opens it, and
// asked its user_version alone; a store that is not there has every step
// pending, and nothing is made. A link at the database or a sidecar is
// refused, as OpenStore refuses one.
func PendingMigrations(path string) (int, error) {
	if name, ok := symlinkAtStore(path); ok {
		return 0, fmt.Errorf("%w at %s", errStoreSymlink, name)
	}
	if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
		return len(migrations), nil
	} else if err != nil {
		return 0, err
	}
	db, err := sql.Open("sqlite", dsn.File(path, "mode=ro&_pragma=busy_timeout(5000)"))
	if err != nil {
		return 0, err
	}
	defer db.Close()
	var current int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&current); err != nil {
		return 0, fmt.Errorf("read user_version: %w", err)
	}
	pending := 0
	for _, m := range migrations {
		if m.version > current {
			pending++
		}
	}
	return pending, nil
}
