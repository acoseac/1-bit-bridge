package engine

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
)

// handoff plays the first of refs, waits for its successor to be named,
// and, when submitted is set, reports the successor's frames in the sink
// and waits for the successor after it to be named. It returns the IDs,
// the session, the first occurrence and the successor.
func handoff(t *testing.T, h *harness, submitted bool, refs ...string) ([]uint32, *fakeSession, OccurrenceID, *Occurrence) {
	t.Helper()
	ids, s := h.playing(0, refs...)
	a := s.first.ID
	b := s.prepared(a, 1)
	if b == nil || b.Entry != ids[1] {
		t.Fatalf("the successor of the first entry is %+v", b)
	}
	s.preparedEvent(b.ID, Join{Gapless: true})
	if submitted {
		s.submitted(b.ID)
		s.prepared(b.ID, 1)
	}
	return ids, s, a, b
}

// TestAnEditBeforeSubmissionReplacesTheSuccessor pins each kind of edit
// that changes a successor not yet submitted: a new Prepare, no cut.
func TestAnEditBeforeSubmissionReplacesTheSuccessor(t *testing.T) {
	t.Run("deleting it", func(t *testing.T) {
		h := startHarness(t)
		ids, s, a, _ := handoff(t, h, false, "a", "b", "c")
		h.must("delete b", h.eng.Delete(bg, ids[1]))
		if next := s.prepared(a, 2); next == nil || next.Entry != ids[2] {
			t.Fatalf("the successor after deleting b is %+v, want c", next)
		}
		if h.be.count("cut") != 0 || h.be.count("release") != 0 {
			t.Fatalf("calls %v", h.be.callList())
		}
	})
	t.Run("inserting before it", func(t *testing.T) {
		h := startHarness(t)
		ids, s, a, _ := handoff(t, h, false, "a", "b", "c")
		x, err := h.eng.InsertAfter(bg, ids[0], ref("x"), nil)
		h.must("insert x after a", err)
		if next := s.prepared(a, 2); next == nil || next.Entry != x {
			t.Fatalf("the successor after inserting x is %+v, want x", next)
		}
	})
	t.Run("repeat off when it was the wrap-around", func(t *testing.T) {
		h := startHarness(t)
		h.must("repeat", h.eng.SetRepeat(bg, true))
		ids, s := h.playing(1, "a", "b")
		if next := s.prepared(s.first.ID, 1); next == nil || next.Entry != ids[0] {
			t.Fatalf("the wrap-around successor is %+v", next)
		}
		h.must("repeat off", h.eng.SetRepeat(bg, false))
		if next := s.prepared(s.first.ID, 2); next != nil {
			t.Fatalf("the successor after repeat off is %+v, want none", next)
		}
	})
	t.Run("delete all", func(t *testing.T) {
		h := startHarness(t)
		_, s, a, _ := handoff(t, h, false, "a", "b", "c")
		h.must("delete all", h.eng.DeleteAll(bg))
		h.waitSnap("stopped", func(sn *Snapshot) bool { return sn.State() == Stopped })
		if len(s.preparesAfter(a)) != 1 || h.be.count("cut") != 0 || !s.isAcked() {
			t.Fatalf("calls %v", h.be.callList())
		}
	})
	t.Run("play id elsewhere", func(t *testing.T) {
		h := startHarness(t)
		ids, s, a, _ := handoff(t, h, false, "a", "b", "c")
		h.must("play c", h.eng.PlayID(bg, ids[2]))
		if s2 := h.be.nth(2); s2.first.Entry != ids[2] || !s.isAcked() || len(s.preparesAfter(a)) != 1 {
			t.Fatalf("calls %v", h.be.callList())
		}
	})
}

