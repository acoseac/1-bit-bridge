package updater

import (
	"context"
	"errors"
	"fmt"

	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

// The updater's own writes of the install marker (backlog B692): Install,
// the operator's Rollback, the refused candidate's hold (recordHold) and the
// auto-installer's restamp (refreshInstallAttemptedAt) each hold the install
// marker's lock (update-state.lock, LockState) from their read of the marker
// through their writes and, for Install and Rollback, the binary's swap, as
// the early rollback, serve's admission, the boot's housekeeping and every
// migrating store open have since B613. They run in serve (the console's
// install and rollback, the auto-installer with its hold and its restamp),
// which holds server.lock for its life and whose store's open has let the
// ladder go by then, or in `bridge update`'s own process, which holds
// neither; none of them takes server.lock or the ladder while it holds this
// one, so the documented order (server.lock, the ladder, then
// update-state.lock) holds. Each takes it once: the helpers under it
// (recordRejection, putMarkerBack, swapAwaitingRestart, HeldBack) take no
// lock.

// markerLockError is an updater write refused for the install marker's lock:
// what was not done, and why, in words that name no path. It matches
// ErrMarkerLockUnavailable, and unwraps to the lock's own answer, a
// *StateLockError or the error of a filesystem that keeps no locks.
type markerLockError struct {
	what    string
	cause   error
	noLocks bool
}

func (e *markerLockError) Error() string {
	switch {
	case e.noLocks:
		return fmt.Sprintf("%s: the data dir's filesystem keeps no locks (%s), so nothing would keep a store's migration "+
			"or a start's housekeeping of the install marker apart from it: stop the bridge and replace its binary by hand",
			e.what, fsutil.ErrorWithoutPath(e.cause))
	case errors.Is(e.cause, ErrStateLockHeld):
		return fmt.Sprintf("%s: %s (a start of the bridge, a store's migration, or another install or rollback): "+
			"try again once it is done", e.what, e.cause)
	}
	return fmt.Sprintf("%s: %s", e.what, e.cause)
}

func (e *markerLockError) Is(target error) bool { return target == ErrMarkerLockUnavailable }

func (e *markerLockError) Unwrap() error { return e.cause }

// lockMarkerForUpdate takes the install marker's lock in dataDir for one of
// the updater's writes, what saying what is left undone where it cannot be
// held ("nothing was installed"). Where the lock is held past LockState's
// wait, every try answers ENOLCK, the lock fails another way, or the
// filesystem keeps no locks (EOPNOTSUPP, ENOTSUP, errors.ErrUnsupported:
// LockState's hold that is not held), it refuses with a *markerLockError,
// before the caller reads, writes or swaps anything: a write the lock does
// not keep apart from a migration's record or the boot's housekeeping is the
// interleaving this lock exists to stop, and on a filesystem that keeps none
// the updater does not write at all, where a store's migration goes on
// without it (backlog B692, the brief's binding decision). A wait ctx ended
// returns ctx's error, never a refusal of the lock's; and since LockState
// takes a lock no one holds whatever ctx says, ctx is asked again once it is
// taken, so a caller stopped by then mutates nothing.
func lockMarkerForUpdate(ctx context.Context, dataDir, what string) (*StateLock, error) {
	lock, err := LockState(ctx, dataDir)
	if err != nil {
		var refused *StateLockError
		if errors.As(err, &refused) {
			return nil, &markerLockError{what: what, cause: err}
		}
		return nil, err
	}
	if !lock.Held() {
		return nil, &markerLockError{what: what, cause: lock.Unheld(), noLocks: true}
	}
	if err := ctx.Err(); err != nil {
		lock.Release()
		return nil, err
	}
	return lock, nil
}

// putMarkerBack writes back the marker an install or a rollback read under
// the install marker's lock, where its swap failed and the binaries are as
// they were: prior as read, or no marker where there was none or it could
// not be read (readErr), as Install's swap failure always left it. The lock
// is the caller's.
func putMarkerBack(dataDir string, prior State, readErr error) error {
	if readErr != nil || prior == (State{}) {
		return ClearState(dataDir)
	}
	return SaveState(dataDir, prior)
}
