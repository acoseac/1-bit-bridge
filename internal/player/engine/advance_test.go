package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestAdvanceFollowsAudibleReportsNotHandOff pins autonomous advance across
// three entries: the selection moves on each audible report, never when the
// successor is handed off, and the end of the queue stops on the last entry,
// which Play then replays.
func TestAdvanceFollowsAudibleReportsNotHandOff(t *testing.T) {
	h := startHarness(t)
	ids, s := h.playing(0, "a", "b", "c")
	a := s.first.ID
	b := s.prepared(a, 1)
	if b == nil || b.Entry != ids[1] {
		t.Fatalf("the successor of a is %+v, want b", b)
	}
	s.preparedEvent(b.ID, Join{Gapless: true})
	s.submitted(b.ID)
	c := s.prepared(b.ID, 1)
	if c == nil || c.Entry != ids[2] {
		t.Fatalf("the successor of b is %+v, want c", c)
	}
	if sel := h.snap().Selected(); sel != ids[0] {
		t.Fatalf("handing b off selected %d: the selection moves on the audible report", sel)
	}
	s.audible(b.ID)
	h.waitSnap("b selected", func(sn *Snapshot) bool { a, _ := sn.Audible(); return sn.Selected() == ids[1] && a.Occurrence == b.ID })
	s.submitted(c.ID)
	if end := s.prepared(c.ID, 1); end != nil {
		t.Fatalf("the successor of the last entry with repeat off is %+v, want none", end)
	}
	if sel := h.snap().Selected(); sel != ids[1] {
		t.Fatalf("handing c off selected %d", sel)
	}
	s.audible(c.ID)
	h.waitSnap("c selected", func(sn *Snapshot) bool { return sn.Selected() == ids[2] })
	s.finished()
	sn := h.waitSnap("stopped at the end", func(sn *Snapshot) bool { return sn.State() == Stopped })
	if sn.Selected() != ids[2] || sn.LastStop().Reason != ReasonEndOfQueue {
		t.Fatalf("at the end: selected %d, last stop %+v", sn.Selected(), sn.LastStop())
	}
	h.must("play", h.eng.Play(bg))
	if s2 := h.be.nth(2); s2.first.Entry != ids[2] {
		t.Fatalf("Play after the end started %d, want the last entry", s2.first.Entry)
	}
}

// TestSeveralShortEntriesCrossedInOneUpdate pins boundaries reported back
// to back, as several short entries crossed within one position update
// are: each is its own step.
func TestSeveralShortEntriesCrossedInOneUpdate(t *testing.T) {
	h := startHarness(t)
	ids, s := h.playing(0, "a", "b", "c", "d")
	occ := []OccurrenceID{s.first.ID}
	for i := 1; i < 4; i++ {
		next := s.prepared(occ[i-1], 1)
		if next == nil || next.Entry != ids[i] {
			t.Fatalf("successor %d is %+v", i, next)
		}
		s.submitted(next.ID)
		occ = append(occ, next.ID)
	}
	s.prepared(occ[3], 1)
	before := h.snap().Generation()
	s.audible(occ[1])
	s.audible(occ[2])
	s.audible(occ[3])
	sn := h.waitSnap("d audible", func(sn *Snapshot) bool { a, _ := sn.Audible(); return a.Occurrence == occ[3] })
	if sn.Selected() != ids[3] {
		t.Fatalf("selected %d, want d", sn.Selected())
	}
	if got := sn.Generation().n - before.n; got != 3 {
		t.Fatalf("three boundaries moved the generation %d times, want once each", got)
	}
}

// TestRepeatWithOneEntryGivesTwoOccurrences pins one entry following
// itself: two hand-offs, two boundaries.
func TestRepeatWithOneEntryGivesTwoOccurrences(t *testing.T) {
	h := startHarness(t)
	ids := h.insert("a")
	h.must("repeat", h.eng.SetRepeat(bg, true))
	h.available()
	h.must("play", h.eng.Play(bg))
	s := h.be.nth(1)
	s.audible(s.first.ID)
	second := s.prepared(s.first.ID, 1)
	if second == nil || second.Entry != ids[0] || second.ID == s.first.ID {
		t.Fatalf("the successor of a with repeat on is %+v", second)
	}
	s.submitted(second.ID)
	third := s.prepared(second.ID, 1)
	if third == nil || third.ID == second.ID || third.ID == s.first.ID {
		t.Fatalf("the next hand-off is %+v", third)
	}
	s.audible(second.ID)
	sn := h.waitSnap("the second occurrence audible", func(sn *Snapshot) bool { a, _ := sn.Audible(); return a.Occurrence == second.ID })
	if sn.Selected() != ids[0] {
		t.Fatalf("selected %d", sn.Selected())
	}
}

// TestTheEndOfTheQueueWithRepeatOn pins the first entry following the last.
func TestTheEndOfTheQueueWithRepeatOn(t *testing.T) {
	h := startHarness(t)
	h.must("repeat", h.eng.SetRepeat(bg, true))
	ids, s := h.playing(1, "a", "b")
	next := s.prepared(s.first.ID, 1)
	if next == nil || next.Entry != ids[0] {
		t.Fatalf("the successor of the last entry with repeat on is %+v, want the first", next)
	}
	s.submitted(next.ID)
	s.audible(next.ID)
	h.waitSnap("a selected", func(sn *Snapshot) bool { return sn.Selected() == ids[0] })
}

