package manifest

import (
	"errors"
	"fmt"
	"os"
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
// and starts to wait. Nil in production. ladderTryLock takes the lock:
// fsutil.TryLock in production, and a test's stand-in for what a filesystem
// that keeps no locks, or one that fails, answers.
var (
	ladderLockHookMu  sync.Mutex
	ladderLockWaiting func(path string)
	ladderTryLock     = fsutil.TryLock
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

func setLadderTryLock(fn func(*os.File) error) func() {
	ladderLockHookMu.Lock()
	prev := ladderTryLock
	ladderTryLock = fn
	ladderLockHookMu.Unlock()
	return func() {
		ladderLockHookMu.Lock()
		ladderTryLock = prev
		ladderLockHookMu.Unlock()
	}
}

func tryLadderLock(f *os.File) error {
	ladderLockHookMu.Lock()
	try := ladderTryLock
	ladderLockHookMu.Unlock()
	return try(f)
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
// at all is logged and the ladder runs without it, as before: serve's
// data-dir lock makes the same call, since a lock that cannot be taken
// must not stop a bridge from starting. The line says which kind it met
// (B537 item 1): an answer fsutil.KeepsNoLocks reads, a filesystem that
// gave no lock (ENOLCK, EOPNOTSUPP, ENOTSUP, errors.ErrUnsupported and the
// ENOSYS it matches; Windows' ERROR_NOT_SUPPORTED and
// ERROR_INVALID_FUNCTION), worded to claim nothing of it, since ENOLCK is
// also an NFS mount whose lock manager is not running and a kernel out of
// lock records while another opener holds the lock; or any other failure,
// of the lock (a permission, I/O or descriptor error) or of the lock file's
// open (a lock file this user cannot open).
func lockLadder(dbPath string) (release func(), err error) {
	release = func() {
		// Nothing to release: no lock has been taken.
	}
	dir, name := filepath.Dir(dbPath), filepath.Base(dbPath)+ladderLockSuffix
	// unlocked logs that the ladder runs without its lock, noLocks saying
	// the lock's answer is one fsutil.KeepsNoLocks reads, and answers no
	// refusal.
	unlocked := func(noLocks bool, err error) (func(), error) {
		if noLocks {
			logger.Warn("no migration lock was granted: another process may still hold one, and the ladder runs without exclusive use of the database",
				"lock", name, "err", err)
			return release, nil
		}
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
		return unlocked(false, err)
	}
	start := time.Now()
	waiting, said := false, false
	for {
		err := tryLadderLock(f)
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
			return unlocked(fsutil.KeepsNoLocks(err), err)
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
