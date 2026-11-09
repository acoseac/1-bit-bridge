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
	// Recorded says what was found was written into the record, durably,
	// merged with what it held (mergeVerdict): false where the snapshot
	// predates verification (Legacy), the merge changed nothing so nothing
	// was written (a check given up over a completed verdict, which it keeps),
	// or the write failed, was not known durable, or was given up. Record is
	// what every reader reads either way.
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
// ErrCheckStillRunning (a verification given up holds the snapshots),
// ErrRecheckRefused (nothing checked), ErrRecheckNotRecorded (checked, and
// what it found not recorded over a VERIFIED where no directory can be
// synced), a stop (ctx's error), the deadline, ErrCheckGivenUp (a step given
// up), the write's failure (ErrVerdictNotDurable among them).
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
// Once it holds the lock, each of its steps is bounded, from the moment the
// lock was taken (verifyHeld, hold.go), and a step that overruns is given up:
// the worker answers, runs nothing more but its INCOMPLETE's write and its
// read of the record, and keeps what it holds until every step it gave up has
// ended. Its caller waits for it until it answers, or, once the stop (ctx) or
// the verification's own bound (watchdogSlack past its lock wait and
// deadline) has come, for StopWriteBound more, then answers that the
// verification was abandoned (abandoned true), the worker then given up where
// it is (a step before the lock, which no hold bounds): an abandoned worker
// keeps its hold until it ends, so nothing protected runs beside it, in this
// process or another, and a lock it takes once its context has ended it lets
// go at once.
//
// The worker answers its caller as soon as it can, and only then lets go of
// what it holds, each once, in this order: the steps it gave up waited for,
// the marker removed and the lock let go, then the slot, then the ownership,
// and last the mark of a hold given up, which stands until all of them are
// let go (stuck.go). A panic in the check is recorded as its INCOMPLETE
// (ReasonAborted), one in the worker's own steps answers an error, and
// either lets go of all the same. A verification of a folder a hold given up
// still holds is refused at once, asking no filesystem.
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
	parent := filepath.Dir(dir)
	if check, held := stuckBy(parent, o.BackupsRoot); held {
		end()
		return res, stillRunning(check), false
	}
	h := newHold(ctx, filepath.Base(spellingKey(dir)), parent, o.BackupsRoot)
	releaseSlot := func() {}
	if !o.asAnotherProcess {
		release, err := takeSlot(ctx, h)
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
		defer h.drop()
		defer end()
		defer releaseSlot()
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
			h.abandon()
			why, when := ctx.Err(), "its stop"
			if why == nil {
				why, when = context.DeadlineExceeded, "its deadline"
			}
			return res, fmt.Errorf("the verification was left to end on its own %s after %s, holding the snapshots' lock until it does: %w",
				StopWriteBound, when, why), true
		}
	}
}

// ErrCheckGivenUp marks a verification one of whose steps under the backups
// folder's lock did not answer within its bound (its check past its stop or
// its deadline, or any other step: hold.go): its record says what it says,
// the INCOMPLETE it was given up with merged in where that could be written,
// and the step, left to end on its own, holds the backups folder until it
// does (ErrCheckStillRunning, stuck.go). Its error names the step, and wraps
// the stop's or the deadline's too.
var ErrCheckGivenUp = errors.New("a step of the verification did not end and was given up, and holds the snapshots until it does")

// givenUp is ErrCheckGivenUp for the hold given up, naming its step, with
// the error of the INCOMPLETE's write where it failed.
func (h *hold) givenUp(ctx context.Context, werr error) error {
	check, _ := h.marked()
	step := check.Step
	if step == "" {
		step = "a step"
	}
	why := ctx.Err()
	if why == nil {
		why = context.DeadlineExceeded
	}
	err := fmt.Errorf("%w: %s did not end within its bound (%w)", ErrCheckGivenUp, step, why)
	if werr != nil {
		err = fmt.Errorf("%w; and its write: %w", err, werr)
	}
	return err
}