// TestDeletingTheSelection pins a deleted selection while playing, paused
// and stopped, and what a play command waiting for the deleted entry
// answers, behind a held release and while its session starts, with an
// entry after it and with none: ReasonSuperseded, naming the deletion.
func TestDeletingTheSelection(t *testing.T) {
	t.Run("while playing plays the next", func(t *testing.T) {
		h := startHarness(t)
		ids, s := h.playing(1, "a", "b", "c")
		h.must("delete b", h.eng.Delete(bg, ids[1]))
		if sel := h.snap().Selected(); sel != ids[2] {
			t.Fatalf("selected %d right after the delete, want c", sel)
		}
		if s2 := h.be.nth(2); s2.first.Entry != ids[2] {
			t.Fatalf("started %d, want c", s2.first.Entry)
		}
		if !s.isAcked() {
			t.Fatal("the deleted entry's session was not released")
		}
	})
	t.Run("the last while playing stops and selects nothing", func(t *testing.T) {
		h := startHarness(t)
		ids, _ := h.playing(2, "a", "b", "c")
		h.must("delete c", h.eng.Delete(bg, ids[2]))
		sn := h.waitSnap("stopped", func(sn *Snapshot) bool { return sn.State() == Stopped })
		if sn.Selected() != 0 || h.be.starts() != 1 {
			t.Fatalf("selected %d, %d sessions", sn.Selected(), h.be.starts())
		}
	})
	t.Run("while paused selects the next and stays paused", func(t *testing.T) {
		h := startHarness(t)
		ids, _ := h.playing(1, "a", "b", "c")
		h.must("pause", h.eng.Pause(bg))
		h.must("delete b", h.eng.Delete(bg, ids[1]))
		sn := h.waitSnap("paused at c", func(sn *Snapshot) bool { return sn.State() == Paused && sn.Selected() == ids[2] })
		if _, ok := sn.Audible(); ok || h.be.starts() != 1 {
			t.Fatal("the paused content was not dropped, or a session started")
		}
		h.must("play", h.eng.Play(bg))
		if s2 := h.be.nth(2); s2.first.Entry != ids[2] {
			t.Fatalf("play started %d, want c", s2.first.Entry)
		}
	})
	t.Run("while stopped selects the next, or nothing", func(t *testing.T) {
		h := startHarness(t)
		ids := h.insert("a", "b")
		h.must("delete a", h.eng.Delete(bg, ids[0]))
		if sel := h.snap().Selected(); sel != ids[1] {
			t.Fatalf("selected %d, want b", sel)
		}
		h.must("delete b", h.eng.Delete(bg, ids[1]))
		if sel := h.snap().Selected(); sel != 0 {
			t.Fatalf("selected %d, want none", sel)
		}
	})
	// The entry a waiting play command is to play, with an entry after it:
	// the transport advances to that entry as for any deleted selection,
	// and the play command answers ReasonSuperseded, the playback it asked
	// for replaced before it began.
	superseded := func(t *testing.T, h *harness, ids []uint32, play <-chan error) {
		t.Helper()
		h.refused("the play id whose entry was deleted", h.result("play id", play), ReasonSuperseded)
		if s2 := h.be.nth(2); s2.first.Entry != ids[2] {
			t.Fatalf("started %d after the deletion, want c", s2.first.Entry)
		}
		if sn := h.waitSnap("playing c", func(sn *Snapshot) bool { return sn.State() == Playing }); sn.Selected() != ids[2] {
			t.Fatalf("selected %d, want c", sn.Selected())
		}
	}
	t.Run("a pending play's entry, while its release is held", func(t *testing.T) {
		h := startHarness(t)
		ids, _ := h.playing(0, "a", "b", "c")
		g := holdReleases(h)
		mark := h.appliedMark()
		play := async(func() error { return h.eng.PlayID(bg, ids[1]) })
		h.waitApplied("play id", mark)
		h.must("delete b", h.eng.Delete(bg, ids[1]))
		g.open()
		superseded(t, h, ids, play)
	})
	t.Run("a play's entry, while its session starts", func(t *testing.T) {
		h := startHarness(t)
		ids := h.insert("a", "b", "c")
		h.available()
		held := newGate()
		h.be.script(func() {
			h.be.onStart = func(ctx context.Context, s *fakeSession) error {
				if s.id == 1 {
					if err := held.wait(ctx, h.be, false); err != nil {
						return err
					}
				}
				return s.open()
			}
		})
		play := async(func() error { return h.eng.PlayID(bg, ids[1]) })
		h.be.nth(1)
		h.must("delete b", h.eng.Delete(bg, ids[1]))
		held.open()
		superseded(t, h, ids, play)
	})
	// The same with nothing after the deleted entry: playback stops and
	// nothing is selected, and the play command answers ReasonSuperseded
	// too, its entry deleted before it played. The answer names the
	// deletion: the stop that follows records no reason over it. The
	// transport is read as the answer comes, which is after the step's
	// snapshot.
	supersededAndStopped := func(t *testing.T, h *harness, play <-chan error) {
		t.Helper()
		err := h.result("play id", play)
		h.refused("the play id whose entry, the last, was deleted", err, ReasonSuperseded)
		if !strings.Contains(err.Error(), "the entry to play was deleted") {
			t.Fatalf("the play id answered %q, want the deletion's reason, not that of the stop after it", err)
		}
		if sn := h.snap(); sn.State() != Stopped || sn.Selected() != 0 || h.be.starts() != 1 {
			t.Fatalf("%s with %d selected after %d sessions, want stopped with none after one", sn.State(), sn.Selected(), h.be.starts())
		}
	}
	t.Run("a pending play's entry, the last, while its release is held", func(t *testing.T) {
		h := startHarness(t)
		ids, _ := h.playing(0, "a", "b")
		g := holdReleases(h)
		mark := h.appliedMark()
		play := async(func() error { return h.eng.PlayID(bg, ids[1]) })
		h.waitApplied("play id", mark)
		h.must("delete b", h.eng.Delete(bg, ids[1]))
		g.open()
		supersededAndStopped(t, h, play)
	})
	t.Run("a play's entry, the last, while its session starts", func(t *testing.T) {
		h := startHarness(t)
		ids := h.insert("a", "b")
		h.available()
		held := holdStart(h, 1)
		play := async(func() error { return h.eng.PlayID(bg, ids[1]) })
		h.be.nth(1)
		h.must("delete b", h.eng.Delete(bg, ids[1]))
		held.open()
		supersededAndStopped(t, h, play)
	})
}