// TestAnEditAfterSubmissionAsksForACut pins each kind of edit that changes
// a submitted successor: a cut at the boundary, then the new successor.
func TestAnEditAfterSubmissionAsksForACut(t *testing.T) {
	t.Run("deleting it", func(t *testing.T) {
		h := startHarness(t)
		ids, s, a, b := handoff(t, h, true, "a", "b", "c")
		mark := h.be.mark()
		h.must("delete b", h.eng.Delete(bg, ids[1]))
		if next := s.prepared(a, 2); next == nil || next.Entry != ids[2] {
			t.Fatalf("the successor after the cut is %+v, want c", next)
		}
		if got := h.be.callsSince(mark); !slices.Equal(got, []string{"cut " + occName(b.ID), "prepare " + idName(ids[2])}) {
			t.Fatalf("calls %v", got)
		}
	})
	t.Run("inserting before it", func(t *testing.T) {
		h := startHarness(t)
		ids, s, a, b := handoff(t, h, true, "a", "b", "c")
		mark := h.be.mark()
		x, err := h.eng.InsertAfter(bg, ids[0], ref("x"), nil)
		h.must("insert x after a", err)
		if next := s.prepared(a, 2); next == nil || next.Entry != x {
			t.Fatalf("the successor after the cut is %+v, want x", next)
		}
		if got := h.be.callsSince(mark); !slices.Equal(got, []string{"cut " + occName(b.ID), "prepare " + idName(x)}) {
			t.Fatalf("calls %v", got)
		}
	})
	t.Run("repeat off when it was the wrap-around", func(t *testing.T) {
		h := startHarness(t)
		h.must("repeat", h.eng.SetRepeat(bg, true))
		ids, s := h.playing(1, "a", "b")
		wrap := s.prepared(s.first.ID, 1)
		if wrap == nil || wrap.Entry != ids[0] {
			t.Fatalf("the wrap-around successor is %+v", wrap)
		}
		s.submitted(wrap.ID)
		s.prepared(wrap.ID, 1)
		mark := h.be.mark()
		h.must("repeat off", h.eng.SetRepeat(bg, false))
		if next := s.prepared(s.first.ID, 2); next != nil {
			t.Fatalf("the successor after repeat off is %+v, want none", next)
		}
		if got := h.be.callsSince(mark); !slices.Equal(got, []string{"cut " + occName(wrap.ID), "prepare end"}) {
			t.Fatalf("calls %v", got)
		}
	})
	t.Run("delete all", func(t *testing.T) {
		h := startHarness(t)
		_, s, _, _ := handoff(t, h, true, "a", "b", "c")
		h.must("delete all", h.eng.DeleteAll(bg))
		h.waitSnap("stopped", func(sn *Snapshot) bool { return sn.State() == Stopped })
		if h.be.count("cut") != 0 || !s.isAcked() {
			t.Fatalf("calls %v", h.be.callList())
		}
	})
	t.Run("play id elsewhere", func(t *testing.T) {
		h := startHarness(t)
		ids, s, _, _ := handoff(t, h, true, "a", "b", "c")
		h.must("play c", h.eng.PlayID(bg, ids[2]))
		if s2 := h.be.nth(2); s2.first.Entry != ids[2] || !s.isAcked() || h.be.count("cut") != 0 {
			t.Fatalf("calls %v", h.be.callList())
		}
	})
}

// TestAFailedCutStopsWhenTheRemovedEntryBecomesAudible pins a cut that came
// too late: the deleted entry, or the one no longer next, plays no further
// than its first report, and the selection never names an ID outside the
// queue. The report is sent once the owner has handled the cut's answer, so
// the test is of that order alone.
func TestAFailedCutStopsWhenTheRemovedEntryBecomesAudible(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(h *harness, ids []uint32)
	}{
		{"deleted", func(h *harness, ids []uint32) { h.must("delete b", h.eng.Delete(bg, ids[1])) }},
		{"no longer next", func(h *harness, ids []uint32) {
			if _, err := h.eng.InsertAfter(bg, ids[0], ref("x"), nil); err != nil {
				h.t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := startHarness(t)
			ids, s, a, b := handoff(t, h, true, "a", "b", "c")
			h.be.script(func() {
				h.be.onCut = func(context.Context, *fakeSession, OccurrenceID) (bool, error) { return false, nil }
			})
			mark := h.appliedMark()
			tc.edit(h, ids)
			h.waitApplied("handled call cut", mark)
			s.audible(b.ID)
			sn := h.waitSnap("stopped", func(sn *Snapshot) bool { return sn.State() == Stopped })
			if sn.LastStop().Reason != ReasonRemovedAudible {
				t.Fatalf("last stop %+v, want %s", sn.LastStop(), ReasonRemovedAudible)
			}
			if sel := sn.Selected(); sel != ids[0] {
				t.Fatalf("selected %d, want a, the entry before", sel)
			}
			if len(s.preparesAfter(a)) != 1 {
				t.Fatalf("a successor was named after the failed cut: %v", s.preparesAfter(a))
			}
		})
	}
}

