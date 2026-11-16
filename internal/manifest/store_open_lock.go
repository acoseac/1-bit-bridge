package manifest

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

// storeOpenLockSuffix names the file beside the database on which every
// opener of the live database holds a kernel lock for as long as it has the
// database open, shared, and a restore holds it exclusively (backlog B653 PR
// 2a, the store's lifetime lock): the full restore since PR 2a, and the
// database-only restore once PR 2b brings it. `bridge restore` kept a
// running serve out by server.lock, and nothing kept out the commands that
// open the store without it (scan, analyze, upscale, variants, artwork,
// library add and remove, the manifest commands, the doctor's probes, a live
// backup's VACUUM INTO): a restore renamed a fresh database over the one such
// a process had open, which went on writing into the unlinked files, and its
// later connections, opened by path, wrote into the restored one from a
// process that read the old one. The ladder's lock (ladderLockSuffix) lasts
// through the open alone.
//
// The file is a sidecar like the ladder's lock (storeOpenPaths): a link there
// is refused, and the lock file is opened without following one
// (fsutil.OpenLockFileNoFollow), so a link to nothing makes nothing. It is
// never removed or moved: only the lock on it means anything.
const storeOpenLockSuffix = "-open.lock"

// StoreOpenLockPath is the lifetime lock's file for the database at dbPath.
func StoreOpenLockPath(dbPath string) string { return dbPath + storeOpenLockSuffix }

// The lifetime lock's refusals. Each names the lock file by its base name
// and nothing else; each is a *storeLockError, which matches its kind and
// what the lock answered with errors.Is.
var (
	// ErrStoreRestoring: a shared try (an opener's) met an exclusive hold,
	// another process's restore.
	ErrStoreRestoring = errors.New("a bridge restore holds the library database")
	// ErrStoreHeld: an exclusive try (a restore's) met a holder, in another
	// process or this one.
	ErrStoreHeld = errors.New("a bridge command has the library database open")
	// ErrStoreLockUnavailable: no lock could be taken, for any reason but the
	// filesystem's definitive answer that it keeps none
	// (fsutil.DefinitelyKeepsNoLocks), which lets an opener go on and refuses
	// a restore; ENOLCK, a lock file that cannot be opened or is a link, an
	// I/O or a descriptor error all refuse.
	ErrStoreLockUnavailable = errors.New("the library database's lock could not be taken")
)

// storeLockError is a refusal of the lifetime lock: its kind, the lock file's
// base name and, where the lock or its file's open answered one, what it
// answered, said without its path.
type storeLockError struct {
	kind  error
	lock  string
	cause error
}

func (e *storeLockError) Error() string {
	s := fmt.Sprintf("%v (%s)", e.kind, e.lock)
	switch {
	case errors.Is(e.kind, ErrStoreRestoring):
		s += ": let the restore finish, then run this again"
	case errors.Is(e.kind, ErrStoreHeld):
		s += ": stop it, then restore again"
	}
	if e.cause != nil {
		s += ": " + fsutil.ErrorWithoutPath(e.cause)
	}
	return s
}

func (e *storeLockError) Unwrap() []error {
	if e.cause == nil {
		return []error{e.kind}
	}
	return []error{e.kind, e.cause}
}

// storeLockEntry is this process's one hold of one database's lifetime lock:
// one descriptor, however many holders. On a Linux NFS mount flock is carried
// out as a POSIX record lock, which every open of the file in one process
// shares and closing any of them releases (fsutil.TryLock's doc), so a second
// descriptor, serve's store beside its ticker's snapshot, would drop the
// first's hold when it closed. f is nil where the filesystem answered that it
// keeps no locks and the openers went on without one. fi is the lock file as
// it was opened, its identity, by which a hold through another spelling of
// the same file finds this entry (storeLockEntryOf); nil where its stat
// failed.
type storeLockEntry struct {
	f      *os.File
	fi     os.FileInfo
	shared int
	excl   int
}

// storeLocks is the process's registry of holds, keyed by the database's
// absolute, cleaned path (storeLockKey), and found by the lock file's
// identity whatever the key (storeLockEntryOf). Its mutex is held across a
// lock file's lookup, its open and its try, none of which waits for anything.
var storeLocks = struct {
	sync.Mutex
	byKey map[string]*storeLockEntry
}{byKey: map[string]*storeLockEntry{}}

