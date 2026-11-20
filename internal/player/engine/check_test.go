package engine

import (
	"context"
	"errors"
	"slices"
	"sync/atomic"
	"testing"
	"time"
)

// The DSD check's hold's tests (B728 B12 PR 2): EnterCheck decided in the one
// owner step that grants it, its grant never lost to its caller, and every
// Stop under it ending the check. Every wait is for a signal the engine or the
// fake publishes, bounded by the watchdog; nothing sleeps. What the owner has
// answered is read from the replies it sends (the replying seam, noted as
// "replied" and the command's op), once a later step of the owner's shows that
// the step under test is over.

// checkOp is EnterCheck's command as the owner names it.
const checkOp = "enter check"

// checkHarness is a booted harness whose owner notes each reply it sends
// (repliedSince), with more applied to its Config.
func checkHarness(t *testing.T, more func(c *Config)) *harness {
	t.Helper()
	h := newHarness(t)
	h.tweak = func(c *Config) {
		c.seams.replying = func(op string, _ reply, _ *Snapshot) { h.noteApplied("replied " + op) }
		if more != nil {
			more(c)
		}
	}
	return h.boot()
}

// repliedSince reports whether the owner has sent a reply to a command of op
// since mark (appliedMark).
func (h *harness) repliedSince(op string, mark int) bool {
	h.appliedMu.Lock()
	defer h.appliedMu.Unlock()
	return slices.Contains(h.appliedOps[min(mark, len(h.appliedOps)):], "replied "+op)
}

// barrier returns once the owner has run a step after every command's step
// before it: a SetShuffle(false), refused under a hold, its reply sent after
// those steps' replies.
func (h *harness) barrier() {
	h.t.Helper()
	_ = h.result("a barrier", async(func() error { return h.eng.SetShuffle(bg, false) }))
}

// checkAnswer is what EnterCheck answered.
type checkAnswer struct {
	hold Hold
	err  error
}

// checkAsync is EnterCheck on ctx on a goroutine of its own.
func (h *harness) checkAsync(ctx context.Context, interrupt bool) <-chan checkAnswer {
	ch := make(chan checkAnswer, 1)
	go func() {
		hold, err := h.eng.EnterCheck(ctx, interrupt)
		ch <- checkAnswer{hold, err}
	}()
	return ch
}

// checkResult waits, under the watchdog, for an EnterCheck's answer.
func (h *harness) checkResult(what string, ch <-chan checkAnswer) checkAnswer {
	h.t.Helper()
	select {
	case a := <-ch:
		return a
	case <-time.After(*watchdog):
		h.t.Fatalf("waited %v for %s to answer", *watchdog, what)
	}
	return checkAnswer{}
}

// granted is EnterCheck on bg, required to answer a hold whose release is
// confirmed and whose stop signal is open.
func (h *harness) granted(what string, interrupt bool) Hold {
	h.t.Helper()
	a := h.checkResult(what, h.checkAsync(bg, interrupt))
	h.must(what, a.err)
	if a.hold.Lease.IsZero() || !a.hold.ReleaseConfirmed || a.hold.Stop == nil || signalled(a.hold.Stop) {
		h.t.Fatalf("%s: %+v; want a lease, its release confirmed and its stop signal open", what, a.hold)
	}
	if m, ok := h.snap().Maintenance(); !ok || m.Reason != "check" || !m.ReleaseConfirmed {
		h.t.Fatalf("%s: the snapshot's hold %+v %v; want the check's, its release confirmed", what, m, ok)
	}
	return a.hold
}

// refusedCheck requires EnterCheck asked now, with interrupt or without, to be
// refused for reason, changing nothing: the state and the generation as they
// were.
func refusedCheck(t *testing.T, h *harness, what string, interrupt bool, reason Reason) {
	t.Helper()
	before := h.snap()
	a := h.checkResult(what, h.checkAsync(bg, interrupt))
	if got := ReasonOf(a.err); got != reason {
		t.Fatalf("%s: %v (reason %q), want a refusal %q", what, a.err, got, reason)
	}
	if a.hold != (Hold{}) {
		t.Errorf("%s: refused with a hold %+v", what, a.hold)
	}
	if sn := h.snap(); sn.Generation() != before.Generation() || sn.State() != before.State() {
		t.Errorf("%s: the refusal changed the engine from %s at %v to %s at %v", what, before.State(), before.Generation(), sn.State(), sn.Generation())
	}
}

// signalled reports whether a hold's stop signal is closed now.
func signalled(stop <-chan struct{}) bool {
	select {
	case <-stop:
		return true
	default:
		return false
	}
}

// waitClosed waits, under the watchdog, until ch is closed or yields.
func waitClosed(t *testing.T, what string, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(*watchdog):
		t.Fatalf("waited %v for %s", *watchdog, what)
	}
}

// grantWaits asks the check's hold with interrupt while the release it makes,
// or waits for, of s is parked, and returns once the owner has granted it (the
// snapshot shows the check's hold) and that release has reached the fake: its
// answer is to come.
func (h *harness) grantWaits(s *fakeSession) <-chan checkAnswer {
	h.t.Helper()
	check := h.checkAsync(bg, true)
	h.waitSnap("the check's hold granted", func(sn *Snapshot) bool { m, ok := sn.Maintenance(); return ok && m.Reason == "check" })
	h.be.waitFor("the release the check waits for", func() bool { return s.releaseAsked })
	return check
}

