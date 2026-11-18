package engine

import (
	"context"
	"slices"
	"sync"
	"testing"
)

// recordSteps has h's owner keep the snapshot each of its steps published,
// read at the step's notes (applied or handled), which come after the step's
// one publication, and returns what it kept so far, in order. It keeps the
// tweak h already has, and is called before h boots.
func recordSteps(h *harness) func() []*Snapshot {
	var mu sync.Mutex
	var kept []*Snapshot
	tweak := h.tweak
	h.tweak = func(c *Config) {
		if tweak != nil {
			tweak(c)
		}
		applied, handled := c.seams.applied, c.seams.handled
		keep := func() {
			sn := h.eng.Snapshot()
			mu.Lock()
			kept = append(kept, sn)
			mu.Unlock()
		}
		c.seams.applied = func(op string) { keep(); applied(op) }
		c.seams.handled = func(what string) { keep(); handled(what) }
	}
	return func() []*Snapshot {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(kept)
	}
}

// faultStep is the snapshot the step that recorded the n-th fault published.
func faultStep(t *testing.T, steps []*Snapshot, n int) *Snapshot {
	t.Helper()
	for _, sn := range steps {
		if len(sn.Faults()) == n {
			return sn
		}
	}
	t.Fatalf("no step published %d faults in %d steps", n, len(steps))
	return nil
}

// stoppedBy requires sn's last fault to be entry faulted's, its last stop the
// fault's and naming that entry, and its selection selected, each said apart.
func stoppedBy(t *testing.T, what string, sn *Snapshot, faulted, selected uint32) {
	t.Helper()
	if fs := sn.Faults(); len(fs) == 0 || fs[len(fs)-1].Entry != faulted {
		t.Errorf("%s: faults %+v, the last of entry %d wanted", what, fs, faulted)
	}
	if st := sn.LastStop(); st.Reason != ReasonFault || st.Entry != faulted {
		t.Errorf("%s: last stop %+v, the fault's, naming entry %d, wanted", what, st, faulted)
	}
	if sel := sn.Selected(); sel != selected {
		t.Errorf("%s: selected %d, %d wanted", what, sel, selected)
	}
}

// faultStops reports f on s, waits for the transport to stop, and requires
// the fault's own step and the stopped transport to say entry faulted
// faulted, its stop naming it, and the selection on selected (stoppedBy):
// the step publishes the selection, the stop and the fault together, and
// nothing after it moves them.
func faultStops(t *testing.T, h *harness, steps func() []*Snapshot, s *fakeSession, f Fault, faulted, selected uint32) {
	t.Helper()
	n := len(h.snap().Faults()) + 1
	s.fault(f)
	sn := h.waitSnap("stopped by the fault", func(sn *Snapshot) bool { return sn.State() == Stopped && len(sn.Faults()) == n })
	stoppedBy(t, "the fault's step", faultStep(t, steps(), n), faulted, selected)
	stoppedBy(t, "stopped", sn, faulted, selected)
}

// playsFrom requires a Play from the stopped transport to start a session at
// entry want, and returns that session.
func playsFrom(t *testing.T, h *harness, want uint32) *fakeSession {
	t.Helper()
	n := h.be.starts()
	h.must("play", h.eng.Play(bg))
	s := h.be.nth(n + 1)
	if s.first.Entry != want {
		t.Errorf("Play from stopped started entry %d, %d wanted", s.first.Entry, want)
	}
	return s
}

