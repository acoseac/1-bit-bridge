package manifest

import (
	"context"
	"database/sql"
	"database/sql/driver"
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

// SetConnectorForTest makes every store opened at path from now on open its
// connections through wrap of the registered "sqlite" driver's connector for
// the store's DSN, until restore: sqlitetest's RollbackWatch.Connector, which
// a test that waits for one transaction's rollback at the driver needs the
// store's connections made through (backlog B869). The connector wrap is
// handed is sqlite.NewConnector's, so every function, collation and
// connection hook registered on the driver stays, and a wrap that passes on
// what database/sql asks of a connection leaves the store as it is. Scoped to
// path, cleaned: a store opened anywhere else, or by another opener of the
// file, opens as in production. Process-global, so restore it in a t.Cleanup
// registered before anything that may still open a store there, such as a
// serve's drain, so that it runs after it. Production code never calls it.
func SetConnectorForTest(path string, wrap func(driver.Connector) driver.Connector) (restore func()) {
	return setStoreConnector(path, wrap)
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

// SetAcoustIDMatchForTest records acoustid_match for path alone, with no
// re-queue: a test's fixture of a row the sweeper matched. The sweeper writes
// it only with its re-queue (ResetEnrichedByPaths), never apart from it.
func (s *Store) SetAcoustIDMatchForTest(ctx context.Context, path, acoustID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx,
		`UPDATE tracks SET acoustid_match = ? WHERE path = ?`, acoustID, path)
	return err
}

// SetResetEnrichedBeforeCommitForTest makes fn run in every
// ResetEnrichedByPaths of s once its statements are written and before its
// transaction commits, under the store's writer lock, with the reset's
// context and its transaction: a test's hold on the fingerprint sweep's
// re-queue, where a cancel of that context rolls the transaction back
// (backlog B617), whose rollback a test takes on the transaction's own
// connection before the cancel (sqlitetest's RollbackOf, over a store opened
// through SetConnectorForTest): sql.ErrTxDone answers before that rollback
// runs (backlog B869, B662's rule). An error fn returns is the reset's, and
// the transaction rolls back.
// On the store it serves, never a package variable. Call it before s is
// shared; production code never calls it with a non-nil fn.
func (s *Store) SetResetEnrichedBeforeCommitForTest(fn func(ctx context.Context, tx *sql.Tx) error) {
	s.resetEnrichedBeforeCommit = fn
}