// interrupts asks the check's hold with interrupt beside s, its release parked
// at g: the owner grants it in the step that takes it, and answers it only
// once g lets the release go, with a Hold whose release is confirmed.
func (h *harness) interrupts(s *fakeSession, g gate) Hold {
	h.t.Helper()
	mark := h.appliedMark()
	check := h.grantWaits(s)
	h.barrier()
	if h.repliedSince(checkOp, mark) {
		h.t.Fatal("the check's hold was answered while the release it waits for is parked")
	}
	g.open()
	a := h.checkResult("the check's hold once its release is let go", check)
	h.must("the check's hold once its release is let go", a.err)
	if !a.hold.ReleaseConfirmed || a.hold.Stop == nil || signalled(a.hold.Stop) {
		h.t.Fatalf("the check's hold %+v: want its release confirmed and its stop signal open", a.hold)
	}
	if !s.isAcked() {
		h.t.Fatal("the check's hold was answered before the release it waits for was acknowledged")
	}
	return a.hold
}

// TestACheckHoldIsRefusedBesideAConfigurationReservation pins the check's hold
// against a DSD choice's write (B728 B12): a configuration reservation held,
// the hold is refused ReasonConfiguring, with interrupt or without, changing
// nothing, the reservation still held; a check's hold held, a reservation is
// refused ReasonMaintenance, the hold still held; neither is granted beside the
// other, in either order, and each is once the other is let go. Before B12 the
// one hold, EnterMaintenance's, was granted beside a lease.
func TestACheckHoldIsRefusedBesideAConfigurationReservation(t *testing.T) {
	h := checkHarness(t, nil)
	h.insert("a")
	h.available()
	l, err := h.reserveWithin("the reservation")
	h.must("the reservation", err)
	refusedCheck(t, h, "a check's hold beside a reservation", false, ReasonConfiguring)
	refusedCheck(t, h, "a check's hold that may interrupt, beside a reservation", true, ReasonConfiguring)
	if sn := h.snap(); !sn.ConfigurationReserved() || sn.State() != Stopped {
		t.Fatalf("after the refused holds: reserved %v, %s; want the reservation held, stopped", sn.ConfigurationReserved(), sn.State())
	}
	h.releaseWithin("the reservation's release", l)
	hold := h.granted("a check's hold once the reservation is let go", false)
	refusedReservation(t, h, "a reservation beside a check's hold", ReasonMaintenance)
	if m, ok := h.snap().Maintenance(); !ok || m.Reason != "check" {
		t.Fatal("the reservation refused let the check's hold go")
	}
	h.must("the check's exit", h.eng.ExitMaintenance(bg, hold.Lease))
	l, err = h.reserveWithin("a reservation once the hold is let go")
	h.must("a reservation once the hold is let go", err)
	h.releaseWithin("its release", l)
}

