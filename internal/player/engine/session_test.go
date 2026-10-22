package engine

import (
	"context"
	"strconv"
	"testing"
	"time"
)

// TestNoOpenOverlapsAnUnacknowledgedRelease pins the first term of the
// backend contract from the engine's side: DeleteAll, inserts and Play in
// quick succession open only once the release is acknowledged. The fake
// fails the test on any Start while a release is unacknowledged.
func TestNoOpenOverlapsAnUnacknowledgedRelease(t *testing.T) {
	h := startHarness(t)
	_, s := h.playing(0, "a", "b")
	g := holdReleases(h)
	h.must("delete all", h.eng.DeleteAll(bg))
	x := h.insert("x", "y")[0]
	play := async(func() error { return h.eng.Play(bg) })
	h.waitSnap("play pending", func(sn *Snapshot) bool { return sn.State() == Releasing && sn.Pending() == IntentPlay })
	if n := h.be.starts(); n != 1 {
		t.Fatalf("%d sessions started before the release was acknowledged", n)
	}
	g.open()
	h.must("play", h.result("play", play))
	if s2 := h.be.nth(2); s2.first.Entry != x || !s.isAcked() {
		t.Fatalf("the second session started %d, want x", s2.first.Entry)
	}
}

// TestNextThreeTimesWhileReleasingRunsOnceForTheLastTarget pins the one
// pending intent: the latest target wins, and one session opens for it.
func TestNextThreeTimesWhileReleasingRunsOnceForTheLastTarget(t *testing.T) {
	h := startHarness(t)
	ids, _ := h.playing(0, "a", "b", "c", "d", "e")
	g := holdReleases(h)
	var done []<-chan error
	for i := 0; i < 3; i++ {
		applied := h.appliedMark()
		done = append(done, async(func() error { return h.eng.Next(bg) }))
		h.waitApplied("next", applied)
	}
	if sn := h.snap(); sn.Selected() != ids[3] || sn.State() != Releasing || sn.Pending() != IntentPlay {
		t.Fatalf("%s pending %s at %d", sn.State(), sn.Pending(), sn.Selected())
	}
	g.open()
	for i, ch := range done {
		h.must("next", h.result("next "+strconv.Itoa(i), ch))
	}
	if n := h.be.starts(); n != 2 {
		t.Fatalf("%d sessions, want the first and one for the last target", n)
	}
	if s2 := h.be.nth(2); s2.first.Entry != ids[3] {
		t.Fatalf("the session started at %d, want d", s2.first.Entry)
	}
	if n := h.be.count("release"); n != 1 {
		t.Fatalf("%d releases, want 1", n)
	}
}

// TestALateOpenIsClosedAndNeverWrittenTo pins a Stop during an open: the
// session is released while its Start is still opening, the open that
// completes is closed at once and never written to, and its late success is
// not taken for playback; once within the deadline and once past it, where
// the late acknowledgement clears the unconfirmed release.
func TestALateOpenIsClosedAndNeverWrittenTo(t *testing.T) {
	for _, pastDeadline := range []bool{false, true} {
		name := "within the deadline"
		if pastDeadline {
			name = "past the deadline"
		}
		t.Run(name, func(t *testing.T) {
			h := startHarness(t)
			h.insert("a")
			h.available()
			open := newGate()
			h.be.script(func() {
				h.be.onStart = func(ctx context.Context, s *fakeSession) error {
					if err := open.wait(ctx, h.be, false); err != nil {
						return err
					}
					return s.open()
				}
			})
			play := async(func() error { return h.eng.Play(bg) })
			s := h.be.nth(1)
			stop := async(func() error { return h.eng.Stop(bg) })
			h.be.waitFor("the release", func() bool { return s.releaseAsked })
			if pastDeadline {
				h.clk.Advance(ReleaseTimeout)
				h.must("stop", h.result("stop", stop))
				if _, ok := h.snap().ReleaseUnconfirmed(); !ok {
					t.Fatal("a release past its deadline is not unconfirmed")
				}
			}
			open.open()
			if !pastDeadline {
				h.must("stop", h.result("stop", stop))
			}
			_ = h.result("play", play)
			sn := h.waitSnap("stopped and confirmed", func(sn *Snapshot) bool {
				_, unconfirmed := sn.ReleaseUnconfirmed()
				return sn.State() == Stopped && !unconfirmed
			})
			opened, closed, wrote := s.state()
			if !opened || !closed || wrote {
				t.Fatalf("the late open: opened %v closed %v written %v; want opened, closed, never written", opened, closed, wrote)
			}
			if _, ok := sn.Audible(); ok || h.be.starts() != 1 {
				t.Fatal("the late open was taken for playback")
			}
		})
	}
}

