//go:build windows

package main

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"golang.org/x/sys/windows/svc"

	"github.com/acoseac/1-bit-bridge/internal/supervision"
)

// executeUntilReturn runs h.Execute as svc.Run would, with a control
// channel that sends req when it is not nil, and returns what Execute
// returned and every status it reported, in order.
func executeUntilReturn(t *testing.T, h *windowsServiceHandler, req *svc.ChangeRequest) (bool, uint32, []svc.State) {
	t.Helper()
	requests := make(chan svc.ChangeRequest, 1)
	statuses := make(chan svc.Status)
	type result struct {
		ssec  bool
		errno uint32
	}
	returned := make(chan result, 1)
	go func() {
		ssec, errno := h.Execute([]string{"com.acoseac.1-bit-bridge"}, requests, statuses)
		returned <- result{ssec, errno}
	}()
	var states []svc.State
	deadline := time.After(10 * time.Second)
	for {
		select {
		case s := <-statuses:
			states = append(states, s.State)
			if s.State == svc.Running && req != nil {
				requests <- *req
				req = nil
			}
		case r := <-returned:
			return r.ssec, r.errno, states
		case <-deadline:
			t.Fatalf("Execute did not return; statuses so far %v", states)
		}
	}
}

// TestTheServiceAnswersARestartWithItsRestartCode — serve exiting to be
// restarted makes Execute return supervision.RestartExitCode as a
// service-specific exit code, which svc.Run reports with SERVICE_STOPPED:
// the SCM counts it as a failure and its recovery actions start the service
// again. And Execute reports no Stopped status of its own first: the SCM took
// the exit code from that one, 0, so until backlog B201 a failure read as a
// clean stop.
func TestTheServiceAnswersARestartWithItsRestartCode(t *testing.T) {
	var stderr bytes.Buffer
	var ensured []string
	h := &windowsServiceHandler{
		ctx:    context.Background(),
		serve:  func(context.Context) error { return errRestartRequested },
		stderr: &stderr,
		ensureRecovery: func(name string) (bool, error) {
			ensured = append(ensured, name)
			return false, nil
		},
	}
	ssec, errno, states := executeUntilReturn(t, h, nil)
	if !ssec || errno != supervision.RestartExitCode {
		t.Errorf("Execute returned (%v, %d), want (true, %d)", ssec, errno, supervision.RestartExitCode)
	}
	for _, s := range states {
		if s == svc.Stopped {
			t.Errorf("Execute reported Stopped before it returned (statuses %v): the SCM takes that status's exit code, 0", states)
		}
	}
	if len(ensured) != 1 || ensured[0] != "com.acoseac.1-bit-bridge" {
		t.Errorf("recovery actions ensured for %q, want the service's own name once", ensured)
	}
}

// TestTheServiceAnswersAFailureAndAStopAsBefore — a serve that fails is
// (true, 1) and logged, with no Stopped reported first either; a clean exit
// is (false, 0); the SCM's Stop cancels serve and is (false, 0), which no
// recovery action answers.
func TestTheServiceAnswersAFailureAndAStopAsBefore(t *testing.T) {
	var stderr bytes.Buffer
	failing := &windowsServiceHandler{ctx: context.Background(), stderr: &stderr,
		serve: func(context.Context) error { return errors.New("listen tcp :7788: bind: address already in use") }}
	ssec, errno, states := executeUntilReturn(t, failing, nil)
	if !ssec || errno != 1 {
		t.Errorf("a failing serve: Execute returned (%v, %d), want (true, 1)", ssec, errno)
	}
	for _, s := range states {
		if s == svc.Stopped {
			t.Errorf("a failing serve: Execute reported Stopped before it returned (statuses %v)", states)
		}
	}
	if !bytes.Contains(stderr.Bytes(), []byte("address already in use")) {
		t.Errorf("the failure was not logged: %q", stderr.String())
	}

	clean := &windowsServiceHandler{ctx: context.Background(), stderr: &stderr,
		serve: func(context.Context) error { return nil }}
	if ssec, errno, _ := executeUntilReturn(t, clean, nil); ssec || errno != 0 {
		t.Errorf("a clean exit: Execute returned (%v, %d), want (false, 0)", ssec, errno)
	}

	stopped := &windowsServiceHandler{ctx: context.Background(), stderr: &stderr,
		serve: func(ctx context.Context) error { <-ctx.Done(); return nil }}
	ssec, errno, states = executeUntilReturn(t, stopped, &svc.ChangeRequest{Cmd: svc.Stop})
	if ssec || errno != 0 {
		t.Errorf("the SCM's Stop: Execute returned (%v, %d), want (false, 0)", ssec, errno)
	}
	if len(states) == 0 || states[len(states)-1] != svc.Stopped {
		t.Errorf("the SCM's Stop: statuses %v, want them to end Stopped", states)
	}
}