// TestACheckHoldDecidesWouldInterruptInItsGrantsStep pins the one step that
// grants the check's hold (B728 B12): stopped with nothing under way it is
// granted at once, nothing released; in every state that is not (a start the
// backend holds, playing, paused with its session, releasing to stop, paused
// with no session, paused at a seek's target) it is refused ReasonNotStopped
// without interrupt, changing nothing; with it, it is granted in the step that
// takes it, the session released and nothing started, and answered only once
// the release the fake parks is let go; where no session holds the device it
// is answered at once, the seek's target and its track dropped. Its exit leaves
// the transport stopped.
func TestACheckHoldDecidesWouldInterruptInItsGrantsStep(t *testing.T) {
	t.Run("stopped with nothing under way", func(t *testing.T) {
		h := checkHarness(t, nil)
		h.insert("a")
		h.available()
		mark := h.be.mark()
		hold := h.granted("a check's hold while stopped", false)
		if calls := h.be.callsSince(mark); slices.Contains(calls, "release") || slices.Contains(calls, "start a") {
			t.Errorf("a check's hold while stopped made the calls %v", calls)
		}
		h.must("the check's exit", h.eng.ExitMaintenance(bg, hold.Lease))
		if sn := h.snap(); sn.State() != Stopped {
			t.Errorf("after the exit %s, want stopped", sn.State())
		}
	})
	t.Run("a start the backend holds", func(t *testing.T) {
		h := checkHarness(t, nil)
		ids := h.insert("a", "b")
		h.available()
		g := newGate()
		letGoAtEnd(t, g)
		h.be.script(func() {
			h.be.onStart = func(ctx context.Context, s *fakeSession) error {
				if err := g.wait(ctx, h.be, true); err != nil {
					return err
				}
				return s.open()
			}
		})
		play := async(func() error { return h.eng.PlayID(bg, ids[0]) })
		s := h.be.nth(1)
		h.waitSnap("the start under way", func(sn *Snapshot) bool { return sn.State() == Playing && sn.Starting() })
		refusedCheck(t, h, "a check's hold beside a start", false, ReasonNotStopped)
		mark := h.appliedMark()
		check := h.grantWaits(s)
		h.barrier()
		if h.repliedSince(checkOp, mark) {
			t.Fatal("the check's hold was answered while the start it released is held")
		}
		h.refused("the play the check interrupted", h.result("play", play), ReasonMaintenance)
		g.open()
		a := h.checkResult("the check's hold once the start is let go", check)
		h.must("the check's hold once the start is let go", a.err)
		if _, _, wrote := s.state(); wrote || !s.isAcked() {
			t.Errorf("the start the check released: wrote %v, acknowledged %v; want nothing written, released", wrote, s.isAcked())
		}
		h.must("the check's exit", h.eng.ExitMaintenance(bg, a.hold.Lease))
		if sn := h.snap(); sn.State() != Stopped || h.be.starts() != 1 {
			t.Errorf("after the exit %s with %d sessions; want stopped, the one", sn.State(), h.be.starts())
		}
	})
	t.Run("playing", func(t *testing.T) {
		h := checkHarness(t, nil)
		_, s := h.playing(0, "a", "b")
		refusedCheck(t, h, "a check's hold while playing", false, ReasonNotStopped)
		g := holdReleases(h)
		letGoAtEnd(t, g)
		hold := h.interrupts(s, g)
		h.must("the check's exit", h.eng.ExitMaintenance(bg, hold.Lease))
		if sn := h.snap(); sn.State() != Stopped || h.be.starts() != 1 {
			t.Errorf("after the exit %s with %d sessions; want stopped, the one", sn.State(), h.be.starts())
		}
	})
	t.Run("paused with its session", func(t *testing.T) {
		h := checkHarness(t, nil)
		_, s := h.playing(0, "a", "b")
		h.must("pause", h.eng.Pause(bg))
		refusedCheck(t, h, "a check's hold while paused", false, ReasonNotStopped)
		g := holdReleases(h)
		letGoAtEnd(t, g)
		hold := h.interrupts(s, g)
		h.must("the check's exit", h.eng.ExitMaintenance(bg, hold.Lease))
		if sn := h.snap(); sn.State() != Stopped || h.be.count("resume") != 0 {
			t.Errorf("after the exit %s with %d resumes; want stopped, none", sn.State(), h.be.count("resume"))
		}
	})
	t.Run("releasing to stop", func(t *testing.T) {
		h := checkHarness(t, nil)
		_, s := h.playing(0, "a", "b")
		g := holdReleases(h)
		letGoAtEnd(t, g)
		stop := async(func() error { return h.eng.Stop(bg) })
		h.be.waitFor("the stop's release", func() bool { return s.releaseAsked })
		h.waitSnap("releasing to stop", func(sn *Snapshot) bool { return sn.State() == Releasing && sn.Pending() == IntentStop })
		refusedCheck(t, h, "a check's hold while a release is under way", false, ReasonNotStopped)
		hold := h.interrupts(s, g)
		h.must("the stop", h.result("stop", stop))
		h.must("the check's exit", h.eng.ExitMaintenance(bg, hold.Lease))
		if sn := h.snap(); sn.State() != Stopped {
			t.Errorf("after the exit %s, want stopped", sn.State())
		}
	})
	t.Run("paused with no session", func(t *testing.T) {
		h := checkHarness(t, nil)
		ids, _ := h.playing(0, "a", "b")
		h.must("pause", h.eng.Pause(bg))
		h.must("delete the paused entry", h.eng.Delete(bg, ids[0]))
		h.waitSnap("paused at b with no session", func(sn *Snapshot) bool { return sn.State() == Paused && sn.Selected() == ids[1] })
		refusedCheck(t, h, "a check's hold while paused with no session", false, ReasonNotStopped)
		mark := h.be.mark()
		hold := h.granted("a check's hold while paused with no session, interrupting it", true)
		if calls := h.be.callsSince(mark); slices.Contains(calls, "release") || slices.Contains(calls, "start b") {
			t.Errorf("the check's hold with no session made the calls %v", calls)
		}
		h.must("the check's exit", h.eng.ExitMaintenance(bg, hold.Lease))
		if sn := h.snap(); sn.State() != Stopped || sn.Selected() != ids[1] {
			t.Errorf("after the exit %s at %d; want stopped at b", sn.State(), sn.Selected())
		}
	})
	t.Run("paused at a seek's target", func(t *testing.T) {
		h := checkHarness(t, nil)
		h.heardAt(0, pcmMinute, 5*44100, "a", "b")
		h.must("pause", h.eng.Pause(bg))
		h.must("a seek while paused", h.eng.SeekAbsolute(bg, 30))
		sn := h.waitSnap("paused at the seek's target", func(sn *Snapshot) bool {
			return sn.State() == Paused && sn.Position().Frames == 30*44100
		})
		if _, ok := sn.Track(); !ok {
			t.Fatal("the paused seek kept no track")
		}
		refusedCheck(t, h, "a check's hold at a paused seek", false, ReasonNotStopped)
		mark := h.be.mark()
		hold := h.granted("a check's hold at a paused seek, interrupting it", true)
		if _, ok := h.snap().Track(); ok {
			t.Error("the check's hold kept the seek's track")
		}
		if calls := h.be.callsSince(mark); slices.Contains(calls, "release") || slices.Contains(calls, "start a") {
			t.Errorf("the check's hold at a paused seek made the calls %v", calls)
		}
		h.must("the check's exit", h.eng.ExitMaintenance(bg, hold.Lease))
		if sn := h.snap(); sn.State() != Stopped {
			t.Errorf("after the exit %s, want stopped", sn.State())
		}
		h.must("play once the check let go", h.eng.Play(bg))
		if s := h.be.nth(2); s.from != 0 {
			t.Errorf("play once the check let go started from %d, want the start: the seek was dropped", s.from)
		}
	})
}

