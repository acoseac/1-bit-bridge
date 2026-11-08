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
// shutdown, a cancelled command) goes on to write INCOMPLETE once it has
// seen the stop, its check's own return included: past it the write is
// abandoned, to land or not, and the verification returns, so a stop is
// never held up by a filesystem that will not answer. Serve's wait for the
// backup ticker (backupShutdownWait) is far longer.
const StopWriteBound = 2 * time.Second

// VerifyOptions say how a verification runs.
type VerifyOptions struct {
	// Level is the database check's level; "" is CheckQuick.
	Level CheckLevel
	// LockWait is how long it waits for another process's verification or
	// prune to let the backups folder's lock go; 0 is AutoLockWait.
	LockWait time.Duration
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
// the lock of the folder that holds it, waiting its turn behind this
// process's other verifications: the database's check (its baseline, then
// SQLite's own at o.Level, the schema its user_version requires, and the core
// tables' counts) and the other files' (presence, size and SHA-256), on a
// thread of the lowest priority, its reads paced. It answers what it found
// and what the record says now. An error says the verdict was not recorded,
// or not durably: ErrBackupsBusy, a stop (ctx's error), the write's failure
// (ErrVerdictNotDurable among them).
func VerifySnapshot(ctx context.Context, dir string, o VerifyOptions) (VerifyResult, error) {
	res, err, _ := verifyAndThen(ctx, dir, o, nil)
	return res, err
}

// verifyAndThen is VerifySnapshot running after, under the same hold of the
// lock, once the verdict is written: the prune that follows a snapshot
// (Finish). abandoned says the stop's bound passed with the write under way.
func verifyAndThen(ctx context.Context, dir string, o VerifyOptions, after func(context.Context)) (res VerifyResult, err error, abandoned bool) {
	if o.Level == "" {
		o.Level = CheckQuick
	}
	if o.LockWait == 0 {
		o.LockWait = AutoLockWait
	}
	dir = filepath.Clean(dir)
	thenUnder := func() {
		if after != nil && ctx.Err() == nil {
			after(ctx)
		}
	}
	releaseSlot, err := takeSlot(ctx)
	if err != nil {
		abandonPending(dir, ctx)
		return res, err, false
	}
	defer releaseSlot()
	unlock, err := lockBackups(ctx, filepath.Dir(dir), o.LockWait)
	if err != nil {
		if ctx.Err() != nil {
			abandonPending(dir, ctx)
		}
		return res, err, false
	}
	m, err := readManifest(filepath.Join(dir, ManifestFile))
	if err != nil {
		thenUnder()
		unlock()
		return res, fmt.Errorf("read the snapshot's manifest: %w", err), false
	}
	checkCtx, cancel := context.WithTimeout(ctx, VerifyDeadline)
	defer cancel()
	stop := &stopBound{ctx: ctx}
	found, gaveUp := runCheck(stop, checkCtx, dir, m, o.Level)
	if gaveUp {
		// The check did not return within the stop's bound: what it was
		// doing is left to it, and the record says it did not end.
		found = incompleteOf(checkCtx)
	}
	res.Found = found
	if afterCheck != nil {
		afterCheck(dir)
	}
	if m.legacy() {
		// Nothing to record: a reader reads of it what it would write.
		res.Legacy = true
		res.Record = readVerdict(m, time.Now(), vetoedAt(dir))
		thenUnder()
		unlock()
		return res, nil, false
	}
	done := make(chan error, 1)
	go func() {
		_, err := publishVerdict(dir, found)
		// Whatever the write answered: a kept bundle is followed by its
		// prune, which reads the record as it now stands.
		thenUnder()
		unlock()
		done <- err
	}()
	var werr error
	select {
	case werr = <-done:
	case <-ctx.Done():
		select {
		case werr = <-done:
		case <-stop.expired():
			return res, fmt.Errorf("the verdict's write was abandoned %s after the stop: %w", StopWriteBound, ctx.Err()), true
		}
	}
	if now, rerr := readManifest(filepath.Join(dir, ManifestFile)); rerr == nil {
		res.Record = readVerdict(now, time.Now(), vetoedAt(dir))
	}
	if werr != nil {
		return res, werr, false
	}
	res.Recorded = true
	if err := ctx.Err(); err != nil && found.DatabaseState == StateIncomplete {
		return res, err, false
	}
	return res, nil, false
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
// waited its turn), within StopWriteBound, and abandons the write past it.
// The lock is tried until then: the verification that holds it is ending too.
// A record that holds a verdict keeps it (mergeVerdict).
func abandonPending(dir string, ctx context.Context) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		wait, cancel := context.WithTimeout(context.Background(), StopWriteBound)
		defer cancel()
		unlock, err := lockBackups(wait, filepath.Dir(dir), StopWriteBound)
		if err != nil {
			return
		}
		defer unlock()
		// A snapshot from before verification takes no record.
		if m, err := readManifest(filepath.Join(dir, ManifestFile)); err != nil || m.legacy() {
			return
		}
		_, _ = publishVerdict(dir, incompleteOf(ctx))
	}()
	t := time.NewTimer(StopWriteBound)
	defer t.Stop()
	select {
	case <-done:
	case <-t.C:
	}
}

