package updater

import (
	"errors"
	"fmt"
	"io/fs"
	"sync/atomic"
	"time"
)

// The updater's renames of the bridge's binary, and the probes before them,
// are made again while Windows answers that another process holds the file,
// and only while the binary is in place and nothing is armed (backlog B898).
// A real-time scanner or the search indexer holds an executable it has just
// seen written or run, and the rollbacks at a start run the backup for its
// version just before they rename it. Each step is retried on its own, never
// the swap or the rollback whole, on one of swapbound.go's schedules.

// RenameStage names a step of the swap or the rollback, which a test's
// observer hears of when it is refused (SetRenameObserverForTest).
type RenameStage string

const (
	// StageStaging is the candidate's rename beside the binary.
	StageStaging RenameStage = "staging"
	// StageProbe is a file a later rename moves, opened for deletion first.
	StageProbe RenameStage = "probe"
	// StageVacate is the binary's rename out of its place: to the backup in
	// the swap, aside in the rollback.
	StageVacate RenameStage = "vacate"
	// StagePlace is the new bytes' rename into the binary's place: the
	// candidate's in the swap, the backup's in the rollback.
	StagePlace RenameStage = "place"
	// StagePutBack is the vacated binary's rename back into its place, once
	// the place was refused.
	StagePutBack RenameStage = "putback"
	// StageInstall is the backup's rename onto the binary's path where
	// nothing stands there.
	StageInstall RenameStage = "install"
)

// RenameRefusal is one refused attempt at a step: its stage, the file it moves
// (From) and where to (To, empty for a probe), the error, which attempt it was,
// from 1, and the wait admitted before the next, as it is asked of the clock
// (the schedule's, or what remains of the deadline where that is less), 0
// where none is: an admitted wait is always followed by its attempt.
type RenameRefusal struct {
	Stage    RenameStage
	From, To string
	Err      error
	Attempt  int
	Wait     time.Duration
}

// renameObserver, nil in production, hears every refused attempt
// (SetRenameObserverForTest).
var renameObserver atomic.Pointer[func(RenameRefusal)]

// SetRenameObserverForTest makes fn hear every refused attempt at a step of the
// swap or the rollback, on the goroutine that made it and before the wait or
// the step after it, and returns what puts the observer before it back. A test
// lets a hold go from it, after a refusal it heard, and a diagnostic names
// what held the file. Production code MUST NOT call it: with none set, a
// refused attempt costs one atomic load. Process-global: a test that sets it
// MUST NOT call t.Parallel(), and restores it once every operation it started
// has returned.
func SetRenameObserverForTest(fn func(RenameRefusal)) (restore func()) {
	var p *func(RenameRefusal)
	if fn != nil {
		p = &fn
	}
	prev := renameObserver.Swap(p)
	return func() { renameObserver.Store(prev) }
}

// moves is how the updater moves the bridge's binary: rename makes one rename,
// probe one probe (movable), inUse says whether a refusal is another process's
// hold (inUse), wait sleeps before an attempt, and now reads the clock its
// deadlines are kept on.
type moves struct {
	rename func(oldpath, newpath string) error
	probe  func(path string) error
	inUse  func(error) bool
	wait   func(time.Duration)
	now    func() time.Time
}

// renameInUse, probeMovable, renameWait and renameNow are moves' inUse,
// probe, wait and now in production: the platform's predicate and probe
// (held_windows.go, and held_other.go's false and nil), time.Sleep and
// time.Now, whose monotonic reading the deadlines compare. Test seams, as
// renameFunc is: production code MUST NOT mutate them, and a test that sets
// renameWait sets renameNow to a clock its waits advance.
var (
	renameInUse  = inUse
	probeMovable = movable
	renameWait   = time.Sleep
	renameNow    = time.Now
)

// liveMoves is production's moves, read from the seams as each operation
// begins.
func liveMoves() moves {
	return moves{rename: renameFunc, probe: probeMovable, inUse: renameInUse, wait: renameWait, now: renameNow}
}

// deadline is the time past which an operation admits no wait and begins no
// step: the forward one, set as the operation begins and shared by every step
// made while the binary is in place and nothing is armed, or the recovery
// one, set as the put-back begins. A wait is admitted only while some of it
// remains, and shortened to what remains, and the attempt after an admitted
// wait is made however long the wait or the steps before it took, so that a
// schedule's last attempt comes at the deadline, past it by no more than its
// wait overran, and on a clock nothing but the waits moves the schedules are
// exact: eight attempts, the last at 4.75 s, and seven, the last at 3.15 s.
// A step under way is never cut short (a rename takes no context): once the
// deadline is seen to have passed, no further step is begun and no wait
// admitted, but for the two the destructive renames need, the place after a
// vacate that landed and the put-back's first attempt after a refused place.
type deadline struct{ at time.Time }

// deadlineOf is the deadline schedule's waits set from now.
func (m moves) deadlineOf(schedule []time.Duration) deadline {
	return deadline{at: m.now().Add(waitsTotal(schedule))}
}

