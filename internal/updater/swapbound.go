package updater

import "time"

// scmStopWait is how long a swap or a rollback on Windows waits for the SCM
// service to reach the Stopped state after it sends svc.Stop
// (stopServiceIfRunning, swap_windows.go). Long enough for an in-flight
// `/v1/download` to flush (the active-sessions gate above us already
// refused if any are in flight, but transient TCP teardown can still take a
// beat); short enough that an operator running `bridge update` gets
// feedback within a typical patience window.
//
// Here rather than beside its one use so that its bound is checked on every
// platform: an install and an operator's rollback hold the install marker's
// lock through that wait (backlog B692), and every other taker of the lock
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
