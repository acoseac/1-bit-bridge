package backup

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
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
// Its admission, before anything is written: this process's marks are asked
// by the spelling given (SourceStillHeld), asking no filesystem, and a hold
// given up there refuses the restore at once, ErrCheckStillRunning, its
// folder never resolved, since the resolution can hang on the very mount the
// hold's step stalled on. Then the snapshot is resolved, the resolved folder's
// marks are asked, and its lock is taken (lockBackupsWith): waited for within
// the restore's wait while another holder has it, ErrBackupsBusy past it, and
// ErrCheckStillRunning at once where that holder is a hold given up (this
// process's mark, or another process's marker beside the lock). Held, a marker
// that a process which has ended left there is removed (hold.clearStale), and
// the manifest is read again: the snapshot its caller read before its prompt
// may have been pruned meanwhile (ErrSnapshotGone).
//
// The admission runs on a worker of its own, under a hold of the folder
// (newRestoreHold), since the lock's wait bounds its polling alone: the
// resolution, the lock file's open, a marker's read, the stale marker's
// removal and the manifest's read can each hang on a mount that stopped
// answering. The restore waits for the worker until its context ends or its
// wait and watchdogSlack have passed, then StopWriteBound more
// (awaitSourceWorker), and is then refused, nothing restored, the worker given
// up (hold.abandon) and left to end on its own. A worker given up keeps no
// lock it takes late: its hold's context ends with the give-up, and the lock
// taken on it is let go at once (lockBackupsWith asks it after the try). It
// still touches the snapshot's folder, never the destination: an open of the
// lock file under way may make that file (as root, with the folder's owner),
// and a removal of a stale marker under way may finish. A worker given up
// while it holds the lock (in the stale marker's removal or the manifest's
// read) marks the folder as a restore's (StuckCheck.Restore), in this process
// and in the marker beside the lock, until it has let the lock go; that marker
// is written only once a removal of a stale marker under way has ended, so the
// removal never takes it away. One given up before it holds the lock marks
// nothing, holding nothing a mark would say it holds.
//
// The restore writes the destination on its caller's goroutine alone, once
// the worker has handed the hold over, so nothing is written there once it
// has answered. A context that ends as the hold is handed over still refuses
// the restore, the lock let go; once the restore has begun its writes its
// context no longer stops it, since a restore stopped between its writes would
// leave the install half restored, and its tenure is not bounded as a hold's
// steps are (hold.go): a restore is a command of its own, whose end, however
// it comes, lets the kernel's lock go. Its let-go of the lock is bounded
// (hold.release): past releaseBound the folder is marked in this process,
// naming the restore, until the unlock, the close and this process's holder
// of the folder are let go, and the restore answers meanwhile.
//
// bridge restore looks at the snapshot before its prompt, bounded alike
// (CheckRestoreSource), and takes the lock only once the prompt is answered.
//
// The restore reads the snapshot where it resolves, in the folder whose lock
// it holds, and says what it says of it as it was given it, never as it
// resolves (backlog B826's third round): Windows gives a temporary folder in
// its 8.3 spelling, which resolves to the long one, and a link resolves to
// where it leads, and the operator looks for the path typed. Each refusal of
// the admission names the snapshot by the path given (bridge restore's
// argument, cleaned: CleanSnapshotPath), and every path below the snapshot
// that a read's error names is spelled below that path (spelledAsGiven).
// Where the snapshot resolves may follow the path given in a failed call's own
// words alone: the lock's file, in the folder the snapshot resolves into, and
// a lookup that found the snapshot gone, which names where it led. A hold
// given up that refuses the restore keeps its own name, its snapshot's folder
// name as the marker beside the lock holds it for every process
// (StuckCheck.Snapshot), which names no path: the marker is unchanged.

// ErrSnapshotGone is a restore's refusal of a snapshot that is no longer
// there, or whose manifest is no longer there, once the restore holds the
// lock of the folder it was in: a prune may have removed it while the
// restore's caller asked for confirmation. Nothing was restored.
var ErrSnapshotGone = errors.New("the snapshot is no longer there")

// ErrNotASnapshot is bridge restore's refusal, before its prompt, of a path
// that holds no snapshot (CheckRestoreSource): nothing there, or no
// manifest.json that reads with a schema. One of a schema this bridge does not
// take reads as a snapshot here, and the restore refuses it in its own words.
var ErrNotASnapshot = errors.New("not a recognized snapshot directory (no readable manifest.json with a supported schema)")

// onSourceWorkerEnd, nil in production, runs last on a restore's source
// worker, given the snapshot as the restore was given it, once the worker has
// handed its hold over, or let go of what it took and dropped its hold's mark
// (SetOnSourceWorkerEndForTest).
var onSourceWorkerEnd atomic.Pointer[func(string)]