// holdCalls holds the prepare-th Prepare that names a successor (counted
// from 1) until the test ends, and every Cut while cut is set: a call under
// way, which keeps its occurrence in the chain. For prepare 0 it holds no
// Prepare and leaves each to the fake's own answer, which refuses a
// reference scripted unplayable.
func holdCalls(h *harness, prepare int, cut bool) {
	held := newGate()
	h.t.Cleanup(held.open)
	h.be.script(func() {
		if prepare > 0 {
			named := 0
			h.be.onPrepare = func(ctx context.Context, _ *fakeSession, _ OccurrenceID, next *Occurrence) error {
				h.be.mu.Lock()
				if next != nil {
					named++
				}
				hold := next != nil && named == prepare
				h.be.mu.Unlock()
				if hold {
					return held.wait(ctx, h.be, false)
				}
				return nil
			}
		}
		if cut {
			h.be.onCut = func(ctx context.Context, _ *fakeSession, _ OccurrenceID) (bool, error) {
				if err := held.wait(ctx, h.be, false); err != nil {
					return false, err
				}
				return true, nil
			}
		}
	})
}

// TestASuccessorsFaultBeforeItIsHeardMovesTheSelectionOn pins where a fault
// of a successor never heard leaves the selection (B903AUD): a heard, its
// successor b handed off, b faults, and a Play from the stopped transport
// starts at b, never at a, the track that played. Until then the selection
// stayed on a, which only b's audible report moves, so that Play played a
// again from its start. The production trace is a decode of b that fails
// before any of its frames reached the sink: the backend plays out a's
// content, then faults naming b, which was never heard. The fault is
// recorded against b, playback stops and nothing restarts, as for any
// fault, and its stop names b. The engine has no state between a's last
// frame and b's first, so the cases are b's stages: named (its Prepare
// under way, or answered), prepared, submitted (a Prepare or a Cut of what
// follows it under way), and the session paused. Each reads the snapshot the
// fault's own step published: the selection, the stop and the fault come out
// together.
func TestASuccessorsFaultBeforeItIsHeardMovesTheSelectionOn(t *testing.T) {
	for _, tc := range []struct {
		name string
		// hold sets the calls to hold before a plays.
		hold func(h *harness)
		// stage takes b, named after a heard, to the stage the case names.
		stage func(t *testing.T, h *harness, ids []uint32, s *fakeSession, b *Occurrence)
	}{
		{"named, its Prepare under way", func(h *harness) { holdCalls(h, 1, false) },
			func(*testing.T, *harness, []uint32, *fakeSession, *Occurrence) {}},
		{"named", nil, func(_ *testing.T, h *harness, _ []uint32, _ *fakeSession, _ *Occurrence) {
			h.waitAppliedTimes("handled call prepare", 0, 1)
		}},
		{"prepared", nil, func(_ *testing.T, h *harness, _ []uint32, s *fakeSession, b *Occurrence) {
			h.waitAppliedTimes("handled call prepare", 0, 1)
			s.preparedEvent(b.ID, Join{Gapless: true})
			h.waitAppliedTimes("handled prepared", 0, 1)
		}},
		{"submitted", nil, func(_ *testing.T, h *harness, _ []uint32, s *fakeSession, b *Occurrence) {
			s.submitted(b.ID)
			s.prepared(b.ID, 1)
			h.waitAppliedTimes("handled call prepare", 0, 2)
		}},
		{"submitted, a Prepare of what follows it under way", func(h *harness) { holdCalls(h, 2, false) },
			func(_ *testing.T, _ *harness, _ []uint32, s *fakeSession, b *Occurrence) {
				s.submitted(b.ID)
				s.prepared(b.ID, 1)
			}},
		{"submitted, a Cut of what follows it under way", func(h *harness) { holdCalls(h, 0, true) },
			func(t *testing.T, h *harness, ids []uint32, s *fakeSession, b *Occurrence) {
				s.submitted(b.ID)
				c := s.prepared(b.ID, 1)
				s.submitted(c.ID)
				s.prepared(c.ID, 1)
				h.waitAppliedTimes("handled call prepare", 0, 3)
				if _, err := h.eng.InsertAfter(bg, ids[1], ref("x"), nil); err != nil {
					t.Fatal(err)
				}
				h.be.waitFor("the cut of c", func() bool { return h.be.countLocked("cut") == 1 })
			}},
		{"the session paused", nil, func(_ *testing.T, h *harness, _ []uint32, _ *fakeSession, _ *Occurrence) {
			h.waitAppliedTimes("handled call prepare", 0, 1)
			h.must("pause", h.eng.Pause(bg))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			steps := recordSteps(h)
			h.boot()
			if tc.hold != nil {
				tc.hold(h)
			}
			ids, s := h.playing(0, "a", "b", "c")
			b := s.prepared(s.first.ID, 1)
			if b == nil || b.Entry != ids[1] {
				t.Fatalf("the successor named after a is %+v, b wanted", b)
			}
			tc.stage(t, h, ids, s, b)
			faultStops(t, h, steps, s, Fault{Reason: ReasonDecodeFailed, Detail: "b's first chunk", Occurrence: b.ID}, ids[1], ids[1])
			playsFrom(t, h, ids[1])
		})
	}
}

