package backup

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"time"
)

// A restore's source (backlog B826). A restore reads the snapshot it restores
// holding the lock of the folder that holds it, LockFileName, the one every
// verification, claim and prune of that folder takes, in this process or
// another: from before it touches the destination, a player seal it completes
// there included, until it returns. So no prune removes the snapshot while it
// is restored, and no restore reads one that a hold given up (stuck.go) still
// holds, whose step may have its files open still.
//
// The folder is the snapshot's parent where the snapshot resolves
// (resolveSnapshotIn), whatever it is named and wherever it is: a live backups
// folder and a copy of a snapshot kept elsewhere alike, since nothing tells
// them apart for certain. The manifest records no folder, and a live folder
// holds no lock file until its first verification, claim or prune makes one (a
// capture takes no lock), so a missing lock file is no mark of a copy. The
// restore makes the lock file where it is missing, as every holder does
// (fsutil.OpenLockFile, which as root gives it the folder's owner); where no
// lock can be had there, on a filesystem that keeps none or one that takes no
// file (read-only media, a folder this user cannot write), it is refused,
// ErrBackupsUnlocked, nothing restored, and a copy is restored from a folder
// that grants the lock.
//
// Before anything asks a filesystem, this process's marks are asked by the
// spelling given (SourceStillHeld): a hold given up there refuses the restore
// at once, ErrCheckStillRunning, its folder never resolved, since the
// resolution can hang on the very mount the hold's step stalled on. Then the
// snapshot is resolved, the resolved folder's marks are asked, and its lock is
// taken (lockBackupsWith): waited for within the restore's wait while another
// holder has it, ErrBackupsBusy past it, and ErrCheckStillRunning at once
// where that holder is a hold given up (this process's mark, or another
// process's marker beside the lock). Held, a marker that a process which has
// ended left there is removed (clearStaleMarker), and the manifest is read
// again: the snapshot its caller read before its prompt may have been pruned
// meanwhile (ErrSnapshotGone).
//
// All of that runs on a worker of its own, since the lock's wait bounds its
// polling alone: a resolution, the lock file's open or a marker's read can
// hang on a mount that stopped answering. The restore waits for the worker
// until its context ends or its wait and watchdogSlack have passed, then
// StopWriteBound more, and is then refused, nothing restored, the worker left
// to end on its own: its context ended, what it takes from then on it lets go
// at once, writing nothing. The restore writes the destination on its
// caller's goroutine alone, once the worker has handed the lock over, so
// nothing is written there once it has answered. Once it holds the lock its
// context no longer stops it, since a restore stopped between its writes would
// leave the install half restored; and its tenure is not bounded as a hold's
// steps are (hold.go): a restore is a command of its own, whose end, however
// it comes, lets the kernel's lock go.

// ErrSnapshotGone is a restore's refusal of a snapshot that is no longer
// there, or whose manifest is no longer there, once the restore holds the
// lock of the folder it was in: a prune may have removed it while the
// restore's caller asked for confirmation. Nothing was restored.
var ErrSnapshotGone = errors.New("the snapshot is no longer there")

// onSourceWorkerEnd, nil in production, runs last on a restore's source
// worker, given the snapshot as the restore was given it, once the worker has
// handed its hold over or let go of what it took (SetOnSourceWorkerEndForTest).
var onSourceWorkerEnd atomic.Pointer[func(string)]

// sourceHold is the lock of the folder that holds a restore's snapshot, taken
// (holdSource): the snapshot where it resolves, its manifest as read under
// the lock, and the lock's release; or why none was taken.
type sourceHold struct {
	snap    string
	m       Manifest
	release func()
	err     error
}

// sourceSpelling is the folder the snapshot at snapshotDir is in as this
// process's marks key it by its spelling (spellingKey), asking no filesystem.
func sourceSpelling(snapshotDir string) string {
	return filepath.Dir(spellingKey(snapshotDir))
}