// onSourceStep, nil in production, runs on a restore's source worker as it
// passes a step under the lock, given the snapshot as the restore was given
// it: "held" once the lock is held, before a stale marker's removal,
// "cleared" once that removal has ended, "read" once the manifest is read,
// before the hold is handed over (SetOnSourceStepForTest).
var onSourceStep atomic.Pointer[func(dir, step string)]

// sourceStep runs onSourceStep's hook, if any.
func sourceStep(snapshotDir, step string) {
	if hook := onSourceStep.Load(); hook != nil {
		(*hook)(snapshotDir, step)
	}
}

// sourceHold is the lock of the folder that holds a restore's snapshot, taken
// (holdSource): the hold that holds it, the snapshot where it resolves, which
// the restore reads, the snapshot as the restore was given it, which its words
// name, and its manifest as read under the lock; or why none was taken.
type sourceHold struct {
	h     *hold
	snap  string
	given string
	m     Manifest
	err   error
}

// asGiven is err, what the restore answers once it holds its snapshot, in
// words that name the snapshot as the restore was given it (spelledAsGiven).
func (s *sourceHold) asGiven(err error) error {
	return spelledAsGiven(err, s.snap, s.given)
}

// spelledAsGiven is err, a restore's refusal or failure that a read of the
// snapshot where it resolves (snap) answered, in words that name the snapshot
// as the restore was given it (given): each path below snap spelled below
// given, as filepath.Join(given, name) spells it. errors.Is and errors.As
// read err through it, so every advice chosen from it is chosen as before;
// where its words name nothing below snap, or the snapshot was given as it
// resolves, it is err itself.
func spelledAsGiven(err error, snap, given string) error {
	if err == nil || snap == "" || filepath.Dir(snap) == snap {
		return err
	}
	below := snap + string(filepath.Separator)
	// The given path's own spelling of a name below it: filepath.Join's,
	// which a given "." spells as the name alone.
	as := strings.TrimSuffix(filepath.Join(given, "x"), "x")
	words := err.Error()
	if as == below || !strings.Contains(words, below) {
		return err
	}
	return &givenSpelling{err: err, words: strings.ReplaceAll(words, below, as)}
}

// givenSpelling is err in the words spelledAsGiven gives it.
type givenSpelling struct {
	err   error
	words string
}

func (e *givenSpelling) Error() string { return e.words }

// Unwrap is the error whose words these are.
func (e *givenSpelling) Unwrap() error { return e.err }

// heldRefusal is the refusal of a restore of the snapshot at snapshotDir, as
// the restore was given it, whose folder a hold given up still holds, held:
// ErrCheckStillRunning for that hold (a StillRunningError), which keeps its
// own name, a verification's, a prune's or another restore's, by its
// snapshot's folder name, so the snapshot refused and the hold that refuses
// it are each named.
func heldRefusal(snapshotDir string, held error) error {
	return fmt.Errorf("%s: %w; nothing was restored", snapshotDir, held)
}

// letGo lets the lock go once the restore is done with it, or refuses to
// begin: the hold's release, bounded by releaseBound, past which the folder
// stays marked in this process until the unlock, the close and this process's
// holder of the folder are let go (hold.release), and the mark dropped once
// they are, on a goroutine of its own where they have not (hold.dropSoon), so
// the restore answers meanwhile.
func (s *sourceHold) letGo() {
	s.h.release()
	s.h.dropSoon()
}

// sourceSpelling is the folder the snapshot at snapshotDir is in as this
// process's marks key it by its spelling (spellingKey), asking no filesystem.
func sourceSpelling(snapshotDir string) string {
	return filepath.Dir(spellingKey(snapshotDir))
}

// SourceStillHeld is a restore's first check, made before anything reads the
// snapshot at snapshotDir: ErrCheckStillRunning where a hold given up in this
// process still holds the folder the snapshot is in, by the spelling given,
// asking no filesystem (stuckBy), naming the snapshot so (heldRefusal); nil
// otherwise. RestoreWithOptions asks it first, and CheckRestoreSource.
func SourceStillHeld(snapshotDir string) error {
	if check, held := stuckBy(sourceSpelling(snapshotDir)); held {
		return heldRefusal(snapshotDir, stillRunning(check))
	}
	return nil
}

