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
// (TestAnSCMStopFitsInsideTheMarkersLockWait). Serve's stop takes no part of
// the lock, so the service the swap stops never waits on it
// (TestServeStopsWhileAnotherProcessHoldsTheMarkersLock, cmd/bridge).
const scmStopWait = 15 * time.Second
