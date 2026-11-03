package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// busyOpen is a Start's or a Resume's error for a device another process
// holds, as a backend wraps it: the sentinel beside the device layer's own
// error, behind the backend's words.
func busyOpen() error {
	return fmt.Errorf("player backend: the device could not be opened: %w",
		errors.Join(errors.New("alsapcm: card 5 device 0: open: busy: another process has the node open"), ErrDeviceBusy))
}

// deniedOpen is a Start's or a Resume's error for a device node this process
// may not open (EACCES or EPERM, B676), as a backend wraps it: the sentinel
// beside the device layer's own error, behind the backend's words.
func deniedOpen() error {
	return fmt.Errorf("player backend: the device could not be opened: %w",
		errors.Join(errors.New("alsapcm: card 5 device 0: open: permission_denied: this process may not open /dev/snd/pcmC5D0p (the audio group owns it) (EACCES)"), ErrDeviceDenied))
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
			if a := h.waitAttempt(2); !a.Failed || a.Busy || a.Denied {
				t.Fatalf("a later failure for another reason: %+v, want failed, neither busy nor denied", a)
			}
		})
	}
}

// TestADeniedStartIsTheLastAttemptUntilAStartSucceeds pins the permission
// cause (B676) as the busy one is pinned: a start whose device node this
// process may not open is failed and denied, not busy, a start and no
// resume, with the failure's words for the log, until a start succeeds,
// whatever LastStop says, which that start does not replace.
func TestADeniedStartIsTheLastAttemptUntilAStartSucceeds(t *testing.T) {
	h := startHarness(t)
	ids := h.insert("a")
	h.available()
	h.be.script(func() {
		h.be.onStart = func(context.Context, *fakeSession) error { return deniedOpen() }
	})
	err := h.eng.PlayID(bg, ids[0])
	if !errors.Is(err, ErrDeviceDenied) || errors.Is(err, ErrDeviceBusy) {
		t.Fatalf("a denied start answered %v, want the denied error and not the busy one", err)
	}
	a := h.waitAttempt(1)
	if !a.Failed || !a.Denied || a.Busy || a.Resume || !strings.Contains(a.Detail, "permission_denied") {
		t.Fatalf("after a denied start: %+v, want a start, failed and denied, with its words", a)
	}
	sn := h.waitSnap("stopped", func(sn *Snapshot) bool { return sn.State() == Stopped })
	if sn.LastStop().Reason != ReasonBackend {
		t.Fatalf("the denied start's stop: %+v", sn.LastStop())
	}

	h.be.script(func() { h.be.onStart = nil })
	n := h.be.starts()
	h.must("play again", h.eng.PlayID(bg, ids[0]))
	s := h.be.nth(n + 1)
	s.audible(s.first.ID)
	if a := h.waitAttempt(2); a != (Attempt{Seq: 2}) {
		t.Fatalf("after a start that succeeded: %+v, want attempt 2 and nothing else", a)
	}
	if sn := h.snap(); sn.LastStop().Reason != ReasonBackend {
		t.Fatalf("a start that succeeds replaced LastStop: %+v", sn.LastStop())
	}
}

// TestADeniedResumeIsTheLastAttemptUntilAResumeSucceeds pins the cause for a
// resume after the pause released the device, whose reopen this process may
// not make: the resume is refused denied, a resume, the session stays paused,
// and the next resume that succeeds clears it. The session's start before it
// is no resume.
func TestADeniedResumeIsTheLastAttemptUntilAResumeSucceeds(t *testing.T) {
	h := startHarness(t)
	_, s := h.playing(0, "a")
	start := h.waitAttempt(1)
	if start.Failed || start.Resume {
		t.Fatalf("the session's start: %+v, want a start that succeeded", start)
	}
	h.must("pause", h.eng.Pause(bg))
	s.deviceReleased()
	h.waitSnap("the device released", func(sn *Snapshot) bool { return sn.DeviceReleased() })
	h.be.script(func() {
		h.be.onResume = func(context.Context, *fakeSession) (ResumeResult, error) { return 0, deniedOpen() }
	})
	if err := h.eng.Play(bg); !errors.Is(err, ErrDeviceDenied) {
		t.Fatalf("a denied resume answered %v, want the denied error", err)
	}
	a := h.waitAttempt(start.Seq + 1)
	if !a.Failed || !a.Denied || a.Busy || !a.Resume || !strings.Contains(a.Detail, "permission_denied") {
		t.Fatalf("after a denied resume: %+v, want a resume, failed and denied, with its words", a)
	}
	if sn := h.snap(); sn.State() != Paused {
		t.Fatalf("after a denied resume the transport is %s, want paused", sn.State())
	}
	h.be.script(func() { h.be.onResume = nil })
	h.must("resume", h.eng.Play(bg))
	if a := h.waitAttempt(start.Seq + 2); a != (Attempt{Seq: start.Seq + 2, Resume: true}) {
		t.Fatalf("after a resume that succeeded: %+v, want a resume and nothing else", a)
	}
	if sn := h.snap(); sn.State() != Playing {
		t.Fatalf("after the resume the transport is %s, want playing", sn.State())
	}
}

