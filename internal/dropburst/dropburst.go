// Package dropburst keeps the books a job pool needs to log its dropped jobs
// once per run of drops, never once per job (backlog B155).
//
// Both job pools (internal/analyze and internal/transcode) ask their live
// gate when a worker takes a job, and drop the job when the gate is closed.
// A switch-off drops a whole queue at once, up to 5,000 jobs a lane, and a
// line per job would bury every other line in the journal, so a pool writes
// ONE line per run of drops, with its count. It exists so there is one
// definition of when a run ends: the two pools carried a copy each.
//
// A run ends once no job the pool accepted is left undecided, or as soon as
// a job is decided to run, the gate having opened again. A job is undecided
// from the moment Enqueue accepts it to the moment a worker decides it (runs
// it, drops it, or passes over it at shutdown), so the count covers a job a
// worker has received from the queue and not yet decided, which the queue's
// length does not: a run ended at an empty queue would end under such a job,
// whose drop then logs a second line. TestAJobStillBeingDecidedKeepsItsBurstOpen
// and TestARenderStillBeingDecidedKeepsItsBurstOpen park one in the gate.
package dropburst

import (
	"log/slog"
	"sync"
)

// Tracker counts a pool's undecided jobs and its current run of drops, and
// writes the run's line when the run ends.
//
// It has no lock of its own: its state is guarded by the pool's mutex (mu,
// given to New), the one Enqueue holds while it accepts a job and the one
// the pool holds while it gives a job's path back and counts it. So a job is
// counted as undecided before any worker can take it. AcceptedLocked and
// PassedOverLocked are called with mu held; Ran and Drop take mu themselves.
// Nothing here calls back into the pool, and every line is written with mu
// released.
//
// **Drop takes a critical section of its own, and the pool calls it BEFORE
// the section in which it gives the dropped job's path back and counts it.**
// Drop writes the run's line, when this drop ends the run, before it
// returns, so the count of the drop that ends a run means its line has
// landed: the order every exit of a pool keeps (its bookkeeping, then its
// count), and the one that lets a caller who sees the pool idle after a
// switch-off read the line. Folding Drop into the counting section would
// write the line only after that section's unlock, after the count is
// visible, and a test that waited for the pool to go idle then read the log
// before the line landed, in 6 of 20 runs under -race
// (TestAJobStillBeingDecidedKeepsItsBurstOpen, during B155). Don't merge the
// two sections.
type Tracker struct {
	mu     sync.Locker
	logger *slog.Logger
	msg    string

	// undecided counts the jobs accepted and not yet decided; run counts the
	// drops since the last line. Both are guarded by mu. run > 0 implies
	// undecided > 0 between calls: a run that leaves nothing undecided ends.
	undecided int
	run       uint64
}

// New returns a Tracker guarded by mu, the pool's own mutex, that writes
// each finished run as one Info line, msg with a `count` attribute, through
// logger.
func New(mu sync.Locker, logger *slog.Logger, msg string) *Tracker {
	return &Tracker{mu: mu, logger: logger, msg: msg}
}

// AcceptedLocked records a job Enqueue accepted: it is undecided until a
// worker decides it. Called with mu held, in the section that hands the job
// to the queue, so no worker can decide it first.
func (t *Tracker) AcceptedLocked() {
	t.undecided++
}

// Ran records that a job a worker took passed its gate and runs: the job
// leaves the undecided count, and a run of drops before it is over.
func (t *Tracker) Ran() {
	t.mu.Lock()
	t.undecided--
	n := t.endLocked(true)
	t.mu.Unlock()
	t.Write(n)
}

// Drop records a job a worker took while its gate was closed: the job leaves
// the undecided count and joins the run, and the drop that leaves nothing
// undecided writes the run's line. Call it before the section that gives the
// job's path back and counts it, never inside it (see Tracker).
func (t *Tracker) Drop() {
	t.mu.Lock()
	t.undecided--
	t.run++
	n := t.endLocked(false)
	t.mu.Unlock()
	t.Write(n)
}

// PassedOverLocked records a job that leaves the undecided count with no
// decision (one the pool took while stopping, or one whose gate panicked),
// which can end a run it was the last undecided job of. Called with mu held;
// it returns the finished run's count, for Write once mu is released.
func (t *Tracker) PassedOverLocked() uint64 {
	t.undecided--
	return t.endLocked(false)
}

// Write writes a finished run's line, with mu released. A zero count writes
// nothing.
func (t *Tracker) Write(n uint64) {
	if n > 0 {
		t.logger.Info(t.msg, "count", n)
	}
}

// endLocked ends the current run if it is over and returns its count, else
// 0: over once nothing accepted is left undecided, or when a job runs (ran).
// mu is held.
func (t *Tracker) endLocked(ran bool) uint64 {
	if t.run == 0 || (!ran && t.undecided > 0) {
		return 0
	}
	n := t.run
	t.run = 0
	return n
}