// TestAPlayAfterTheCheckHoldsGrantIsRefused pins the one step against a
// release and a Play between the grant and its answer (B728 B12): the check's
// hold granted with interrupt while a session plays, its release parked in the
// fake, a Play, a PlayID and a Next sent then are each refused maintenance, and
// the fake records no Start, before the release is let go and after.
func TestAPlayAfterTheCheckHoldsGrantIsRefused(t *testing.T) {
	h := checkHarness(t, nil)
	ids, s := h.playing(0, "a", "b")
	g := holdReleases(h)
	letGoAtEnd(t, g)
	check := h.grantWaits(s)
	h.refused("a play while the check's release is parked", h.eng.Play(bg), ReasonMaintenance)
	h.refused("a play id while the check's release is parked", h.eng.PlayID(bg, ids[1]), ReasonMaintenance)
	h.refused("a next while the check's release is parked", h.eng.Next(bg), ReasonMaintenance)
	if n := h.be.starts(); n != 1 {
		t.Fatalf("%d sessions started while the check's release is parked, want the first alone", n)
	}
	g.open()
	a := h.checkResult("the check's hold", check)
	h.must("the check's hold", a.err)
	if n := h.be.starts(); n != 1 {
		t.Fatalf("%d sessions started once the check's release was let go, want the first alone", n)
	}
	h.must("the check's exit", h.eng.ExitMaintenance(bg, a.hold.Lease))
}

// TestACheckHoldIsRefusedWhileAReleaseIsUnconfirmed pins the fence (B728 B12):
// with a release left unconfirmed past ReleaseTimeout, the check's hold is
// refused ReasonReleaseUnconfirmed, with interrupt or without, changing
// nothing; once the late acknowledgement comes, it is granted.
func TestACheckHoldIsRefusedWhileAReleaseIsUnconfirmed(t *testing.T) {
	h := checkHarness(t, nil)
	mark := h.appliedMark()
	_, s := h.playing(0, "a", "b")
	// The clock is moved past the release's deadline, and so past the
	// successor's Prepare's: its answer is the owner's first.
	h.waitApplied("handled call prepare", mark)
	never := newGate()
	letGoAtEnd(t, never)
	h.be.script(func() {
		h.be.onRelease = func(ctx context.Context, _ *fakeSession) error { return never.wait(ctx, h.be, true) }
	})
	stop := async(func() error { return h.eng.Stop(bg) })
	h.be.waitFor("the stop's release", func() bool { return s.releaseAsked })
	h.clk.Advance(ReleaseTimeout)
	h.must("the stop past its release's deadline", h.result("stop", stop))
	h.waitSnap("the release unconfirmed", func(sn *Snapshot) bool {
		_, ok := sn.ReleaseUnconfirmed()
		return ok && sn.State() == Stopped
	})
	refusedCheck(t, h, "a check's hold while a release is unconfirmed", false, ReasonReleaseUnconfirmed)
	refusedCheck(t, h, "a check's hold that may interrupt, while a release is unconfirmed", true, ReasonReleaseUnconfirmed)
	s.releasedLater()
	h.waitSnap("the release acknowledged late", func(sn *Snapshot) bool { _, ok := sn.ReleaseUnconfirmed(); return !ok })
	hold := h.granted("a check's hold once the release is acknowledged", false)
	h.must("the check's exit", h.eng.ExitMaintenance(bg, hold.Lease))
}

// TestAnInterruptWhoseReleaseIsNotConfirmedSaysSo pins the grant's own release
// left unconfirmed (B728 B12): the check's hold asked with interrupt while a
// session plays, whose release is never acknowledged, is answered
// ReasonReleaseUnconfirmed once ReleaseTimeout has passed, never a hold: the
// hold has ended (no maintenance, the transport stopped, the release shown
// unconfirmed), so its caller holds nothing, and a Play and another check's
// hold are refused ReasonReleaseUnconfirmed, nothing started. A caller whose
// context ended during that release's wait gets the same answer, not a
// cancellation, and no hold stands after it.
func TestAnInterruptWhoseReleaseIsNotConfirmedSaysSo(t *testing.T) {
	for _, gone := range []bool{false, true} {
		t.Run(map[bool]string{false: "its caller waiting", true: "its caller gone during the wait"}[gone], func(t *testing.T) {
			h := checkHarness(t, nil)
			mark := h.appliedMark()
			_, s := h.playing(0, "a", "b")
			h.waitApplied("handled call prepare", mark)
			never := newGate()
			letGoAtEnd(t, never)
			h.be.script(func() {
				h.be.onRelease = func(ctx context.Context, _ *fakeSession) error { return never.wait(ctx, h.be, true) }
			})
			base, cancel := context.WithCancel(bg)
			defer cancel()
			ctx, admission := WithAdmission(base)
			check := h.checkAsync(ctx, true)
			waitClosed(t, "the check's admission", admission)
			h.be.waitFor("the check's release", func() bool { return s.releaseAsked })
			if gone {
				cancel()
			}
			h.clk.Advance(ReleaseTimeout)
			a := h.checkResult("the check's hold whose release is not confirmed", check)
			h.refused("the check's hold whose release is not confirmed", a.err, ReasonReleaseUnconfirmed)
			if errors.Is(a.err, ErrNotAdmitted) || a.hold != (Hold{}) {
				t.Errorf("the check's hold whose release is not confirmed: %+v, %v; want no hold, and an answer of a command that ran", a.hold, a.err)
			}
			sn := h.snap()
			if _, held := sn.Maintenance(); held || sn.State() != Stopped {
				t.Fatalf("after the refused grant: held %v, %s; want no hold, stopped", held, sn.State())
			}
			if _, ok := sn.ReleaseUnconfirmed(); !ok {
				t.Fatal("the snapshot calls the check's release confirmed")
			}
			h.refused("a play after the check's unconfirmed release", h.eng.Play(bg), ReasonReleaseUnconfirmed)
			refusedCheck(t, h, "another check's hold", true, ReasonReleaseUnconfirmed)
			if n := h.be.starts(); n != 1 {
				t.Errorf("%d sessions started, want the first alone", n)
			}
		})
	}
}

