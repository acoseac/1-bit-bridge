package engine

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// TestACommandHeldBeforeItsAdmissionPastItsCallersDeadlineDidNotRun pins
// ErrNotAdmitted (B470 PR 5a4) on the one window ReasonCancelled alone could
// not tell: the owner has taken the command and not admitted it yet (it is
// held at the admitted seam, before it asks the command's context) when the
// caller's deadline passes. The caller is answered at its deadline, without
// the owner, with a refusal that wraps ErrNotAdmitted and the context's error,
// and the command never runs once the owner goes on.
func TestACommandHeldBeforeItsAdmissionPastItsCallersDeadlineDidNotRun(t *testing.T) {
	h := newHarness(t)
	reached, release := make(chan struct{}, 1), newGate()
	h.tweak = func(c *Config) {
		c.seams.admitted = func(cmd *command) {
			if cmd.op != opInsert {
				return
			}
			reached <- struct{}{}
			<-release
		}
	}
	h.boot()
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			release.open()
		}
	})
	ctx, cancel := context.WithTimeout(bg, 50*time.Millisecond)
	defer cancel()
	insert := async(func() error { _, err := h.eng.InsertAfter(ctx, 0, ref("a"), nil); return err })
	select {
	case <-reached:
	case <-time.After(*watchdog):
		t.Fatalf("waited %v for the owner to take the insert", *watchdog)
	}
	err := h.result("the insert held before its admission", insert)
	if !errors.Is(err, ErrNotAdmitted) || ReasonOf(err) != ReasonCancelled || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the insert held before its admission past its deadline: %v; want a cancellation wrapping ErrNotAdmitted and the deadline", err)
	}
	release.open()
	// A later command's reply comes after the owner's step over the insert.
	h.must("a command after it", h.eng.SetRepeat(bg, false))
	if n := h.snap().Len(); n != 0 {
		t.Fatalf("the insert its caller was told did not run ran: the queue holds %d entries", n)
	}
}

// TestACommandWhoseContextEndsAsTheOwnerAdmitsItDidNotRun pins the decision's
// other window: the owner has found the command's context alive (it is held at
// the checked seam, before it admits it) when the caller's context ends. The
// caller abandons it and is told it did not run, and the owner, finding it
// abandoned, refuses it: it never runs, though its context was alive when the
// owner looked.
func TestACommandWhoseContextEndsAsTheOwnerAdmitsItDidNotRun(t *testing.T) {
	h := newHarness(t)
	reached, release := make(chan struct{}, 1), newGate()
	h.tweak = func(c *Config) {
		c.seams.checked = func(cmd *command) {
			if cmd.op != opInsert {
				return
			}
			reached <- struct{}{}
			<-release
		}
	}
	h.boot()
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			release.open()
		}
	})
	ctx, cancel := context.WithCancel(bg)
	defer cancel()
	insert := async(func() error { _, err := h.eng.InsertAfter(ctx, 0, ref("a"), nil); return err })
	select {
	case <-reached:
	case <-time.After(*watchdog):
		t.Fatalf("waited %v for the owner to find the insert's context alive", *watchdog)
	}
	cancel()
	err := h.result("the insert its caller abandoned", insert)
	if !errors.Is(err, ErrNotAdmitted) || !errors.Is(err, context.Canceled) {
		t.Fatalf("the insert whose context ended as the owner admitted it: %v; want a cancellation wrapping ErrNotAdmitted", err)
	}
	release.open()
	h.must("a command after it", h.eng.SetRepeat(bg, false))
	if n := h.snap().Len(); n != 0 {
		t.Fatalf("the insert its caller was told did not run ran: the queue holds %d entries", n)
	}
}