// stopBound is a verification's one bound once its caller has stopped it
// (StopWriteBound), counted from the first wait that sees the stop, which
// the check's return and the verdict's write share: so a stopped
// verification returns within StopWriteBound of seeing the stop, whatever
// it was doing.
type stopBound struct {
	ctx context.Context
	at  time.Time // when a wait first saw the stop; zero before
}

// expired is the channel that fires once the bound has run out, counted
// from now if no wait has seen the stop before. Its caller has seen ctx end.
func (b *stopBound) expired() <-chan time.Time {
	if b.at.IsZero() {
		b.at = time.Now()
	}
	return time.After(time.Until(b.at.Add(StopWriteBound)))
}

// runCheck runs checkBundle on a goroutine of its own whose OS thread takes
// the lowest priority (runLowPriority), and waits for it: until it returns,
// or, once stop's context (the caller's) has ended, for what is left of the
// stop's bound, then gives up on it (gaveUp), a read the stop cannot reach
// (a hung mount) left to it. The check runs on checkCtx, the caller's with
// the deadline.
func runCheck(stop *stopBound, checkCtx context.Context, dir string, m Manifest, level CheckLevel) (v Verification, gaveUp bool) {
	out := make(chan Verification, 1)
	runLowPriority(func() { out <- checkBundle(checkCtx, dir, m, level) })
	select {
	case v = <-out:
		return v, false
	case <-stop.ctx.Done():
	}
	select {
	case v = <-out:
		return v, false
	case <-stop.expired():
		return Verification{}, true
	}
}

// onLowPriority, when set by a test, runs on the verification's thread once
// its priority is lowered.
var onLowPriority func()

// runLowPriority runs fn on a goroutine of its own, locked to its OS thread,
// whose CPU and I/O priority it lowers first (lowerThreadPriority), and never
// unlocks: the thread ends with the goroutine, so no other goroutine ever
// runs at the lowered priority. SQLite (modernc's, Go code on the caller's
// goroutine) and the hash read on that thread.
func runLowPriority(fn func()) {
	go func() {
		runtime.LockOSThread()
		lowerThreadPriority()
		if onLowPriority != nil {
			onLowPriority()
		}
		fn()
	}()
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
	// PruneErr its keep-policy's error, PruneSkipped a prune that found the
	// lock held.
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
	// Verify says how the bundle's verification runs.
	Verify VerifyOptions
	// OnKept, when set, is called once the bundle is kept, before its
	// verification.
	OnKept func(dir string)
}

// SnapshotAndVerify is the one path every snapshot takes: capture (Snapshot),
// then, for a kept bundle, its verification and the prune after it, whatever
// the verdict (Finish). The console's create takes the capture and Finish
// apart, returning between them.
func SnapshotAndVerify(ctx context.Context, src Sources, o SnapshotOptions) Result {
	dir, err := Snapshot(ctx, src)
	if err != nil {
		return Result{CaptureErr: err}
	}
	if o.OnKept != nil {
		o.OnKept(dir)
	}
	return Finish(ctx, dir, o.Keep, o.Verify)
}

// Finish verifies the kept bundle in dir and prunes the folder that holds it
// after it, whatever its verdict, under one hold of that folder's lock: so
// no other process's prune runs between the verdict and this one, and the
// prune keeps what recovery needs (PruneContext). A verification that cannot
// take the lock prunes nothing this round; one its caller stopped prunes
// nothing either.
func Finish(ctx context.Context, dir string, keep func() int, o VerifyOptions) Result {
	res := Result{Dir: dir}
	root := filepath.Dir(filepath.Clean(dir))
	pruned := make(chan struct{})
	var prune PruneResult
	var pruneErr error
	after := func(ctx context.Context) {
		n := 0
		if keep != nil {
			n = keep()
		}
		prune, pruneErr = pruneHeld(ctx, root, n)
		close(pruned)
	}
	var abandoned bool
	res.Verify, res.VerifyErr, abandoned = verifyAndThen(ctx, dir, o, after)
	select {
	case <-pruned:
		res.Prune, res.PruneErr = prune, pruneErr
	default:
		// No prune ran: the lock was held elsewhere, the caller stopped,
		// or the write was abandoned with the prune still to come after it.
		res.PruneSkipped = !abandoned
	}
	return res
}