// TestACheckHoldAskedByAGoneCallerStopsNothing pins the caller's context (B728
// B12): the owner holds the check's command before it admits it, at the
// admitted seam (before it asks the context) and at the checked seam (after),
// and the caller's context ends there. The caller is answered, without the
// owner, with a cancellation that wraps ErrNotAdmitted; once the owner goes on
// it refuses the command in turn: no release is asked, and the session plays
// on, no hold held.
func TestACheckHoldAskedByAGoneCallerStopsNothing(t *testing.T) {
	for _, seam := range []string{"admitted", "checked"} {
		t.Run("held at the "+seam+" seam", func(t *testing.T) {
			reached, release := make(chan struct{}, 1), newGate()
			hook := func(cmd *command) {
				if cmd.op.String() == checkOp {
					reached <- struct{}{}
					<-release
				}
			}
			h := checkHarness(t, func(c *Config) {
				if seam == "checked" {
					c.seams.checked = hook
					return
				}
				admitted := c.seams.admitted
				c.seams.admitted = func(cmd *command) {
					admitted(cmd)
					hook(cmd)
				}
			})
			letGoAtEnd(t, release)
			_, s := h.playing(0, "a", "b")
			ctx, cancel := context.WithCancel(bg)
			defer cancel()
			check := h.checkAsync(ctx, true)
			waitClosed(t, "the owner holding the check before its admission", reached)
			cancel()
			a := h.checkResult("the check's hold whose caller left before its admission", check)
			if !errors.Is(a.err, ErrNotAdmitted) || ReasonOf(a.err) != ReasonCancelled || !errors.Is(a.err, context.Canceled) {
				t.Fatalf("the check's hold whose caller left before its admission: %v; want a cancellation wrapping ErrNotAdmitted", a.err)
			}
			release.open()
			h.barrier()
			sn := h.snap()
			if _, held := sn.Maintenance(); held || sn.State() != Playing {
				t.Fatalf("after the refused check: held %v, %s; want no hold, playing", held, sn.State())
			}
			if heard, ok := sn.Audible(); !ok || heard.Occurrence != s.first.ID {
				t.Errorf("after the refused check the occurrence heard is %+v %v, want the one playing", heard, ok)
			}
			if n := h.be.count("release"); n != 0 {
				t.Errorf("the refused check asked %d releases", n)
			}
		})
	}
}

// TestTheCheckHoldsGrantIsNeverLostToItsCaller pins the grant kept for its
// caller (B728 B12): the check's hold asked with interrupt while a session
// plays, admitted, its release parked, and its caller's context ended there.
// The caller waits for the owner's answer all the same; once the release is
// acknowledged it exits the hold the owner granted and answers ReasonCancelled
// without ErrNotAdmitted (it ran): no hold stands, the transport is stopped,
// what it interrupted is not started again, and a new hold is granted at once.
func TestTheCheckHoldsGrantIsNeverLostToItsCaller(t *testing.T) {
	h := checkHarness(t, nil)
	_, s := h.playing(0, "a", "b")
	g := holdReleases(h)
	letGoAtEnd(t, g)
	base, cancel := context.WithCancel(bg)
	defer cancel()
	ctx, admission := WithAdmission(base)
	check := h.checkAsync(ctx, true)
	waitClosed(t, "the check's admission", admission)
	h.be.waitFor("the check's release", func() bool { return s.releaseAsked })
	cancel()
	g.open()
	a := h.checkResult("the check's hold whose caller left during its release", check)
	if ReasonOf(a.err) != ReasonCancelled || errors.Is(a.err, ErrNotAdmitted) || !errors.Is(a.err, context.Canceled) {
		t.Fatalf("the check's hold whose caller left during its release: %v; want a cancellation of a command that ran", a.err)
	}
	if a.hold != (Hold{}) {
		t.Errorf("the check's hold whose caller left was answered with a hold %+v", a.hold)
	}
	sn := h.snap()
	if _, held := sn.Maintenance(); held || sn.State() != Stopped {
		t.Fatalf("once the caller that left was answered: held %v, %s; want no hold, stopped", held, sn.State())
	}
	if !s.isAcked() || h.be.starts() != 1 {
		t.Errorf("the session the check stopped: acknowledged %v, %d sessions; want released, the one", s.isAcked(), h.be.starts())
	}
	hold := h.granted("a check's hold at once after", false)
	h.must("the check's exit", h.eng.ExitMaintenance(bg, hold.Lease))
}