// TestAnEntryHeardWhileItsCallIsUnderWayStops pins the edit's mark: an
// occurrence whose entry an edit removed, or made no longer next, is marked
// in the edit's own step, even while a Cut or a Prepare is under way, so its
// audible report stops playback whatever that call answers after it, the
// selection never names an entry outside the queue, and the next start loads
// the queue. An entry made no longer next is still in the queue, so only the
// mark can stop it.
func TestAnEntryHeardWhileItsCallIsUnderWayStops(t *testing.T) {
	type edit struct {
		name string
		// run edits the queue a, b, c and returns the queue it leaves.
		run func(h *harness, ids []uint32) []uint32
	}
	edits := []edit{
		{"deleted", func(h *harness, ids []uint32) []uint32 {
			h.must("delete b", h.eng.Delete(bg, ids[1]))
			return []uint32{ids[0], ids[2]}
		}},
		{"no longer next", func(h *harness, ids []uint32) []uint32 {
			x, err := h.eng.InsertAfter(bg, ids[0], ref("x"), nil)
			h.must("insert x after a", err)
			return []uint32{ids[0], x, ids[1], ids[2]}
		}},
	}
	for _, tc := range []struct {
		name      string
		submitted bool
		// prepare is the Prepare held (counted from 1), or 0 for the Cut;
		// cut is what the held Cut answers.
		prepare int
		cut     bool
	}{
		{"a cut that answers it could", true, 0, true},
		{"a cut that answers it could not", true, 0, false},
		{"a prepare of it", false, 1, false},
		{"a prepare of what follows it", true, 2, false},
	} {
		for _, ed := range edits {
			t.Run(ed.name+"/"+tc.name, func(t *testing.T) {
				h := startHarness(t)
				held := newGate()
				h.be.script(func() {
					n := 0
					h.be.onPrepare = func(ctx context.Context, _ *fakeSession, _ OccurrenceID, _ *Occurrence) error {
						h.be.mu.Lock()
						n++
						hold := n == tc.prepare
						h.be.mu.Unlock()
						if hold {
							return held.wait(ctx, h.be, false)
						}
						return nil
					}
					h.be.onCut = func(ctx context.Context, _ *fakeSession, _ OccurrenceID) (bool, error) {
						if err := held.wait(ctx, h.be, false); err != nil {
							return false, err
						}
						return tc.cut, nil
					}
				})
				ids, s, _, b := handoff(t, h, tc.submitted, "a", "b", "c")
				mark := h.appliedMark()
				queue := ed.run(h, ids)
				if tc.prepare == 0 {
					h.be.waitFor("the cut", func() bool { return h.be.countLocked("cut") == 1 })
				}
				if !tc.submitted {
					s.submitted(b.ID)
				}
				s.audible(b.ID)
				h.waitApplied("handled audible", mark)
				sn := h.snap()
				if sn.Selected() != ids[0] || sn.LastStop().Reason != ReasonRemovedAudible {
					t.Fatalf("b was heard with its call under way: selected %d (b is %d), %s, last stop %+v",
						sn.Selected(), ids[1], sn.State(), sn.LastStop())
				}
				mark = h.appliedMark()
				held.open()
				call := "handled call cut"
				if tc.prepare != 0 {
					call = "handled call prepare"
				}
				h.waitApplied(call, mark)
				sn = h.waitSnap("stopped", func(sn *Snapshot) bool { return sn.State() == Stopped })
				if sn.Selected() != ids[0] || h.be.starts() != 1 {
					t.Fatalf("after the call answered: selected %d, %d sessions", sn.Selected(), h.be.starts())
				}
				h.restart()
				if aside, _ := filepath.Glob(h.queuePath() + ".bad-*"); len(aside) != 0 || !slices.Equal(h.snap().IDs(), queue) {
					t.Fatalf("the next start: queue %v, want %v, moved aside %v", h.snap().IDs(), queue, aside)
				}
			})
		}
	}
}