// TestDeleteAllStopsAndSelectsNothing pins DeleteAll while playing.
func TestDeleteAllStopsAndSelectsNothing(t *testing.T) {
	h := startHarness(t)
	_, s := h.playing(0, "a", "b")
	h.must("delete all", h.eng.DeleteAll(bg))
	if sn := h.snap(); sn.Len() != 0 || sn.Selected() != 0 {
		t.Fatalf("after DeleteAll: %d entries, selected %d", sn.Len(), sn.Selected())
	}
	h.waitSnap("stopped", func(sn *Snapshot) bool { return sn.State() == Stopped })
	if !s.isAcked() {
		t.Fatal("DeleteAll did not release the session")
	}
}

// TestAPlayReplacedBeforePlaybackSaysSo pins what a Play answers when a
// later command replaces its playback before it begins, as a pending intent,
// while its session is still starting, or while its Resume is under way:
// why (ReasonSuperseded, or the output's reason), never nil with the
// transport stopped.
func TestAPlayReplacedBeforePlaybackSaysSo(t *testing.T) {
	deleteAll := func(h *harness) error { return h.eng.DeleteAll(bg) }
	stop := func(h *harness) error { return h.eng.Stop(bg) }
	gone := func(h *harness) error {
		h.outGen++
		return h.eng.ReportOutput(bg, OutputReport{Generation: h.outGen, Available: false, Reason: "device_missing"})
	}
	for _, tc := range []struct {
		name string
		// from is where the Play waits: "pending" behind a held release,
		// "starting" in its held Start, "resuming" in its held Resume.
		from    string
		replace func(h *harness) error
		op      string
		want    Reason
	}{
		{"a pending play, by delete all", "pending", deleteAll, "delete all", ReasonSuperseded},
		{"a pending play, by stop", "pending", stop, "stop", ReasonSuperseded},
		{"a starting play, by delete all", "starting", deleteAll, "delete all", ReasonSuperseded},
		{"a starting play, by stop", "starting", stop, "stop", ReasonSuperseded},
		{"a resuming play, by stop", "resuming", stop, "stop", ReasonSuperseded},
		{"a resuming play, by the output going away", "resuming", gone, "report output", ReasonOutputUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := startHarness(t)
			held := newGate()
			var play, stopped <-chan error
			switch tc.from {
			case "starting":
				h.insert("a", "b")
				h.available()
				h.be.script(func() {
					h.be.onStart = func(ctx context.Context, s *fakeSession) error {
						if err := held.wait(ctx, h.be, false); err != nil {
							return err
						}
						return s.open()
					}
				})
				play = async(func() error { return h.eng.Play(bg) })
				h.be.nth(1)
			case "pending":
				h.playing(0, "a", "b")
				held = holdReleases(h)
				stopped = async(func() error { return h.eng.Stop(bg) })
				h.waitSnap("releasing", func(sn *Snapshot) bool { return sn.State() == Releasing })
				play = async(func() error { return h.eng.Play(bg) })
				h.waitSnap("play pending", func(sn *Snapshot) bool { return sn.Pending() == IntentPlay })
			case "resuming":
				h.playing(0, "a", "b")
				h.must("pause", h.eng.Pause(bg))
				h.be.script(func() {
					h.be.onResume = func(ctx context.Context, _ *fakeSession) (ResumeResult, error) {
						if err := held.wait(ctx, h.be, false); err != nil {
							return 0, err
						}
						return ResumeContinued, nil
					}
				})
				play = async(func() error { return h.eng.Play(bg) })
				h.be.waitFor("the resume", func() bool { return h.be.countLocked("resume") == 1 })
			}
			mark := h.appliedMark()
			replaced := async(func() error { return tc.replace(h) })
			h.waitApplied(tc.op, mark)
			held.open()
			h.must("the replacing command", h.result("the replacing command", replaced))
			h.refused("the replaced play", h.result("play", play), tc.want)
			if stopped != nil {
				h.must("stop", h.result("stop", stopped))
			}
			if sn := h.waitSnap("stopped", func(sn *Snapshot) bool { return sn.State() == Stopped }); h.be.starts() != 1 {
				t.Fatalf("%s with %d sessions", sn.State(), h.be.starts())
			}
		})
	}
}

