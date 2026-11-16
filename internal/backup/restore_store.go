package backup

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"

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

// holdRestoreMarker takes the install marker's lock in the data dir that
// holds the database for a restore that replaces it
// (updater.HoldMarkerForRestore), before the restore's turn of the config
// file's lock (the marker's lock first, as every writer that takes both
// takes them) and before anything of the destination but the store's
// lifetime lock is made, and makes the restore's admission under it,
// writing nothing: a lock that cannot be held, a veto, a hold of the
// install, and this version the one an armed install replaces each refuse
// the restore with nothing restored. The restore holds it through its
// record and the database's replacement, so no install is armed between
// them (backlog B653 PR 2a's third round), and lets it go right after the
// database's copy, before the epoch's open, which can take it again. Its wait
// is LockState's, never stopped by the restore's context, which stops
// nothing once the source's lock is held.
func holdRestoreMarker(dbTarget string) (*updater.RestoreHold, error) {
	h, err := updater.HoldMarkerForRestore(context.Background(), filepath.Dir(dbTarget), version.ServerVersion)
	if err != nil {
		return nil, fmt.Errorf("the install marker refuses the restore of the library database: %w; nothing was restored", err)
	}
	return h, nil
}

// recordRestoredStore records under the restore's hold of the install marker's
// lock (RestoreHold.Record), before the restore's first write, the player
// seal's completion included, that this version changes the store, in an
// armed install marker in the data dir that holds the database: the record
// every migrating open makes (B613, B660) and the restore's own refusal beside
// it, asked whatever the snapshot's version (backlog B653 PR 2a). A snapshot
// at this version's ladder top, from another installation or a version with
// the same ladder, migrates nothing, so the epoch's open records nothing, and
// an armed install's crash rollback would then put the previous binary back
// over a store it does not read. Where an install is armed and this version is
// not the backup's it records DataDirOpenedBy and StoreMigratedBy durably, in
// one write, and every rollback then refuses; it refuses, writing nothing,
// under a veto of this version's migrations (ErrMigrationVetoed), while the
// install awaits its confirmation after a start that did not end cleanly
// (ErrMigrationHeld), and where this version is the one the armed install
// replaces (ErrRestoreByPreviousVersion: a migration of its own needs no
// record, a restore of a newer store would have none). A marker it could not
// read is removed and said, as the store's own open says it.
func recordRestoredStore(h *updater.RestoreHold) error {
	res, err := h.Record()
	if err != nil {
		return fmt.Errorf("record in the install marker that the restore replaces the library database: %w; nothing was restored", err)
	}
	if res.Unreadable != nil {
		logging.Component("backup").Warn("removed an install marker that could not be read before restoring the store",
			"err", fsutil.ErrorWithoutPath(res.Unreadable))
	}
	return nil
}

// mintRestoredEpoch mints the backup epoch of the database a restore put at
// target: a restored library is a new backup generation. Minting after the
// copy, then dropping the WAL the mint left, keeps the epoch in the main
// file. Only where the mint's checkpoint completed (backlog B653 PR 2a): a
// checkpoint that did not leaves the epoch, and any migration step the open
// ran, in the WAL, which is kept for the next open to replay, and said; any
// other error keeps it too.
func mintRestoredEpoch(target string, opts RestoreOptions) error {
	switch err := manifest.ResetBackupEpoch(target); {
	case errors.Is(err, manifest.ErrEpochCheckpointBusy):
		warnRestore(opts, fmt.Sprintf("restore: wrote %s, but its new backup epoch is still in its WAL (%s): "+
			"the next open replays it, so leave %s and %s beside it", ManifestDBFileName, filepath.Base(target)+"-wal",
			filepath.Base(target)+"-wal", filepath.Base(target)+"-shm"))
		return nil
	case err != nil:
		return fmt.Errorf("mint backup epoch: %w", err)
	}
	for _, sidecar := range []string{target + "-wal", target + "-shm"} {
		if err := os.Remove(sidecar); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove epoch sidecar %s: %w", sidecar, err)
		}
	}
	return nil
}

// SetOnSourceWorkerEndForTest installs the hook a restore's source worker
// runs last, given the snapshot as the restore was given it, once it has
// handed its hold over or let go of what it took, and bridge restore's look
// before its prompt (CheckRestoreSource) once its look has answered, and
// returns the restore. Its callback must return, and ignore every snapshot
// but its test's.
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

// beforeLiveStoreHold, nil in production, runs in holdLiveStore once the
// database's directory is there and before the hold is taken, with the
// database's path: a test's stand-in for another process that makes the
// database and holds it as a restore does, in that window.
var beforeLiveStoreHold atomic.Pointer[func(dbPath string)]

// SetBeforeLiveStoreHoldForTest installs that seam and returns what puts the
// one before it back. Process-global: a test that uses it must not call
// t.Parallel().
func SetBeforeLiveStoreHoldForTest(fn func(dbPath string)) (restore func()) {
	var p *func(string)
	if fn != nil {
		p = &fn
	}
	prev := beforeLiveStoreHold.Swap(p)
	return func() { beforeLiveStoreHold.Store(prev) }
}

// holdLiveStore is the live snapshot's admission (captureInto): the
// database's directory made where it is not there, as the store's own open
// makes it (0755), then the store's lifetime lock held shared
// (manifest.HoldLiveStore) and, under it, the restore's journal asked
// (manifest.CheckRestoreJournal), before the capture writes anything else.
// There is no way through without the hold: a directory that was not there
// can be made, with a database in it, by a restore that holds it before the
// capture's VACUUM INTO opens that database (B653 PR 2a's second round).
// manifest.PendingMigrations, a reader that opens nothing where the directory
// is not there, makes no directory. release is never nil when err is nil.
func holdLiveStore(dbPath string) (release func(), err error) {
	if err := fsutil.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		return nil, fmt.Errorf("create the library database's directory for its snapshot: %w", err)
	}
	if fn := beforeLiveStoreHold.Load(); fn != nil {
		(*fn)(dbPath)
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
// database open in a directory that is not there. Under the hold it asks the
// restore's journal (manifest.CheckRestoreJournal), before the restore looks
// at its snapshot: an unfinished restore of the database refuses every
// caller with nothing restored, its snapshot unread (B653 PR 2a's second
// round). A nil hold and no error where the restore has no database target.
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
	if err := manifest.CheckRestoreJournal(dbTarget); err != nil {
		hold.Release()
		return nil, fmt.Errorf("%w; nothing was restored", err)
	}
	return hold, nil
}
