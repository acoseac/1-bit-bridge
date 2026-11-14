package backup

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/ctxerr"
)

// TestAGiveUpIsTheStopOnlyWhereTheStopMadeItAlone pins the give-up a hold's
// answer names (givenUp, givenFor) and what its caller takes it for once
// stopped (AsTheStop; backlog B895): a give-up its caller's stop made, by
// every step it gave up and with no error of the worker's own published
// before it, is that stop; a step given up at a bound of its own is the
// deadline it is, named whether the stop gave another step up before it or
// came before its answer was made; an error the worker published (a write,
// a read or a recheck that failed) beside a give-up the stop made is said,
// and one the give-up itself made is said too, as no news, which leaves the
// stop the stop (newsOf); and a context the stop did not cancel takes nothing
// for it.
func TestAGiveUpIsTheStopOnlyWhereTheStopMadeItAlone(t *testing.T) {
	failed := errors.New("injected: the staging failed")
	refusedByTheGiveUp := fmt.Errorf("%w: %w", ErrRecheckRefused, ErrCheckGivenUp)
	for _, tc := range []struct {
		name      string
		give      func(h *hold, stop context.CancelFunc)
		published error
		news      bool
		cause     error
		step      string
		isTheStop bool
	}{
		{"a step its stop gave up", func(h *hold, stop context.CancelFunc) {
			stop()
			h.giveUpAtStop(nil, stepWrite)
		}, nil, false, context.Canceled, stepWrite, true},
		{"a step its stop gave up, beside an error published before it", func(h *hold, stop context.CancelFunc) {
			stop()
			h.giveUpAtStop(nil, stepCheck)
		}, failed, true, context.Canceled, stepCheck, false},
		{"a step its stop gave up, beside a refusal the give-up made", func(h *hold, stop context.CancelFunc) {
			stop()
			h.giveUpAtStop(nil, stepVeto)
		}, refusedByTheGiveUp, false, context.Canceled, stepVeto, true},
		{"a step its own bound gave up, answered once stopped", func(h *hold, stop context.CancelFunc) {
			h.giveUp(nil, stepWrite)
			stop()
		}, nil, false, context.DeadlineExceeded, stepWrite, false},
		{"a step its stop gave up after one its own bound did", func(h *hold, stop context.CancelFunc) {
			h.giveUp(nil, stepReadRecord)
			stop()
			h.giveUpAtStop(nil, stepReadBack)
		}, nil, false, context.DeadlineExceeded, stepReadRecord, false},
		{"a step its own bound gave up after one its stop did", func(h *hold, stop context.CancelFunc) {
			stop()
			h.giveUpAtStop(nil, stepWrite)
			h.giveUp(nil, stepReadBack)
		}, nil, false, context.DeadlineExceeded, stepReadBack, false},
		{"its caller's give-up once stopped", func(h *hold, stop context.CancelFunc) {
			stop()
			h.abandon()
		}, nil, false, context.Canceled, stepLockWait, true},
		{"its caller's give-up at its own bound, then its stop", func(h *hold, stop context.CancelFunc) {
			h.abandon()
			stop()
		}, nil, false, context.DeadlineExceeded, stepLockWait, false},
		{"a stop's bound with no stop", func(h *hold, _ context.CancelFunc) {
			h.giveUpAtStop(nil, stepCheck)
		}, nil, false, context.DeadlineExceeded, stepCheck, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, stop := context.WithCancel(context.Background())
			defer stop()
			h := newHold(ctx, "snap", t.TempDir())
			defer h.drop()
			if tc.published != nil {
				if (newsOf(tc.published) != nil) != tc.news {
					t.Fatalf("newsOf(%q) is news: %v, want %v", tc.published, newsOf(tc.published) != nil, tc.news)
				}
				if !h.knowIn(context.Background(), tc.published) {
					t.Fatalf("knowIn(%q) over a step not given up published nothing", tc.published)
				}
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
// worker had published, its check's finding (with ErrRecheckNotRecorded where
// it was taken with it) and its errors of its own, with WriteUnknown, beside
// its own error and the first give-up a step's own bound had made. Taken as
// its ticker takes it (ctxerr.WithoutCancellation after AsTheStop), the
// expiry is silent at a stop only where the stop alone gave the hold up and
// nothing of the worker's own was published, and a watchdog's expiry, with no
// stop, is said.
func TestACallersExpiryAnswersWhatItsWorkerPublished(t *testing.T) {
	readFailed := errors.New("injected: the read failed")
	failed := Verification{Format: verificationFormat, DatabaseState: StateFailed, DatabaseReasons: []Reason{ReasonHash},
		FilesState: StateVerified}
	verified := Verification{Format: verificationFormat, DatabaseState: StateVerified, FilesState: StateVerified}
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
			h.takeFinding(failed, false, false)
			stop()
		}, true, failed, nil, nil},
		{"a finding published where no directory can be synced", func(h *hold, stop context.CancelFunc) {
			h.takeFinding(verified, false, true)
			stop()
		}, true, verified, []error{ErrRecheckNotRecorded}, nil},
		{"a check's answer after a give-up of the hold", func(h *hold, stop context.CancelFunc) {
			stop()
			h.giveUpAtStop(nil, stepReadRecord)
			h.takeFinding(failed, false, true)
		}, true, Verification{}, nil, []error{ErrRecheckNotRecorded}},
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
			h.knowIn(context.Background(), readFailed)
			stop()
		}, true, Verification{}, []error{readFailed}, nil},
		// What a give-up made says no more than that give-up.
		{"a refusal a give-up made, published", func(h *hold, stop context.CancelFunc) {
			stop()
			h.giveUpAtStop(nil, stepCheck)
			h.knowIn(context.Background(), fmt.Errorf("%w: %w", ErrRecheckRefused, ErrCheckGivenUp))
		}, true, Verification{}, nil, []error{ErrRecheckRefused}},
		{"a read of the record that failed", func(h *hold, stop context.CancelFunc) {
			h.knowReadIn(context.Background(), readFailed)
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

// TestAStepGivenUpPublishesNothingItAnswersLate pins the publication of what
// a step answers (stepKnowing, runStep: knowIn as it answers, keep as its
// worker takes it; backlog B895's third round): what a step answers once its
// own bound gave it up is no answer, as runStep drops it, and publishing it
// as the step answers must not make it one while the next step runs; what a
// step answers before a give-up after it stands; and what a step answers as a
// give-up of its hold ends its signal, which its worker takes all the same, is
// its worker's to say.
func TestAStepGivenUpPublishesNothingItAnswersLate(t *testing.T) {
	h := newHold(context.Background(), "snap", t.TempDir())
	defer h.drop()
	late := errors.New("injected: the late step's failure")
	release, lateAnswered := make(chan struct{}), make(chan struct{})
	// The hook runs once the step's answer was published, or refused.
	t.Cleanup(SetOnStepAnsweredForTest(func(_, step string) {
		if step == stepReadRecord {
			close(lateAnswered)
		}
	}))
	if _, answered := stepKnowing(h, time.Now().Add(20*time.Millisecond), stepReadRecord, func(context.Context, context.Context) error {
		<-release
		return late
	}, sameError); answered {
		t.Fatal("a step that did not answer within its bound was taken")
	}
	// The next step runs while the first, given up, answers.
	next := errors.New("injected: the next step's failure")
	if _, answered := stepKnowing(h, time.Now().Add(time.Minute), stepReadBack, func(context.Context, context.Context) error {
		close(release)
		select {
		case <-lateAnswered:
		case <-time.After(time.Minute):
			t.Error("the step given up never answered")
		}
		return next
	}, sameError); !answered {
		t.Fatal("the next step was not taken")
	}
	// A step whose signal the hold's give-up ends as it answers, taken all
	// the same: its worker keeps it.
	taken := errors.New("injected: a failure answered as its hold was given up")
	if _, answered := stepKnowing(h, time.Now().Add(time.Minute), stepPrune, func(_, given context.Context) error {
		h.abandon()
		if given.Err() == nil {
			t.Error("the hold's give-up did not end its step's signal")
		}
		return taken
	}, sameError); !answered {
		t.Fatal("a step that answered was not taken")
	}
	errs, _ := h.knownErrs(false)
	if len(errs) != 2 || errs[0] != next || errs[1] != taken {
		t.Errorf("published %v, want the next step's failure, then the one its worker took, never the late answer", errs)
	}
}

// TestTheChecksAnswerIsTakenOnceBeforeAnyGiveUp pins the check's slot
// (takeFinding, dropCheck, findingOf; backlog B895's third round): a check
// that answers before any give-up is published whole, its finding with
// ErrRecheckNotRecorded where no directory can be synced, and stands though
// its caller gives it up after; one that answers after a give-up of the hold,
// or after its caller gave it up, publishes nothing, and its worker takes its
// INCOMPLETE.
func TestTheChecksAnswerIsTakenOnceBeforeAnyGiveUp(t *testing.T) {
	failed := Verification{Format: verificationFormat, DatabaseState: StateFailed, DatabaseReasons: []Reason{ReasonHash},
		FilesState: StateVerified}
	incomplete := Verification{Format: verificationFormat, DatabaseState: StateIncomplete, DatabaseReasons: []Reason{ReasonCancelled},
		FilesState: StateIncomplete, FilesReasons: []Reason{ReasonCancelled}}
	for _, tc := range []struct {
		name        string
		run         func(h *hold) (took bool)
		taken       bool
		notRecorded bool
	}{
		{"its answer before any give-up", func(h *hold) bool {
			return h.takeFinding(failed, false, true)
		}, true, true},
		{"its answer, then its caller's give-up of it", func(h *hold) bool {
			took := h.takeFinding(failed, false, true)
			if v, taken := h.dropCheck(); !taken || v.DatabaseState != StateFailed {
				t.Errorf("dropCheck after the answer = %v, %v; want the answer taken", v.DatabaseState, taken)
			}
			return took
		}, true, true},
		{"its answer after a give-up of the hold", func(h *hold) bool {
			h.giveUpAtStop(nil, stepReadRecord)
			return h.takeFinding(failed, false, true)
		}, false, false},
		{"its answer after its caller gave it up", func(h *hold) bool {
			if _, taken := h.dropCheck(); taken {
				t.Error("dropCheck before any answer took one")
			}
			return h.takeFinding(failed, false, true)
		}, false, false},
		{"no answer before its worker took its INCOMPLETE", func(h *hold) bool {
			h.findingOf(incomplete, false)
			return h.takeFinding(failed, false, true)
		}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, stop := context.WithCancel(context.Background())
			defer stop()
			h := newHold(ctx, "snap", t.TempDir())
			defer h.drop()
			if took := tc.run(h); took != tc.taken {
				t.Fatalf("the answer was taken: %v, want %v", took, tc.taken)
			}
			want := incomplete
			if tc.taken {
				want = failed
			}
			if got := h.findingOf(incomplete, false); got.DatabaseState != want.DatabaseState {
				t.Errorf("the worker took database %s, want %s", got.DatabaseState, want.DatabaseState)
			}
			stop()
			h.abandon()
			res, err := h.expired(ctx)
			if res.Found.DatabaseState != want.DatabaseState {
				t.Errorf("the expiry answered database %s, want %s", res.Found.DatabaseState, want.DatabaseState)
			}
			if got := errors.Is(err, ErrRecheckNotRecorded); got != tc.notRecorded {
				t.Errorf("the expiry's error %q holds ErrRecheckNotRecorded: %v, want %v", err, got, tc.notRecorded)
			}
			if got := errors.Is(h.givenUp(ctx), ErrRecheckNotRecorded); got != tc.notRecorded {
				t.Errorf("the give-up's answer holds ErrRecheckNotRecorded: %v, want %v", got, tc.notRecorded)
			}
		})
	}
}

// TestWhatAGiveUpMadeIsNoNews pins newsOf (backlog B895's third round): what
// an operation a give-up stopped answers says no more than that give-up, and a
// cache stamp skipped alone is no news either; a failure the operation met on
// its own is, whatever a give-up skipped beside it.
func TestWhatAGiveUpMadeIsNoNews(t *testing.T) {
	syncFailed := fmt.Errorf("%w: %w", ErrVerdictNotDurable, errors.New("injected: the sync failed"))
	for _, tc := range []struct {
		name string
		err  error
		news bool
	}{
		{"a veto attempt the give-up refused", fmt.Errorf("the verdict was not written: the veto could not be confirmed: %w",
			errors.Join(errors.New("injected: the bundle refused it"), ErrCheckGivenUp)), false},
		{"a recheck the give-up refused", fmt.Errorf("%w: %w", ErrRecheckRefused, ErrCheckGivenUp), false},
		{"a rename the give-up refused", fmt.Errorf("the verdict was not written: %w", context.Canceled), false},
		{"a cache stamp the give-up skipped", fmt.Errorf("its write: %w", errCacheStampSkipped), false},
		{"a vetoed record whose stamp the give-up skipped", fmt.Errorf("the record is vetoed: %w", errCacheStampSkipped), false},
		{"a sync that failed beside a stamp skipped", fmt.Errorf("its write: %w", errors.Join(syncFailed, errCacheStampSkipped)), true},
		{"a staging that failed", errors.New("the verdict was not written: injected: the staging failed"), true},
		{"a recheck refused by both its vetoes", fmt.Errorf("%w: %w", ErrRecheckRefused,
			errors.Join(errors.New("sync the veto: injected"), errors.New("sync the veto: injected"))), true},
		{"a read that failed", errors.New("read the snapshot's manifest: unexpected end of JSON input"), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := newsOf(tc.err) != nil; got != tc.news {
				t.Errorf("newsOf(%q) is news: %v, want %v", tc.err, got, tc.news)
			}
		})
	}
}
