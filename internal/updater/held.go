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
// from 1, and the wait before the next, 0 where no attempt follows.
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
// hold (inUse), and wait sleeps before an attempt.
type moves struct {
	rename func(oldpath, newpath string) error
	probe  func(path string) error
	inUse  func(error) bool
	wait   func(time.Duration)
}

// renameInUse, probeMovable and renameWait are moves' inUse, probe and wait
// in production: the platform's predicate and probe (held_windows.go, and
// held_other.go's false and nil) and time.Sleep. Test seams, as renameFunc
// is: production code MUST NOT mutate them.
var (
	renameInUse  = inUse
	probeMovable = movable
	renameWait   = time.Sleep
)

// liveMoves is production's moves, read from the seams as each operation
// begins.
func liveMoves() moves {
	return moves{rename: renameFunc, probe: probeMovable, inUse: renameInUse, wait: renameWait}
}

// waitBudget is what an operation may still wait on one of its deadlines: the
// forward one, shared by every step made while the binary is in place and
// nothing is armed, or the recovery one, kept for the put-back alone. A wait
// that does not fit in what is left is not made.
type waitBudget struct{ left time.Duration }

func newBudget(schedule []time.Duration) *waitBudget {
	return &waitBudget{left: waitsTotal(schedule)}
}

// stepResult is a step's answer: its error, nil where it landed, the attempts
// it made and its waits between them, and whether its last refusal was a hold.
type stepResult struct {
	err      error
	attempts int
	waited   time.Duration
	inUse    bool
}

// try makes a step at once, and again after each of schedule's waits while it
// is refused for a hold and the wait fits in budget; a refusal of any other
// kind ends it at once, as it was answered, and a hold past the schedule or
// the budget ends it saying so (inUseError). The renames take no context: a
// step is bounded by its schedule, and a stop waits at most its waits.
func (m moves) try(stage RenameStage, from, to string, schedule []time.Duration, budget *waitBudget, step func() error) stepResult {
	var res stepResult
	for i := range schedule {
		if d := schedule[i]; d > 0 {
			m.wait(d)
			budget.left -= d
			res.waited += d
		}
		res.attempts++
		err := step()
		if err == nil {
			return stepResult{attempts: res.attempts, waited: res.waited}
		}
		res.inUse = m.inUse(err)
		again := res.inUse && i+1 < len(schedule) && schedule[i+1] <= budget.left
		var next time.Duration
		if again {
			next = schedule[i+1]
		}
		observeRefusal(RenameRefusal{Stage: stage, From: from, To: to, Err: err, Attempt: res.attempts, Wait: next})
		if !again {
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
// armed and nothing may wait (the swap's vacate, either place): its refusal is
// answered as it was, its kind recorded.
func (m moves) once(stage RenameStage, from, to string) stepResult {
	err := m.rename(from, to)
	if err == nil {
		return stepResult{attempts: 1}
	}
	observeRefusal(RenameRefusal{Stage: stage, From: from, To: to, Err: err, Attempt: 1})
	return stepResult{err: err, attempts: 1, inUse: m.inUse(err)}
}

// renameStep is try for a rename of from to to.
func (m moves) renameStep(stage RenameStage, from, to string, schedule []time.Duration, budget *waitBudget) stepResult {
	return m.try(stage, from, to, schedule, budget, func() error { return m.rename(from, to) })
}

// probeStep is try for a probe of path on the forward schedule.
func (m moves) probeStep(path string, budget *waitBudget) stepResult {
	return m.try(StageProbe, path, "", forwardWaits, budget, func() error { return m.probe(path) })
}

// probeIfThere is probeStep for a rename's destination, which need not be
// there: a probe that finds nothing at path passes.
func (m moves) probeIfThere(path string, budget *waitBudget) stepResult {
	return m.try(StageProbe, path, "", forwardWaits, budget, func() error {
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

// inUseError is a step's refusal for another process's hold past its schedule:
// the hold's own error, and the attempts and waits made for it to end.
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
