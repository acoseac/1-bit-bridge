package updater

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

// stateLockFileName is the file in the data dir on which the early rollback,
// a store open that migrates, serve's first disarm and the boot's
// housekeeping hold an exclusive kernel lock (fsutil.TryLock) while each
// reads and writes the install marker (LockState; backlog B613's rounds 3
// and 4). Never update-state.json
// itself: every write of the marker renames a new file over it, and a lock
// on the old file would not follow. Never removed: only the lock on it
// means anything, and unlinking a lock file while it is held lets the next
// taker lock a new file at the same path beside the old holder.
const stateLockFileName = "update-state.lock"

// StateLockPath is where the install marker's lock lives in dataDir, beside
// the marker (StatePath).
func StateLockPath(dataDir string) string {
	return filepath.Join(dataDir, stateLockFileName)
}

// stateLockRetry is how often LockState asks for the lock while it waits.
const stateLockRetry = 50 * time.Millisecond

// stateLockHookMu guards the test seams below: stateLockWait, which bounds
// how long LockState waits for another holder (the early rollback holds the
// lock across its backup's version, ProbeVersion's 10 s at most, a record and
// a rename; a disarm, across one marker write; the boot's housekeeping,
// across two marker writes and a rename or the backup's removal), and which a test shortens
// (SetStateLockWaitForTest); stateTryLock, fsutil.TryLock in production and
// a test's stand-in for what a filesystem answers; and stateLockWaiting, nil
// in production, run once when a taker finds the lock held and starts to
// wait. Production code MUST NOT mutate them.
var (
	stateLockHookMu  sync.Mutex
	stateLockWait    = 30 * time.Second
	stateTryLock     = fsutil.TryLock
	stateLockWaiting func(dataDir string)
)

func stateLockWaitNow() time.Duration {
	stateLockHookMu.Lock()
	defer stateLockHookMu.Unlock()
	return stateLockWait
}

func tryStateLock(f *os.File) error {
	stateLockHookMu.Lock()
	try := stateTryLock
	stateLockHookMu.Unlock()
	return try(f)
}

func callStateLockWaiting(dataDir string) {
	stateLockHookMu.Lock()
	fn := stateLockWaiting
	stateLockHookMu.Unlock()
	if fn != nil {
		fn(dataDir)
	}
}

// StateLock is a hold of the install marker's lock (LockState). Held says
// the lock was taken; a hold that is not held is a filesystem that keeps no
// locks (fsutil.KeepsNoLocks's definitive answers), where no process can
// hold one. Release lets it go; it is safe on a nil or unheld lock.
type StateLock struct {
	f *os.File
	// unheld is the lock's answer on a filesystem that keeps none.
	unheld error
}

// Held reports whether the lock was taken.
func (l *StateLock) Held() bool { return l != nil && l.f != nil }

// Unheld is why no lock was taken on a filesystem that keeps none, nil for a
// lock that was.
func (l *StateLock) Unheld() error {
	if l == nil {
		return nil
	}
	return l.unheld
}

// Release lets the lock go: unlocked before the close, since on Windows the
// release a close makes can lag.
func (l *StateLock) Release() {
	if l == nil || l.f == nil {
		return
	}
	_ = fsutil.Unlock(l.f)
	_ = l.f.Close()
	l.f = nil
}

// ErrStateLockHeld is what a *StateLockError matches when another open held
// the lock for all of LockState's wait.
var ErrStateLockHeld = errors.New("another process holds the install marker's lock")

// StateLockError is LockState's refusal: the lock could not be taken. held
// says another open held it for the whole wait (ErrStateLockHeld); noLock,
// that every try answered there was no lock to give (ENOLCK: an NFS mount
// whose lock manager is not running, or a kernel out of lock records, which
// does not show that the filesystem keeps none); otherwise Err is the lock
// file's open, or the lock's own failure. It names the lock file by its
// role, never its path.
type StateLockError struct {
	held, noLock bool
	Wait         time.Duration
	Err          error
}

func (e *StateLockError) Error() string {
	switch {
	case e.held:
		return fmt.Sprintf("another process held the install marker's lock for %s", e.Wait.Round(time.Second))
	case e.noLock:
		return fmt.Sprintf("no lock was given on the install marker's lock file for %s (%s): "+
			"the filesystem's lock manager may not be running", e.Wait.Round(time.Second), fsutil.ErrorWithoutPath(e.Err))
	}
	return fmt.Sprintf("the install marker's lock could not be taken (%s)", fsutil.ErrorWithoutPath(e.Err))
}

// Is makes a *StateLockError of a held lock match ErrStateLockHeld.
func (e *StateLockError) Is(target error) bool { return e.held && target == ErrStateLockHeld }

func (e *StateLockError) Unwrap() error { return e.Err }

// noLocksSaid keeps LockState's line about a filesystem that keeps no locks
// to one a process.
var noLocksSaid sync.Once

