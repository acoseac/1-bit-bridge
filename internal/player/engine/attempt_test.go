package engine

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// busyOpen is a Start's or a Resume's error for a device another process
// holds, as a backend wraps it: the sentinel beside the device layer's own
// error, behind the backend's words.
func busyOpen() error {
	return fmt.Errorf("player backend: the device could not be opened: %w",
		errors.Join(errors.New("alsapcm: card 5 device 0: open: busy: another process has the node open"), ErrDeviceBusy))
}

// waitAttempt waits until the snapshot's last attempt is the seq'th and
// returns it.
func (h *harness) waitAttempt(seq uint64) Attempt {
	h.t.Helper()
	return h.waitSnap(fmt.Sprintf("attempt %d", seq), func(sn *Snapshot) bool { return sn.LastAttempt().Seq >= seq }).LastAttempt()
}

// TestABusyStartIsTheLastAttemptUntilAStartSucceeds pins the last-attempt
// marker against a start that fails because another process holds the
// device (B470 PR 5b's lastError): busy until the next start succeeds,
// whatever LastStop says, which that start does not replace; a pause is no
// attempt and leaves it alone.
func TestABusyStartIsTheLastAttemptUntilAStartSucceeds(t *testing.T) {
	h := startHarness(t)
	if a := h.snap().LastAttempt(); a != (Attempt{}) {
		t.Fatalf("before any attempt: %+v, want none", a)
	}
	ids := h.insert("a")
	h.available()
	h.be.script(func() {
		h.be.onStart = func(context.Context, *fakeSession) error { return busyOpen() }
	})
	err := h.eng.PlayID(bg, ids[0])
	if !errors.Is(err, ErrDeviceBusy) {
		t.Fatalf("a busy start answered %v, want the busy error", err)
	}
	a := h.waitAttempt(1)
	if !a.Failed || !a.Busy {
		t.Fatalf("after a busy start: %+v, want failed and busy", a)
	}
	sn := h.waitSnap("stopped", func(sn *Snapshot) bool { return sn.State() == Stopped })
	if sn.LastStop().Reason != ReasonBackend {
		t.Fatalf("the busy start's stop: %+v", sn.LastStop())
	}

	// A retry that succeeds clears it; LastStop still names the busy start.
	h.be.script(func() { h.be.onStart = nil })
	n := h.be.starts()
	h.must("play again", h.eng.PlayID(bg, ids[0]))
	s := h.be.nth(n + 1)
	s.audible(s.first.ID)
	a = h.waitAttempt(2)
	if a.Failed || a.Busy || a.Seq != 2 {
		t.Fatalf("after a start that succeeded: %+v, want attempt 2, not failed", a)
	}
	if sn := h.snap(); sn.LastStop().Reason != ReasonBackend {
		t.Fatalf("a start that succeeds replaced LastStop: %+v", sn.LastStop())
	}

	// A pause is no attempt.
	h.must("pause", h.eng.Pause(bg))
	sn = h.waitSnap("paused", func(sn *Snapshot) bool { return sn.State() == Paused })
	if got := sn.LastAttempt(); got != a {
		t.Fatalf("a pause changed the last attempt: %+v, was %+v", got, a)
	}
}

// TestABusyResumeIsTheLastAttemptUntilAResumeSucceeds pins the marker for a
// resume after the pause released the device: another process took it
// meanwhile, the resume is refused busy and the session stays paused, and
// the next resume that succeeds clears it.
func TestABusyResumeIsTheLastAttemptUntilAResumeSucceeds(t *testing.T) {
	h := startHarness(t)
	_, s := h.playing(0, "a")
	start := h.waitAttempt(1)
	h.must("pause", h.eng.Pause(bg))
	s.deviceReleased()
	h.waitSnap("the device released", func(sn *Snapshot) bool { return sn.DeviceReleased() })
	h.be.script(func() {
		h.be.onResume = func(context.Context, *fakeSession) (ResumeResult, error) { return 0, busyOpen() }
	})
	if err := h.eng.Play(bg); !errors.Is(err, ErrDeviceBusy) {
		t.Fatalf("a busy resume answered %v, want the busy error", err)
	}
	a := h.waitAttempt(start.Seq + 1)
	if !a.Failed || !a.Busy {
		t.Fatalf("after a busy resume: %+v, want failed and busy", a)
	}
	if sn := h.snap(); sn.State() != Paused {
		t.Fatalf("after a busy resume the transport is %s, want paused", sn.State())
	}
	h.be.script(func() { h.be.onResume = nil })
	h.must("resume", h.eng.Play(bg))
	a = h.waitAttempt(start.Seq + 2)
	if a.Failed || a.Busy {
		t.Fatalf("after a resume that succeeded: %+v, want not failed", a)
	}
	if sn := h.snap(); sn.State() != Playing {
		t.Fatalf("after the resume the transport is %s, want playing", sn.State())
	}
}

