package backup

import (
	"context"
	"errors"
	"testing"
)

// TestAGiveUpIsTheStopOnlyWhereTheStopMadeItAlone pins the cause a hold's
// answer carries (givenUp) and what its caller takes it for once stopped
// (AsTheStop; backlog B895): a give-up its caller's stop made, by every step
// it gave up and with no write that failed, is that stop; a step given up at
// a bound of its own is the deadline it is, though the stop came before its
// answer was made, and makes every later give-up of the hold its own too; a
// write that failed beside a give-up the stop made is said; and a context the
// stop did not cancel takes nothing for it.
func TestAGiveUpIsTheStopOnlyWhereTheStopMadeItAlone(t *testing.T) {
	failed := errors.New("injected: the staging failed")
	for _, tc := range []struct {
		name      string
		give      func(h *hold, stop context.CancelFunc)
		werr      error
		cause     error
		isTheStop bool
	}{
		{"a step its stop gave up", func(h *hold, stop context.CancelFunc) {
			stop()
			h.giveUpAtStop(nil, stepWrite)
		}, nil, context.Canceled, true},
		{"a step its stop gave up, beside a write that failed", func(h *hold, stop context.CancelFunc) {
			stop()
			h.giveUpAtStop(nil, stepCheck)
		}, failed, context.Canceled, false},
		{"a step its own bound gave up, answered once stopped", func(h *hold, stop context.CancelFunc) {
			h.giveUp(nil, stepWrite)
			stop()
		}, nil, context.DeadlineExceeded, false},
		{"a step its stop gave up after one its own bound did", func(h *hold, stop context.CancelFunc) {
			h.giveUp(nil, stepReadRecord)
			stop()
			h.giveUpAtStop(nil, stepReadBack)
		}, nil, context.DeadlineExceeded, false},
		{"a step its own bound gave up after one its stop did", func(h *hold, stop context.CancelFunc) {
			stop()
			h.giveUpAtStop(nil, stepWrite)
			h.giveUp(nil, stepReadBack)
		}, nil, context.Canceled, false},
		{"its caller's give-up once stopped", func(h *hold, stop context.CancelFunc) {
			stop()
			h.abandon()
		}, nil, context.Canceled, true},
		{"its caller's give-up at its own bound, then its stop", func(h *hold, stop context.CancelFunc) {
			h.abandon()
			stop()
		}, nil, context.DeadlineExceeded, false},
		{"a stop's bound with no stop", func(h *hold, _ context.CancelFunc) {
			h.giveUpAtStop(nil, stepCheck)
		}, nil, context.DeadlineExceeded, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, stop := context.WithCancel(context.Background())
			defer stop()
			h := newHold(ctx, "snap", t.TempDir())
			defer h.drop()
			tc.give(h, stop)
			err := h.givenUp(ctx, tc.werr)
			if !errors.Is(err, ErrCheckGivenUp) || !errors.Is(err, tc.cause) {
				t.Fatalf("the answer %q is not ErrCheckGivenUp of %v", err, tc.cause)
			}
			if tc.cause == context.DeadlineExceeded && errors.Is(err, context.Canceled) {
				t.Errorf("the answer %q names the stop that came after its own bound", err)
			}
			if tc.werr != nil && !errors.Is(err, tc.werr) {
				t.Errorf("the answer %q dropped its write's failure", err)
			}
			got := AsTheStop(ctx, err)
			switch {
			case tc.isTheStop && got != ctx.Err():
				t.Errorf("AsTheStop(%q) = %v, want the stop's own error", err, got)
			case !tc.isTheStop && got != err:
				t.Errorf("AsTheStop(%q) = %v, want the answer itself", err, got)
			}
			if live := AsTheStop(context.Background(), err); live != err {
				t.Errorf("AsTheStop over a context not stopped = %v, want the answer itself", live)
			}
		})
	}
}