// TestACommandTheOwnerNeverTookDidNotRun pins ErrNotAdmitted on the two
// windows before the owner has the command: a context that has ended before
// the call, and one that ends while the owner, held in another command's step
// (the admitted seam), cannot take the send. Each is answered with a refusal
// that wraps ErrNotAdmitted and the context's error, and neither runs.
func TestACommandTheOwnerNeverTookDidNotRun(t *testing.T) {
	h := newHarness(t)
	reached, release := make(chan struct{}, 1), newGate()
	h.tweak = func(c *Config) {
		c.seams.admitted = func(cmd *command) {
			if cmd.op != opInsert {
				return
			}
			reached <- struct{}{}
			<-release
		}
	}
	h.boot()
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			release.open()
		}
	})
	ended, cancel := context.WithCancel(bg)
	cancel()
	if err := h.eng.SetRepeat(ended, true); !errors.Is(err, ErrNotAdmitted) || ReasonOf(err) != ReasonCancelled || !errors.Is(err, context.Canceled) {
		t.Fatalf("a command whose context had ended before the call: %v; want a cancellation wrapping ErrNotAdmitted", err)
	}
	insert := async(func() error { _, err := h.eng.InsertAfter(bg, 0, ref("a"), nil); return err })
	select {
	case <-reached:
	case <-time.After(*watchdog):
		t.Fatalf("waited %v for the owner to take the insert", *watchdog)
	}
	ctx, done := context.WithTimeout(bg, 50*time.Millisecond)
	defer done()
	if err := h.eng.SetRepeat(ctx, true); !errors.Is(err, ErrNotAdmitted) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a command whose send the owner could not take before its deadline: %v; want a cancellation wrapping ErrNotAdmitted", err)
	}
	release.open()
	h.must("the insert the owner held", h.result("the insert", insert))
	if h.snap().Repeat() {
		t.Fatal("a command its caller was told did not run ran: repeat is on")
	}
}

// TestACommandAdmittedBeforeItsCallersDeadlineMayHaveRun pins the other side:
// an insert the owner admitted and parked on a reservation write that has not
// landed, whose caller's context then ends, is answered cancelled without
// ErrNotAdmitted, and it does run once the write lands: a parked edit does not
// ask its context again.
func TestACommandAdmittedBeforeItsCallersDeadlineMayHaveRun(t *testing.T) {
	h := startHarness(t)
	h.insert("a")
	g, calls := h.hangReservations()
	ctx, cancel := context.WithCancel(bg)
	defer cancel()
	mark := h.appliedMark()
	insert := async(func() error { _, err := h.eng.InsertAfter(ctx, 0, ref("b"), nil); return err })
	h.waitApplied("admitted insert", mark)
	h.files.waitFor(t, "the reservation write the insert waits for", func() bool { return h.files.reserveCalls > calls })
	cancel()
	err := h.result("the parked insert", insert)
	if ReasonOf(err) != ReasonCancelled || errors.Is(err, ErrNotAdmitted) {
		t.Fatalf("the parked insert whose caller left: %v; want a cancellation that does not say it did not run", err)
	}
	g.open()
	h.waitSnap("the parked insert run once its write landed", func(sn *Snapshot) bool { return sn.Len() == 2 })
}

// TestAParkedEditIsAnsweredWithinReserveTimeoutOfItsPark pins ReserveTimeout's
// promise, which the OpenHome surface's edit gate rests on (B470 PR 5a4): the
// bound is armed as the first edit parks, so that edit is answered when it
// passes and not a nanosecond before, and an edit that parks later, under the
// same timer, is answered then too, no later than ReserveTimeout after its own
// park. The reservation write never lands.
func TestAParkedEditIsAnsweredWithinReserveTimeoutOfItsPark(t *testing.T) {
	h := startHarness(t)
	ids := h.insert("a", "b")
	_, calls := h.hangReservations()
	mark := h.appliedMark()
	first := async(func() error { _, err := h.eng.InsertAfter(bg, 0, ref("x"), nil); return err })
	h.waitApplied("admitted insert", mark)
	h.files.waitFor(t, "the reservation write the insert waits for", func() bool { return h.files.reserveCalls > calls })
	// The insert parks in the step that admits it, which arms the bound only
	// after it has started the write (values): a write under way says nothing
	// of the bound, and a command of no lane answers after that step (B605).
	h.must("a command after the insert", h.eng.SetRepeat(bg, false))
	const later = 2 * time.Second
	h.clk.Advance(later)
	mark = h.appliedMark()
	second := async(func() error { return h.eng.Delete(bg, ids[0]) })
	h.waitApplied("admitted delete", mark)
	// The delete parks in the step that admits it; a command of no lane
	// answers after that step.
	h.must("a command after the delete", h.eng.SetRepeat(bg, false))
	h.clk.Advance(ReserveTimeout - later - time.Nanosecond)
	h.must("a command a nanosecond before the bound", h.eng.SetRepeat(bg, false))
	select {
	case err := <-first:
		t.Fatalf("the first edit answered %v before ReserveTimeout passed", err)
	case err := <-second:
		t.Fatalf("the second edit answered %v before the first's bound", err)
	default:
	}
	h.clk.Advance(time.Nanosecond)
	h.refused("the first edit, ReserveTimeout after it parked", h.result("first", first), ReasonReservation)
	h.refused("the second edit, parked under the first's timer", h.result("second", second), ReasonReservation)
	if n := h.snap().Len(); n != 2 {
		t.Fatalf("the queue holds %d entries after both edits were refused, want a and b", n)
	}
}

