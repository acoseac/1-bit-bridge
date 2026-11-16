package manifest

import (
	"context"
	"database/sql"
	"fmt"
	"os"

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

// SetStoreLockTryForTest makes every try at the store's lifetime lock answer
// what fn answers (exclusive says which kind: a restore's, or an opener's),
// fn standing in for a filesystem's answers (ENOLCK, an I/O error, a
// definitive no-locks answer) in another package's tests, and
// SetStoreLockOpenForTest the lock file's open (write says for writing). Each
// returns what puts the seam before it back. Process-global: a test that uses
// one MUST NOT call t.Parallel(), and restores it in a t.Cleanup. Production
// code never calls them.
func SetStoreLockTryForTest(fn func(f *os.File, exclusive bool) error) (restore func()) {
	storeLockSeamMu.Lock()
	prev := storeLockTry
	storeLockTry = fn
	storeLockSeamMu.Unlock()
	return func() {
		storeLockSeamMu.Lock()
		storeLockTry = prev
		storeLockSeamMu.Unlock()
	}
}

// SetStoreLockOpenForTest: see SetStoreLockTryForTest. fn is handed the
// platform's own open, to answer for the opens it does not stand in for.
func SetStoreLockOpenForTest(fn func(real func(dir, name, like string, write bool) (*os.File, error), dir, name, like string, write bool) (*os.File, error)) (restore func()) {
	storeLockSeamMu.Lock()
	prev := storeLockOpen
	storeLockOpen = func(dir, name, like string, write bool) (*os.File, error) {
		return fn(prev, dir, name, like, write)
	}
	storeLockSeamMu.Unlock()
	return func() {
		storeLockSeamMu.Lock()
		storeLockOpen = prev
		storeLockSeamMu.Unlock()
	}
}

// SetStoreHoldTakenForTest makes fn run in every opener of the live database
// once its shared hold of the store's lifetime lock is taken and before it
// asks the restore's journal (OpenStoreAdmitted, PendingMigrations), with the
// database's path: a test's park in that window. Process-global, restored as
// SetStoreLockTryForTest's.
func SetStoreHoldTakenForTest(fn func(dbPath string)) (restore func()) {
	storeLockSeamMu.Lock()
	prev := storeHoldTaken
	storeHoldTaken = fn
	storeLockSeamMu.Unlock()
	return func() {
		storeLockSeamMu.Lock()
		storeHoldTaken = prev
		storeLockSeamMu.Unlock()
	}
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
// context: a test's hold on the fingerprint sweep's re-queue, where a cancel
// of that context rolls the transaction back (backlog B617). An error fn
// returns is the reset's, and the transaction rolls back. On the store it
// serves, never a package variable. Call it before s is shared; production
// code never calls it with a non-nil fn.
func (s *Store) SetResetEnrichedBeforeCommitForTest(fn func(ctx context.Context) error) {
	s.resetEnrichedBeforeCommit = fn
}