// SourceStillHeld is a restore's first check, made before anything reads the
// snapshot at snapshotDir: ErrCheckStillRunning where a hold given up in this
// process still holds the folder the snapshot is in, by the spelling given,
// asking no filesystem (stuckBy); nil otherwise. RestoreWithOptions asks it
// first, and bridge restore before its first read of the snapshot.
func SourceStillHeld(snapshotDir string) error {
	if check, held := stuckBy(sourceSpelling(snapshotDir)); held {
		return fmt.Errorf("%w; nothing was restored", stillRunning(check))
	}
	return nil
}

// holdSource takes the lock of the folder that holds the snapshot at
// snapshotDir, waiting for another holder within wait, and answers the hold,
// whose release its caller runs once the restore has ended; or the restore's
// refusal, nothing restored.
func holdSource(ctx context.Context, snapshotDir string, wait time.Duration) (*sourceHold, error) {
	if err := SourceStillHeld(snapshotDir); err != nil {
		return nil, err
	}
	work, cancel := context.WithCancel(ctx)
	defer cancel()
	// out is handed over, never left in a buffer: the hold is its caller's,
	// or, once gone is closed, the worker's to let go.
	out := make(chan *sourceHold)
	gone := make(chan struct{})
	go func() {
		defer func() {
			if hook := onSourceWorkerEnd.Load(); hook != nil {
				(*hook)(snapshotDir)
			}
		}()
		h := takeSource(work, snapshotDir, wait)
		select {
		case out <- h:
		case <-gone:
			if h.release != nil {
				h.release()
			}
		}
	}()
	watchdog := time.NewTimer(wait + watchdogSlack)
	defer watchdog.Stop()
	stop := ctx.Done()
	var expire <-chan time.Time
	for {
		select {
		case h := <-out:
			if h.err != nil {
				return nil, h.err
			}
			if err := ctx.Err(); err != nil {
				// Stopped as the lock came: the restore has not begun.
				letGoWithin(h.release)
				return nil, sourceStopped(snapshotDir, err)
			}
			return h, nil
		case <-stop:
			stop = nil
			if expire == nil {
				expire = time.After(StopWriteBound)
			}
		case <-watchdog.C:
			if expire == nil {
				expire = time.After(StopWriteBound)
			}
		case <-expire:
			close(gone)
			return nil, sourceExpiryError(snapshotDir, ctx.Err())
		}
	}
}

// takeSource is holdSource's work, on its worker: the snapshot resolved, the
// resolved folder's marks asked, its lock taken, a stale marker removed and
// the manifest read again, ctx asked before the lock and once it is taken, so
// that once its caller has given up (which ends ctx) it takes no lock it
// keeps and removes nothing.
func takeSource(ctx context.Context, snapshotDir string, wait time.Duration) *sourceHold {
	snap, root, err := resolveSnapshotIn(snapshotDir, "")
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return &sourceHold{err: snapshotGone(snapshotDir, err)}
	case err != nil:
		return &sourceHold{err: fmt.Errorf("%w; nothing was restored", err)}
	}
	if check, held := stuckBy(root); held {
		return &sourceHold{err: fmt.Errorf("%w; nothing was restored", stillRunning(check))}
	}
	if err := ctx.Err(); err != nil {
		return &sourceHold{err: sourceStopped(snapshotDir, err)}
	}
	unlock, err := lockBackupsWith(ctx, root, wait, folderToken(ownerKey(root)))
	if err != nil {
		return &sourceHold{err: sourceLockRefusal(ctx, snapshotDir, root, wait, err)}
	}
	if err := ctx.Err(); err != nil {
		unlock()
		return &sourceHold{err: sourceStopped(snapshotDir, err)}
	}
	// The lock held, a marker beside it is a process's that has ended, which
	// would read as a check still running to whoever waits for the lock.
	clearStaleMarker(root)
	m, err := readManifest(filepath.Join(snap, ManifestFile))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		err = snapshotGone(snapshotDir, err)
	case err != nil:
		err = fmt.Errorf("read snapshot manifest: %w", err)
	case m.SchemaVersion < 1 || m.SchemaVersion > SchemaVersion:
		err = fmt.Errorf("snapshot schema v%d not supported (this bridge supports v%d)", m.SchemaVersion, SchemaVersion)
	}
	if err != nil {
		unlock()
		return &sourceHold{err: err}
	}
	return &sourceHold{snap: snap, m: m, release: unlock}
}

