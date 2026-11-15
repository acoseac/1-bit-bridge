package manifest

import (
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/acoseac/1-bit-bridge/internal/dsn"
)

// PendingMigrations is how many steps of this build's migration ladder the
// live store at path has not run, read without running one (backlog B660's
// second round): `bridge doctor` says it where a command may not migrate the
// store (updater.MigrationHeld), rather than open the store and refuse. It is
// the LIVE database's reader: it holds the store's lifetime lock shared while
// it reads, and asks the restore's journal under it, both before anything of
// the database is touched (backlog B653 PR 2a), refusing as OpenStore
// refuses (ErrStoreRestoring, ErrStoreLockUnavailable, ErrRestoreIncomplete).
// A snapshot's database is never read through it: the hold would make its
// lock file in the snapshot (internal/backup reads a snapshot's user_version
// through its own one-file reader). The database is opened read-only, as a
// live backup's snapshot opens it, and asked its user_version alone; a store
// that is not there has every step pending, and nothing is made but the lock
// file, beside it in a directory that is there. A link at the database or a
// sidecar is refused, as OpenStore refuses one.
func PendingMigrations(path string) (int, error) {
	if _, err := os.Lstat(filepath.Dir(path)); errors.Is(err, fs.ErrNotExist) {
		// No directory, no store and no restore under way: nothing to hold.
		return len(migrations), nil
	}
	release, err := holdStoreShared(path)
	if err != nil {
		return 0, err
	}
	defer release()
	callStoreHoldTaken(path)
	if err := CheckRestoreJournal(path); err != nil {
		return 0, err
	}
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
	// Closed before the hold goes (defers run last-registered-first): its one
	// connection is back in the pool once the read below has scanned.
	defer db.Close()
	var current int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&current); err != nil {
		return 0, fmt.Errorf("read user_version: %w", err)
	}
	return PendingFrom(current), nil
}

// PendingFrom is how many steps of this build's migration ladder a database
// whose user_version is current has not run.
func PendingFrom(current int) int {
	pending := 0
	for _, m := range migrations {
		if m.version > current {
			pending++
		}
	}
	return pending
}