// TestASuccessorsFaultSelectsTheQueuesNextAfterTheEntryHeard pins the rule
// from the queue as it stands at the fault (B903AUD): the selection moves to
// the entry that follows the one heard, a, now, passing over this pass's
// refusals (following), which is the successor that faulted while it is
// still next, the entry that took its place once it was deleted or bypassed
// by an insert, with its occurrence kept in the chain by a Cut under way or
// one that came too late (deleting b, which is not the selection, moves no
// selection of itself), and the first entry with repeat on. With nothing
// playable after a in the pass the selection stays on a, as the end of the
// queue leaves it: a is last with repeat off, or b, all that follows it, was
// refused in the pass (b's next lap refused at its hand-off with repeat on,
// which leaves b's occurrence no longer next, its Cut held, before repeat
// goes off). The entry heard is the session's first occurrence once it is
// heard, or the occurrence a seek's session continues, heard before it: a
// fault two successors past it selects the first, never heard, which is
// skipped no more than any entry a fault follows; and with nothing heard yet
// the selection stays on the session's first entry, which never played,
// wherever that entry stands in the queue (never the queue's first, which
// following from no entry gives). Whatever the selection becomes, the fault
// and its stop name the entry that faulted.
func TestASuccessorsFaultSelectsTheQueuesNextAfterTheEntryHeard(t *testing.T) {
	// cuts holds every Cut, or answers each that it came too late.
	cuts := func(h *harness, held bool) {
		if held {
			holdCalls(h, 0, true)
			return
		}
		h.be.script(func() {
			h.be.onCut = func(context.Context, *fakeSession, OccurrenceID) (bool, error) { return false, nil }
		})
	}
	// submittedB plays a, b, c from a heard to b submitted, c named after it
	// and every Prepare answered, and returns the IDs, the session and b.
	submittedB := func(t *testing.T, h *harness) ([]uint32, *fakeSession, *Occurrence) {
		t.Helper()
		ids, s := h.playing(0, "a", "b", "c")
		b := s.prepared(s.first.ID, 1)
		s.submitted(b.ID)
		s.prepared(b.ID, 1)
		h.waitAppliedTimes("handled call prepare", 0, 2)
		return ids, s, b
	}
	for _, tc := range []struct {
		name string
		// run plays to the fault and returns the session, the occurrence that
		// faults, its entry and the entry the selection rests on.
		run func(t *testing.T, h *harness) (s *fakeSession, o OccurrenceID, faulted, selected uint32)
	}{
		{"deleted, its cut under way", func(t *testing.T, h *harness) (*fakeSession, OccurrenceID, uint32, uint32) {
			cuts(h, true)
			ids, s, b := submittedB(t, h)
			h.must("delete b", h.eng.Delete(bg, ids[1]))
			h.be.waitFor("the cut of b", func() bool { return h.be.countLocked("cut") == 1 })
			return s, b.ID, ids[1], ids[2]
		}},
		{"deleted, its cut too late", func(t *testing.T, h *harness) (*fakeSession, OccurrenceID, uint32, uint32) {
			cuts(h, false)
			ids, s, b := submittedB(t, h)
			h.must("delete b", h.eng.Delete(bg, ids[1]))
			h.waitAppliedTimes("handled call cut", 0, 1)
			return s, b.ID, ids[1], ids[2]
		}},
		{"no longer next, its cut under way", func(t *testing.T, h *harness) (*fakeSession, OccurrenceID, uint32, uint32) {
			cuts(h, true)
			ids, s, b := submittedB(t, h)
			x, err := h.eng.InsertAfter(bg, ids[0], ref("x"), nil)
			h.must("insert x after a", err)
			h.be.waitFor("the cut of b", func() bool { return h.be.countLocked("cut") == 1 })
			return s, b.ID, ids[1], x
		}},
		{"no longer next, its cut too late", func(t *testing.T, h *harness) (*fakeSession, OccurrenceID, uint32, uint32) {
			cuts(h, false)
			ids, s, b := submittedB(t, h)
			x, err := h.eng.InsertAfter(bg, ids[0], ref("x"), nil)
			h.must("insert x after a", err)
			h.waitAppliedTimes("handled call cut", 0, 1)
			return s, b.ID, ids[1], x
		}},
		{"the wrap-around, repeat on", func(t *testing.T, h *harness) (*fakeSession, OccurrenceID, uint32, uint32) {
			h.must("repeat", h.eng.SetRepeat(bg, true))
			ids, s := h.playing(1, "b", "a")
			wrap := s.prepared(s.first.ID, 1)
			if wrap == nil || wrap.Entry != ids[0] {
				t.Fatalf("the wrap-around successor is %+v, b wanted", wrap)
			}
			h.waitAppliedTimes("handled call prepare", 0, 1)
			return s, wrap.ID, ids[0], ids[0]
		}},
		{"the last entry, repeat off", func(t *testing.T, h *harness) (*fakeSession, OccurrenceID, uint32, uint32) {
			cuts(h, true)
			h.must("repeat", h.eng.SetRepeat(bg, true))
			ids, s := h.playing(1, "b", "a")
			wrap := s.prepared(s.first.ID, 1)
			s.submitted(wrap.ID)
			s.prepared(wrap.ID, 1)
			h.waitAppliedTimes("handled call prepare", 0, 2)
			h.must("repeat off", h.eng.SetRepeat(bg, false))
			h.be.waitFor("the cut of the wrap-around", func() bool { return h.be.countLocked("cut") == 1 })
			return s, wrap.ID, ids[0], ids[1]
		}},
		{"every entry after it refused in the pass", func(t *testing.T, h *harness) (*fakeSession, OccurrenceID, uint32, uint32) {
			cuts(h, true)
			h.must("repeat", h.eng.SetRepeat(bg, true))
			ids, s := h.playing(0, "a", "b")
			b := s.successor(s.first.ID)
			s.submitted(b.ID)
			wrap := s.successor(b.ID)
			if b.Entry != ids[1] || wrap.Entry != ids[0] {
				t.Fatalf("the successors named are %+v and %+v, b and a wanted", b, wrap)
			}
			// b's next lap, named once a's (wrap) is submitted, is refused:
			// b is then refused in the pass, so its occurrence here no longer
			// follows a, and the Cut that asks is held.
			unplayable(h, "b")
			s.submitted(wrap.ID)
			h.be.waitFor("the cut of b", func() bool { return h.be.countLocked("cut") == 1 })
			if _, ok := h.snap().Skipped(ids[1]); !ok {
				t.Fatal("b's next lap was not refused")
			}
			h.must("repeat off", h.eng.SetRepeat(bg, false))
			return s, b.ID, ids[1], ids[0]
		}},
		{"two successors past the entry heard", func(t *testing.T, h *harness) (*fakeSession, OccurrenceID, uint32, uint32) {
			ids, s := h.playing(0, "a", "b", "c")
			b := s.prepared(s.first.ID, 1)
			s.submitted(b.ID)
			c := s.prepared(b.ID, 1)
			h.waitAppliedTimes("handled call prepare", 0, 2)
			return s, c.ID, ids[2], ids[1]
		}},
		{"the session a seek started, before it is heard", func(t *testing.T, h *harness) (*fakeSession, OccurrenceID, uint32, uint32) {
			ids, _ := h.heardAt(0, pcmMinute, 5*44100, "a", "b")
			h.must("seek", h.eng.SeekAbsolute(bg, 30))
			s := h.be.nth(2)
			b := s.successor(s.first.ID)
			return s, b.ID, ids[1], ids[1]
		}},
		{"nothing heard yet", func(t *testing.T, h *harness) (*fakeSession, OccurrenceID, uint32, uint32) {
			ids := h.insert("a", "b", "c")
			h.available()
			h.must("play", h.eng.Play(bg))
			s := h.be.nth(1)
			b := s.prepared(s.first.ID, 1)
			h.waitAppliedTimes("handled call prepare", 0, 1)
			return s, b.ID, ids[1], ids[0]
		}},
		{"nothing heard yet, the session started past the queue's first entry", func(t *testing.T, h *harness) (*fakeSession, OccurrenceID, uint32, uint32) {
			ids := h.insert("a", "b", "c")
			h.available()
			h.must("play b", h.eng.PlayID(bg, ids[1]))
			s := h.be.nth(1)
			c := s.prepared(s.first.ID, 1)
			if c == nil || c.Entry != ids[2] {
				t.Fatalf("the successor named after b is %+v, c wanted", c)
			}
			h.waitAppliedTimes("handled call prepare", 0, 1)
			return s, c.ID, ids[2], ids[1]
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			steps := recordSteps(h)
			h.boot()
			s, o, faulted, selected := tc.run(t, h)
			faultStops(t, h, steps, s, Fault{Reason: ReasonDecodeFailed, Detail: "its first chunk", Occurrence: o}, faulted, selected)
			playsFrom(t, h, selected)
		})
	}
}

