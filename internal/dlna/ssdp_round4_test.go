package dlna

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

// TestCloseClampsAStaleWithdrawalDeadline parks after the withdrawal's
// deadline read, and holds Close before its asynchronous sender interruption.
func TestCloseClampsAStaleWithdrawalDeadline(t *testing.T) {
	n := newFakeNet()
	sub := fakeSubsystem(t, n)
	selected, install := make(chan struct{}), make(chan struct{})
	sub.beforeByebyeDeadline = func() { close(selected); <-install }
	begun, resumeClose := make(chan struct{}), make(chan struct{})
	cancel := sub.cancel
	sub.cancel = func() { cancel(); close(begun); <-resumeClose }
	h, _ := mustRegister(t, sub, legacyReg(msUDN, fakeServed(), SSDPEndpoint{Interface: en0, Location: msOnEn0}))
	owned := h.endpoints[0].sender.(*ssdpOwnedSender)
	sender := owned.ssdpSendConn.(*fakeSender)
	withdrawn := make(chan error, 1)
	go func() { withdrawn <- h.Withdraw(context.Background()) }()
	<-selected
	deadline := time.Now().Add(-time.Second)
	ctx, stop := context.WithDeadline(context.Background(), deadline)
	defer stop()
	closed := make(chan error, 1)
	go func() { closed <- sub.Close(ctx) }()
	<-begun
	// Close must cut an installed deadline too, including a pending write;
	// the watcher has not run and cannot provide that guarantee for it.
	sender.mu.Lock()
	current := sender.deadline
	sender.mu.Unlock()
	if current.After(deadline) || current.IsZero() {
		t.Errorf("Close left current write deadline %v later than %v", current, deadline)
	}
	close(install)
	err := <-withdrawn
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Errorf("stale deadline write = %v, want sender timeout", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Withdraw = %v, want deadline exceeded", err)
	}
	if got := len(notifies(n, 0, "ssdp:byebye")); got != 0 {
		t.Errorf("sent %d byebyes after Close's deadline", got)
	}
	close(resumeClose)
	if err := <-closed; !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Close = %v, want deadline exceeded", err)
	}
}

// TestWithdrawalBudgetIncludesListenerRelease holds the last release after
// all byebyes. Close owns the handle until that release and its result commit.
func TestWithdrawalBudgetIncludesListenerRelease(t *testing.T) {
	for _, when := range []string{"expired", "in_time", "after_release", "own_expired"} {
		t.Run(when, func(t *testing.T) {
			n := newFakeNet()
			sub := fakeSubsystem(t, n)
			h, _ := mustRegister(t, sub, legacyReg(msUDN, fakeServed(), SSDPEndpoint{Interface: en0, Location: msOnEn0}))
			entered, release := make(chan struct{}), make(chan struct{})
			n.openListener(t, "en0").onClose = func() { close(entered); <-release }
			withdrawCtx := context.Background()
			if when == "own_expired" {
				var cancel context.CancelFunc
				withdrawCtx, cancel = context.WithTimeout(withdrawCtx, 100*time.Millisecond)
				defer cancel()
			}
			withdrawn := make(chan error, 1)
			go func() { withdrawn <- h.Withdraw(withdrawCtx) }()
			<-entered
			if got := len(notifies(n, 0, "ssdp:byebye")); got != len(h.targets) {
				t.Errorf("byebyes before release = %d, want %d", got, len(h.targets))
			}
			closed := make(chan error, 1)
			if when == "expired" || when == "in_time" {
				budget := time.Second * 5
				if when == "expired" {
					budget = 50 * time.Millisecond
				}
				ctx, cancel := context.WithTimeout(context.Background(), budget)
				defer cancel()
				go func() { closed <- sub.Close(ctx) }()
				waitFor(t, "Close to begin", func() bool { sub.mu.Lock(); defer sub.mu.Unlock(); return sub.closed })
				if when == "expired" {
					<-ctx.Done()
				}
			} else if when == "own_expired" {
				<-withdrawCtx.Done()
			}
			close(release)
			err := <-withdrawn
			if when == "expired" || when == "own_expired" {
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Errorf("Withdraw = %v, want deadline exceeded during release", err)
				}
			} else if err != nil {
				t.Errorf("Withdraw finished in budget: %v", err)
			}
			if when == "expired" || when == "in_time" {
				closeErr := <-closed
				if when == "in_time" && closeErr != nil {
					t.Errorf("Close finished in budget: %v", closeErr)
				}
			} else {
				sub.mu.Lock()
				remaining := len(sub.handles)
				sub.mu.Unlock()
				if remaining != 0 {
					t.Errorf("completed withdrawal left %d handles", remaining)
				}
				ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
				defer cancel()
				_ = sub.Close(ctx) // a later Close cannot own this withdrawal
			}
		})
	}
}
