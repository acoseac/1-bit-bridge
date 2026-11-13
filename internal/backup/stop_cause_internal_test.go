package backup

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/ctxerr"
)

// TestAGiveUpIsTheStopOnlyWhereTheStopMadeItAlone pins the give-up a hold's
// answer names (givenUp, givenFor) and what its caller takes it for once
// stopped (AsTheStop; backlog B895): a give-up its caller's stop made, by
// every step it gave up and with no error of the worker's own published
// before it, is that stop; a step given up at a bound of its own is the
// deadline it is, named whether the stop gave another step up before it or
// came before its answer was made; an error the worker published (a write,
// a read or a recheck that failed) beside a give-up the stop made is said;
// and a context the stop did not cancel takes nothing for it.
func TestAGiveUpIsTheStopOnlyWhereTheStopMadeItAlone(t *testing.T) {
	failed := errors.New("injected: the staging failed")
	for _, tc := range []struct {
		name      string
		give      func(h *hold, stop context.CancelFunc)
		published error
		cause     error
		step      string
		isTheStop bool
	}{
		{"a step its stop gave up", func(h *hold, stop context.CancelFunc) {
			stop()
			h.giveUpAtStop(nil, stepWrite)
		}, nil, context.Canceled, stepWrite, true},
		{"a step its stop gave up, beside an error published before it", func(h *hold, stop context.CancelFunc) {
			stop()
			h.giveUpAtStop(nil, stepCheck)
		}, failed, context.Canceled, stepCheck, false},
		{"a step its own bound gave up, answered once stopped", func(h *hold, stop context.CancelFunc) {
			h.giveUp(nil, stepWrite)
			stop()
		}, nil, context.DeadlineExceeded, stepWrite, false},
		{"a step its stop gave up after one its own bound did", func(h *hold, stop context.CancelFunc) {
			h.giveUp(nil, stepReadRecord)
			stop()
			h.giveUpAtStop(nil, stepReadBack)
		}, nil, context.DeadlineExceeded, stepReadRecord, false},
		{"a step its own bound gave up after one its stop did", func(h *hold, stop context.CancelFunc) {
			stop()
			h.giveUpAtStop(nil, stepWrite)
			h.giveUp(nil, stepReadBack)
		}, nil, context.DeadlineExceeded, stepReadBack, false},
		{"its caller's give-up once stopped", func(h *hold, stop context.CancelFunc) {
			stop()
			h.abandon()
		}, nil, context.Canceled, stepLockWait, true},
		{"its caller's give-up at its own bound, then its stop", func(h *hold, stop context.CancelFunc) {
			h.abandon()
			stop()
		}, nil, context.DeadlineExceeded, stepLockWait, false},
		{"a stop's bound with no stop", func(h *hold, _ context.CancelFunc) {
			h.giveUpAtStop(nil, stepCheck)
		}, nil, context.DeadlineExceeded, stepCheck, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, stop := context.WithCancel(context.Background())
			defer stop()
			h := newHold(ctx, "snap", t.TempDir())
			defer h.drop()
			if tc.published != nil {
				h.know(tc.published)
			}
			tc.give(h, stop)
			err := h.givenUp(ctx)
			if !errors.Is(err, ErrCheckGivenUp) || !errors.Is(err, tc.cause) {
				t.Fatalf("the answer %q is not ErrCheckGivenUp of %v", err, tc.cause)
			}
			if want := tc.step + " did not end within its bound (" + tc.cause.Error() + ")"; !strings.Contains(err.Error(), want) {
				t.Errorf("the answer %q does not name %q", err, want)
			}
			if tc.cause == context.DeadlineExceeded && errors.Is(err, context.Canceled) {
				t.Errorf("the answer %q names a stop beside the bound of its own", err)
			}
			if tc.published != nil && !errors.Is(err, tc.published) {
				t.Errorf("the answer %q dropped the error published before it", err)
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

// TestACallersExpiryAnswersWhatItsWorkerPublished pins the answer a caller
// that stopped waiting for its worker makes (expired; backlog B895): what the
// worker had published, its check's finding and its errors of its own, with
// WriteUnknown, beside its own error and the first give-up a step's own bound
// had made. Taken as its ticker takes it (ctxerr.WithoutCancellation after
// AsTheStop), the expiry is silent at a stop only where the stop alone gave
// the hold up and nothing of the worker's own was published, and a
// watchdog's expiry, with no stop, is said.
func TestACallersExpiryAnswersWhatItsWorkerPublished(t *testing.T) {
	readFailed := errors.New("injected: the read failed")
	failed := Verification{Format: verificationFormat, DatabaseState: StateFailed, DatabaseReasons: []Reason{ReasonHash},
		FilesState: StateVerified}
	for _, tc := range []struct {
		name    string
		before  func(h *hold, stop context.CancelFunc)
		stopped bool
		found   Verification
		said    []error
		notSaid []error
	}{
		{"nothing published, a step its stop gave up", func(h *hold, stop context.CancelFunc) {
			stop()
			h.giveUpAtStop(nil, stepWrite)
		}, true, Verification{}, nil, nil},
		{"a finding published, nothing given up", func(h *hold, stop context.CancelFunc) {
			h.knowFound(failed, false)
			stop()
		}, true, failed, nil, nil},
		{"a step its own bound gave up before the stop", func(h *hold, stop context.CancelFunc) {
			h.giveUp(nil, stepWrite)
			stop()
		}, true, Verification{}, []error{ErrCheckGivenUp, context.DeadlineExceeded}, nil},
		{"the stop, then a step its own bound gave up", func(h *hold, stop context.CancelFunc) {
			stop()
			h.giveUpAtStop(nil, stepWrite)
			h.giveUp(nil, stepReadBack)
		}, true, Verification{}, []error{ErrCheckGivenUp, context.DeadlineExceeded}, nil},
		{"an error of the worker's own published", func(h *hold, stop context.CancelFunc) {
			h.know(readFailed)
			stop()
		}, true, Verification{}, []error{readFailed}, nil},
		{"a read of the record that failed", func(h *hold, stop context.CancelFunc) {
			h.knowRead(readFailed)
			stop()
		}, true, Verification{}, []error{ErrRecordNotReadBack, readFailed}, nil},
		// The expiry is its caller's own give-up, never a step's.
		{"its watchdog, with no stop", func(h *hold, _ context.CancelFunc) {}, false, Verification{}, []error{context.DeadlineExceeded},
			[]error{ErrCheckGivenUp}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, stop := context.WithCancel(context.Background())
			defer stop()
			h := newHold(ctx, "snap", t.TempDir())
			defer h.drop()
			tc.before(h, stop)
			h.abandon()
			res, err := h.expired(ctx)
			if res.Write != WriteUnknown || res.Found.DatabaseState != tc.found.DatabaseState {
				t.Errorf("the expiry answered write %s, found %q; want unknown, %q", res.Write, res.Found.DatabaseState, tc.found.DatabaseState)
			}
			if err == nil || !strings.Contains(err.Error(), "was left to end on its own") {
				t.Fatalf("the expiry's error %v does not say it was left to end on its own", err)
			}
			left := ctxerr.WithoutCancellation(ctx, AsTheStop(ctx, err))
			if (left != nil) != (len(tc.said) > 0) {
				t.Fatalf("the expiry's error %q leaves %v once its stop is taken out; want something said: %v", err, left, len(tc.said) > 0)
			}
			for _, want := range tc.said {
				if !errors.Is(left, want) {
					t.Errorf("what is left, %q, is not %v", left, want)
				}
			}
			for _, never := range tc.notSaid {
				if errors.Is(err, never) {
					t.Errorf("the expiry's error %q is %v", err, never)
				}
			}
			if tc.stopped && left != nil && errors.Is(left, context.Canceled) {
				t.Errorf("what is left at the stop, %q, still holds the stop", left)
			}
		})
	}
}
