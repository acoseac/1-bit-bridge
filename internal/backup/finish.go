package backup

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"time"
)

// StopWriteBound is how long a verification its caller stopped (serve's
// shutdown, a cancelled command) goes on once it has seen the stop: within it
// the check's return and the INCOMPLETE it writes; past it the caller is
// answered and the verification is left to end on its own, holding what it
// took until it does (verifyAndThen), so a stop is never held up by a
// filesystem that will not answer. Serve's wait for the backup ticker
// (backupShutdownWait) is far longer.
const StopWriteBound = 2 * time.Second

// verifyDeadline is VerifyDeadline, and a test's shorter one
// (SetVerifyDeadlineForTest).
var verifyDeadline = VerifyDeadline

// VerifyOptions say how a verification runs.
type VerifyOptions struct {
	// Level is the database check's level; "" is CheckQuick.
	Level CheckLevel
	// LockWait is how long it waits for another process's verification or
	// prune to let the backups folder's lock go; 0 is AutoLockWait.
	LockWait time.Duration
	// BackupsRoot is the install's backups folder, which must hold the
	// snapshot by where both resolve (resolveSnapshotIn, ErrOutsideBackups):
	// its lock is the one every verification and every prune takes. "" is
	// the folder the snapshot resolves into.
	BackupsRoot string
	// asAnotherProcess runs the verification outside this process's slot and
	// its holder of the backups folder, its lock the kernel's alone, as
	// another process's is (VerifySnapshotAsAnotherProcessForTest).
	asAnotherProcess bool
}

// VerifyResult is what a verification of one snapshot did.
type VerifyResult struct {
	// Found is what the check found.
	Found Verification
	// Record is the snapshot's record as a reader reads it once the
	// verification has written (readVerdict): Found merged with what it held,
	// so a quick check over a full one keeps the full one's verdict.
	Record Verification
	// Recorded says the record holds what was found: false where the
	// snapshot predates verification (Legacy), nothing was written, or the
	// write failed or was abandoned.
	Recorded bool
	// Legacy says the snapshot was written before verification: its files
	// were checked for presence alone, and nothing was recorded.
	Legacy bool
}

// VerifySnapshot checks the snapshot in dir and records its verdict, under
// the lock of the backups folder that holds it, waiting its turn behind this
// process's other verifications: the database's check (its baseline, then
// SQLite's own at o.Level, the schema its user_version requires, and the core
// tables' counts) and the other files' (presence, size and SHA-256), on a
// thread of the lowest priority, its reads paced. It answers what it found
// and what the record says now. An error says the verdict was not recorded,
// or not durably: ErrOutsideBackups, ErrBackupsBusy, ErrBackupsUnlocked,
// ErrCheckStillRunning (another check that did not end holds the snapshots),
// ErrRecheckRefused (nothing checked), a stop (ctx's error), the deadline,
// ErrCheckGivenUp, the write's failure (ErrVerdictNotDurable among them).
func VerifySnapshot(ctx context.Context, dir string, o VerifyOptions) (VerifyResult, error) {
	res, err, _ := verifyAndThen(ctx, dir, o, nil, nil)
	return res, err
}

// onManifestRead, nil in production, runs before each read of a snapshot's
// manifest a verification makes (SetOnManifestReadForTest).
var onManifestRead func(path string)

// readRecord reads the manifest of the snapshot in dir for a verification.
func readRecord(dir string) (Manifest, error) {
	path := filepath.Join(dir, ManifestFile)
	if onManifestRead != nil {
		onManifestRead(path)
	}
	return readManifest(path)
}

// outcome is what a verification's worker did.
type outcome struct {
	res VerifyResult
	err error
}

// watchdogSlack is what a caller's wait allows a verification past its lock
// wait and its deadline, before it answers without the worker: the check's
// own give-up and the INCOMPLETE it writes come within it.
const watchdogSlack = 2 * StopWriteBound