// TestAStopUnderACheckHoldSignalsItAndAnswersAtItsExit pins the Stop's one
// interception (B728 B12): under the check's hold a Stop closes the hold's
// stop signal, once whatever the Stops, and waits, two Stops each, until the
// check lets go (its exit), when each answers success; or until ReleaseTimeout
// on the engine's clock from the first, when it answers success, the hold held
// still, and every later Stop is answered at once; a Stop that comes while the
// grant waits for its release ends the hold as that release settles, the
// grant answered ReasonStopped and the Stop success; and under the harness's
// plain maintenance a Stop still answers at once, its hold carrying no signal.
func TestAStopUnderACheckHoldSignalsItAndAnswersAtItsExit(t *testing.T) {
	t.Run("at its exit", func(t *testing.T) {
		h := checkHarness(t, nil)
		h.insert("a")
		h.available()
		hold := h.granted("the check's hold", false)
		mark := h.appliedMark()
		first := async(func() error { return h.eng.Stop(bg) })
		waitClosed(t, "the hold's stop signal at a Stop", hold.Stop)
		second := async(func() error { return h.eng.Stop(bg) })
		h.waitAppliedTimes("stop", mark, 2)
		if h.repliedSince("stop", mark) {
			t.Fatal("a Stop under the check's hold was answered before the check let go")
		}
		if m, ok := h.snap().Maintenance(); !ok || m.Reason != "check" {
			t.Fatal("a Stop ended the check's hold itself")
		}
		h.must("the check lets go", h.eng.ExitMaintenance(bg, hold.Lease))
		h.must("the first Stop, once the check let go", h.result("the first stop", first))
		h.must("the second Stop, once the check let go", h.result("the second stop", second))
		if sn := h.snap(); sn.State() != Stopped {
			t.Errorf("after the exit %s, want stopped", sn.State())
		}
	})
	t.Run("at ReleaseTimeout", func(t *testing.T) {
		h := checkHarness(t, nil)
		h.insert("a")
		h.available()
		hold := h.granted("the check's hold", false)
		mark := h.appliedMark()
		stop := async(func() error { return h.eng.Stop(bg) })
		h.waitApplied("stop", mark)
		if h.repliedSince("stop", mark) {
			t.Fatal("a Stop under the check's hold was answered at once")
		}
		h.clk.Advance(ReleaseTimeout - time.Nanosecond)
		h.barrier()
		if h.repliedSince("stop", mark) {
			t.Fatal("a Stop under the check's hold was answered before ReleaseTimeout")
		}
		due := h.appliedMark()
		h.clk.Advance(time.Nanosecond)
		h.waitApplied("handled check stops", due)
		h.must("the Stop at ReleaseTimeout", h.result("stop", stop))
		if m, ok := h.snap().Maintenance(); !ok || m.Reason != "check" || !signalled(hold.Stop) {
			t.Fatalf("at the Stops' deadline: held %v (%+v), signalled %v; want the check's hold held, signalled", ok, m, signalled(hold.Stop))
		}
		h.must("a Stop past the deadline, answered at once", h.result("a Stop past the deadline", async(func() error { return h.eng.Stop(bg) })))
		h.must("the check lets go", h.eng.ExitMaintenance(bg, hold.Lease))
	})
	t.Run("before the grant's answer", func(t *testing.T) {
		h := checkHarness(t, nil)
		_, s := h.playing(0, "a", "b")
		g := holdReleases(h)
		letGoAtEnd(t, g)
		mark := h.appliedMark()
		check := h.grantWaits(s)
		stop := async(func() error { return h.eng.Stop(bg) })
		h.waitApplied("stop", mark)
		if h.repliedSince(checkOp, mark) || h.repliedSince("stop", mark) {
			t.Fatal("the check's hold or the Stop was answered while the check's release is parked")
		}
		g.open()
		a := h.checkResult("the check's hold a Stop came before", check)
		h.refused("the check's hold a Stop came before", a.err, ReasonStopped)
		if a.hold != (Hold{}) {
			t.Errorf("the check's hold a Stop came before was handed over: %+v", a.hold)
		}
		h.must("the Stop that came before the grant's answer", h.result("stop", stop))
		if sn := h.snap(); func() bool { _, held := sn.Maintenance(); return held }() || sn.State() != Stopped || h.be.starts() != 1 {
			t.Fatalf("after the stopped grant: %s with %d sessions; want no hold, stopped, the one", sn.State(), h.be.starts())
		}
		hold := h.granted("a check's hold after", false)
		h.must("its exit", h.eng.ExitMaintenance(bg, hold.Lease))
	})
	t.Run("plain maintenance", func(t *testing.T) {
		h := checkHarness(t, nil)
		h.insert("a")
		hold, err := h.eng.EnterMaintenance(bg, h.snap().Generation(), "the harness's")
		h.must("the harness's hold", err)
		if hold.Stop != nil {
			t.Error("a plain maintenance hold carries a stop signal")
		}
		h.must("a Stop under plain maintenance, answered at once", h.result("stop", async(func() error { return h.eng.Stop(bg) })))
		if m, ok := h.snap().Maintenance(); !ok || m.Reason != "the harness's" {
			t.Fatal("a Stop ended the plain hold")
		}
		h.must("the harness's exit", h.eng.ExitMaintenance(bg, hold.Lease))
	})
}

// TestACheckHoldsStopDeadlineIsItsOwn pins the Stops' deadline to its hold
// (B728 B12): the deadline of an earlier check's hold, its message reaching
// the owner under a later one, answers none of the later hold's Stops and
// passes no deadline of its own, a later Stop still waiting; the later hold's
// own deadline answers both.
func TestACheckHoldsStopDeadlineIsItsOwn(t *testing.T) {
	var held atomic.Pointer[maintHold]
	var h *harness
	h = checkHarness(t, func(c *Config) {
		admitted := c.seams.admitted
		c.seams.admitted = func(cmd *command) {
			admitted(cmd)
			// On the owner's goroutine, as it admits a barrier.
			if cmd.op.String() == "set shuffle" {
				held.Store(h.eng.o.maint)
			}
		}
	})
	h.insert("a")
	h.available()
	first := h.granted("the first check's hold", false)
	h.barrier()
	stale := held.Load()
	if stale == nil || stale.check == nil {
		t.Fatal("the first check's hold was not seen")
	}
	h.must("the first check lets go", h.eng.ExitMaintenance(bg, first.Lease))
	second := h.granted("the second check's hold", false)
	mark := h.appliedMark()
	stop := async(func() error { return h.eng.Stop(bg) })
	h.waitApplied("stop", mark)
	if h.repliedSince("stop", mark) {
		t.Fatal("a Stop under the second check's hold was answered at once")
	}
	due := h.appliedMark()
	h.eng.post(msgCheckStops{m: stale})
	h.waitApplied("handled check stops", due)
	later := async(func() error { return h.eng.Stop(bg) })
	h.waitAppliedTimes("stop", mark, 2)
	if h.repliedSince("stop", mark) {
		t.Fatal("the first hold's deadline answered a Stop under the second")
	}
	h.clk.Advance(ReleaseTimeout)
	h.must("the Stop at the second hold's own deadline", h.result("stop", stop))
	h.must("the later Stop at the same deadline", h.result("the later stop", later))
	h.must("the second check lets go", h.eng.ExitMaintenance(bg, second.Lease))
}