// TestALateEventFromAnOldSessionIsDiscarded pins the session check: an old
// session's audible report, position, fault and finish change nothing.
func TestALateEventFromAnOldSessionIsDiscarded(t *testing.T) {
	h := startHarness(t)
	ids, s1 := h.playing(0, "a", "b", "c")
	b := s1.prepared(s1.first.ID, 1)
	h.must("play c", h.eng.PlayID(bg, ids[2]))
	s2 := h.be.nth(2)
	s2.audible(s2.first.ID)
	h.waitSnap("c audible", func(sn *Snapshot) bool { a, _ := sn.Audible(); return a.Occurrence == s2.first.ID })
	s1.submitted(b.ID)
	s1.audible(b.ID)
	s1.position(b.ID, 1000)
	s1.fault(Fault{Reason: ReasonXrun, Occurrence: b.ID})
	s1.finished()
	// The current session's position is the marker: events reach the owner
	// in order, so the old ones were handled before it.
	s2.position(s2.first.ID, 600)
	sn := h.waitSnap("the marker or the old events", func(sn *Snapshot) bool {
		return sn.Position().Frames == 600 || sn.Selected() != ids[2] || len(sn.Faults()) > 0 || sn.State() != Playing
	})
	if sn.Selected() != ids[2] || sn.State() != Playing || len(sn.Faults()) != 0 || sn.Position().Frames != 600 {
		t.Fatalf("an old session's events took effect: %s at %d, faults %v, position %+v", sn.State(), sn.Selected(), sn.Faults(), sn.Position())
	}
}

// TestADeadlineQueuedBehindItsAcknowledgementChangesNothing pins an
// acknowledged release as finished: a release deadline whose timer fired
// while the owner was busy, and so reached it behind the acknowledgement
// (stopping a timer cannot take back a message it has sent), marks no
// release unconfirmed and leaves the transport alone: a Stop's stopped
// transport plays again, and a Next's new session plays on.
func TestADeadlineQueuedBehindItsAcknowledgementChangesNothing(t *testing.T) {
	for _, tc := range []struct {
		name  string
		run   func(h *harness) error
		state State
	}{
		{"stop", func(h *harness) error { return h.eng.Stop(bg) }, Stopped},
		{"next", func(h *harness) error { return h.eng.Next(bg) }, Playing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			hold := make(chan chan struct{}, 1)
			holding := make(chan struct{}, 1)
			h.tweak = func(c *Config) {
				c.seams.admitted = func(cmd *command) {
					if cmd.op != opOutput {
						return
					}
					select {
					case g := <-hold:
						holding <- struct{}{}
						<-g
					default:
					}
				}
			}
			h.boot()
			ids, s := h.playing(0, "a", "b")
			never := newGate()
			h.be.script(func() {
				h.be.onRelease = func(ctx context.Context, fs *fakeSession) error {
					if fs.id != s.id {
						<-fs.startDone
						fs.ack()
						return nil
					}
					_ = never.wait(ctx, h.be, false)
					return context.Canceled
				}
			})
			done := async(func() error { return tc.run(h) })
			h.be.waitFor("the release", func() bool { return s.releaseAsked })
			// The owner is held in a report's admission while the
			// acknowledgement and then the deadline queue up behind it.
			owner := make(chan struct{})
			hold <- owner
			h.outGen++
			report := async(func() error {
				return h.eng.ReportOutput(bg, OutputReport{Generation: h.outGen, Available: true})
			})
			select {
			case <-holding:
			case <-time.After(*watchdog):
				t.Fatalf("waited %v for the owner to take the report", *watchdog)
			}
			mark := h.appliedMark()
			s.ack()
			s.ev.Released(s.id)
			h.clk.Advance(ReleaseTimeout)
			close(owner)
			h.must("report", h.result("report", report))
			h.must(tc.name, h.result(tc.name, done))
			h.waitApplied("handled release deadline", mark)
			sn := h.snap()
			if info, ok := sn.ReleaseUnconfirmed(); ok || sn.State() != tc.state {
				t.Fatalf("a deadline behind its acknowledgement: %s, want %s; unconfirmed %+v %v", sn.State(), tc.state, info, ok)
			}
			if tc.state == Playing {
				if s2 := h.be.nth(2); s2.first.Entry != ids[1] || sn.Selected() != ids[1] {
					t.Fatalf("the next session started %d, selected %d", s2.first.Entry, sn.Selected())
				}
			} else {
				h.must("play after the acknowledged release", h.eng.Play(bg))
				if h.be.starts() != 2 {
					t.Fatalf("%d sessions, want the second started", h.be.starts())
				}
			}
			never.open()
		})
	}
}