// verifyAndThen is VerifySnapshot running after, under the same hold of the
// lock, once the verdict is written: the prune that follows a snapshot
// (Finish), given the backups folder where it resolves. onEnd, when set,
// runs once the verification has ended, the worker's end included where its
// caller stopped waiting: what a capture held goes then (FinishCapture).
//
// Everything a verification does that can wait on a filesystem, the
// resolution of its paths, the lock file's open and its wait, every read of
// the manifest, the check, the verdict's write, the prune and the read of the
// record after it, runs on one worker goroutine, which holds this process's
// slot, the backups folder's lock and the snapshot's ownership until it ends.
// Its caller waits for it until it answers, or, once the stop (ctx) or the
// verification's own bound (watchdogSlack past its lock wait and deadline)
// has come, for StopWriteBound more, then answers that the verification was
// abandoned (abandoned true): an abandoned worker keeps its hold until it
// ends, so nothing protected runs beside it, in this process or another, and
// a lock it takes once its context has ended it lets go at once.
//
// The worker answers its caller as soon as its verdict is written, and only
// then lets go of what it holds, each once, the lock first, then the slot,
// then the ownership: at once, or, for a check it gave up (stuck.go), once
// the check's goroutine has ended, its caller long answered. A panic in the
// check is recorded as its INCOMPLETE (ReasonAborted), one in the worker's own
// steps answers an error, and either lets go of all the same.
func verifyAndThen(ctx context.Context, dir string, o VerifyOptions, after func(context.Context, string), onEnd func()) (res VerifyResult, err error, abandoned bool) {
	if o.Level == "" {
		o.Level = CheckQuick
	}
	if o.LockWait == 0 {
		o.LockWait = AutoLockWait
	}
	end := func() {
		if onEnd != nil {
			onEnd()
		}
	}
	releaseSlot := func() {}
	if !o.asAnotherProcess {
		release, err := takeSlot(ctx)
		switch {
		case errors.Is(err, ErrCheckStillRunning):
			// Nothing was begun: its record says what it said, its owner's
			// end aside.
			end()
			return res, err, false
		case err != nil:
			abandonPending(dir, o.BackupsRoot, ctx, end)
			return res, err, false
		}
		releaseSlot = release
	}
	done := make(chan outcome, 1)
	go func() {
		defer end()
		defer releaseSlot()
		h := new(held)
		defer h.release()
		defer func() {
			if r := recover(); r != nil {
				done <- outcome{err: fmt.Errorf("the verification stopped on an error of the bridge's own: %v", r)}
			}
		}()
		r, e := verifyHeld(ctx, dir, o, after, h)
		done <- outcome{r, e}
	}()
	stop := ctx.Done()
	watchdog := time.NewTimer(o.LockWait + verifyDeadline + watchdogSlack)
	defer watchdog.Stop()
	var expire <-chan time.Time
	for {
		select {
		case out := <-done:
			return out.res, out.err, false
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
			why, when := ctx.Err(), "its stop"
			if why == nil {
				why, when = context.DeadlineExceeded, "its deadline"
			}
			return res, fmt.Errorf("the verification was left to end on its own %s after %s, holding the snapshots' lock until it does: %w",
				StopWriteBound, when, why), true
		}
	}
}

// held is what a verification's worker keeps once it has answered, and lets
// go of then (release): the backups folder's lock, held until the check's
// goroutine has ended (checker), and the mark of a check that did not end
// (clear).
type held struct {
	unlock  func()
	checker <-chan struct{}
	clear   func()
}

// release waits for the check's goroutine to end, then clears its mark and
// lets the lock go, each once.
func (h *held) release() {
	if h.checker != nil {
		<-h.checker
	}
	if h.clear != nil {
		h.clear()
	}
	if h.unlock != nil {
		h.unlock()
	}
	*h = held{}
}

// ErrCheckGivenUp marks a verification whose check did not return within its
// bound past its stop or its deadline: its record says INCOMPLETE, and the
// check, left to end on its own, holds the backups folder until it does
// (ErrCheckStillRunning, stuck.go). Its error wraps the stop's or the
// deadline's too.
var ErrCheckGivenUp = errors.New("the check did not end and was given up, and holds the snapshots until it does")