// TestAnUnwantedOccurrenceCrossedUnreportedStops pins the crossing: an
// audible report of an occurrence after one an edit made no longer next,
// with no report of that one first (a backend that keeps the contract
// reports every boundary, in order), stops playback, since the unwanted
// content was heard on the way.
func TestAnUnwantedOccurrenceCrossedUnreportedStops(t *testing.T) {
	h := startHarness(t)
	ids, s, _, b := handoff(t, h, true, "a", "b", "c")
	c := s.prepared(b.ID, 1)
	h.be.script(func() {
		h.be.onCut = func(context.Context, *fakeSession, OccurrenceID) (bool, error) { return false, nil }
	})
	mark := h.appliedMark()
	if _, err := h.eng.InsertAfter(bg, ids[0], ref("x"), nil); err != nil {
		t.Fatal(err)
	}
	h.waitApplied("handled call cut", mark)
	s.audible(c.ID)
	sn := h.waitSnap("stopped", func(sn *Snapshot) bool { return sn.State() == Stopped })
	if sn.LastStop().Reason != ReasonRemovedAudible || sn.Selected() != ids[0] {
		t.Fatalf("c heard past b, no longer next: last stop %+v, selected %d", sn.LastStop(), sn.Selected())
	}
}

// TestACutAnsweredAfterItsEntryWasHeardStops pins a Cut that answers it
// could after the occurrence it cut was heard, which a backend that keeps
// the contract never does: the edit that asked for it was undone, the entry
// heard and selected, and then the cut. The chain is never left empty;
// playback stops.
func TestACutAnsweredAfterItsEntryWasHeardStops(t *testing.T) {
	h := startHarness(t)
	ids, s, _, b := handoff(t, h, true, "a", "b", "c")
	held := newGate()
	h.be.script(func() {
		h.be.onCut = func(ctx context.Context, _ *fakeSession, _ OccurrenceID) (bool, error) {
			if err := held.wait(ctx, h.be, false); err != nil {
				return false, err
			}
			return true, nil
		}
	})
	x, err := h.eng.InsertAfter(bg, ids[0], ref("x"), nil)
	h.must("insert x after a", err)
	h.be.waitFor("the cut", func() bool { return h.be.countLocked("cut") == 1 })
	h.must("delete x", h.eng.Delete(bg, x))
	mark := h.appliedMark()
	s.audible(b.ID)
	h.waitApplied("handled audible", mark)
	if sn := h.snap(); sn.Selected() != ids[1] || sn.State() != Playing {
		t.Fatalf("b, wanted again, was heard: selected %d, %s", sn.Selected(), sn.State())
	}
	mark = h.appliedMark()
	held.open()
	h.waitApplied("handled call cut", mark)
	sn := h.waitSnap("stopped", func(sn *Snapshot) bool { return sn.State() == Stopped })
	if sn.LastStop().Reason != ReasonRemovedAudible || sn.Selected() != ids[1] {
		t.Fatalf("last stop %+v, selected %d", sn.LastStop(), sn.Selected())
	}
}

// TestAPrepareRefusedAfterItsSuccessorWasHeardKeepsTheChain pins a Prepare
// that answers it took no successor after that successor was reported
// heard, which a backend that keeps the contract never does: the chain keeps
// the occurrence heard, so the next edit reconciles a session that plays on.
func TestAPrepareRefusedAfterItsSuccessorWasHeardKeepsTheChain(t *testing.T) {
	h := startHarness(t)
	held := newGate()
	h.be.script(func() {
		h.be.onPrepare = func(ctx context.Context, _ *fakeSession, _ OccurrenceID, _ *Occurrence) error {
			if err := held.wait(ctx, h.be, false); err != nil {
				return err
			}
			return errors.New("the backend took no successor")
		}
	})
	ids, s := h.playing(0, "a", "b", "c")
	b := s.prepared(s.first.ID, 1)
	s.submitted(b.ID)
	mark := h.appliedMark()
	s.audible(b.ID)
	h.waitApplied("handled audible", mark)
	mark = h.appliedMark()
	held.open()
	h.waitApplied("handled call prepare", mark)
	h.insert("d")
	if sn := h.snap(); sn.State() != Playing || sn.Selected() != ids[1] {
		t.Fatalf("after the refusal and an edit: %s at %d, want playing b", sn.State(), sn.Selected())
	}
}