// TestACheckHoldsEveryEndClosesItsSignal pins the stop signal (B728 B12): it
// closes at each end of the check's hold, with no Stop before it: the exit, the
// expiry (after which the lease is refused) and the shutdown.
func TestACheckHoldsEveryEndClosesItsSignal(t *testing.T) {
	t.Run("its exit", func(t *testing.T) {
		h := checkHarness(t, nil)
		h.insert("a")
		h.available()
		hold := h.granted("the check's hold", false)
		h.must("the check's exit", h.eng.ExitMaintenance(bg, hold.Lease))
		if !signalled(hold.Stop) {
			t.Error("the exit left the stop signal open")
		}
	})
	t.Run("its expiry", func(t *testing.T) {
		h := checkHarness(t, nil)
		h.insert("a")
		h.available()
		hold := h.granted("the check's hold", false)
		h.clk.Advance(DefaultMaintenanceTTL)
		h.waitSnap("the hold's expiry", func(sn *Snapshot) bool { _, ok := sn.Maintenance(); return !ok })
		if !signalled(hold.Stop) {
			t.Error("the expiry left the stop signal open")
		}
		h.refused("the exit of an expired hold", h.eng.ExitMaintenance(bg, hold.Lease), ReasonLease)
		if sn := h.snap(); sn.State() != Stopped {
			t.Errorf("after the expiry %s, want stopped", sn.State())
		}
	})
	t.Run("the shutdown", func(t *testing.T) {
		h := checkHarness(t, nil)
		h.insert("a")
		h.available()
		hold := h.granted("the check's hold", false)
		h.shutdown()
		if !signalled(hold.Stop) {
			t.Error("the shutdown left the stop signal open")
		}
	})
}

// TestACheckHoldThatExpiresBeforeItsGrantIsNeverHandedOver pins an expiry
// before the grant is answered (B728 B12): with a hold of a second
// (MaintenanceTTL's floor, shorter than ReleaseTimeout), the check's hold asked
// with interrupt while its release is parked expires first, and its caller is
// answered ReasonLease (the hold expired), or ReasonStopped where a Stop came
// first, never a lease already gone: no hold stands, the parked release
// carries the transport to stopped once let go, and nothing starts.
func TestACheckHoldThatExpiresBeforeItsGrantIsNeverHandedOver(t *testing.T) {
	for _, stopFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "no Stop", true: "a Stop first"}[stopFirst], func(t *testing.T) {
			h := checkHarness(t, func(c *Config) { c.MaintenanceTTL = time.Second })
			_, s := h.playing(0, "a", "b")
			g := holdReleases(h)
			letGoAtEnd(t, g)
			mark := h.appliedMark()
			check := h.grantWaits(s)
			var stop <-chan error
			want := ReasonLease
			if stopFirst {
				stop = async(func() error { return h.eng.Stop(bg) })
				h.waitApplied("stop", mark)
				want = ReasonStopped
			}
			h.clk.Advance(time.Second)
			a := h.checkResult("the check's hold that expired before it was handed over", check)
			h.refused("the check's hold that expired before it was handed over", a.err, want)
			if a.hold != (Hold{}) {
				t.Errorf("the check's hold that expired was handed over: %+v", a.hold)
			}
			if _, held := h.snap().Maintenance(); held {
				t.Fatal("the expired hold still stands")
			}
			if stopFirst {
				h.must("the Stop that came first", h.result("stop", stop))
			}
			g.open()
			h.waitSnap("stopped once the release is acknowledged", func(sn *Snapshot) bool { return sn.State() == Stopped })
			if n := h.be.starts(); n != 1 {
				t.Errorf("%d sessions started, want the first alone", n)
			}
		})
	}
}