// sourceLockRefusal is the restore's refusal for err, what the lock of root,
// the folder that holds the snapshot at snapshotDir, answered: a hold given up
// that holds it, another holder for all of wait, no lock to be had there (one
// the filesystem keeps none of, or a lock file it cannot make: read-only
// media, a folder this user cannot write), the folder gone, the restore's
// stop, or another failure of the folder's own.
func sourceLockRefusal(ctx context.Context, snapshotDir, root string, wait time.Duration, err error) error {
	switch {
	case errors.Is(err, ErrCheckStillRunning):
		return fmt.Errorf("%w; nothing was restored", err)
	case errors.Is(err, ErrBackupsBusy):
		return fmt.Errorf("%w, in %s, for all of the restore's wait (%s); nothing was restored", err, root, wait)
	case ctx.Err() != nil && errors.Is(err, ctx.Err()):
		return sourceStopped(snapshotDir, err)
	case errors.Is(err, ErrBackupsUnlocked), errors.Is(err, syscall.EROFS), errors.Is(err, fs.ErrPermission):
		return &sourceUnlockedError{folder: root, cause: lockCause(err)}
	case errors.Is(err, fs.ErrNotExist):
		return snapshotGone(snapshotDir, err)
	}
	return fmt.Errorf("take the lock of %s, the folder that holds the snapshot: %w; nothing was restored", root, err)
}

// lockCause is what a lock's failure says of its file, past the words of a
// verification's refusal it may carry (ErrBackupsUnlocked's): the failed
// call's *fs.PathError where there is one, err otherwise.
func lockCause(err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe
	}
	return err
}

// sourceUnlockedError is a restore's refusal where no lock could be had on
// the folder that holds its snapshot: a filesystem that keeps none, or a lock
// file it could not make there (read-only media, a folder this user cannot
// write). errors.Is reads it as ErrBackupsUnlocked; its words are a
// restore's, its cause the failed call's.
type sourceUnlockedError struct {
	folder string
	cause  error
}

func (e *sourceUnlockedError) Error() string {
	return fmt.Sprintf("no lock could be had on %s, the folder that holds the snapshot (%v); nothing was restored", e.folder, e.cause)
}

// Is reads the refusal as ErrBackupsUnlocked.
func (e *sourceUnlockedError) Is(target error) bool { return target == ErrBackupsUnlocked }

// Unwrap is the failed call's error.
func (e *sourceUnlockedError) Unwrap() error { return e.cause }

// snapshotGone is ErrSnapshotGone for the snapshot at snapshotDir, err what
// its lookup answered.
func snapshotGone(snapshotDir string, err error) error {
	return fmt.Errorf("%w: %s (%w); nothing was restored", ErrSnapshotGone, snapshotDir, err)
}

// sourceStopped is the refusal of a restore stopped, why its context's error,
// before it held the lock of the folder that holds its snapshot.
func sourceStopped(snapshotDir string, why error) error {
	return fmt.Errorf("the restore was stopped while it waited for the lock of the folder that holds the snapshot %s: %w; nothing was restored",
		snapshotDir, why)
}

// sourceExpiryError is a restore's refusal once it stopped waiting for the
// worker that takes its source's lock (holdSource): StopWriteBound past its
// stop, why being its context's error, or past its own bound where nothing
// stopped it (why nil).
func sourceExpiryError(snapshotDir string, why error) error {
	when := "its stop"
	if why == nil {
		why, when = context.DeadlineExceeded, "the end of its wait for the lock"
	}
	return fmt.Errorf("the folder that holds the snapshot %s did not answer within %s of %s, and nothing was restored: what the restore takes of it from now on it lets go at once (%w)",
		snapshotDir, StopWriteBound, when, why)
}