// TestAPlaysAnswerIsItsOwn pins what decides a Play's answer: what
// happened to the playback it asked for, recorded as it happened, never
// what a later command did. During a held release a Play, a Stop and a
// Play: the Stop replaces the first Play's playback, so that Play answers
// ReasonSuperseded although the second plays. And a Play whose start the
// backend refused answers why, although a Play sent during the release
// that followed plays.
func TestAPlaysAnswerIsItsOwn(t *testing.T) {
	t.Run("play, stop, play", func(t *testing.T) {
		h := startHarness(t)
		h.playing(0, "a", "b")
		g := holdReleases(h)
		mark := h.appliedMark()
		stopped := async(func() error { return h.eng.Stop(bg) })
		h.waitApplied("stop", mark)
		mark = h.appliedMark()
		first := async(func() error { return h.eng.Play(bg) })
		h.waitApplied("play", mark)
		mark = h.appliedMark()
		again := async(func() error { return h.eng.Stop(bg) })
		h.waitApplied("stop", mark)
		mark = h.appliedMark()
		second := async(func() error { return h.eng.Play(bg) })
		h.waitApplied("play", mark)
		g.open()
		h.refused("the play the stop replaced", h.result("the first play", first), ReasonSuperseded)
		h.must("the play after the stop", h.result("the second play", second))
		h.must("the stop that began the release", h.result("the first stop", stopped))
		h.must("the stop between the plays", h.result("the second stop", again))
		if sn := h.snap(); sn.State() != Playing || h.be.starts() != 2 {
			t.Fatalf("%s with %d sessions, want the second play's session playing", sn.State(), h.be.starts())
		}
	})
	t.Run("play, stop, then maintenance", func(t *testing.T) {
		h := startHarness(t)
		h.playing(0, "a", "b")
		g := holdReleases(h)
		mark := h.appliedMark()
		stopped := async(func() error { return h.eng.Stop(bg) })
		h.waitApplied("stop", mark)
		mark = h.appliedMark()
		first := async(func() error { return h.eng.Play(bg) })
		h.waitApplied("play", mark)
		mark = h.appliedMark()
		again := async(func() error { return h.eng.Stop(bg) })
		h.waitApplied("stop", mark)
		enter := async(func() error {
			_, err := h.eng.EnterMaintenance(bg, h.snap().Generation(), "the test")
			return err
		})
		h.refused("the play the stop replaced, answered as maintenance began", h.result("the first play", first), ReasonSuperseded)
		g.open()
		h.must("enter", h.result("enter", enter))
		h.must("the stop that began the release", h.result("the first stop", stopped))
		h.must("the stop after the play", h.result("the second stop", again))
	})
	t.Run("a refused start, then a play", func(t *testing.T) {
		h := startHarness(t)
		h.insert("a", "b")
		h.available()
		busy := errors.New("the device is busy")
		h.be.script(func() {
			h.be.onStart = func(ctx context.Context, s *fakeSession) error {
				if s.id == 1 {
					return busy
				}
				return s.open()
			}
		})
		g := holdReleases(h)
		mark := h.appliedMark()
		first := async(func() error { return h.eng.Play(bg) })
		h.waitApplied("handled call start", mark)
		mark = h.appliedMark()
		second := async(func() error { return h.eng.Play(bg) })
		h.waitApplied("play", mark)
		g.open()
		if err := h.result("the first play", first); ReasonOf(err) != ReasonBackend || !errors.Is(err, busy) {
			t.Fatalf("the play whose start was refused: %v, want %s with the backend's error", err, ReasonBackend)
		}
		h.must("the play after the refusal", h.result("the second play", second))
		if sn := h.snap(); sn.State() != Playing || h.be.starts() != 2 {
			t.Fatalf("%s with %d sessions, want the second play's session playing", sn.State(), h.be.starts())
		}
	})
}