// TestAnUnrelatedEditLeavesTheSessionAlone pins edits that do not change a
// successor: no call reaches the backend. And after a skip: queue a, b, c,
// b refused, so c follows a and is submitted; appending d, which starts a
// new pass, changes nothing between a and c, so c is neither cut nor
// marked, and heard it plays, whether a Cut would have come too late or a
// Prepare is under way when it is heard. The only call is the Prepare that
// names d after c.
func TestAnUnrelatedEditLeavesTheSessionAlone(t *testing.T) {
	t.Run("no successor changes", func(t *testing.T) {
		h := startHarness(t)
		ids, _, _, _ := handoff(t, h, true, "a", "b", "c", "d")
		mark := h.be.mark()
		h.must("delete d", h.eng.Delete(bg, ids[3]))
		if _, err := h.eng.InsertAfter(bg, ids[2], ref("x"), nil); err != nil {
			t.Fatal(err)
		}
		h.must("repeat off, as it was", h.eng.SetRepeat(bg, false))
		if got := h.be.callsSince(mark); len(got) != 0 {
			t.Fatalf("an unrelated edit made calls %v", got)
		}
		if sn := h.snap(); sn.State() != Playing || sn.Selected() != ids[0] {
			t.Fatalf("%s at %d", sn.State(), sn.Selected())
		}
	})
	// skipped plays a, b, c with b refused when it is named after a, so c
	// follows a, submits c, and returns the IDs, the session and c.
	skipped := func(t *testing.T, h *harness) ([]uint32, *fakeSession, *Occurrence) {
		t.Helper()
		ids, s := h.playing(0, "a", "b", "c")
		c := s.prepared(s.first.ID, 2)
		if c == nil || c.Entry != ids[2] {
			t.Fatalf("the successor named after b was refused is %+v, want c", c)
		}
		s.submitted(c.ID)
		return ids, s, c
	}
	t.Run("after a skip, its cut late", func(t *testing.T) {
		h := startHarness(t)
		h.be.script(func() {
			h.be.unplayable["b"] = Unplayable{Reason: ReasonMissing}
			h.be.onCut = func(context.Context, *fakeSession, OccurrenceID) (bool, error) { return false, nil }
		})
		ids, s, c := skipped(t, h)
		// b's Prepare, c's and the end's after c have all been answered.
		h.waitAppliedTimes("handled call prepare", 0, 3)
		mark := h.be.mark()
		d, err := h.eng.InsertAfter(bg, ids[2], ref("d"), nil)
		h.must("append d", err)
		h.be.waitFor("the call the append makes", func() bool { return len(h.be.calls) > mark })
		if got := h.be.callsSince(mark); !slices.Equal(got, []string{"prepare " + idName(d)}) {
			t.Fatalf("appending d made calls %v, want only d named after c", got)
		}
		applied := h.appliedMark()
		s.audible(c.ID)
		h.waitApplied("handled audible", applied)
		if sn := h.snap(); sn.State() != Playing || sn.Selected() != ids[2] {
			t.Fatalf("c, heard after the append: %s at %d (c is %d), last stop %+v", sn.State(), sn.Selected(), ids[2], sn.LastStop())
		}
	})
	t.Run("after a skip, a prepare under way", func(t *testing.T) {
		h := startHarness(t)
		held := newGate()
		h.be.script(func() {
			h.be.onPrepare = func(ctx context.Context, fs *fakeSession, _ OccurrenceID, next *Occurrence) error {
				switch {
				case next == nil:
					return held.wait(ctx, h.be, false)
				case next.Ref.String() == "b":
					fs.ev.Refused(fs.id, next.ID, Unplayable{Reason: ReasonMissing})
				}
				return nil
			}
		})
		ids, s, c := skipped(t, h)
		h.be.waitFor("the end named after c", func() bool { return h.be.countLocked("prepare") == 3 })
		d, err := h.eng.InsertAfter(bg, ids[2], ref("d"), nil)
		h.must("append d", err)
		applied := h.appliedMark()
		s.audible(c.ID)
		h.waitApplied("handled audible", applied)
		if sn := h.snap(); sn.State() != Playing || sn.Selected() != ids[2] {
			t.Fatalf("c, heard with the end's Prepare under way: %s at %d (c is %d), last stop %+v", sn.State(), sn.Selected(), ids[2], sn.LastStop())
		}
		held.open()
		if next := s.prepared(c.ID, 2); next == nil || next.Entry != d {
			t.Fatalf("named after c once the Prepare answered: %+v, want d", next)
		}
	})
}

// occName is an occurrence as fakeCall prints it.
func occName(o OccurrenceID) string { return strconv.FormatUint(uint64(o), 10) }

// idName is an entry ID as fakeCall prints it.
func idName(i uint32) string { return strconv.FormatUint(uint64(i), 10) }