// storeLockKey keys the registry: the database's path made absolute and
// cleaned, its directory resolved where it is there (filepath.EvalSymlinks:
// macOS's /var reached as /private/var, a Windows short name); a directory
// not there yet keys as written. A key is a spelling, not an identity: on a
// volume that folds case (macOS's APFS by default) EvalSymlinks keeps each
// component as it was written, so /data/Library and /data/library are two
// keys for one directory, and a hard link is another name for the same lock
// file. The entry is therefore found by the lock file's identity whenever its
// key finds none (storeLockEntryOf).
func storeLockKey(dbPath string) string {
	abs, err := filepath.Abs(filepath.Clean(dbPath))
	if err != nil {
		abs = filepath.Clean(dbPath)
	}
	if dir, err := filepath.EvalSymlinks(filepath.Dir(abs)); err == nil {
		return filepath.Join(dir, filepath.Base(abs))
	}
	return abs
}

// storeLockEntryOf returns this process's entry for the database at dbPath
// and the key it is kept under, with storeLocks held: the entry key names,
// else the one whose lock file is the file at the lock's path now
// (os.SameFile against each entry's fi), whatever spelling took it, so the
// caller counts on it and its release finds it by that key; nil where it has
// none. It opens nothing: a match is decided before another descriptor
// could be opened on a file this process holds (B653 PR 2a's second round).
// A lock file that is not a regular file matches nothing, and is the open's
// to refuse.
func storeLockEntryOf(key, dbPath string) (string, *storeLockEntry) {
	if e := storeLocks.byKey[key]; e != nil {
		return key, e
	}
	if len(storeLocks.byKey) == 0 {
		return "", nil
	}
	fi, err := os.Lstat(StoreOpenLockPath(dbPath))
	if err != nil || !fi.Mode().IsRegular() {
		return "", nil
	}
	for k, e := range storeLocks.byKey {
		if e.fi != nil && os.SameFile(e.fi, fi) {
			return k, e
		}
	}
	return "", nil
}

// lockFileInfo is f's identity for its entry (storeLockEntry.fi), nil where
// it cannot be read.
func lockFileInfo(f *os.File) os.FileInfo {
	fi, err := f.Stat()
	if err != nil {
		return nil
	}
	return fi
}

// The lifetime lock's seams, the platform's calls in production: the lock
// file's open (write says for writing), the try (exclusive says which kind)
// and the unlock. A test stands in for a filesystem's answers through them
// (setStoreLockSeams). storeHoldTaken runs in OpenStoreAdmitted and
// PendingMigrations once the shared hold is taken and before the journal is
// asked, nil in production.
var (
	storeLockSeamMu sync.Mutex
	storeLockOpen   = func(dir, name, like string, write bool) (*os.File, error) {
		return fsutil.OpenLockFileNoFollow(dir, name, like, 0o644, write)
	}
	storeLockTry = func(f *os.File, exclusive bool) error {
		if exclusive {
			return fsutil.TryLock(f)
		}
		return fsutil.TryLockShared(f)
	}
	storeLockUnlock = fsutil.Unlock
	storeHoldTaken  func(dbPath string)
)

// storeLockSeams returns the seams as they stand.
func storeLockSeams() (open func(dir, name, like string, write bool) (*os.File, error), try func(*os.File, bool) error, unlock func(*os.File) error) {
	storeLockSeamMu.Lock()
	defer storeLockSeamMu.Unlock()
	return storeLockOpen, storeLockTry, storeLockUnlock
}

// callStoreHoldTaken runs storeHoldTaken where a test set it.
func callStoreHoldTaken(dbPath string) {
	storeLockSeamMu.Lock()
	fn := storeHoldTaken
	storeLockSeamMu.Unlock()
	if fn != nil {
		fn(dbPath)
	}
}

// noStoreLockSaid keeps the line that the openers go on without the lock to
// once a process.
var noStoreLockSaid atomic.Bool