// TestAPlayWhosePlaybackBeganAnswersAsPlayed pins a play command whose
// playback began while it waited: the session started for it was heard
// (its first Audible) before Start answered, so the command still waited,
// and then something stopped or replaced that playback, still before Start
// answered. Its playback happened, so it answers nil, never the reason of
// what came after: the deletion of its entry, whether the next entry plays
// or nothing follows; a Stop; the output going away; a fault; maintenance.
// A reason recorded for a waiting command before its session was heard
// stands: during a held release a Play, a Stop and a Play, the second
// Play's session then heard while it starts, answer the first
// ReasonSuperseded and the second nil.
func TestAPlayWhosePlaybackBeganAnswersAsPlayed(t *testing.T) {
	stopped := func(why Reason) func(h *harness, ids []uint32) {
		return func(h *harness, ids []uint32) {
			h.t.Helper()
			sn := h.waitSnap("stopped", func(sn *Snapshot) bool { return sn.State() == Stopped })
			if sn.LastStop().Reason != why || h.be.starts() != 1 {
				h.t.Fatalf("stopped with %+v after %d sessions, want %s after one", sn.LastStop(), h.be.starts(), why)
			}
		}
	}
	for _, tc := range []struct {
		name string
		refs []string
		// play is the index of the entry PlayID plays; then is what comes
		// once its session was heard, before Start answers, with the answer of
		// a command that waits on the release, if one does; check is what the
		// transport did.
		play  int
		then  func(h *harness, ids []uint32, s *fakeSession) <-chan error
		check func(h *harness, ids []uint32)
	}{
		{"its entry deleted, the next plays", []string{"a", "b", "c"}, 1,
			func(h *harness, ids []uint32, _ *fakeSession) <-chan error {
				h.must("delete b", h.eng.Delete(bg, ids[1]))
				return nil
			},
			func(h *harness, ids []uint32) {
				if s2 := h.be.nth(2); s2.first.Entry != ids[2] {
					h.t.Fatalf("started %d after the deletion, want c", s2.first.Entry)
				}
				if sn := h.waitSnap("playing c", func(sn *Snapshot) bool { return sn.State() == Playing }); sn.Selected() != ids[2] {
					h.t.Fatalf("selected %d, want c", sn.Selected())
				}
			}},
		{"its entry deleted, nothing follows", []string{"a", "b"}, 1,
			func(h *harness, ids []uint32, _ *fakeSession) <-chan error {
				h.must("delete b", h.eng.Delete(bg, ids[1]))
				return nil
			},
			func(h *harness, ids []uint32) {
				stopped(ReasonStopped)(h, ids)
				if sel := h.snap().Selected(); sel != 0 {
					h.t.Fatalf("selected %d, want none", sel)
				}
			}},
		{"a stop", []string{"a", "b"}, 0,
			func(h *harness, _ []uint32, _ *fakeSession) <-chan error {
				mark := h.appliedMark()
				stop := async(func() error { return h.eng.Stop(bg) })
				h.waitApplied("stop", mark)
				return stop
			},
			stopped(ReasonStopped)},
		{"the output going away", []string{"a", "b"}, 0,
			func(h *harness, _ []uint32, _ *fakeSession) <-chan error {
				h.report(OutputReport{Available: false, Reason: "device_missing"})
				return nil
			},
			stopped(ReasonOutputUnavailable)},
		{"a fault", []string{"a", "b"}, 0,
			func(h *harness, _ []uint32, s *fakeSession) <-chan error {
				mark := h.appliedMark()
				s.fault(Fault{Reason: ReasonXrun, Occurrence: s.first.ID})
				h.waitApplied("handled fault", mark)
				return nil
			},
			stopped(ReasonFault)},
		{"maintenance", []string{"a", "b"}, 0,
			func(h *harness, _ []uint32, _ *fakeSession) <-chan error {
				mark := h.appliedMark()
				enter := async(func() error {
					_, err := h.eng.EnterMaintenance(bg, h.snap().Generation(), "the test")
					return err
				})
				h.waitApplied("enter maintenance", mark)
				return enter
			},
			func(h *harness, _ []uint32) {
				if st := h.snap().State(); st != Maintenance {
					h.t.Fatalf("%s after the enter, want maintenance", st)
				}
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := startHarness(t)
			ids := h.insert(tc.refs...)
			h.available()
			held := holdStart(h, 1)
			play := async(func() error { return h.eng.PlayID(bg, ids[tc.play]) })
			s := h.be.nth(1)
			heard(h, s)
			after := tc.then(h, ids, s)
			held.open()
			h.must("the play whose session was heard", h.result("play id", play))
			if after != nil {
				h.must("what came after", h.result("what came after", after))
			}
			tc.check(h, ids)
		})
	}
	t.Run("a reason recorded before it was heard stands", func(t *testing.T) {
		h := startHarness(t)
		h.playing(0, "a", "b")
		g := holdReleases(h)
		held := holdStart(h, 2)
		mark := h.appliedMark()
		stop := async(func() error { return h.eng.Stop(bg) })
		h.waitApplied("stop", mark)
		mark = h.appliedMark()
		first := async(func() error { return h.eng.Play(bg) })
		h.waitApplied("play", mark)
		mark = h.appliedMark()
		again := async(func() error { return h.eng.Stop(bg) })
		h.waitApplied("stop", mark)
		mark = h.appliedMark()
		second := async(func() error { return h.eng.Play(bg) })
		h.waitApplied("play", mark)
		g.open()
		heard(h, h.be.nth(2))
		held.open()
		h.refused("the play the stop replaced", h.result("the first play", first), ReasonSuperseded)
		h.must("the play whose session was heard", h.result("the second play", second))
		h.must("the stop that began the release", h.result("the first stop", stop))
		h.must("the stop between the plays", h.result("the second stop", again))
	})
}