// TestALaterFailureForAnotherReasonEndsDenied: the cause is the last
// attempt's, so a start that fails for another reason after a denied one
// (its first entry unplayable, the device another process holds, a refusal
// whose words say permission without the sentinel) is no longer denied.
func TestALaterFailureForAnotherReasonEndsDenied(t *testing.T) {
	for _, tc := range []struct {
		name string
		fail func(h *harness)
		busy bool
	}{
		{"its first entry refused", func(h *harness) {
			h.be.unplayable["a"] = Unplayable{Reason: ReasonMissing, Detail: "gone"}
		}, false},
		{"the device another process holds", func(h *harness) {
			h.be.onStart = func(context.Context, *fakeSession) error { return busyOpen() }
		}, true},
		{"a refusal whose words alone say permission", func(h *harness) {
			h.be.onStart = func(context.Context, *fakeSession) error {
				return errors.New("player backend: permission_denied: the audio group owns it (EACCES)")
			}
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := startHarness(t)
			ids := h.insert("a")
			h.available()
			h.be.script(func() {
				h.be.onStart = func(context.Context, *fakeSession) error { return deniedOpen() }
			})
			_ = h.eng.PlayID(bg, ids[0])
			if a := h.waitAttempt(1); !a.Denied {
				t.Fatalf("the denied start: %+v", a)
			}
			h.waitSnap("stopped", func(sn *Snapshot) bool { return sn.State() == Stopped })
			h.be.script(func() {
				h.be.onStart = nil
				tc.fail(h)
			})
			_ = h.eng.PlayID(bg, ids[0])
			if a := h.waitAttempt(2); !a.Failed || a.Denied || a.Busy != tc.busy {
				t.Fatalf("a later failure for another reason: %+v, want failed, not denied, busy %v", a, tc.busy)
			}
		})
	}
}

// TestAResumePastItsDeadlineIsAFailedResume: a resume the backend never
// answers in time is a failed attempt that is a resume, neither busy nor
// denied, and its words are the deadline's; so it ends a denied record
// before it.
func TestAResumePastItsDeadlineIsAFailedResume(t *testing.T) {
	h := startHarness(t)
	_, s := h.playing(0, "a")
	start := h.waitAttempt(1)
	h.must("pause", h.eng.Pause(bg))
	s.deviceReleased()
	h.waitSnap("the device released", func(sn *Snapshot) bool { return sn.DeviceReleased() })
	h.be.script(func() {
		h.be.onResume = func(context.Context, *fakeSession) (ResumeResult, error) { return 0, deniedOpen() }
	})
	_ = h.eng.Play(bg)
	if a := h.waitAttempt(start.Seq + 1); !a.Denied || !a.Resume {
		t.Fatalf("the denied resume: %+v", a)
	}
	held := make(chan struct{})
	h.be.script(func() {
		h.be.onResume = func(ctx context.Context, _ *fakeSession) (ResumeResult, error) {
			close(held)
			<-ctx.Done()
			return 0, ctx.Err()
		}
	})
	played := async(func() error { return h.eng.Play(bg) })
	<-held
	h.clk.Advance(CallTimeout)
	if err := h.result("the play whose resume expired", played); ReasonOf(err) != ReasonTimeout {
		t.Fatalf("a resume past its deadline answered %v, want a timeout", err)
	}
	if a := h.waitAttempt(start.Seq + 2); !a.Failed || a.Denied || a.Busy || !a.Resume || !strings.Contains(a.Detail, "deadline") {
		t.Fatalf("a resume past its deadline: %+v, want a failed resume, neither busy nor denied, in the deadline's words", a)
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
