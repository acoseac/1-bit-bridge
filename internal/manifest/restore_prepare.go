package manifest

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
)

// restoreStagingSuffix names the staged copy of a snapshot's database that
// the database-only restore prepares beside the live one (backlog B653 PR
// 2b1): bridge.db-restore, in the data dir, so that the store's own open of
// it records a migration in the install marker the live database's opens
// record in.
const restoreStagingSuffix = "-restore"

// RestoreStagingPath is where the database-only restore stages, prepares and
// checks the snapshot's database in dataDir before it moves the originals
// out and renames the prepared copy in. Its lock files are its own
// (bridge.db-restore-open.lock, bridge.db-restore-migrate.lock), never the
// live database's, and nothing removes them.
func RestoreStagingPath(dataDir string) string {
	return DefaultDBPath(dataDir) + restoreStagingSuffix
}

// RestoredHistory is the sync history a prepared database holds: the backup
// epoch PrepareRestoredDatabase wrote, and the start of the deletion
// journal's coverage it restarted, in UnixNano.
type RestoredHistory struct {
	Epoch    string
	Coverage int64
}

// PrepareRestoredDatabase makes the staged copy at RestoreStagingPath(dataDir)
// the database a restore installs (backlog B653 PR 2b1): it opens it through
// the store's own open, which runs the ladder (and records a pending step in
// the install marker beside it, as every migrating open does), then, under
// the store's writer lock and in one transaction, writes a new backup epoch
// and restarts the deletion journal's coverage (resetDeletionJournalCoverageTx,
// unchanged), so every cursor the discarded history issued reads incomplete;
// then checkpoints with TRUNCATE and reads the checkpoint's answer
// (checkpointTruncate): one that did not complete is ErrEpochCheckpointBusy,
// the commit still in the WAL. It closes the store before it answers. The
// caller checks the staged WAL afterwards: a prepared copy is its main file
// alone.
func PrepareRestoredDatabase(ctx context.Context, dataDir string) (h RestoredHistory, err error) {
	s, err := OpenStore(RestoreStagingPath(dataDir))
	if err != nil {
		return RestoredHistory{}, fmt.Errorf("open the staged database: %w", err)
	}
	defer func() {
		if cerr := s.Close(); err == nil && cerr != nil {
			err = fmt.Errorf("close the staged database: %w", cerr)
		}
	}()
	if h, err = s.renewHistory(ctx); err != nil {
		return RestoredHistory{}, fmt.Errorf("renew the staged database's history: %w", err)
	}
	if err := checkpointTruncate(s.db); err != nil {
		return RestoredHistory{}, err
	}
	return h, nil
}

// renewHistory is PrepareRestoredDatabase's one transaction: a new epoch and
// the coverage restarted at the store's clock, under s.mu as every writer is.
func (s *Store) renewHistory(ctx context.Context) (RestoredHistory, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	epoch, err := newEpoch()
	if err != nil {
		return RestoredHistory{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return RestoredHistory{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO backup_epoch (id, epoch) VALUES (1, ?)
		ON CONFLICT(id) DO UPDATE SET epoch = excluded.epoch
	`, epoch); err != nil {
		return RestoredHistory{}, err
	}
	if err := resetDeletionJournalCoverageTx(ctx, tx, s.now().UnixNano()); err != nil {
		return RestoredHistory{}, err
	}
	h, err := readRestoredHistory(ctx, tx)
	if err != nil {
		return RestoredHistory{}, err
	}
	if h.Epoch != epoch {
		return RestoredHistory{}, errors.New("the epoch read back is not the one written")
	}
	return h, tx.Commit()
}

// historyQuerier is what readRestoredHistory reads through: a transaction of
// the store's, or the snapshot reader's database.
type historyQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// readRestoredHistory reads the epoch and the coverage start a database holds.
func readRestoredHistory(ctx context.Context, q historyQuerier) (RestoredHistory, error) {
	var h RestoredHistory
	if err := q.QueryRowContext(ctx, `SELECT epoch FROM backup_epoch WHERE id = 1`).Scan(&h.Epoch); err != nil {
		return RestoredHistory{}, fmt.Errorf("read the backup epoch: %w", err)
	}
	var v string
	if err := q.QueryRowContext(ctx, `SELECT v FROM scan_state WHERE k = ?`, deletionJournalCoverageKey).Scan(&v); err != nil {
		return RestoredHistory{}, fmt.Errorf("read the deletion journal's coverage: %w", err)
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return RestoredHistory{}, fmt.Errorf("read the deletion journal's coverage: %w", err)
	}
	h.Coverage = n
	return h, nil
}

// ReadRestoredHistory reads the history a prepared database holds through db,
// a read-only reader of the file (the restore's check opens it through the
// snapshot reader, never the store's own open), so the restore can show
// that what it installs is what it prepared.
func ReadRestoredHistory(ctx context.Context, db *sql.DB) (RestoredHistory, error) {
	return readRestoredHistory(ctx, db)
}
