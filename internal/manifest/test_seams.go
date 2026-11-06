package manifest

import (
	"database/sql"
	"fmt"

	"github.com/acoseac/1-bit-bridge/internal/dsn"
)

// WriteDatabaseAtVersionForTest writes the database at path as a binary whose
// migration ladder ended at version left it: every step up to version, its
// sql and its post, on a connection of its own, and user_version set to it,
// with none of OpenStore's checks, locks or records. For tests in other
// packages that need a store genuinely older than this build's, whose next
// open migrates (cmd/bridge's restarts over one); production code never
// calls it.
func WriteDatabaseAtVersionForTest(path string, version int) error {
	db, err := sql.Open("sqlite", dsn.File(path, "_pragma=journal_mode(WAL)"))
	if err != nil {
		return err
	}
	defer db.Close()
	for _, m := range migrations {
		if m.version > version {
			continue
		}
		if _, err := db.Exec(m.sql); err != nil {
			return fmt.Errorf("apply migration %d (%s): %w", m.version, m.name, err)
		}
		if m.post != nil {
			if err := m.post(db); err != nil {
				return fmt.Errorf("migration %d (%s) post: %w", m.version, m.name, err)
			}
		}
	}
	_, err = db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, version))
	return err
}

// SetBeforeMigrationLockForTest makes fn run in every OpenStore after its
// symlink check and before it takes the migration lock (the ladder) or SQLite
// opens a file, with the database's path: a test in another package's hold
// on a store's open that has taken no lock yet. It returns what puts the seam
// before it back. Process-global: a test that uses it MUST NOT call
// t.Parallel(), and restores it in a t.Cleanup.
func SetBeforeMigrationLockForTest(fn func(dbPath string)) (restore func()) {
	return setOpenStoreBeforeSQLite(fn)
}

// SetUpscaleTargetBeforeCommitForTest makes fn run in every SetUpscaleTarget
// of s once both keys are written and before the transaction commits, under
// the store's writer lock, with the transaction: a test's hold on the write
// between its last statement and its commit, where a cancel of the write's
// context lets database/sql roll the transaction back before Commit is
// reached (backlog B662). An error fn returns is SetUpscaleTarget's, and the
// transaction rolls back. On the store it serves, never a package variable:
// cmd/bridge hands it serve's store through serveOpts. Call it before s is
// shared; production code never calls it with a non-nil fn.
func (s *Store) SetUpscaleTargetBeforeCommitForTest(fn func(*sql.Tx) error) {
	s.upscaleTargetBeforeCommit = fn
}
