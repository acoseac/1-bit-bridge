package engine

import (
	"context"
	"errors"
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
