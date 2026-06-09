//go:build windows

package main

import (
	"context"
	"fmt"
	"io"

	"golang.org/x/sys/windows/svc"
)

// isWindowsService reports whether this process was launched by the
// Service Control Manager. We call it in serveCmd to decide between
// the normal foreground loop and the svc.Run dispatch.
//
// `svc.IsWindowsService` is the canonical check — it detects a session
// id of 0 plus the SCM parent process. Returns false on any error
// (including "not on Windows", which is impossible in this file since
// the `//go:build windows` constraint gates it, but the stub in
// service_other.go covers that branch).
func isWindowsService() bool {
	ok, err := svc.IsWindowsService()
	if err != nil {
		return false
	}
	return ok
}

// runAsWindowsService wraps the existing serve body in an svc.Handler.
// The SCM delivers lifecycle events via the args channel; we translate
// SERVICE_CONTROL_STOP into context cancel so the normal shutdown path
// runs unchanged. Status updates (StartPending → Running → StopPending
// → Stopped) give the SCM UI accurate state — without them, the service
// shows as "Starting" forever.
//
// This function only returns once the service exits. Caller (serveCmd
// on windows) should treat that as a successful termination.
func runAsWindowsService(ctx context.Context, name string, serve func(context.Context) error, stderr io.Writer) error {
	// svc.Run blocks until the handler's Execute returns. The handler
	// we pass delegates to the supplied serve func with a cancellable
	// context derived from the outer one.
	return svc.Run(name, &windowsServiceHandler{
		ctx:    ctx,
		serve:  serve,
		stderr: stderr,
	})
}

type windowsServiceHandler struct {
	ctx    context.Context
	serve  func(context.Context) error
	stderr io.Writer
}

// Execute is called on a dedicated goroutine by svc.Run. It MUST:
//
//   - Promptly send a StartPending then Running status to `status`.
//   - Drain `r` for Stop / Shutdown requests and respond with StopPending
//     then Stopped before returning.
//   - Return `ssec=false` (not a service-specific error code) unless we
//     want to surface a specific Win32 error to the SCM UI.
func (h *windowsServiceHandler) Execute(args []string, r <-chan svc.ChangeRequest, status chan<- svc.Status) (ssec bool, errno uint32) {
	// Advertise that we're starting and what controls we accept. Stop +
	// Shutdown are the minimum — we don't handle pause/continue because
	// the bridge has no meaningful paused state.
	const acceptedCommands = svc.AcceptStop | svc.AcceptShutdown
	status <- svc.Status{State: svc.StartPending}

	// Run serve in its own goroutine so this handler can keep draining
	// `r` while it runs.
	ctx, cancel := context.WithCancel(h.ctx)
	defer cancel()
	serveErr := make(chan error, 1)
	go func() {
		// Recover a panic in serve so the SCM Stop branch's `<-serveErr` can't
		// hang forever — a dead goroutine never sends, leaving the service stuck
		// and unable to shut down. Convert the panic to an error so Stop drains
		// cleanly. DeepSeek review.
		defer func() {
			// `rec`, not `r` — the outer `r <-chan svc.ChangeRequest` parameter
			// is in scope here; avoid shadowing it. CodeRabbit on PR #368.
			if rec := recover(); rec != nil {
				serveErr <- fmt.Errorf("serve panicked: %v", rec)
			}
		}()
		serveErr <- h.serve(ctx)
	}()

	// Declare ourselves Running once the listener is up. We can't
	// actually observe the listen-ready moment from here without
	// instrumenting the serve body; the gap is negligible in practice
	// (Go's net.Listen returns before SCM polls).
	status <- svc.Status{State: svc.Running, Accepts: acceptedCommands}

	for {
		select {
		case req := <-r:
			switch req.Cmd {
			case svc.Interrogate:
				status <- req.CurrentStatus
			case svc.Stop, svc.Shutdown:
				status <- svc.Status{State: svc.StopPending}
				cancel()
				<-serveErr // wait for serve to exit so we don't yank listeners mid-shutdown
				status <- svc.Status{State: svc.Stopped}
				return false, 0
			}
		case err := <-serveErr:
			// serve exited on its own — usually a bind failure or
			// config error. Treat as a service-internal error so the
			// SCM UI surfaces a real errno, not just "the service
			// stopped unexpectedly".
			status <- svc.Status{State: svc.Stopped}
			if err != nil {
				// Log to stderr (which in service mode the packaging
				// redirects to a file under %PROGRAMDATA%). svcerrno
				// 1 = ERROR_INVALID_FUNCTION — an honest "we failed
				// and you should check the logs".
				io.WriteString(h.stderr, "bridge service exited: "+err.Error()+"\n")
				return true, 1
			}
			return false, 0
		}
	}
}