// verifyHeld is a verification's worker (verifyAndThen): it resolves the
// snapshot and its backups folder, takes the folder's lock, then, each step
// bounded and counted from the moment the lock was taken (stepOf, hold.go),
// reads the manifest, sets a VERIFIED in it aside before the check
// (setAside), runs the check on checkCtx (the deadline is the lock's moment
// and verifyDeadline), writes the verdict, runs after under the same hold,
// and reads the record back, in that order, holding the lock throughout. The
// steps before the check and the check are given up half of StopWriteBound
// past that deadline, and each step after it as long after the later of the
// deadline and its own start; each step also StopWriteBound past the caller's
// stop. Once one is given up (or its caller gives the worker up), the worker
// runs no check and no prune, writes nothing but its INCOMPLETE, whose merge
// keeps a completed verdict and clears no veto, reads the record back, and
// answers ErrCheckGivenUp naming the step; a step given up that resumes
// runs nothing further, and what it answers late is dropped. A check given up
// past its deadline records INCOMPLETE (deadline). What the worker holds is
// in h, which its caller lets go of once it has answered.
func verifyHeld(ctx context.Context, dir string, o VerifyOptions, after func(context.Context, string), h *hold) (res VerifyResult, err error) {
	snap, root, err := resolveSnapshotIn(dir, o.BackupsRoot)
	if err != nil {
		return res, err
	}
	key := ownerKey(root)
	token := folderToken(key)
	if o.asAnotherProcess {
		token = nil
	}
	unlock, err := lockBackupsWith(ctx, root, o.LockWait, token)
	if err != nil {
		if ctx.Err() != nil && !errors.Is(err, ErrBackupsUnlocked) && !errors.Is(err, ErrCheckStillRunning) {
			// The worker waits for that write now, and its caller, giving it
			// up there, names it so; a mark it raises stands until the write
			// has ended, though the slot goes as the worker ends, so the
			// folder that write holds is never left unmarked between the two
			// holds' ends.
			h.in(stepIncomplete)
			h.keepMarkUntil(abandonPending(snap, root, ctx, nil, h.spellings...))
		}
		return res, err
	}
	h.held(unlock, root, key, filepath.Base(snap))
	if h.gaveUp() {
		// Its caller gave it up while it waited for the lock: nothing runs.
		return res, h.givenUp(ctx, nil)
	}
	deadline := h.lockedAt().Add(verifyDeadline)
	early := deadline.Add(StopWriteBound / 2)
	thenUnder := func() {
		if after == nil || ctx.Err() != nil || h.gaveUp() {
			return
		}
		stepOf(h, laterBound(deadline), stepPrune, func(c context.Context) struct{} {
			after(c, root)
			return struct{}{}
		}, nil)
	}
	readBack := func() Verification {
		v, _ := stepOf(h, laterBound(deadline), stepReadBack, func(context.Context) Verification {
			now, err := readRecord(snap)
			if err != nil {
				return Verification{}
			}
			own := OwnerNone
			if now.hasPending() {
				own = probeOwner(snap)
			}
			return readVerdict(now, time.Now(), vetoedAt(snap), own, boundName(snap))
		}, nil)
		return v
	}
	// The record, and whether a veto stands beside it before this
	// verification begins: a write's that failed, which only a verdict wholly
	// this check's may clear (publishVerdict). A marker left by a process
	// that has ended goes first.
	type opened struct {
		m       Manifest
		err     error
		doubted bool
	}
	op, ok := stepOf(h, early, stepReadRecord, func(context.Context) opened {
		clearStaleMarker(root)
		m, err := readRecord(snap)
		return opened{m: m, err: err, doubted: vetoedAt(snap)}
	}, nil)
	if !ok || h.gaveUp() {
		return res, h.givenUp(ctx, nil)
	}
	if op.err != nil {
		thenUnder()
		if h.gaveUp() {
			return res, h.givenUp(ctx, nil)
		}
		return res, fmt.Errorf("read the snapshot's manifest: %w", op.err)
	}
	m, prior := op.m, op.m.Verification
	// The record before the check, which the verdict merges with: a VERIFIED
	// in it is set aside first, durably, or nothing is checked; the bundle is
	// followed by its prune all the same. Where no directory can be synced,
	// no veto is made, and what the check finds is not recorded over it.
	recheck := holdsVerified(prior) && !m.legacy()
	noBarrier := recheck && !syncsParentDir
	if recheck && !noBarrier {
		refused, ok := stepOf(h, early, stepVeto, func(context.Context) error { return setAside(snap) }, nil)
		switch {
		case !ok || h.gaveUp():
			// No check: its INCOMPLETE, merged over the VERIFIED, changes
			// nothing, so nothing is written.
			res.Record = readBack()
			return res, h.givenUp(ctx, nil)
		case refused != nil:
			res.Record = readBack()
			thenUnder()
			if h.gaveUp() {
				return res, h.givenUp(ctx, nil)
			}
			return res, refused
		}
	}
	checkCtx, cancel := context.WithDeadline(h.ctx, deadline)
	defer cancel()
	found, gaveUp, finished := runCheck(checkCtx, snap, m, o.Level)
	h.track(finished)
	if gaveUp {
		h.giveUp(nil, stepCheck)
	}
	if h.gaveUp() {
		// What the check answers, then or later, is dropped: the record says
		// it did not end.
		found = incompleteOf(checkCtx)
	}
	res.Found = found
	if afterCheck != nil {
		afterCheck(snap)
	}
	if m.legacy() {
		// Nothing to record: a reader reads of it what it would write.
		res.Legacy = true
		res.Record = readBack()
		thenUnder()
		if h.gaveUp() {
			return res, h.givenUp(ctx, nil)
		}
		return res, nil
	}
	if noBarrier {
		res.Record = readBack()
		thenUnder()
		if h.gaveUp() {
			return res, h.givenUp(ctx, nil)
		}
		return res, ErrRecheckNotRecorded
	}
	mayClear := !h.gaveUp()
	type published struct {
		written bool
		err     error
	}
	pub, wrote := stepOf(h, laterBound(deadline), stepWrite, func(context.Context) published {
		_, written, err := publishVerdict(snap, found, prior, op.doubted, mayClear)
		return published{written: written, err: err}
	}, nil)
	// Whatever the write answered: a kept bundle is followed by its prune,
	// which reads the record as it now stands; but never once a step was
	// given up.
	thenUnder()
	res.Record = readBack()
	if h.gaveUp() {
		var werr error
		if wrote {
			werr = pub.err
		}
		res.Recorded = wrote && pub.written && pub.err == nil
		return res, h.givenUp(ctx, werr)
	}
	if pub.err != nil {
		return res, pub.err
	}
	res.Recorded = pub.written
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
// Once it holds the lock, its one step (the record's read and the write) is
// bounded as a verification's are, StopWriteBound from the lock (hold.go),
// and given up past it, the folder marked until the step has ended. Where no
// lock is granted it writes nothing. A record that holds a verdict keeps it
// (mergeVerdict), and a veto that stood beside a VERIFIED stays (doubted).
// Its mark stands under the spellings given too, its caller's. It answers a
// channel that closes once its goroutine has ended.
func abandonPending(dir, backupsRoot string, ctx context.Context, onEnd func(), spellings ...string) <-chan struct{} {
	done := make(chan struct{})
	h := newHold(context.Background(), filepath.Base(spellingKey(dir)), append(spellings, filepath.Dir(dir), backupsRoot)...)
	go func() {
		defer close(done)
		defer h.drop()
		if onEnd != nil {
			defer onEnd()
		}
		defer h.release()
		wait, cancel := context.WithTimeout(context.Background(), StopWriteBound)
		defer cancel()
		snap, root, err := resolveSnapshotIn(dir, backupsRoot)
		if err != nil {
			return
		}
		key := ownerKey(root)
		unlock, err := lockBackupsWith(wait, root, StopWriteBound, folderToken(key))
		if err != nil {
			return
		}
		h.held(unlock, root, key, filepath.Base(snap))
		stepOf(h, h.lockedAt().Add(StopWriteBound), stepIncomplete, func(context.Context) struct{} {
			clearStaleMarker(root)
			// A snapshot from before verification takes no record.
			if m, err := readRecord(snap); err == nil && !m.legacy() {
				_, _, _ = publishVerdict(snap, incompleteOf(ctx), nil, vetoedAt(snap), true)
			}
			return struct{}{}
		}, nil)
	}()
	t := time.NewTimer(StopWriteBound)
	defer t.Stop()
	select {
	case <-done:
	case <-t.C:
	}
	return done
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