// passed reports whether d is behind m's clock.
func (m moves) passed(d deadline) bool { return m.now().After(d.at) }

// stepResult is a step's answer: its error, nil where it landed, the attempts
// it made and its waits between them, each as it was asked of the clock (the
// schedule's, or what remained of the deadline where that was less; never
// what the clock read after it), and whether its last refusal was a hold. A
// step not tried for its deadline made no attempt, and its error is
// ErrDeadlinePassed.
type stepResult struct {
	err      error
	attempts int
	waited   time.Duration
	inUse    bool
}

// try makes a step whose deadline has not passed (retry); one it has
// passed before its first attempt is not tried at all (ErrDeadlinePassed).
func (m moves) try(stage RenameStage, from, to string, schedule []time.Duration, dl deadline, step func() error) stepResult {
	if m.passed(dl) {
		return stepResult{err: ErrDeadlinePassed}
	}
	return m.retry(stage, from, to, schedule, dl, step)
}

// retry makes a step at once, whatever dl says, and again after each of
// schedule's waits while it is refused for a hold: a wait is admitted only
// while some of dl remains, shortened to what remains, and the attempt after
// it is made however long the wait took. A refusal of any other kind ends the
// step at once, as it was answered, and a hold past the schedule, or once dl
// leaves no wait to admit, ends it saying so (inUseError). The renames take no
// context: a step's waits are bounded by its schedule, and a stop waits at
// most those.
func (m moves) retry(stage RenameStage, from, to string, schedule []time.Duration, dl deadline, step func() error) stepResult {
	var res stepResult
	var wait time.Duration
	for i := range schedule {
		if wait > 0 {
			m.wait(wait)
			res.waited += wait
		}
		res.attempts++
		err := step()
		if err == nil {
			return stepResult{attempts: res.attempts, waited: res.waited}
		}
		res.inUse, wait = m.inUse(err), 0
		if left := dl.at.Sub(m.now()); res.inUse && i+1 < len(schedule) && left > 0 {
			wait = min(schedule[i+1], left)
		}
		observeRefusal(RenameRefusal{Stage: stage, From: from, To: to, Err: err, Attempt: res.attempts, Wait: wait})
		if wait == 0 {
			res.err = err
			if res.inUse {
				res.err = &inUseError{err: err, attempts: res.attempts, waited: res.waited}
			}
			return res
		}
	}
	return res
}

// once makes a step one time, where the binary is not in place or a marker is
// armed and nothing may wait (the swap's vacate, either place), whatever the
// deadline says: its refusal is answered as it was, its kind recorded.
func (m moves) once(stage RenameStage, from, to string) stepResult {
	err := m.rename(from, to)
	if err == nil {
		return stepResult{attempts: 1}
	}
	observeRefusal(RenameRefusal{Stage: stage, From: from, To: to, Err: err, Attempt: 1})
	return stepResult{err: err, attempts: 1, inUse: m.inUse(err)}
}

// renameStep is try for a rename of from to to.
func (m moves) renameStep(stage RenameStage, from, to string, schedule []time.Duration, dl deadline) stepResult {
	return m.try(stage, from, to, schedule, dl, func() error { return m.rename(from, to) })
}

// putBack is the put-back of from to to on the recovery schedule, its
// deadline set as it begins and its first attempt made whatever the clock
// reads by then, so that no deadline leaves it untried; only its later
// attempts are bounded by that deadline, and a rename that lands is the
// put-back's success however late it lands.
func (m moves) putBack(from, to string) stepResult {
	return m.retry(StagePutBack, from, to, recoveryWaits, m.deadlineOf(recoveryWaits),
		func() error { return m.rename(from, to) })
}

// probeStep is try for a probe of path on the forward schedule.
func (m moves) probeStep(path string, dl deadline) stepResult {
	return m.try(StageProbe, path, "", forwardWaits, dl, func() error { return m.probe(path) })
}

// probeIfThere is probeStep for a rename's destination, which need not be
// there: a probe that finds nothing at path passes.
func (m moves) probeIfThere(path string, dl deadline) stepResult {
	return m.try(StageProbe, path, "", forwardWaits, dl, func() error {
		if err := m.probe(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	})
}

// observeRefusal tells the observer a test set, where one is, of a refused
// attempt.
func observeRefusal(r RenameRefusal) {
	if fn := renameObserver.Load(); fn != nil {
		(*fn)(r)
	}
}

// inUseError is a step's refusal for another process's hold past its schedule
// or its deadline: the hold's own error, and the attempts and the waits made
// for it to end, each wait as it was asked of the clock (stepResult), never
// the time the clock read.
type inUseError struct {
	err      error
	attempts int
	waited   time.Duration
}

func (e *inUseError) Error() string {
	attempts := "attempts"
	if e.attempts == 1 {
		attempts = "attempt"
	}
	return fmt.Sprintf("%v (in use by another process: %d %s over %s)", e.err, e.attempts, attempts, e.waited)
}

func (e *inUseError) Unwrap() error { return e.err }