// TestALateAnswerIsRefusedWhateverTheOrder pins a call's deadline: its
// context carries the deadline, on the engine's clock, and an answer that
// comes at or past it is late even when it reaches the owner before the
// expiry does (the expiry's timer has not run yet): the call is treated as
// failed, its session released with ReasonTimeout, and the command that
// waited on it answers so. The session's own Prepare is taken before the
// clock moves, so that only the held call answers late.
func TestALateAnswerIsRefusedWhateverTheOrder(t *testing.T) {
	for _, tc := range []struct {
		name    string
		call    string
		timeout time.Duration
		// setup brings the engine to where cmd makes the call; hook holds
		// the call on held and records its context's deadline.
		setup func(h *harness)
		hook  func(h *harness, held gate, deadline func(context.Context))
		cmd   func(h *harness) error
	}{
		{"start", "start", StartTimeout,
			func(h *harness) { h.insert("a", "b"); h.available() },
			func(h *harness, held gate, deadline func(context.Context)) {
				h.be.onStart = func(ctx context.Context, s *fakeSession) error {
					deadline(ctx)
					if err := held.wait(ctx, h.be, false); err != nil {
						return err
					}
					return s.open()
				}
			},
			func(h *harness) error { return h.eng.Play(bg) }},
		{"pause", "pause", CallTimeout,
			func(h *harness) { h.playing(0, "a", "b"); h.waitApplied("handled call prepare", 0) },
			func(h *harness, held gate, deadline func(context.Context)) {
				h.be.onPause = func(ctx context.Context, _ *fakeSession) error {
					deadline(ctx)
					return held.wait(ctx, h.be, false)
				}
			},
			func(h *harness) error { return h.eng.Pause(bg) }},
		{"resume", "resume", CallTimeout,
			func(h *harness) {
				h.playing(0, "a", "b")
				h.waitApplied("handled call prepare", 0)
				h.must("pause", h.eng.Pause(bg))
			},
			func(h *harness, held gate, deadline func(context.Context)) {
				h.be.onResume = func(ctx context.Context, _ *fakeSession) (ResumeResult, error) {
					deadline(ctx)
					if err := held.wait(ctx, h.be, false); err != nil {
						return 0, err
					}
					return ResumeContinued, nil
				}
			},
			func(h *harness) error { return h.eng.Play(bg) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := startHarness(t)
			tc.setup(h)
			held := newGate()
			deadlines := make(chan time.Time, 1)
			h.be.script(func() {
				tc.hook(h, held, func(ctx context.Context) {
					d, ok := ctx.Deadline()
					if !ok {
						d = time.Time{}
					}
					deadlines <- d
				})
			})
			called := h.clk.Now()
			done := async(func() error { return tc.cmd(h) })
			var deadline time.Time
			select {
			case deadline = <-deadlines:
			case <-time.After(*watchdog):
				t.Fatalf("waited %v for the %s", *watchdog, tc.call)
			}
			if want := called.Add(tc.timeout); !deadline.Equal(want) {
				t.Errorf("the %s's context has the deadline %v, want %v, the engine's clock at the call plus %v", tc.call, deadline, want, tc.timeout)
			}
			// The deadline passes; its timer's callback has not run.
			h.clk.Skip(tc.timeout)
			mark := h.appliedMark()
			held.open()
			h.waitApplied("handled call "+tc.call, mark)
			h.refused("the command whose call answered late", h.result(tc.name, done), ReasonTimeout)
			h.clk.Advance(0)
			sn := h.waitSnap("stopped", func(sn *Snapshot) bool { return sn.State() == Stopped })
			if sn.LastStop().Reason != ReasonTimeout || h.be.starts() != 1 {
				t.Fatalf("last stop %+v, %d sessions", sn.LastStop(), h.be.starts())
			}
		})
	}
}

// TestAStopThatNeverAnswersLeavesTheReleaseUnconfirmed pins the confirmed
// release: past ReleaseTimeout the release is unconfirmed, everything that
// would open the device is refused with that reason, and the late
// acknowledgement clears it.
func TestAStopThatNeverAnswersLeavesTheReleaseUnconfirmed(t *testing.T) {
	h := startHarness(t)
	ids, s := h.playing(0, "a", "b")
	ack := newGate()
	h.be.script(func() {
		h.be.onRelease = func(ctx context.Context, fs *fakeSession) error {
			_ = ack.wait(ctx, h.be, false)
			return context.Canceled
		}
	})
	stop := async(func() error { return h.eng.Stop(bg) })
	h.be.waitFor("the release", func() bool { return s.releaseAsked })
	h.clk.Advance(ReleaseTimeout)
	h.must("stop", h.result("stop", stop))
	sn := h.snap()
	info, ok := sn.ReleaseUnconfirmed()
	if !ok || info.Session != s.id || sn.State() != Stopped {
		t.Fatalf("after the deadline: %s, unconfirmed %+v %v", sn.State(), info, ok)
	}
	h.refused("play", h.eng.Play(bg), ReasonReleaseUnconfirmed)
	h.refused("play id", h.eng.PlayID(bg, ids[1]), ReasonReleaseUnconfirmed)
	h.refused("play index", h.eng.PlayIndex(bg, 1), ReasonReleaseUnconfirmed)
	h.refused("next", h.eng.Next(bg), ReasonReleaseUnconfirmed)
	h.refused("previous", h.eng.Previous(bg), ReasonReleaseUnconfirmed)
	h.must("stop while unconfirmed", h.eng.Stop(bg))
	if h.be.starts() != 1 {
		t.Fatal("a session opened while the release was unconfirmed")
	}
	ack.open()
	s.ack()
	s.ev.Released(s.id)
	h.waitSnap("the late acknowledgement", func(sn *Snapshot) bool { _, ok := sn.ReleaseUnconfirmed(); return !ok })
	h.must("play after the acknowledgement", h.eng.Play(bg))
	if h.be.starts() != 2 {
		t.Fatal("play after the acknowledgement started nothing")
	}
}
