package main

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/acoseac/1-bit-bridge/internal/supervision"
)

// serveRestart is how a restart request stops serve: the console's Restart
// (POST /api/restart, which "Install & restart" calls after the install) and
// the auto-installer's restart. It goes through the same cancel SIGINT and
// SIGTERM reach, so the shutdown is the whole one (the writers joined, the
// store checkpointed and closed, the auth store flushed: CLAUDE.md, "POST
// /api/restart must invoke the same cancellation closure"), and it marks the
// stop, so that serve's exit asks the supervisor to start it again
// (supervision.RestartExitCode).
//
// Until backlog B201 a restart request exited 0, as a stop does: launchd,
// whose LaunchAgent keeps the job alive on an unsuccessful exit only, left
// the bridge down, and so did the Windows SCM, whose service had no recovery
// actions (measured on both: "restarting": true, then nothing listening).
type serveRestart struct {
	requested atomic.Bool
	cancel    context.CancelFunc
}

// newServeRestart returns the restart request for a serve whose cancel is
// cancel.
func newServeRestart(cancel context.CancelFunc) *serveRestart {
	return &serveRestart{cancel: cancel}
}

// request marks the stop as a restart, then cancels serve. Safe from any
// goroutine, and more than once.
func (r *serveRestart) request() {
	r.requested.Store(true)
	r.cancel()
}

// exitCode is serve's exit code: code as it is, except a clean exit after a
// restart request, which is supervision.RestartExitCode. A failure keeps its
// own code, since it says more (a supervisor restarts it anyway), and a stop
// with no request (SIGINT, SIGTERM, the SCM's Stop) stays 0: a supervisor
// does not start a bridge its operator stopped.
func (r *serveRestart) exitCode(code int) int {
	if code == 0 && r.requested.Load() {
		return supervision.RestartExitCode
	}
	return code
}

// errRestartRequested is what a Windows service's serve function returns
// when serve exited to be restarted: the service handler reports
// supervision.RestartExitCode to the SCM as its service-specific exit code,
// which the service's recovery actions answer by starting it again.
var errRestartRequested = errors.New("serve exited to be restarted")

// serviceServeResult is the error a Windows service's serve function returns
// for run's exit code: nil for 0, errRestartRequested for
// supervision.RestartExitCode, and one naming the code for any other.
func serviceServeResult(code int) error {
	switch code {
	case 0:
		return nil
	case supervision.RestartExitCode:
		return errRestartRequested
	}
	return fmt.Errorf("subcommand exited with code %d", code)
}