// verifyHeld is a verification's worker (verifyAndThen): it resolves the
// snapshot and its backups folder, takes the folder's lock, reads the
// manifest, sets a VERIFIED in it aside before the check (setAside), runs the
// check on checkCtx (ctx's with the deadline, counted from the lock), writes
// the verdict, runs after under the same hold, and reads the record back, in
// that order, holding the lock throughout, and leaves in h what it holds
// still, which its caller lets go of once it has answered. A check it gave up
// (runCheck's gaveUp) is marked as one that did not end and kept until its
// goroutine ends; its verdict is the INCOMPLETE it was given up with, its late
// result dropped, and no prune follows it: the next pass prunes.
func verifyHeld(ctx context.Context, dir string, o VerifyOptions, after func(context.Context, string), h *held) (res VerifyResult, err error) {
	snap, root, err := resolveSnapshotIn(dir, o.BackupsRoot)
	if err != nil {
		return res, err
	}
	token := folderToken(ownerKey(root))
	if o.asAnotherProcess {
		token = nil
	}
	unlock, err := lockBackupsWith(ctx, root, o.LockWait, token)
	if err != nil {
		if ctx.Err() != nil && !errors.Is(err, ErrBackupsUnlocked) && !errors.Is(err, ErrCheckStillRunning) {
			abandonPending(snap, root, ctx, nil)
		}
		return res, err
	}
	h.unlock = unlock
	thenUnder := func() {
		if after != nil && ctx.Err() == nil {
			after(ctx, root)
		}
	}
	m, err := readRecord(snap)
	if err != nil {
		thenUnder()
		return res, fmt.Errorf("read the snapshot's manifest: %w", err)
	}
	// The record before the check, which the verdict merges with: a VERIFIED
	// in it is set aside first, durably, or nothing is checked; the bundle is
	// followed by its prune all the same.
	prior := m.Verification
	if holdsVerified(prior) {
		if err := setAside(snap); err != nil {
			own := OwnerNone
			if m.hasPending() {
				own = probeOwner(snap)
			}
			res.Record = readVerdict(m, time.Now(), vetoedAt(snap), own, boundName(snap))
			thenUnder()
			return res, err
		}
	}
	checkCtx, cancel := context.WithTimeout(ctx, verifyDeadline)
	defer cancel()
	found, gaveUp, finished := runCheck(checkCtx, snap, m, o.Level)
	h.checker = finished
	if gaveUp {
		// The check did not return within its bound past the stop or the
		// deadline: it is left to end on its own, holding all this worker
		// holds until it does, and the record says it did not end.
		found = incompleteOf(checkCtx)
		h.clear = markStuck(root, filepath.Base(snap))
	}
	res.Found = found
	if afterCheck != nil {
		afterCheck(snap)
	}
	givenUp := func(werr error) error {
		why, when := ctx.Err(), "its stop"
		if why == nil {
			why, when = checkCtx.Err(), "its deadline"
		}
		err := fmt.Errorf("%w: it had not returned %s past %s (%w)", ErrCheckGivenUp, StopWriteBound/2, when, why)
		if werr != nil {
			err = fmt.Errorf("%w; and its INCOMPLETE: %w", err, werr)
		}
		return err
	}
	if m.legacy() {
		// Nothing to record: a reader reads of it what it would write.
		res.Legacy = true
		res.Record = readVerdict(m, time.Now(), vetoedAt(snap), OwnerNone, boundName(snap))
		if gaveUp {
			return res, givenUp(nil)
		}
		thenUnder()
		return res, nil
	}
	_, werr := publishVerdict(snap, found, prior)
	// Whatever the write answered: a kept bundle is followed by its prune,
	// which reads the record as it now stands; but never while its check
	// still runs.
	if !gaveUp {
		thenUnder()
	}
	if now, rerr := readRecord(snap); rerr == nil {
		own := OwnerNone
		if now.hasPending() {
			own = probeOwner(snap)
		}
		res.Record = readVerdict(now, time.Now(), vetoedAt(snap), own, boundName(snap))
	}
	if gaveUp {
		res.Recorded = werr == nil
		return res, givenUp(werr)
	}
	if werr != nil {
		return res, werr
	}
	res.Recorded = true
	if err := ctx.Err(); err != nil && found.DatabaseState == StateIncomplete {
		return res, err
	}
	if err := checkCtx.Err(); err != nil && found.DatabaseState == StateIncomplete {
		return res, err
	}
	return res, nil
}