// TestACheckHoldNeedsItsDACAndEndsWithIt pins the output (B728 B12): the
// check's hold is refused while the output is unknown or unavailable; and a
// report that takes the output away, or names another unit, ends a held check
// (its stop signal closed, no hold, the transport stopped, its lease refused)
// and answers a grant still waiting for its release with the report's reason.
func TestACheckHoldNeedsItsDACAndEndsWithIt(t *testing.T) {
	t.Run("no DAC", func(t *testing.T) {
		h := checkHarness(t, nil)
		h.insert("a")
		refusedCheck(t, h, "a check's hold with the output unknown", true, ReasonOutputUnknown)
		h.report(OutputReport{Available: false, Reason: "device_missing"})
		refusedCheck(t, h, "a check's hold with the output unavailable", true, ReasonOutputUnavailable)
		h.available()
		hold := h.granted("a check's hold with the DAC there", false)
		h.must("the check's exit", h.eng.ExitMaintenance(bg, hold.Lease))
	})
	for _, c := range []struct {
		what   string
		report OutputReport
		reason Reason
	}{
		{"unplugged", OutputReport{Available: false, Reason: "device_missing"}, ReasonOutputUnavailable},
		{"another unit", OutputReport{Available: true, UnitChanged: true}, ReasonUnitChanged},
	} {
		t.Run(c.what+" under a held check", func(t *testing.T) {
			h := checkHarness(t, nil)
			h.insert("a")
			h.available()
			hold := h.granted("the check's hold", false)
			h.report(c.report)
			sn := h.snap()
			if _, held := sn.Maintenance(); held || sn.State() != Stopped {
				t.Fatalf("after the report: held %v, %s; want no hold, stopped", held, sn.State())
			}
			if !signalled(hold.Stop) {
				t.Error("the report left the stop signal open")
			}
			h.refused("the check's exit once its DAC went", h.eng.ExitMaintenance(bg, hold.Lease), ReasonLease)
		})
		t.Run(c.what+" before the check's grant", func(t *testing.T) {
			h := checkHarness(t, nil)
			_, s := h.playing(0, "a", "b")
			g := holdReleases(h)
			letGoAtEnd(t, g)
			check := h.grantWaits(s)
			h.report(c.report)
			a := h.checkResult("the check's hold whose DAC went before it was handed over", check)
			h.refused("the check's hold whose DAC went before it was handed over", a.err, c.reason)
			if _, held := h.snap().Maintenance(); held {
				t.Fatal("the hold still stands")
			}
			g.open()
			h.waitSnap("stopped once the release is acknowledged", func(sn *Snapshot) bool { return sn.State() == Stopped })
			if n := h.be.starts(); n != 1 {
				t.Errorf("%d sessions started, want the first alone", n)
			}
		})
	}
}

// TestStopsParkedUnderACheckHoldAreAnsweredAtShutdown pins the shutdown's
// answer (B728 B12): two Stops parked under the check's hold, the shutdown
// begun and held in its closing phase (the queue file's writer held as it
// waits), each Stop is answered closed while Run has not returned, within the
// watchdog, never a hang until the engine's end.
func TestStopsParkedUnderACheckHoldAreAnsweredAtShutdown(t *testing.T) {
	held, letWriterGo := make(chan struct{}, 1), newGate()
	var armed atomic.Bool
	h := checkHarness(t, func(c *Config) {
		c.seams.writerWaits = func(uint64) {
			if armed.CompareAndSwap(true, false) {
				held <- struct{}{}
				<-letWriterGo
			}
		}
	})
	letGoAtEnd(t, letWriterGo)
	h.available()
	armed.Store(true)
	// A change the writer is handed, so it waits again: held there, it can
	// write no flush, and the shutdown cannot pass its closing phase.
	h.insert("a")
	waitClosed(t, "the queue file's writer to wait", held)
	hold := h.granted("the check's hold", false)
	mark := h.appliedMark()
	stops := []<-chan error{async(func() error { return h.eng.Stop(bg) }), async(func() error { return h.eng.Stop(bg) })}
	h.waitAppliedTimes("stop", mark, 2)
	h.cancel()
	for i, stop := range stops {
		h.refused("a Stop parked as the shutdown began", h.result("stop", stop), ReasonClosed)
		if i == 0 && !h.snap().Closed() {
			t.Fatal("a parked Stop was answered before the shutdown began")
		}
	}
	if !signalled(hold.Stop) {
		t.Error("the shutdown left the stop signal open")
	}
	select {
	case <-h.ran:
		t.Fatal("Run returned while its writer was held: the Stops' answers were the engine's end, not the shutdown's")
	default:
	}
	letWriterGo.open()
	h.drive(h.ran)
	h.down = true
}

// TestTheCheckHoldNeverAutoplays pins the exit (B728 B12): a Next's release to
// play b parked, the check's hold asked with interrupt drops the pending play
// (the Next answered maintenance) and is answered once the release is
// acknowledged; its exit leaves the transport stopped, b selected and never
// started. A hold that interrupted a playing session leaves it stopped at its
// exit too, nothing resumed or started.
func TestTheCheckHoldNeverAutoplays(t *testing.T) {
	h := checkHarness(t, nil)
	ids, s := h.playing(0, "a", "b")
	g := holdReleases(h)
	letGoAtEnd(t, g)
	next := async(func() error { return h.eng.Next(bg) })
	h.be.waitFor("the next's release", func() bool { return s.releaseAsked })
	h.waitSnap("releasing to play b", func(sn *Snapshot) bool {
		return sn.State() == Releasing && sn.Pending() == IntentPlay && sn.Selected() == ids[1]
	})
	check := h.grantWaits(s)
	h.refused("the next whose play the check dropped", h.result("next", next), ReasonMaintenance)
	g.open()
	a := h.checkResult("the check's hold", check)
	h.must("the check's hold", a.err)
	h.must("the check's exit", h.eng.ExitMaintenance(bg, a.hold.Lease))
	if sn := h.snap(); sn.State() != Stopped || sn.Selected() != ids[1] || h.be.starts() != 1 {
		t.Fatalf("after the exit %s at %d with %d sessions; want stopped at b, the first session alone", sn.State(), sn.Selected(), h.be.starts())
	}
	h.must("play b", h.eng.Play(bg))
	s2 := h.be.nth(2)
	s2.audible(s2.first.ID)
	h.waitSnap("b heard", func(sn *Snapshot) bool { heard, ok := sn.Audible(); return ok && heard.Occurrence == s2.first.ID })
	hold := h.granted("a check's hold interrupting b", true)
	h.must("its exit", h.eng.ExitMaintenance(bg, hold.Lease))
	if sn := h.snap(); sn.State() != Stopped || h.be.starts() != 2 || h.be.count("resume") != 0 {
		t.Errorf("after the second exit %s with %d sessions and %d resumes; want stopped, two, none", sn.State(), h.be.starts(), h.be.count("resume"))
	}
}
