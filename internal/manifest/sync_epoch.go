package manifest

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
)

// newEpoch draws the 32-hex backup epoch. Sixteen random bytes: the value
// is a label for one history of the library, not a secret.
func newEpoch() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// BackupEpoch is the install's current epoch. A missing row is an error:
// migration v52 mints one, and a reader that invented a fresh value would
// tell every client to reset.
func (s *Store) BackupEpoch(ctx context.Context) (string, error) {
	var epoch string
	err := s.db.QueryRowContext(ctx, `SELECT epoch FROM backup_epoch WHERE id = 1`).Scan(&epoch)
	if errors.Is(err, sql.ErrNoRows) {
		return "", errors.New("manifest: backup epoch is missing")
	}
	return epoch, err
}

// ReplaceBackupEpoch writes a new epoch over the one the database holds.
// Restore calls it after the copied file has been opened, so a restored
// bridge is a new history even when the snapshot carried the old epoch.
func ReplaceBackupEpoch(ctx context.Context, db *sql.DB) (string, error) {
	epoch, err := newEpoch()
	if err != nil {
		return "", err
	}
	_, err = db.ExecContext(ctx, `
		INSERT INTO backup_epoch (id, epoch) VALUES (1, ?)
		ON CONFLICT(id) DO UPDATE SET epoch = excluded.epoch
	`, epoch)
	if err != nil {
		return "", err
	}
	return epoch, nil
}

// ResetBackupEpoch opens the database (running migrations), writes a new
// epoch, and checkpoints before it returns. The checkpoint is load-bearing:
// the caller removes the WAL afterwards, and a commit that was still in the
// WAL would leave the restored file without the new epoch and without the
// migration that created the table.
func ResetBackupEpoch(dbPath string) (err error) {
	s, err := OpenStore(dbPath)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := s.Close(); err == nil {
			err = cerr
		}
	}()
	if _, err = ReplaceBackupEpoch(context.Background(), s.db); err != nil {
		return err
	}
	if _, err = s.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return err
	}
	return nil
}