// incompleteOf is the verdict of a check ctx stopped.
func incompleteOf(ctx context.Context) Verification {
	why := ReasonCancelled
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		why = ReasonDeadline
	}
	now := time.Now().UTC()
	return Verification{
		Format: verificationFormat, DatabaseState: StateIncomplete, DatabaseReasons: []Reason{why},
		FilesState: StateIncomplete, FilesReasons: []Reason{why}, CheckedAt: now, FilesCheckedAt: now,
	}
}

// abandonPending writes INCOMPLETE over the PENDING of the snapshot in dir
// whose verification its caller stopped before it began (a shutdown while it
// waited its turn), within StopWriteBound, and abandons the write past it,
// on a goroutine that holds what it takes until it ends and runs onEnd then.
// The lock is tried until then: the verification that holds it is ending too.
// Where no lock is granted it writes nothing. A record that holds a verdict
// keeps it (mergeVerdict).
func abandonPending(dir, backupsRoot string, ctx context.Context, onEnd func()) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		if onEnd != nil {
			defer onEnd()
		}
		wait, cancel := context.WithTimeout(context.Background(), StopWriteBound)
		defer cancel()
		snap, root, err := resolveSnapshotIn(dir, backupsRoot)
		if err != nil {
			return
		}
		unlock, err := lockBackups(wait, root, StopWriteBound)
		if err != nil {
			return
		}
		defer unlock()
		// A snapshot from before verification takes no record.
		if m, err := readRecord(snap); err != nil || m.legacy() {
			return
		}
		_, _ = publishVerdict(snap, incompleteOf(ctx), nil)
	}()
	t := time.NewTimer(StopWriteBound)
	defer t.Stop()
	select {
	case <-done:
	case <-t.C:
	}
}

// runCheck runs checkBundle on a goroutine of its own whose OS thread takes
// the lowest priority (runLowPriority), and waits for it: until it returns,
// or, once checkCtx has ended (the caller's stop, or the deadline), for half
// of StopWriteBound more, then gives up on it (gaveUp), a read the stop cannot
// reach (a hung mount) left to it, so that the INCOMPLETE written after it
// still comes within the caller's bound; what it would answer then is
// dropped. finished closes once the check's goroutine has ended. A check that
// panics answers INCOMPLETE (ReasonAborted).
func runCheck(checkCtx context.Context, dir string, m Manifest, level CheckLevel) (v Verification, gaveUp bool, finished <-chan struct{}) {
	out := make(chan Verification, 1)
	finished = runLowPriority(func() { out <- checkBundle(checkCtx, dir, m, level) }, func(any) { out <- abortedVerdict() })
	select {
	case v = <-out:
		return v, false, finished
	case <-checkCtx.Done():
	}
	t := time.NewTimer(StopWriteBound / 2)
	defer t.Stop()
	select {
	case v = <-out:
		return v, false, finished
	case <-t.C:
		return Verification{}, true, finished
	}
}

// abortedVerdict is the verdict of a check that stopped on an error of the
// bridge's own.
func abortedVerdict() Verification {
	now := time.Now().UTC()
	return Verification{
		Format: verificationFormat, DatabaseState: StateIncomplete, DatabaseReasons: []Reason{ReasonAborted},
		FilesState: StateIncomplete, FilesReasons: []Reason{ReasonAborted}, CheckedAt: now, FilesCheckedAt: now,
	}
}

// onLowPriority, when set by a test, runs on the verification's thread once
// its priority is lowered.
var onLowPriority func()

// runLowPriority runs fn on a goroutine of its own, locked to its OS thread,
// whose CPU and I/O priority it lowers first (lowerThreadPriority), and never
// unlocks: the thread ends with the goroutine, so no other goroutine ever
// runs at the lowered priority. SQLite (modernc's, Go code on the caller's
// goroutine) and the hash read on that thread. A panic there is recovered
// and handed to onPanic; the channel answered closes once the goroutine has
// ended.
func runLowPriority(fn func(), onPanic func(any)) <-chan struct{} {
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		defer func() {
			if r := recover(); r != nil {
				onPanic(r)
			}
		}()
		runtime.LockOSThread()
		lowerThreadPriority()
		if onLowPriority != nil {
			onLowPriority()
		}
		fn()
	}()
	return finished
}