// LockState takes the exclusive lock on <dataDir>/update-state.lock, the one
// lock the early rollback, every store open that migrates, serve's first
// disarm and the boot's housekeeping share around their reads and writes of
// the install marker (backlog B613's rounds 3 and 4): a rollback holds it
// from its marker read through the backup's rename and any write that puts
// the install back, a migration through its durable record and its barrier,
// serve's first disarm until its record is durable, and the housekeeping
// from its marker read through its decision, its writes and the backup's
// rename or removal (maybeRollbackOnBoot in cmd/bridge, which takes it
// inside the data dir's, once the store's open has let the ladder go). The order of the locks is serve's data dir lock
// (server.lock), then the store's migration lock (the ladder, beside the
// database), then this one; a caller takes the ones it needs in that order,
// and nothing that holds this one takes either of the others. A caller takes
// it once and runs every helper it needs under it: none of the helpers takes
// it.
//
// It waits up to stateLockWait for another holder, asking every
// stateLockRetry, and refuses with a *StateLockError: the lock held for the
// whole wait, every try answered ENOLCK, the lock file's open failed, or the
// lock failed another way. The wait ends sooner when ctx does, with ctx's
// error, never a *StateLockError, so a caller tells its own stop from a lock
// it could not take (backlog B613's round 4): serve passes its own context
// from its admission (AdmitBoot), its early rollback and its boot's
// housekeeping, and a store open, which has none, keeps the bound alone
// (DisarmBeforeMigrating). A context that has ended does not stop a take
// that needs no wait. A filesystem whose lock says it keeps none
// (fsutil.KeepsNoLocks's EOPNOTSUPP, ENOTSUP and errors.ErrUnsupported,
// Windows' ERROR_NOT_SUPPORTED and ERROR_INVALID_FUNCTION; never ENOLCK) is
// a hold that is not held (Held false, Unheld its answer), said once a
// process: there no process can hold the lock, and a caller that needs it to
// exclude another refuses (the early rollback, the boot's housekeeping),
// where one that only writes beside other writers of its kind goes on (a
// disarm). The lock file is opened through
// fsutil.OpenLockFile, so a link there is refused and a root caller makes it
// the data dir's owner's.
func LockState(ctx context.Context, dataDir string) (*StateLock, error) {
	f, err := fsutil.OpenLockFile(dataDir, stateLockFileName)
	if err != nil {
		return nil, &StateLockError{Err: err}
	}
	wait := stateLockWaitNow()
	deadline := time.Now().Add(wait)
	waiting, held := false, false
	var noLock error
	for {
		err := tryStateLock(f)
		switch {
		case err == nil:
			return &StateLock{f: f}, nil
		case errors.Is(err, fsutil.ErrLocked) || errors.Is(err, syscall.ENOLCK):
			if errors.Is(err, fsutil.ErrLocked) {
				held = true
				if !waiting {
					waiting = true
					callStateLockWaiting(dataDir)
				}
			} else {
				noLock = err
			}
			if !time.Now().Before(deadline) {
				_ = f.Close()
				if held {
					return nil, &StateLockError{held: true, Wait: wait}
				}
				return nil, &StateLockError{noLock: true, Wait: wait, Err: noLock}
			}
			if err := sleepUnlessDone(ctx, stateLockRetry); err != nil {
				_ = f.Close()
				return nil, err
			}
		case fsutil.KeepsNoLocks(err):
			_ = f.Close()
			noLocksSaid.Do(func() {
				logger.Warn("the data dir's filesystem keeps no locks, so nothing keeps a store's migration and an install's rollback "+
					"apart there: neither the early rollback nor a start's housekeeping of the install marker runs, "+
					"and the rest goes on without the install marker's lock",
					"lock", stateLockFileName, "reason", fsutil.ErrorWithoutPath(err))
			})
			return &StateLock{unheld: err}, nil
		default:
			_ = f.Close()
			return nil, &StateLockError{Err: err}
		}
	}
}

// sleepUnlessDone waits d, or until ctx ends, whose error it then returns.
func sleepUnlessDone(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// SetStateLockWaitingForTest makes fn run once each time a LockState finds
// the lock held and starts to wait, with the data dir it waits on, for a
// test in another package that orders two takers. It returns what puts the
// seam before it back. Process-global: a test that uses it MUST NOT call
// t.Parallel(), and restores it in a t.Cleanup.
func SetStateLockWaitingForTest(fn func(dataDir string)) (restore func()) {
	stateLockHookMu.Lock()
	prev := stateLockWaiting
	stateLockWaiting = fn
	stateLockHookMu.Unlock()
	return func() {
		stateLockHookMu.Lock()
		stateLockWaiting = prev
		stateLockHookMu.Unlock()
	}
}

// SetStateTryLockForTest makes every try at the install marker's lock answer
// what try answers, in place of fsutil.TryLock, for a test in another package
// that stands in for what a filesystem answers (no lock to give, none kept, a
// failure). It returns what puts the seam before it back. Process-global, as
// SetStateLockWaitingForTest is.
func SetStateTryLockForTest(try func(*os.File) error) (restore func()) {
	stateLockHookMu.Lock()
	prev := stateTryLock
	stateTryLock = try
	stateLockHookMu.Unlock()
	return func() {
		stateLockHookMu.Lock()
		stateTryLock = prev
		stateLockHookMu.Unlock()
	}
}

// SetStateLockWaitForTest shortens LockState's wait for a test in another
// package. It returns what puts the wait before it back. Process-global, as
// SetStateLockWaitingForTest is.
func SetStateLockWaitForTest(d time.Duration) (restore func()) {
	stateLockHookMu.Lock()
	prev := stateLockWait
	stateLockWait = d
	stateLockHookMu.Unlock()
	return func() {
		stateLockHookMu.Lock()
		stateLockWait = prev
		stateLockHookMu.Unlock()
	}
}