// TestAnAudibleFromAReleasingSessionDecidesNoPlayCommand pins whose
// Audible decides a waiting play command's playback: only that of the
// session started for it. A session released (here to play another entry)
// is no longer current, and onAudible drops what it reports, so a PlayID
// waiting behind that release is decided only by the session that follows,
// and answers why that one did not play: a PlayID that replaced a Play whose
// Start is held, the first session then heard, answers the second session's
// refused Start, and the Play ReasonSuperseded, its own session replaced; a
// PlayID behind a held release, the releasing session's successor then
// heard, answers the output going away. With the second session played,
// TestAPlayWhosePlaybackAnotherCommandReplacedIsSuperseded.
func TestAnAudibleFromAReleasingSessionDecidesNoPlayCommand(t *testing.T) {
	t.Run("a PlayID that replaced a starting Play, then the first session heard", func(t *testing.T) {
		h := startHarness(t)
		ids := h.insert("a", "x")
		h.available()
		held := newGate()
		busy := errors.New("the device is busy")
		h.be.script(func() {
			h.be.onStart = func(ctx context.Context, s *fakeSession) error {
				switch s.id {
				case 1:
					if err := held.wait(ctx, h.be, false); err != nil {
						return err
					}
				case 2:
					return busy
				}
				return s.open()
			}
		})
		play := async(func() error { return h.eng.Play(bg) })
		s1 := h.be.nth(1)
		mark := h.appliedMark()
		playID := async(func() error { return h.eng.PlayID(bg, ids[1]) })
		h.waitApplied("play id", mark)
		mark = h.appliedMark()
		s1.audible(s1.first.ID)
		h.waitApplied("handled audible", mark)
		held.open()
		if err := h.result("play id", playID); ReasonOf(err) != ReasonBackend || !errors.Is(err, busy) {
			t.Fatalf("the PlayID whose session's Start was refused answered %v, want %s with the backend's error", err, ReasonBackend)
		}
		h.refused("the Play whose session the PlayID replaced before it was heard", h.result("play", play), ReasonSuperseded)
		if sn := h.waitSnap("stopped", func(sn *Snapshot) bool { return sn.State() == Stopped }); h.be.starts() != 2 {
			t.Fatalf("%s after %d sessions, want stopped after two", sn.State(), h.be.starts())
		}
	})
	t.Run("a PlayID behind a held release, then the releasing session's successor heard", func(t *testing.T) {
		h := startHarness(t)
		ids, s := h.playing(0, "a", "b", "c")
		b := s.prepared(s.first.ID, 1)
		if b == nil || b.Entry != ids[1] {
			t.Fatalf("the successor of a is %+v, want b", b)
		}
		s.submitted(b.ID)
		g := holdReleases(h)
		mark := h.appliedMark()
		playID := async(func() error { return h.eng.PlayID(bg, ids[2]) })
		h.waitApplied("play id", mark)
		mark = h.appliedMark()
		s.audible(b.ID)
		h.waitApplied("handled audible", mark)
		if sel := h.snap().Selected(); sel != ids[2] {
			t.Fatalf("selected %d after the releasing session's Audible, want c, the PlayID's", sel)
		}
		h.report(OutputReport{Available: false, Reason: "device_missing"})
		g.open()
		h.refused("the PlayID behind the release", h.result("play id", playID), ReasonOutputUnavailable)
	})
}

// TestAPlayWhosePlaybackAnotherCommandReplacedIsSuperseded pins a play
// command whose session a later command replaced before it was heard: a
// PlayID sent while the Play's session starts releases that session to play
// another entry, so the Play's playback never happens, and the Play answers
// ReasonSuperseded, naming the PlayID, however the PlayID's own session goes:
// heard before its Start returns (Codex's sequence on round 6, where that
// Audible answered both commands nil), or started and not yet heard, where
// the settle answered the Play nil. The PlayID answers nil and its entry
// plays. So it goes for a Play whose first entry was refused at its start,
// carried on to the session after the skip's release, when a PlayID comes
// during that release (the Claude API on round 7: carried into the commands
// that share that session, the Play answered nil once the PlayID's entry was
// heard). A Play whose Resume is under way, replaced by a Next, answers
// ReasonSuperseded too, naming the Next: the paused content never played on.
func TestAPlayWhosePlaybackAnotherCommandReplacedIsSuperseded(t *testing.T) {
	for _, heardFirst := range []bool{true, false} {
		name := "the PlayID's session heard before its Start returns"
		if !heardFirst {
			name = "the PlayID's session started and not yet heard"
		}
		t.Run(name, func(t *testing.T) {
			h := startHarness(t)
			ids := h.insert("a", "x")
			h.available()
			first, second := newGate(), newGate()
			h.be.script(func() {
				h.be.onStart = func(ctx context.Context, s *fakeSession) error {
					g := first
					if s.id == 2 {
						g = second
					}
					if err := g.wait(ctx, h.be, false); err != nil {
						return err
					}
					return s.open()
				}
			})
			play := async(func() error { return h.eng.Play(bg) })
			h.be.nth(1)
			mark := h.appliedMark()
			playID := async(func() error { return h.eng.PlayID(bg, ids[1]) })
			h.waitApplied("play id", mark)
			first.open()
			s2 := h.be.nth(2)
			if s2.first.Entry != ids[1] {
				t.Fatalf("the second session started %d, want x, the PlayID's", s2.first.Entry)
			}
			if heardFirst {
				heard(h, s2)
			}
			second.open()
			h.must("the PlayID whose session played", h.result("play id", playID))
			err := h.result("play", play)
			h.refused("the Play whose session the PlayID replaced", err, ReasonSuperseded)
			if !strings.Contains(err.Error(), "play id") {
				t.Fatalf("the Play answered %q, want it to name the PlayID that replaced its playback", err)
			}
			sn := h.waitSnap("x playing", func(sn *Snapshot) bool { return sn.State() == Playing && sn.Selected() == ids[1] })
			if h.be.starts() != 2 {
				t.Fatalf("%s at %d after %d sessions, want x playing in the second", sn.State(), sn.Selected(), h.be.starts())
			}
		})
	}
	t.Run("a Play whose first entry was refused, replaced by a PlayID while the skip releases", func(t *testing.T) {
		h := startHarness(t)
		ids := h.insert("a", "b", "x")
		unplayable(h, "a")
		h.available()
		g := holdReleases(h)
		play := async(func() error { return h.eng.Play(bg) })
		// Session 1 starts at a, which is refused: the skip selects b and
		// releases session 1 to play it, the Play carried on to the session
		// after the release.
		h.waitSnap("the skip releasing session 1", func(sn *Snapshot) bool {
			return sn.State() == Releasing && sn.Pending() == IntentPlay && sn.Selected() == ids[1]
		})
		mark := h.appliedMark()
		playID := async(func() error { return h.eng.PlayID(bg, ids[2]) })
		h.waitApplied("play id", mark)
		g.open()
		s2 := h.be.nth(2)
		if s2.first.Entry != ids[2] {
			t.Fatalf("the second session started %d, want x, the PlayID's", s2.first.Entry)
		}
		heard(h, s2)
		h.must("the PlayID whose session was heard", h.result("play id", playID))
		err := h.result("play", play)
		h.refused("the Play whose skip the PlayID replaced", err, ReasonSuperseded)
		if !strings.Contains(err.Error(), "play id") {
			t.Fatalf("the Play answered %q, want it to name the PlayID that replaced its playback", err)
		}
	})
	t.Run("a resuming Play, replaced by a Next", func(t *testing.T) {
		h := startHarness(t)
		ids, _ := h.playing(0, "a", "b")
		h.must("pause", h.eng.Pause(bg))
		held := newGate()
		h.be.script(func() {
			h.be.onResume = func(ctx context.Context, _ *fakeSession) (ResumeResult, error) {
				if err := held.wait(ctx, h.be, false); err != nil {
					return 0, err
				}
				return ResumeContinued, nil
			}
		})
		play := async(func() error { return h.eng.Play(bg) })
		h.be.waitFor("the resume", func() bool { return h.be.countLocked("resume") == 1 })
		mark := h.appliedMark()
		next := async(func() error { return h.eng.Next(bg) })
		h.waitApplied("next", mark)
		held.open()
		h.must("the Next", h.result("next", next))
		err := h.result("play", play)
		h.refused("the resuming Play the Next replaced", err, ReasonSuperseded)
		if !strings.Contains(err.Error(), "next") {
			t.Fatalf("the Play answered %q, want it to name the Next that replaced its playback", err)
		}
		if s2 := h.be.nth(2); s2.first.Entry != ids[1] {
			t.Fatalf("the Next's session started %d, want b", s2.first.Entry)
		}
	})
}

