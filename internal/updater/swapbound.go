package updater

import "time"

// scmStopWait is how long a swap on Windows waits for the SCM service to
// reach the Stopped state after it sends svc.Stop (stopServiceIfRunning,
// swap_windows.go); a rollback stops no service (backlog B898). Long enough
// for an in-flight `/v1/download` to flush (the active-sessions gate above us
// already refused if any are in flight, but transient TCP teardown can still
// take a beat); short enough that an operator running `bridge update` gets
// feedback within a typical patience window.
//
// Here rather than beside its one use so that its bound is checked on every
// platform: an install holds the install marker's lock through that wait
// (backlog B692), and every other taker of the lock
// (a migrating store open, a start's admission and housekeeping) waits
// defaultStateLockWait for it, so the stop may take at most half of that
// wait, leaving the rest for the marker's writes and the swap's renames
// (TestAnSCMStopFitsInsideTheMarkersLockWait). Serve's stop waits for the
// lock only to take back its start's count of an install, and to clear the
// start's token (backlog B660), within its stop's budget from the stop's
// arrival (cmd/bridge's stopBudget, 10 s since the third round), so the
// service the swap stops ends inside this wait; a serve with no count to take
// back takes no part of the lock
// (TestServeStopsWhileAnotherProcessHoldsTheMarkersLock, cmd/bridge).
const scmStopWait = 15 * time.Second

// SCMStopWait is scmStopWait, for serve's stop budget to be held inside it
// (cmd/bridge's TestTheStopBudgetIsInsideEveryServiceManagersWait).
const SCMStopWait = scmStopWait

// forwardWaits is the wait before each attempt of a step the updater makes
// while the bridge's binary is in place and nothing is armed (backlog B898):
// the staging of a candidate beside it, a probe of a file a later rename moves,
// the rollback's vacate. The first is at once, then 50 ms doubling to 1.6 s
// twice: eight attempts over 4.75 s, the deadline every such step of one
// operation shares, set as it begins (deadline), so that the time one step
// took, waiting out a hold or not, is the next's, each wait shortened to what
// remains of it, so that the last attempt comes at the deadline. The swap's
// vacate and either place have one attempt (moves.once).
//
// recoveryWaits is the put-back's, the one wait with the binary not in place,
// made only once a place was refused: seven attempts over 3.15 s, on a
// deadline of its own set as the put-back begins, which no step before it
// draws on and which never leaves its first attempt unmade. Each of its waits
// can only shorten the time with no binary on disk, against giving up, which
// leaves it missing until someone renames it back.
//
// Here, beside scmStopWait, so that what an operation holds the install
// marker's lock for is checked on every platform
// (TestEveryProtectedOperationFitsInsideTheMarkersLockWait). Never mutated.
var (
	forwardWaits = []time.Duration{0, 50 * time.Millisecond, 100 * time.Millisecond, 200 * time.Millisecond,
		400 * time.Millisecond, 800 * time.Millisecond, 1600 * time.Millisecond, 1600 * time.Millisecond}
	recoveryWaits = []time.Duration{0, 50 * time.Millisecond, 100 * time.Millisecond, 200 * time.Millisecond,
		400 * time.Millisecond, 800 * time.Millisecond, 1600 * time.Millisecond}
)

// waitsTotal is what schedule waits in all.
func waitsTotal(schedule []time.Duration) time.Duration {
	var total time.Duration
	for _, d := range schedule {
		total += d
	}
	return total
}

// ForwardWait and RecoveryWait are what an operation waits at most, all its
// steps before its first destructive rename together, and its put-back, for
// cmd/bridge's pin of the rollbacks at a start: waits, each at its length in
// the schedule, never the time a step under way takes.
func ForwardWait() time.Duration { return waitsTotal(forwardWaits) }

// RecoveryWait is ForwardWait's twin for the put-back.
func RecoveryWait() time.Duration { return waitsTotal(recoveryWaits) }