// holdSource takes the lock of the folder that holds the snapshot at
// snapshotDir, waiting for another holder within wait, and answers the hold,
// which its caller lets go once the restore has ended (letGo); or the
// restore's refusal, nothing restored.
func holdSource(ctx context.Context, snapshotDir string, wait time.Duration) (*sourceHold, error) {
	if err := SourceStillHeld(snapshotDir); err != nil {
		return nil, err
	}
	h := newRestoreHold(ctx, filepath.Base(spellingKey(snapshotDir)), sourceSpelling(snapshotDir))
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
		s := takeSource(h, snapshotDir, wait)
		if s.err != nil {
			// Nothing to hand over: what the hold holds is let go here,
			// bounded, before the answer, and its mark once that has ended.
			h.release()
			select {
			case out <- s:
			case <-gone:
			}
			h.drop()
			return
		}
		select {
		case out <- s:
		case <-gone:
			// Given up while it held the lock: the folder is marked, naming
			// the restore, until the lock is let go.
			h.release()
			h.drop()
		}
	}()
	s, answered := awaitSourceWorker(ctx, wait, out)
	if !answered {
		h.abandon()
		close(gone)
		return nil, sourceExpiryError(snapshotDir, ctx.Err())
	}
	if s.err != nil {
		return nil, s.err
	}
	if err := ctx.Err(); err != nil {
		// Stopped as the lock came: the restore has not begun.
		s.letGo()
		return nil, sourceStopped(snapshotDir, err)
	}
	return s, nil
}

// awaitSourceWorker waits for a restore's source worker to answer on out
// (holdSource, CheckRestoreSource) until ctx ends or wait and watchdogSlack
// have passed, then StopWriteBound more, and answers false past that, the
// worker left to end on its own.
func awaitSourceWorker[T any](ctx context.Context, wait time.Duration, out <-chan T) (v T, answered bool) {
	watchdog := time.NewTimer(wait + watchdogSlack)
	defer watchdog.Stop()
	stop := ctx.Done()
	var expire <-chan time.Time
	for {
		select {
		case v = <-out:
			return v, true
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
			return v, false
		}
	}
}

// takeSource is holdSource's work, on its worker, under h: the snapshot
// resolved, the resolved folder's marks asked, its lock taken on h's context,
// which a give-up ends (a lock taken once it has ended is let go at once), a
// stale marker removed unless h was given up first (hold.clearStale), and the
// manifest read again. h marks the folder at a give-up only once it holds
// the lock (marksFromNow), naming its step (enter); a give-up seen between the
// steps ends the work, h then let go by its worker.
func takeSource(h *hold, snapshotDir string, wait time.Duration) *sourceHold {
	snap, root, err := resolveSnapshotIn(snapshotDir, "")
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return &sourceHold{err: snapshotGone(snapshotDir, err)}
	case err != nil:
		return &sourceHold{err: fmt.Errorf("%s: %w; nothing was restored", snapshotDir, err)}
	}
	if check, held := stuckBy(root); held {
		return &sourceHold{err: heldRefusal(snapshotDir, stillRunning(check))}
	}
	if err := h.ctx.Err(); err != nil {
		return &sourceHold{err: sourceStopped(snapshotDir, err)}
	}
	key := ownerKey(root)
	unlock, err := lockBackupsWith(h.ctx, root, wait, folderToken(key))
	if err != nil {
		return &sourceHold{err: sourceLockRefusal(h.ctx, snapshotDir, wait, err)}
	}
	h.held(unlock, root, key, filepath.Base(snap))
	h.marksFromNow()
	given := func() *sourceHold {
		if !h.gaveUp() {
			return nil
		}
		return &sourceHold{err: sourceStopped(snapshotDir, h.ctx.Err())}
	}
	if s := given(); s != nil {
		return s
	}
	sourceStep(snapshotDir, "held")
	h.enter(stepClearMarker)
	h.clearStale()
	sourceStep(snapshotDir, "cleared")
	if s := given(); s != nil {
		return s
	}
	h.enter(stepReadSnapshot)
	m, err := readManifest(filepath.Join(snap, ManifestFile))
	err = spelledAsGiven(err, snap, snapshotDir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		err = snapshotGone(snapshotDir, err)
	case err != nil:
		err = fmt.Errorf("read snapshot manifest: %w", err)
	case m.SchemaVersion < 1 || m.SchemaVersion > SchemaVersion:
		err = fmt.Errorf("snapshot schema v%d not supported (this bridge supports v%d)", m.SchemaVersion, SchemaVersion)
	}
	if err != nil {
		return &sourceHold{err: err}
	}
	sourceStep(snapshotDir, "read")
	if s := given(); s != nil {
		return s
	}
	return &sourceHold{h: h, snap: snap, given: snapshotDir, m: m}
}