// sayNoStoreLock says, once a process, that a shared hold went on without the
// lock, where the lock's try answered err, a definitive no-locks answer. It
// claims nothing of the filesystem beyond what it answered.
func sayNoStoreLock(name string, err error) {
	if noStoreLockSaid.Swap(true) {
		return
	}
	logger.Warn("the library database's lock was not granted: its filesystem answered that it takes no locks, so the store opens "+
		"without it, and a bridge restore refuses to run on this storage",
		"lock", name, "err", fsutil.ErrorWithoutPath(err))
}

// holdStoreShared takes this process's shared hold of the lifetime lock of
// the database at dbPath, for an opener of the live database, and returns
// what lets it go (once, however often it is called). A hold this process has
// already, shared or exclusive, through this spelling or another of the same
// lock file (storeLockEntryOf), is counted on (the full restore's own epoch
// open counts on the restore's exclusive hold). Otherwise the lock file is
// opened for writing, and for reading where that open is refused for want of
// the right to write (fsutil.OpenRefusedForWriting: a doctor run by another
// user who may read the database), and the lock tried shared: another
// process's exclusive hold refuses (ErrStoreRestoring); the filesystem's
// definitive answer that it keeps no locks goes on without one, said once;
// and every other answer refuses (ErrStoreLockUnavailable), ENOLCK included,
// which Linux also gives beside a holder (backlog B653 PR 2a, the design
// review's finding on ENOLCK: an opener that went on there could stand
// beside a restore). Nothing of the database is touched before it answers.
func holdStoreShared(dbPath string) (release func(), err error) {
	key := storeLockKey(dbPath)
	storeLocks.Lock()
	defer storeLocks.Unlock()
	if k, e := storeLockEntryOf(key, dbPath); e != nil {
		e.shared++
		return storeLockRelease(k, false), nil
	}
	dir, name := filepath.Dir(dbPath), filepath.Base(dbPath)+storeOpenLockSuffix
	open, try, _ := storeLockSeams()
	f, err := open(dir, name, dbPath, true)
	if fsutil.OpenRefusedForWriting(err) {
		f, err = open(dir, name, dbPath, false)
	}
	if err != nil {
		return nil, storeLockOpenRefusal(dbPath, name, err)
	}
	switch err := try(f, false); {
	case err == nil:
		storeLocks.byKey[key] = &storeLockEntry{f: f, fi: lockFileInfo(f), shared: 1}
	case errors.Is(err, fsutil.ErrLocked):
		_ = f.Close()
		return nil, &storeLockError{kind: ErrStoreRestoring, lock: name}
	case fsutil.DefinitelyKeepsNoLocks(err):
		fi := lockFileInfo(f)
		_ = f.Close()
		sayNoStoreLock(name, err)
		storeLocks.byKey[key] = &storeLockEntry{fi: fi, shared: 1}
	default:
		_ = f.Close()
		return nil, &storeLockError{kind: ErrStoreLockUnavailable, lock: name, cause: err}
	}
	return storeLockRelease(key, false), nil
}

// HoldLiveStore is holdStoreShared for the live snapshot's capture
// (internal/backup): its VACUUM INTO opens the live database as a store open
// does, so it holds the lifetime lock shared from before it touches anything
// until its source has closed. Serve's ticker counts on serve's own hold.
func HoldLiveStore(dbPath string) (release func(), err error) {
	return holdStoreShared(dbPath)
}

// storeLockOpenRefusal is the refusal of a lock file that could not be
// opened: a link there is the store's link refusal (errStoreSymlink) as well.
func storeLockOpenRefusal(dbPath, name string, err error) error {
	if errors.Is(err, fsutil.ErrLockFileNotAFile) {
		if fi, lerr := os.Lstat(StoreOpenLockPath(dbPath)); lerr == nil && fi.Mode()&os.ModeSymlink != 0 {
			err = fmt.Errorf("%w at %s", errStoreSymlink, name)
		}
	}
	return &storeLockError{kind: ErrStoreLockUnavailable, lock: name, cause: err}
}

// StoreHold is a restore's exclusive hold of the lifetime lock
// (HoldStoreExclusive).
type StoreHold struct {
	release func()
}

// Release lets the hold go; the lock is let go once this process holds it no
// more, unlocked before its file is closed. Safe on a nil hold, and more than
// once.
func (h *StoreHold) Release() {
	if h != nil && h.release != nil {
		h.release()
	}
}

