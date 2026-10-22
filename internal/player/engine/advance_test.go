package engine

import (
	"context"
	"errors"
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
// and stopped.
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