// Result is what the one path every snapshot takes did (SnapshotAndVerify):
// whether a bundle was kept, and what its verification and the prune after
// it did.
type Result struct {
	// Dir is the bundle kept, "" when the capture failed and none was kept.
	Dir string
	// CaptureErr is why no bundle was kept.
	CaptureErr error
	// Verify is the kept bundle's verification.
	Verify VerifyResult
	// VerifyErr is why its verdict was not recorded, or not durably.
	VerifyErr error
	// Prune is the prune after the bundle, which runs whatever its verdict;
	// PruneErr its keep-policy's error, PruneSkipped a prune that did not run
	// (the lock held elsewhere, or none granted: VerifyErr says which).
	Prune        PruneResult
	PruneErr     error
	PruneSkipped bool
}

// Kept reports whether the capture kept a bundle.
func (r Result) Kept() bool { return r.Dir != "" }

// SnapshotOptions say how SnapshotAndVerify runs.
type SnapshotOptions struct {
	// Keep is the prune's retention (PruneContext), read when the prune
	// starts, the one moment it means anything; nil is 0, retain
	// everything.
	Keep func() int
	// Verify says how the bundle's verification runs; its BackupsRoot is
	// the snapshot's own backups folder where it is "".
	Verify VerifyOptions
	// OnKept, when set, is called once the bundle is kept, before its
	// verification.
	OnKept func(dir string)
}

// SnapshotAndVerify is the one path every snapshot takes: capture
// (CaptureSnapshot), then, for a kept bundle, its verification and the prune
// after it, whatever the verdict (FinishCapture), the capture owning the
// bundle until its verification has ended. The console's create takes the
// capture and FinishCapture apart, returning between them.
func SnapshotAndVerify(ctx context.Context, src Sources, o SnapshotOptions) Result {
	c, err := CaptureSnapshot(ctx, src)
	if err != nil {
		return Result{CaptureErr: err}
	}
	if o.OnKept != nil {
		o.OnKept(c.Dir)
	}
	if o.Verify.BackupsRoot == "" {
		o.Verify.BackupsRoot = filepath.Join(src.DataDir, BackupsDirName)
	}
	return FinishCapture(ctx, c, o.Keep, o.Verify)
}

// FinishCapture is Finish over a capture whose ownership it holds until the
// verification has ended, its worker's end included, then lets go
// (Capture.Release): so no prune, in any process, removes the bundle while
// its verification is still to come.
func FinishCapture(ctx context.Context, c *Capture, keep func() int, o VerifyOptions) Result {
	return finish(ctx, c.Dir, keep, o, c.Release)
}

// Finish verifies the kept bundle in dir and prunes the backups folder that
// holds it after it, whatever its verdict, under one hold of that folder's
// lock: so no other process's prune runs between the verdict and this one,
// and the prune keeps what recovery needs (PruneContext). A verification
// that cannot take the lock prunes nothing this round; one its caller
// stopped prunes nothing either.
func Finish(ctx context.Context, dir string, keep func() int, o VerifyOptions) Result {
	return finish(ctx, dir, keep, o, nil)
}

// finish is Finish, onEnd running once the verification has ended.
func finish(ctx context.Context, dir string, keep func() int, o VerifyOptions, onEnd func()) Result {
	res := Result{Dir: dir}
	pruned := make(chan struct{})
	var prune PruneResult
	var pruneErr error
	after := func(ctx context.Context, root string) {
		n := 0
		if keep != nil {
			n = keep()
		}
		prune, pruneErr = pruneHeld(ctx, root, n)
		close(pruned)
	}
	var abandoned bool
	res.Verify, res.VerifyErr, abandoned = verifyAndThen(ctx, dir, o, after, onEnd)
	select {
	case <-pruned:
		res.Prune, res.PruneErr = prune, pruneErr
	default:
		// No prune ran: the lock was held elsewhere or none was granted, the
		// caller stopped, or the verification was left to end on its own,
		// the prune perhaps still to come.
		res.PruneSkipped = !abandoned
	}
	return res
}