// TestAnOccurrenceBudgetFaultIsTheSuccessorsItCouldNotHandOff pins the
// engine's own fault for a successor it found and could not hand off, the
// native occurrence budget spent (B903AUD): the fault names that successor's
// entry, and so does its stop, and it is a successor's fault before it was
// heard, so the selection moves on to it. Until then the fault carried no
// occurrence and was recorded against the selection, b, the entry heard,
// where the selection stayed, and Play replayed b. Under a budget of one,
// b's hand-off takes the slot, and its Prepare is held until b is heard, so
// c is the next named, after b, the entry heard, in the Prepare's answer's
// step.
func TestAnOccurrenceBudgetFaultIsTheSuccessorsItCouldNotHandOff(t *testing.T) {
	h := newHarness(t)
	h.tweak = func(c *Config) { c.Backend = pcmBudgetBackend{c.Backend}; c.OccurrenceLimit = 1 }
	steps := recordSteps(h)
	h.boot()
	native := NativePlan{PolicyID: "native"}
	h.be.script(func() {
		h.be.onStart = func(_ context.Context, s *fakeSession) error {
			s.ev.(NativePlanEvents).NativePlan(s.id, s.first.ID, native)
			return s.open()
		}
	})
	held := newGate()
	open := sync.OnceFunc(held.open)
	t.Cleanup(open)
	h.be.script(func() {
		first := true
		h.be.onPrepare = func(ctx context.Context, _ *fakeSession, _ OccurrenceID, next *Occurrence) error {
			h.be.mu.Lock()
			hold := first && next != nil
			if hold {
				first = false
			}
			h.be.mu.Unlock()
			if hold {
				return held.wait(ctx, h.be, false)
			}
			return nil
		}
	})
	ids, s := h.playing(0, "a", "b", "c")
	b := s.prepared(s.first.ID, 1)
	if b == nil || b.Entry != ids[1] {
		t.Fatalf("the successor named after a is %+v, b wanted", b)
	}
	s.ev.(NativePlanEvents).NativePlan(s.id, b.ID, native)
	s.submitted(b.ID)
	mark := h.appliedMark()
	s.audible(b.ID)
	h.waitApplied("handled audible", mark)
	if sn := h.snap(); sn.Selected() != ids[1] || len(sn.Faults()) != 0 {
		t.Fatalf("b heard: selected %d, faults %+v; b and none wanted", sn.Selected(), sn.Faults())
	}
	open()
	sn := h.waitSnap("stopped by the budget", func(sn *Snapshot) bool { return sn.State() == Stopped })
	if fs := sn.Faults(); len(fs) != 1 || fs[0].Reason != "occurrence_budget_exceeded" {
		t.Fatalf("faults %+v, the budget's alone wanted", fs)
	}
	stoppedBy(t, "the budget's step", faultStep(t, steps(), 1), ids[2], ids[2])
	stoppedBy(t, "stopped", sn, ids[2], ids[2])
	playsFrom(t, h, ids[2])
}

