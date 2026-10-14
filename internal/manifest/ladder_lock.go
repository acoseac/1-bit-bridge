package manifest

import (
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

// ladderLockSuffix names the file beside the database on which OpenStore
// holds an exclusive kernel lock (fsutil.TryLock) from before SQLite opens
// the database until the migration ladder has run (backlog B185). Two
// processes opening one older database at once (a `bridge doctor` run with
// the new binary beside the new serve's first start) ran the ladder side by
// side. Each read user_version once, so both ran every step from it, and a
// step that checks before it writes failed the slower one: the column
// helper's probe and ALTER ("duplicate column name", 78 of 120 opens in the
// reviewer's probe on a real v0.2.0 database), migration 52's epoch row
// ("UNIQUE constraint failed: backup_epoch.id"). The slower opener now
// waits, then reads the version the faster one left and runs nothing. No
// shipped migration changes, and the frozen column helper does not either.
//
// The file is a sidecar like -wal and -shm (storeOpenPaths): a symlink
// there is refused. It is never removed: only the lock on it means
// anything, and unlinking a lock file while it is held lets the next opener
// lock a NEW file at the same path beside the old holder.
const ladderLockSuffix = "-migrate.lock"

// ladderLockWait bounds how long an opener waits for another opener's
// ladder, which can run backfills over a whole library: seconds to
// minutes. ladderLockPoll is how often it tries the lock, and
// ladderLockSayAfter is when it logs that it is waiting. Variables so tests
// can shorten them.
var (
	ladderLockWait     = 5 * time.Minute
	ladderLockPoll     = 50 * time.Millisecond
	ladderLockSayAfter = 2 * time.Second
)

// errLadderLockTimeout is OpenStore's error when another opener held the
// ladder lock for all of ladderLockWait.
var errLadderLockTimeout = errors.New("timed out waiting for another process's migration of the database")

// ladderLockWaiting runs once when an opener finds the ladder lock held
// and starts to wait. Nil in production.
var (
	ladderLockHookMu  sync.Mutex
	ladderLockWaiting func(path string)
)

func setLadderLockWaiting(fn func(string)) func() {
	ladderLockHookMu.Lock()
	prev := ladderLockWaiting
	ladderLockWaiting = fn
	ladderLockHookMu.Unlock()
	return func() {
		ladderLockHookMu.Lock()
		ladderLockWaiting = prev
		ladderLockHookMu.Unlock()
	}
}

func callLadderLockWaiting(path string) {
	ladderLockHookMu.Lock()
	fn := ladderLockWaiting
	ladderLockHookMu.Unlock()
	if fn != nil {
		fn(path)
	}
}

// lockLadder takes the lock that keeps a second opener of the database at
// dbPath from running the migration ladder while another runs it, waiting
// up to ladderLockWait for a holder to finish, and returns what releases
// it. release is never nil, and does nothing when no lock was taken.
//
// It fails when the wait runs out (errLadderLockTimeout): proceeding then
// would run this opener's ladder beside a holder's that may still be
// migrating. It also fails when a link stands at the lock file, planted
// since OpenStore's own check, with the refusal a link at the database or
// another sidecar gets (errStoreSymlink), never taken for a filesystem
// that keeps no locks (Argon on the B185 PR). A lock that cannot be taken
// at all (a filesystem that keeps no locks, a lock file this user cannot
// open) is logged and the ladder runs without it, as before: serve's
// data-dir lock makes the same call, since a lock that cannot be taken
// must not stop a bridge from starting.
func lockLadder(dbPath string) (release func(), err error) {
	release = func() {
		// Nothing to release: no lock has been taken.
	}
	dir, name := filepath.Dir(dbPath), filepath.Base(dbPath)+ladderLockSuffix
	unlocked := func(err error) (func(), error) {
		logger.Warn("could not take the migration lock; migrating without it", "lock", name, "err", err)
		return release, nil
	}
	// The directory's owner's file, never followed out of the directory,
	// and opened again when a create races another opener's on macOS 27
	// (fsutil.OpenLockFile).
	f, err := fsutil.OpenLockFile(dir, name)
	if err != nil {
		if link, ok := symlinkAtStore(dbPath); ok {
			return release, fmt.Errorf("%w at %s", errStoreSymlink, link)
		}
		return unlocked(err)
	}
	start := time.Now()
	waiting, said := false, false
	for {
		err := fsutil.TryLock(f)
		if err == nil {
			return func() {
				// Unlock before the close: on Windows the release a close
				// makes can lag, and the next opener would find it held.
				_ = fsutil.Unlock(f)
				_ = f.Close()
			}, nil
		}
		if !errors.Is(err, fsutil.ErrLocked) {
			_ = f.Close()
			return unlocked(err)
		}
		if !waiting {
			waiting = true
			callLadderLockWaiting(dbPath)
		}
		waited := time.Since(start)
		if waited >= ladderLockWait {
			_ = f.Close()
			return release, fmt.Errorf("%w: %s held for %s", errLadderLockTimeout, name, waited.Round(time.Second))
		}
		if !said && waited >= ladderLockSayAfter {
			said = true
			logger.Info("waiting for another process's migration of the database", "lock", name)
		}
		time.Sleep(ladderLockPoll)
	}
}