// TestWithAdmissionClosesAsTheOwnerAdmits pins WithAdmission (B470 PR 5a4,
// round 2), which the OpenHome surface gives a Stop's request slot back on:
// its channel closes once the owner admits the command, so a Stop that waits
// for a release closes it long before its answer, which comes once the
// release is acknowledged; and it never closes for a command the owner did
// not admit: one whose context had ended before the call, one the owner,
// held at the checked seam, finds abandoned by its caller, and one sent after
// the engine shut down.
func TestWithAdmissionClosesAsTheOwnerAdmits(t *testing.T) {
	h := newHarness(t)
	reached, release := make(chan struct{}, 1), newGate()
	var armed atomic.Bool
	h.tweak = func(c *Config) {
		c.seams.checked = func(cmd *command) {
			if cmd.op != opInsert || !armed.Load() {
				return
			}
			reached <- struct{}{}
			<-release
		}
	}
	h.boot()
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			release.open()
		}
	})
	_, s := h.playing(0, "a")
	ack := newGate()
	h.be.script(func() {
		h.be.onRelease = func(ctx context.Context, fs *fakeSession) error {
			if err := ack.wait(ctx, h.be, true); err != nil {
				return err
			}
			return fs.release(ctx)
		}
	})
	ctx, admitted := WithAdmission(bg)
	stop := async(func() error { return h.eng.Stop(ctx) })
	select {
	case <-admitted:
	case <-time.After(*watchdog):
		t.Fatalf("waited %v for the Stop's admission", *watchdog)
	}
	h.be.waitFor("the release", func() bool { return s.releaseAsked })
	select {
	case err := <-stop:
		t.Fatalf("the Stop answered %v while its release is held", err)
	default:
	}
	ack.open()
	h.must("the Stop once its release is acknowledged", h.result("the stop", stop))

	ended, cancel := context.WithCancel(bg)
	cancel()
	ctx, notAdmittedYet := WithAdmission(ended)
	if err := h.eng.Stop(ctx); !errors.Is(err, ErrNotAdmitted) {
		t.Fatalf("a Stop whose context had ended: %v, want ErrNotAdmitted", err)
	}
	abandon, leave := context.WithCancel(bg)
	defer leave()
	ctx, abandoned := WithAdmission(abandon)
	armed.Store(true)
	insert := async(func() error { _, err := h.eng.InsertAfter(ctx, 0, ref("b"), nil); return err })
	select {
	case <-reached:
	case <-time.After(*watchdog):
		t.Fatalf("waited %v for the owner to find the insert's context alive", *watchdog)
	}
	leave()
	if err := h.result("the insert its caller abandoned", insert); !errors.Is(err, ErrNotAdmitted) {
		t.Fatalf("the insert its caller abandoned: %v, want ErrNotAdmitted", err)
	}
	release.open()
	h.must("a command after it", h.eng.SetRepeat(bg, false))
	h.shutdown()
	ctx, closed := WithAdmission(bg)
	if err := h.eng.Stop(ctx); ReasonOf(err) != ReasonClosed {
		t.Fatalf("a Stop after the shutdown: %v, want closed", err)
	}
	for what, ch := range map[string]<-chan struct{}{"a context that had ended": notAdmittedYet, "an abandoned insert": abandoned, "a Stop after the shutdown": closed} {
		select {
		case <-ch:
			t.Errorf("%s: its admission channel closed", what)
		default:
		}
	}
}