// TestAPlayAfterASuccessorsFaultTakesTheStartPath pins what the Play that
// starts at the successor that faulted, b, does when b fails again at its
// start (B903AUD): what any start does, nothing the earlier fault changes. b
// refused before any of its content played is skipped, once a pass, and the
// entry after it plays; b faulting as the session's first occurrence, or in
// a native Start that fails (a fault, never a skip:
// TestNativeStartFailureDoesNotSkip), stops with b selected and its stop
// naming b; and nothing plays b again until a command does: a Play plays b,
// a Next the entry after it.
func TestAPlayAfterASuccessorsFaultTakesTheStartPath(t *testing.T) {
	for _, tc := range []struct {
		name string
		then func(t *testing.T, h *harness, ids []uint32)
	}{
		{"refused at its start", func(t *testing.T, h *harness, ids []uint32) {
			unplayable(h, "b")
			n := h.be.starts()
			h.must("play", h.eng.Play(bg))
			if s := h.be.nth(n + 1); s.first.Entry != ids[1] {
				t.Errorf("Play started entry %d, b wanted", s.first.Entry)
			}
			if s := h.be.nth(n + 2); s.first.Entry != ids[2] {
				t.Errorf("after b's refusal entry %d started, c wanted", s.first.Entry)
			}
			sn := h.snap()
			if sk, ok := sn.Skipped(ids[1]); !ok || sk.Reason != ReasonMissing {
				t.Errorf("b's skip %+v %v", sk, ok)
			}
			if sn.State() != Playing || sn.Selected() != ids[2] || len(sn.Faults()) != 1 {
				t.Errorf("%s at %d with faults %+v; c playing and the one fault wanted", sn.State(), sn.Selected(), sn.Faults())
			}
		}},
		{"faulting again as its session's first", func(t *testing.T, h *harness, ids []uint32) {
			s := playsFrom(t, h, ids[1])
			s.fault(Fault{Reason: ReasonXrun, Occurrence: s.first.ID})
			sn := h.waitSnap("stopped again", func(sn *Snapshot) bool { return sn.State() == Stopped && len(sn.Faults()) == 2 })
			stoppedBy(t, "stopped again", sn, ids[1], ids[1])
			// The release's answer is the last step the fault makes: nothing
			// started after it, and only the Play that follows starts b again.
			h.waitAppliedTimes("handled call release", 0, 2)
			if n := h.be.starts(); n != 2 {
				t.Errorf("%d sessions once b faulted again, 2 wanted", n)
			}
			playsFrom(t, h, ids[1])
		}},
		{"a native start that fails", func(t *testing.T, h *harness, ids []uint32) {
			h.be.script(func() {
				h.be.onStart = func(_ context.Context, s *fakeSession) error {
					s.ev.(NativePlanEvents).NativePlan(s.id, s.first.ID, NativePlan{PolicyID: "native"})
					return &Unplayable{Reason: ReasonBackend, Detail: "native activation failed"}
				}
			})
			h.refused("play", h.eng.Play(bg), ReasonFault)
			sn := h.waitSnap("stopped again", func(sn *Snapshot) bool { return sn.State() == Stopped && len(sn.Faults()) == 2 })
			stoppedBy(t, "stopped again", sn, ids[1], ids[1])
			if len(sn.Skips()) != 0 || h.be.starts() != 2 {
				t.Errorf("skips %v after %d sessions; none after 2 wanted", sn.Skips(), h.be.starts())
			}
			h.be.script(func() { h.be.onStart = nil })
			h.must("next", h.eng.Next(bg))
			if s := h.be.nth(3); s.first.Entry != ids[2] {
				t.Errorf("Next started entry %d, c wanted", s.first.Entry)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			steps := recordSteps(h)
			h.boot()
			ids, s := h.playing(0, "a", "b", "c")
			b := s.prepared(s.first.ID, 1)
			h.waitAppliedTimes("handled call prepare", 0, 1)
			faultStops(t, h, steps, s, Fault{Reason: ReasonDecodeFailed, Detail: "b's first chunk", Occurrence: b.ID}, ids[1], ids[1])
			if t.Failed() {
				// What follows starts at b.
				t.FailNow()
			}
			tc.then(t, h, ids)
		})
	}
}

// TestASuccessorsFaultAfterTheEntryHeardWasDeletedIsNotTaken pins a
// successor's fault once the entry heard, a, the selection, was deleted
// (B903AUD): it is not taken. The deletion selects b, the entry after a, at
// once and releases a's session to play b (selectionDeleted), so a fault
// that session reports of b, never heard, while its release is held, comes
// from a session no longer current (current): no fault is recorded, the
// transport, the pending play, the last stop and the selection stay as the
// deletion left them, and b starts once the release is acknowledged.
func TestASuccessorsFaultAfterTheEntryHeardWasDeletedIsNotTaken(t *testing.T) {
	h := startHarness(t)
	ids, s := h.playing(0, "a", "b", "c")
	b := s.prepared(s.first.ID, 1)
	if b == nil || b.Entry != ids[1] {
		t.Fatalf("the successor named after a is %+v, b wanted", b)
	}
	h.waitAppliedTimes("handled call prepare", 0, 1)
	held := holdReleases(h)
	h.must("delete a", h.eng.Delete(bg, ids[0]))
	deleted := h.snap()
	if deleted.State() != Releasing || deleted.Pending() != IntentPlay || deleted.Selected() != ids[1] {
		t.Fatalf("a deleted: %s, pending %s, selected %d; releasing to play b wanted", deleted.State(), deleted.Pending(), deleted.Selected())
	}
	mark := h.appliedMark()
	s.fault(Fault{Reason: ReasonDecodeFailed, Detail: "b's first chunk", Occurrence: b.ID})
	h.waitApplied("handled fault", mark)
	sn := h.snap()
	if fs := sn.Faults(); len(fs) != 0 {
		t.Errorf("the released session's fault was recorded: %+v", fs)
	}
	if sn.State() != Releasing || sn.Pending() != IntentPlay || sn.LastStop() != deleted.LastStop() || sn.Selected() != ids[1] {
		t.Fatalf("after the released session's fault: %s, pending %s, last stop %+v, selected %d; as the deletion left them wanted",
			sn.State(), sn.Pending(), sn.LastStop(), sn.Selected())
	}
	held.open()
	if s2 := h.be.nth(2); s2.first.Entry != ids[1] {
		t.Errorf("once the release was acknowledged entry %d started, b wanted", s2.first.Entry)
	}
	if sn := h.snap(); len(sn.Faults()) != 0 || sn.Selected() != ids[1] {
		t.Errorf("b started: faults %+v, selected %d; none and b wanted", sn.Faults(), sn.Selected())
	}
}