// CheckRestoreSource is bridge restore's look at the snapshot at snapshotDir
// before its prompt (backlog B826's second round), bounded as the restore's
// admission is (awaitSourceWorker, the same wait, sourceExpiryError's words
// past it): this process's marks by the spelling given first, asking no
// filesystem (SourceStillHeld); then, on a worker, the snapshot resolved, the
// folder it resolves into asked whether a hold given up holds it (StuckCheckOf:
// this process's mark, then another process's marker there, counted only
// while another open holds the folder's lock, since a marker beside a lock
// nobody holds is one a process that ended left, which the restore removes
// once it holds the lock), and its manifest read. It answers
// ErrCheckStillRunning, ErrNotASnapshot, sourceExpiryError's refusal, or nil.
// It writes nothing, and holds nothing past its return: the lock it tries to
// ask whether another holds it, it lets go at once, and a worker given up
// holds nothing.
func CheckRestoreSource(ctx context.Context, snapshotDir string, wait time.Duration) error {
	if err := SourceStillHeld(snapshotDir); err != nil {
		return err
	}
	out := make(chan error, 1)
	go func() { out <- checkSource(snapshotDir) }()
	err, answered := awaitSourceWorker(ctx, wait, out)
	if !answered {
		return sourceExpiryError(snapshotDir, ctx.Err())
	}
	return err
}

// checkSource is CheckRestoreSource's look, on its worker.
func checkSource(snapshotDir string) error {
	snap, root, err := resolveSnapshotIn(snapshotDir, "")
	if err != nil {
		return fmt.Errorf("%w: %w", ErrNotASnapshot, err)
	}
	if check, held := StuckCheckOf(root); held {
		return heldRefusal(snapshotDir, stillRunning(check))
	}
	m, err := readManifest(filepath.Join(snap, ManifestFile))
	if err != nil {
		return fmt.Errorf("%w: %w", ErrNotASnapshot, spelledAsGiven(err, snap, snapshotDir))
	}
	if m.SchemaVersion <= 0 {
		return ErrNotASnapshot
	}
	return nil
}

// sourceLockRefusal is the restore's refusal for err, what the lock of the
// folder that holds the snapshot at snapshotDir (as the restore was given it)
// answered: a hold given up that holds it, another holder for all of wait, no
// lock to be had there (one the filesystem keeps none of, or a lock file it
// cannot make: read-only media, a folder this user cannot write), the folder
// gone, the restore's stop, or another failure of the folder's own. Each names
// the snapshot as it was given; a failed call's own words, which may follow,
// name the lock file where the snapshot resolves.
func sourceLockRefusal(ctx context.Context, snapshotDir string, wait time.Duration, err error) error {
	switch {
	case errors.Is(err, ErrCheckStillRunning):
		return heldRefusal(snapshotDir, err)
	case errors.Is(err, ErrBackupsBusy):
		return fmt.Errorf("%w, in the folder that holds the snapshot %s, for all of the restore's wait (%s); nothing was restored", err, snapshotDir, wait)
	case ctx.Err() != nil && errors.Is(err, ctx.Err()):
		return sourceStopped(snapshotDir, err)
	case errors.Is(err, ErrBackupsUnlocked), errors.Is(err, syscall.EROFS), errors.Is(err, fs.ErrPermission):
		return &sourceUnlockedError{snapshot: snapshotDir, cause: lockCause(err)}
	case errors.Is(err, fs.ErrNotExist):
		return snapshotGone(snapshotDir, err)
	}
	return fmt.Errorf("take the lock of the folder that holds the snapshot %s: %w; nothing was restored", snapshotDir, err)
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
// restore's, naming the snapshot as the restore was given it, its cause the
// failed call's.
type sourceUnlockedError struct {
	snapshot string
	cause    error
}

func (e *sourceUnlockedError) Error() string {
	return fmt.Sprintf("no lock could be had on the folder that holds the snapshot %s (%v); nothing was restored", e.snapshot, e.cause)
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
// during its admission (holdSource), before it wrote anything: while it took
// the lock of the folder that holds its snapshot and read the snapshot, or as
// the lock came.
func sourceStopped(snapshotDir string, why error) error {
	return fmt.Errorf("the restore was stopped before it wrote anything, while it took the lock of the folder that holds the snapshot %s and read it: %w; nothing was restored",
		snapshotDir, why)
}

// sourceExpiryError is a restore's refusal once it stopped waiting for the
// worker that takes its source's lock (holdSource), or that looks at the
// snapshot before bridge restore's prompt (CheckRestoreSource):
// StopWriteBound past its stop, why being its context's error, or past its own
// bound where nothing stopped it (why nil).
func sourceExpiryError(snapshotDir string, why error) error {
	when := "its stop"
	if why == nil {
		why, when = context.DeadlineExceeded, "the end of its wait"
	}
	return fmt.Errorf("the folder that holds the snapshot %s did not answer within %s of %s, and nothing was restored (%w)",
		snapshotDir, StopWriteBound, when, why)
}