// HoldStoreExclusive takes the lifetime lock of the database at dbPath
// exclusively, for a restore (backlog B653 PR 2a: the full restore, and the
// database-only restore once PR 2b brings it), and refuses on anything but
// success: a holder in another process (ErrStoreHeld: a command has the
// database open), a shared hold of this process's (ErrStoreHeld too), and
// any failure to take it (ErrStoreLockUnavailable), the filesystem's answer
// that it keeps no locks included, since a restore there cannot show that
// nothing has the database open. A hold this process has exclusively already,
// through this spelling or another of the same lock file
// (storeLockEntryOf), is counted on (bridge restore takes it before its
// prompt, and RestoreWithOptions takes it again). It is a try, never a wait.
// The lock file is opened for writing, as an exclusive lock on a Linux NFS
// mount needs.
func HoldStoreExclusive(dbPath string) (*StoreHold, error) {
	key := storeLockKey(dbPath)
	name := filepath.Base(dbPath) + storeOpenLockSuffix
	storeLocks.Lock()
	defer storeLocks.Unlock()
	if k, e := storeLockEntryOf(key, dbPath); e != nil {
		if e.excl > 0 {
			e.excl++
			return &StoreHold{release: storeLockRelease(k, true)}, nil
		}
		return nil, &storeLockError{kind: ErrStoreHeld, lock: name}
	}
	open, try, _ := storeLockSeams()
	f, err := open(filepath.Dir(dbPath), name, dbPath, true)
	if err != nil {
		return nil, storeLockOpenRefusal(dbPath, name, err)
	}
	switch err := try(f, true); {
	case err == nil:
		storeLocks.byKey[key] = &storeLockEntry{f: f, fi: lockFileInfo(f), excl: 1}
	case errors.Is(err, fsutil.ErrLocked):
		_ = f.Close()
		return nil, &storeLockError{kind: ErrStoreHeld, lock: name}
	default:
		_ = f.Close()
		return nil, &storeLockError{kind: ErrStoreLockUnavailable, lock: name, cause: err}
	}
	return &StoreHold{release: storeLockRelease(key, true)}, nil
}

// storeLockRelease is what lets one hold of key go, once: the last hold of
// the process unlocks, then closes (unlock before close, as lockLadder does:
// on Windows the release a close makes can lag).
func storeLockRelease(key string, exclusive bool) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			storeLocks.Lock()
			defer storeLocks.Unlock()
			e := storeLocks.byKey[key]
			if e == nil {
				return
			}
			if exclusive {
				e.excl--
			} else {
				e.shared--
			}
			if e.shared > 0 || e.excl > 0 {
				return
			}
			delete(storeLocks.byKey, key)
			if e.f != nil {
				_, _, unlock := storeLockSeams()
				_ = unlock(e.f)
				_ = e.f.Close()
			}
		})
	}
}

// storeHoldPollStart and storeHoldPollMax pace the wait of a closed store for
// its last connection (letGoOfTheHold): the first pause, doubling to the
// last. Variables so tests can shorten them.
var (
	storeHoldPollStart = 5 * time.Millisecond
	storeHoldPollMax   = 100 * time.Millisecond
)

// letGoOfTheHold lets the store's lifetime hold go once every connection
// SQLite holds for it has closed, after db.Close has returned: database/sql
// closes the idle connections at its Close, and one a transaction, a Conn or
// an open Rows holds when it returns it, so a transaction under way at Close
// can still commit after Close has returned (B653 PR 2a's brief review).
// Where none is open the hold goes at once; otherwise a goroutine waits for
// the last one, never Close's caller.
func (s *Store) letGoOfTheHold() {
	if s.releaseHold == nil {
		return
	}
	s.holdOnce.Do(func() {
		if s.db.Stats().OpenConnections == 0 {
			s.releaseHold()
			return
		}
		go func() {
			pause := storeHoldPollStart
			for s.db.Stats().OpenConnections > 0 {
				time.Sleep(pause)
				pause = min(2*pause, storeHoldPollMax)
			}
			s.releaseHold()
		}()
	})
}
