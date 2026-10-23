package dlna

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// TestRetirementReportsAByebyeTimeoutAsDeadlineExceeded holds the byebye
// until its socket deadline, after the endpoint's normal sends have joined.
func TestRetirementReportsAByebyeTimeoutAsDeadlineExceeded(t *testing.T) {
	for _, operation := range []string{"Update", "Withdraw"} {
		t.Run(operation, func(t *testing.T) {
			n := newFakeNet()
			sub := fakeSubsystem(t, n)
			h, _ := mustRegister(t, sub, legacyReg(msUDN, fakeServed(), SSDPEndpoint{Interface: en0, Location: msOnEn0}))
			n.mu.Lock()
			n.blockSends["en0"] = true
			n.mu.Unlock()
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			var err error
			if operation == "Update" {
				_, err = h.Update(ctx, nil)
			} else {
				err = h.Withdraw(ctx)
			}
			if !errors.Is(err, os.ErrDeadlineExceeded) {
				t.Errorf("%s = %v, want byebye socket timeout", operation, err)
			}
			assertRetirementExpiryOnce(t, err)
			if l, s, r := n.openSockets(); l+s+r != 0 {
				t.Errorf("retirement left sockets open: %d/%d/%d", l, s, r)
			}
			if got := len(notifies(n, 0, "ssdp:byebye")); got != 0 {
				t.Errorf("timed-out retirement sent %d byebyes", got)
			}
		})
	}
}

// TestRollbackReportsAByebyeTimeoutAsDeadlineExceeded parks Close before
// cancellation and its watcher so rollback's byebye reaches its own timeout.
func TestRollbackReportsAByebyeTimeoutAsDeadlineExceeded(t *testing.T) {
	for _, operation := range []string{"Register", "Update"} {
		t.Run(operation, func(t *testing.T) {
			n := newFakeNet()
			sub := fakeSubsystem(t, n)
			reg := legacyReg(msUDN, fakeServed(), SSDPEndpoint{Interface: en1, Location: msOnEn1})
			reg.Targets = reg.Targets[:1]
			var h *SSDPHandle
			if operation == "Update" {
				initial := reg
				initial.Endpoints = []SSDPEndpoint{{Interface: en0, Location: msOnEn0}}
				h, _ = mustRegister(t, sub, initial)
			}
			alive, releaseAlive := make(chan struct{}), make(chan struct{})
			begun, resumeClose := make(chan struct{}), make(chan struct{})
			cancel := sub.cancel
			sub.cancel = func() { close(begun); <-resumeClose; cancel() }
			n.mu.Lock()
			n.onSenderWrite = func(s *fakeSender, pkt string) {
				if s.iface == "en1" && header(pkt, "NTS") == "ssdp:alive" {
					close(alive)
					<-releaseAlive
				}
			}
			n.mu.Unlock()
			done := make(chan error, 1)
			go func() {
				if operation == "Register" {
					_, _, err := sub.Register(context.Background(), reg)
					done <- err
				} else {
					_, err := h.Update(context.Background(), append([]SSDPEndpoint{{Interface: en0, Location: msOnEn0}}, reg.Endpoints...))
					done <- err
				}
			}()
			<-alive
			// The alive write already captured block=false; only the following
			// rollback byebye blocks, keeping the timeout distinct from join expiry.
			n.mu.Lock()
			n.blockSends["en1"] = true
			n.mu.Unlock()
			ctx, stop := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer stop()
			closed := make(chan error, 1)
			go func() { closed <- sub.Close(ctx) }()
			<-begun
			close(releaseAlive)
			err := <-done
			close(resumeClose)
			<-closed
			if !errors.Is(err, ErrSSDPClosed) || !errors.Is(err, os.ErrDeadlineExceeded) {
				t.Errorf("%s rollback = %v, want closed and byebye socket timeout", operation, err)
			}
			assertRetirementExpiryOnce(t, err)
			if l, s, r := n.openSockets(); l+s+r != 0 {
				t.Errorf("rollback left sockets open: %d/%d/%d", l, s, r)
			}
		})
	}
}

// TestUpdateRetirementBudgetIncludesListenerRelease expires the budget
// after every byebye, while the removed endpoint's listener is being released.
func TestUpdateRetirementBudgetIncludesListenerRelease(t *testing.T) {
	for _, budget := range []string{"own", "Close"} {
		t.Run(budget, func(t *testing.T) {
			n := newFakeNet()
			sub := fakeSubsystem(t, n)
			h, _ := mustRegister(t, sub, legacyReg(msUDN, fakeServed(), SSDPEndpoint{Interface: en0, Location: msOnEn0}))
			entered, release := make(chan struct{}), make(chan struct{})
			n.openListener(t, "en0").onClose = func() { close(entered); <-release }
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			updateCtx := context.Background()
			if budget == "own" {
				updateCtx = ctx
			}
			done := make(chan error, 1)
			go func() { _, err := h.Update(updateCtx, nil); done <- err }()
			<-entered
			closed := make(chan error, 1)
			if budget == "Close" {
				go func() { closed <- sub.Close(ctx) }()
				waitFor(t, "Close to begin", func() bool { sub.mu.Lock(); defer sub.mu.Unlock(); return sub.closed })
			}
			<-ctx.Done()
			close(release)
			err := <-done
			if budget == "Close" {
				<-closed
			}
			assertRetirementExpiryOnce(t, err)
			if got := len(notifies(n, 0, "ssdp:byebye")); got != len(h.targets) {
				t.Errorf("byebyes before cleanup = %d, want %d", got, len(h.targets))
			}
			if l, s, r := n.openSockets(); l+s+r != 0 {
				t.Errorf("retirement left sockets open: %d/%d/%d", l, s, r)
			}
		})
	}
}

// TestWithdrawalBudgetIncludesHandleRemoval parks after retirement has
// finished, while Close can still take ownership of the listed handle.
func TestWithdrawalBudgetIncludesHandleRemoval(t *testing.T) {
	n := newFakeNet()
	sub := fakeSubsystem(t, n)
	h, _ := mustRegister(t, sub, legacyReg(msUDN, fakeServed(), SSDPEndpoint{Interface: en0, Location: msOnEn0}))
	entered, release := make(chan struct{}), make(chan struct{})
	cancel := h.cancel
	h.cancel = func() { cancel(); close(entered); <-release }
	withdrawn := make(chan error, 1)
	go func() { withdrawn <- h.Withdraw(context.Background()) }()
	<-entered
	if l, s, r := n.openSockets(); l+s+r != 0 {
		t.Errorf("retirement left sockets open: %d/%d/%d", l, s, r)
	}
	ctx, stop := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer stop()
	closed := make(chan error, 1)
	go func() { closed <- sub.Close(ctx) }()
	waitFor(t, "Close to begin", func() bool { sub.mu.Lock(); defer sub.mu.Unlock(); return sub.closed })
	close(release)
	assertRetirementExpiryOnce(t, <-withdrawn)
	<-closed
}

// assertRetirementExpiryOnce checks both error identity and duplicate reporting.
func assertRetirementExpiryOnce(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("retirement = %v, want context.DeadlineExceeded", err)
	} else if got := strings.Count(err.Error(), context.DeadlineExceeded.Error()); got != 1 {
		t.Errorf("retirement reports deadline exceeded %d times: %v", got, err)
	}
}
