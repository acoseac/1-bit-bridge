package backup

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	"github.com/acoseac/1-bit-bridge/internal/fsutil"
	"github.com/acoseac/1-bit-bridge/internal/logging"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/updater"
	"github.com/acoseac/1-bit-bridge/internal/version"
)

// snapshotUserVersion reads the user_version of the snapshot's database at
// path through the snapshot reader the verification opens it with (the
// one-file VFS, mode=ro&immutable=1: checkSQLite's), so nothing is made in
// the snapshot: manifest.PendingMigrations is the live database's reader, and
// its hold of the store's lifetime lock would make bridge.db-open.lock there
// (backlog B653 PR 2a; design-r2 §6.4).
func snapshotUserVersion(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()
	if fi, err := f.Stat(); err != nil {
		return 0, err
	} else if !fi.Mode().IsRegular() {
		return 0, fmt.Errorf("%s is not a regular file", filepath.Base(path))
	}
	// Unpaced (a pacer with no rate), and stopped by nothing: one page's read.
	db, closeReader, err := openSnapshotReader(f, &pacer{ctx: context.Background()})
	if err != nil {
		return 0, err
	}
	defer closeReader()
	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		return 0, err
	}
	return v, nil
}

// restoreReplacesDatabase reports whether the restore will replace the live
// database: its target is set, the snapshot lists the database, and its file
// is there (one whose stat fails otherwise is attempted, and fails, in the
// loop). That, and nothing else, is what records a store change in an armed
// install (B653 PR 2a's brief review, finding 4): a snapshot that holds no
// database, or a restore whose targets leave it out, changes no store.
func restoreReplacesDatabase(snapshotDir, dbTarget string, files []string) bool {
	if dbTarget == "" || !slices.Contains(files, ManifestDBFileName) {
		return false
	}
	_, err := os.Stat(filepath.Join(snapshotDir, ManifestDBFileName))
	return !errors.Is(err, fs.ErrNotExist)
}

// recordRestoredStore records, before the restore's first write to the
// restored files, that this version changes the store, in an armed install
// marker in the data dir that holds the database: updater.DisarmBeforeMigrating,
// the admission every migrating open makes (B613, B660), asked whatever the
// snapshot's version (backlog B653 PR 2a, codex-1). A snapshot at this
// version's ladder top, from another installation or a version with the same
// ladder, migrates nothing, so the epoch's open records nothing, and an
// armed install's crash rollback would then put the previous binary back over
// a store it does not read. Where an install is armed and this version is not
// the backup's it records DataDirOpenedBy and StoreMigratedBy durably, in one
// write, and every rollback then refuses; it refuses, writing nothing, under
// a veto of this version's migrations (ErrMigrationVetoed), while the install
// awaits its confirmation after a start that did not end cleanly
// (ErrMigrationHeld), and where a record is due and the marker's lock cannot
// be taken. A marker it could not read is removed and said, as the store's
// own open says it.
func recordRestoredStore(dbTarget string) error {
	res, err := updater.DisarmBeforeMigrating(filepath.Dir(dbTarget), version.ServerVersion)
	if err != nil {
		return fmt.Errorf("record in the install marker that the restore replaces the library database: %w; nothing was restored", err)
	}
	if res.Unreadable != nil {
		logging.Component("backup").Warn("removed an install marker that could not be read before restoring the store",
			"err", fsutil.ErrorWithoutPath(res.Unreadable))
	}
	return nil
}

// SetOnSourceWorkerEndForTest installs the hook a restore's source worker
// runs last, given the snapshot as the restore was given it, once it has
// handed its hold over or let go of what it took, and returns the restore.
// Its callback must return, and ignore every snapshot but its test's.
// Exported from a production file (it was export_test.go's until B653 PR 2a)
// so cmd/bridge's tests can show that bridge restore finds an unfinished
// restore before any snapshot is looked at; production code never calls it.
func SetOnSourceWorkerEndForTest(fn func(dir string)) (restore func()) {
	var p *func(string)
	if fn != nil {
		p = &fn
	}
	prev := onSourceWorkerEnd.Swap(p)
	return func() { onSourceWorkerEnd.Store(prev) }
}

// warnRestore says s through the restore's warning sink, where its caller
// gave one (bridge restore prints it on stdout), and logs it otherwise.
func warnRestore(opts RestoreOptions, s string) {
	if opts.Warn != nil {
		opts.Warn(s)
		return
	}
	logging.Component("backup").Warn(s)
}

// holdLiveStore is the live snapshot's admission (captureInto): the store's
// lifetime lock held shared (manifest.HoldLiveStore) and, under it, the
// restore's journal asked (manifest.CheckRestoreJournal), before the capture
// writes anything. A database whose directory is not there has nothing open
// and no restore under way, and is held by nothing. release is never nil
// when err is nil.
func holdLiveStore(dbPath string) (release func(), err error) {
	if _, err := os.Lstat(filepath.Dir(dbPath)); errors.Is(err, fs.ErrNotExist) {
		return func() {
			// Nothing was held.
		}, nil
	}
	release, err = manifest.HoldLiveStore(dbPath)
	if err != nil {
		return nil, fmt.Errorf("hold the library database for its snapshot: %w", err)
	}
	if err := manifest.CheckRestoreJournal(dbPath); err != nil {
		release()
		return nil, fmt.Errorf("snapshot the library database: %w", err)
	}
	return release, nil
}

// holdRestoredStore takes the store's lifetime lock exclusively for the
// restore of the database at dbTarget (manifest.HoldStoreExclusive; backlog B653
// PR 2a): counted on where bridge restore holds it already, and taken here
// for any other caller, so no opener of the live database stands beside the
// restore, in this process or another. Its directory is made first where it
// is not there, as the restore's own copy would make it: nothing has a
// database open in a directory that is not there. A nil hold and no error
// where the restore has no database target.
func holdRestoredStore(dbTarget string) (*manifest.StoreHold, error) {
	if dbTarget == "" {
		return nil, nil
	}
	if err := fsutil.MkdirAll(filepath.Dir(dbTarget), 0o700); err != nil {
		return nil, fmt.Errorf("create the data directory before restoring: %w; nothing was restored", err)
	}
	hold, err := manifest.HoldStoreExclusive(dbTarget)
	if err != nil {
		return nil, fmt.Errorf("%w; nothing was restored", err)
	}
	return hold, nil
}