// TestAPlaySentWhileASkipReleasesSharesTheSessionAfterIt pins the other side
// of a carried play command (carry): a command sent while the skip past an
// entry refused at its start releases the first session, that leaves the
// pending target where the skip put it, replaces nothing, and it and the
// carried Play share the session after the release, both answering nil once
// that session is heard: a Play, which moves no target, and a PlayID or a
// PlayIndex naming the entry the skip moved on to (Codex on round 8: round 8
// superseded the carried Play for any PlayID or PlayIndex, though the same
// session then played it). Only a command that moves the target supersedes
// the carried Play (TestAPlayWhosePlaybackAnotherCommandReplacedIsSuperseded).
func TestAPlaySentWhileASkipReleasesSharesTheSessionAfterIt(t *testing.T) {
	for _, tc := range []struct {
		name, op string
		send     func(h *harness, ids []uint32) error
	}{
		{"a Play", "play", func(h *harness, _ []uint32) error { return h.eng.Play(bg) }},
		{"a PlayID of the entry the skip moved on to", "play id", func(h *harness, ids []uint32) error { return h.eng.PlayID(bg, ids[1]) }},
		{"a PlayIndex of the entry the skip moved on to", "play index", func(h *harness, _ []uint32) error { return h.eng.PlayIndex(bg, 1) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := startHarness(t)
			ids := h.insert("a", "b")
			unplayable(h, "a")
			h.available()
			g := holdReleases(h)
			first := async(func() error { return h.eng.Play(bg) })
			h.waitSnap("the skip releasing session 1", func(sn *Snapshot) bool {
				return sn.State() == Releasing && sn.Pending() == IntentPlay && sn.Selected() == ids[1]
			})
			mark := h.appliedMark()
			second := async(func() error { return tc.send(h, ids) })
			h.waitApplied(tc.op, mark)
			g.open()
			s2 := h.be.nth(2)
			if s2.first.Entry != ids[1] {
				t.Fatalf("the second session started %d, want b, where the skip moved on to", s2.first.Entry)
			}
			heard(h, s2)
			h.must("the carried Play", h.result("the first play", first))
			h.must("the command sent during the skip", h.result(tc.op, second))
		})
	}
}