// TestABusyWordIsNotTheBusyError: the marker reads the sentinel alone,
// never the error's text, so a start that fails for another reason whose
// detail says busy is failed and not busy.
func TestABusyWordIsNotTheBusyError(t *testing.T) {
	h := startHarness(t)
	ids := h.insert("a")
	h.available()
	h.be.script(func() {
		h.be.onStart = func(context.Context, *fakeSession) error {
			return errors.New("player backend: busy: the device is busy, the bus is busy")
		}
	})
	_ = h.eng.PlayID(bg, ids[0])
	if a := h.waitAttempt(1); !a.Failed || a.Busy {
		t.Fatalf("a start whose text says busy: %+v, want failed and not busy", a)
	}
}

// TestALaterFailureForAnotherReasonEndsBusy: the marker is the last
// attempt's, so a start refused for another reason after a busy one, its
// first entry unplayable, or one that fails otherwise, is failed and no
// longer busy.
func TestALaterFailureForAnotherReasonEndsBusy(t *testing.T) {
	for _, tc := range []struct {
		name string
		fail func(h *harness)
	}{
		{"its first entry refused", func(h *harness) {
			h.be.unplayable["a"] = Unplayable{Reason: ReasonMissing, Detail: "gone"}
		}},
		{"the device refused otherwise", func(h *harness) {
			h.be.onStart = func(context.Context, *fakeSession) error { return errors.New("permission denied") }
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := startHarness(t)
			ids := h.insert("a")
			h.available()
			h.be.script(func() {
				h.be.onStart = func(context.Context, *fakeSession) error { return busyOpen() }
			})
			_ = h.eng.PlayID(bg, ids[0])
			if a := h.waitAttempt(1); !a.Busy {
				t.Fatalf("the busy start: %+v", a)
			}
			h.waitSnap("stopped", func(sn *Snapshot) bool { return sn.State() == Stopped })
			h.be.script(func() {
				h.be.onStart = nil
				tc.fail(h)
			})
			_ = h.eng.PlayID(bg, ids[0])
			if a := h.waitAttempt(2); !a.Failed || a.Busy {
				t.Fatalf("a later failure for another reason: %+v, want failed and not busy", a)
			}
		})
	}
}

// TestAStartPastItsDeadlineIsAFailedAttempt: a start the backend never
// answers in time is the attempt's failure too, not a busy one, and so it
// ends a busy record before it.
func TestAStartPastItsDeadlineIsAFailedAttempt(t *testing.T) {
	h := startHarness(t)
	ids := h.insert("a")
	h.available()
	h.be.script(func() {
		h.be.onStart = func(context.Context, *fakeSession) error { return busyOpen() }
	})
	_ = h.eng.PlayID(bg, ids[0])
	if a := h.waitAttempt(1); !a.Busy {
		t.Fatalf("the busy start: %+v", a)
	}
	h.waitSnap("stopped", func(sn *Snapshot) bool { return sn.State() == Stopped })
	held := make(chan struct{})
	h.be.script(func() {
		h.be.onStart = func(ctx context.Context, _ *fakeSession) error {
			close(held)
			<-ctx.Done()
			return ctx.Err()
		}
	})
	played := async(func() error { return h.eng.PlayID(bg, ids[0]) })
	<-held
	h.clk.Advance(StartTimeout)
	if err := h.result("the play whose start expired", played); ReasonOf(err) != ReasonTimeout {
		t.Fatalf("a start past its deadline answered %v, want a timeout", err)
	}
	if a := h.waitAttempt(2); !a.Failed || a.Busy {
		t.Fatalf("a start past its deadline: %+v, want failed and not busy", a)
	}
}