// TestTheSelectionMovesAtOnceOnACommand pins the selection a command, a
// deletion and a skip set while playing: the new target as soon as the
// owner has applied it, with nothing audible, before any backend event.
func TestTheSelectionMovesAtOnceOnACommand(t *testing.T) {
	for _, tc := range []struct {
		name   string
		run    func(h *harness, ids []uint32) error
		target int
	}{
		{"next", func(h *harness, ids []uint32) error { return h.eng.Next(bg) }, 1},
		{"play id", func(h *harness, ids []uint32) error { return h.eng.PlayID(bg, ids[2]) }, 2},
		{"delete", func(h *harness, ids []uint32) error { return h.eng.Delete(bg, ids[0]) }, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := startHarness(t)
			ids, _ := h.playing(0, "a", "b", "c")
			g := holdReleases(h)
			applied := h.appliedMark()
			done := async(func() error { return tc.run(h, ids) })
			h.waitApplied(map[string]string{"next": "next", "play id": "play id", "delete": "delete"}[tc.name], applied)
			sn := h.snap()
			if sn.Selected() != ids[tc.target] {
				t.Fatalf("selected %d right after %s, want %d", sn.Selected(), tc.name, ids[tc.target])
			}
			if _, ok := sn.Audible(); ok {
				t.Fatal("an occurrence is audible before the new session reported")
			}
			g.open()
			h.must(tc.name, h.result(tc.name, done))
		})
	}
	t.Run("skip", func(t *testing.T) {
		h := startHarness(t)
		ids, _ := h.playing(0, "a", "u", "c")
		h.be.script(func() { h.be.unplayable["u"] = Unplayable{Reason: ReasonMissing} })
		g := newGate()
		releases := 0
		h.be.script(func() {
			h.be.onRelease = func(ctx context.Context, s *fakeSession) error {
				h.be.mu.Lock()
				releases++
				first := releases == 1
				h.be.mu.Unlock()
				if !first {
					if err := g.wait(ctx, h.be, false); err != nil {
						return err
					}
				}
				<-s.startDone
				s.ack()
				return nil
			}
		})
		done := async(func() error { return h.eng.PlayID(bg, ids[1]) })
		sn := h.waitSnap("the skip", func(sn *Snapshot) bool { return sn.Selected() == ids[2] })
		if sn.State() != Releasing || h.be.starts() != 2 {
			t.Fatalf("%s with %d sessions: the skip's selection came after the next session", sn.State(), h.be.starts())
		}
		if _, ok := sn.Audible(); ok {
			t.Fatal("an occurrence is audible during the skip")
		}
		g.open()
		h.must("play id", h.result("play id", done))
		if s := h.be.nth(3); s.first.Entry != ids[2] {
			t.Fatalf("after the skip the session started %d, want c", s.first.Entry)
		}
	})
}

// TestTheContractCheckWaitsForAStartStillOnItsWay pins the harness's teardown
// against the fake's contract check (B576). A Delete of the entry playing
// answers in its own step, before the session that plays the entry after it
// has started: an edit answers with its reply and never waits on the
// transport (TestDeletingTheSelection deletes while a Start or a Release is
// held). So a test can end while that session's Start is still on its
// goroutine, short of the backend, and the shutdown's Release of the session
// gets there first, as Backend's contract allows. The Start is held here
// until the harness begins to wait for the engine's goroutines, the latest a
// scheduler could leave it, and the check that every session the backend saw
// had its Start must count it. A check that read the sessions before those
// goroutines were joined reported "session 2 had calls and never a Start"
// (TestTheSelectionMovesAtOnceOnACommand/delete, once in CI under -race).
func TestTheContractCheckWaitsForAStartStillOnItsWay(t *testing.T) {
	h := newHarness(t)
	held := newGate()
	h.joining = func() {
		select {
		case <-held:
		default:
			held.open()
		}
	}
	h.boot()
	ids, _ := h.playing(0, "a", "b", "c")
	g := holdReleases(h)
	arrived := false
	h.be.script(func() {
		h.be.beforeStart = func(req StartRequest) {
			if req.Session != 2 {
				return
			}
			h.be.mu.Lock()
			arrived = true
			h.be.signal()
			h.be.mu.Unlock()
			<-held
		}
	})
	applied := h.appliedMark()
	done := async(func() error { return h.eng.Delete(bg, ids[0]) })
	h.waitApplied("delete", applied)
	g.open()
	h.must("delete", h.result("delete", done))
	h.be.waitFor("session 2's Start on its way", func() bool { return arrived })
	// The teardown's shutdown, begun here so its Release of session 2 is at
	// the backend before the test ends: session 2's Start is not.
	h.cancel()
	h.be.waitFor("the shutdown's release of session 2", func() bool {
		for _, c := range h.be.calls {
			if c.kind == "release" && c.session == 2 {
				return true
			}
		}
		return false
	})
}

// holdStart makes session n's Start wait until the returned gate opens;
// every other Start opens at once.
func holdStart(h *harness, n SessionID) gate {
	g := newGate()
	h.be.script(func() {
		h.be.onStart = func(ctx context.Context, s *fakeSession) error {
			if s.id == n {
				if err := g.wait(ctx, h.be, false); err != nil {
					return err
				}
			}
			return s.open()
		}
	})
	return g
}

// heard reports s's first occurrence audible and waits for the owner to
// apply it.
func heard(h *harness, s *fakeSession) {
	h.t.Helper()
	s.audible(s.first.ID)
	h.waitSnap("the first occurrence heard", func(sn *Snapshot) bool {
		a, ok := sn.Audible()
		return ok && a.Occurrence == s.first.ID
	})
}

// holdReleases makes every Release wait until the returned gate opens.
func holdReleases(h *harness) gate {
	g := newGate()
	h.be.script(func() {
		h.be.onRelease = func(ctx context.Context, s *fakeSession) error {
			if err := g.wait(ctx, h.be, false); err != nil {
				return err
			}
			<-s.startDone
			s.ack()
			return nil
		}
	})
	return g
}
