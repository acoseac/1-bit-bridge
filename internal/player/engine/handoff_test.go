package engine

import (
	"context"
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
// queue.
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
			tc.edit(h, ids)
			h.be.waitFor("the cut", func() bool {
				for _, c := range h.be.calls {
					if c.kind == "cut" {
						return true
					}
				}
				return false
			})
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

// TestAnUnrelatedEditLeavesTheSessionAlone pins edits that do not change a
// successor: no call reaches the backend.
func TestAnUnrelatedEditLeavesTheSessionAlone(t *testing.T) {
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
}

// occName is an occurrence as fakeCall prints it.
func occName(o OccurrenceID) string { return strconv.FormatUint(uint64(o), 10) }

// idName is an entry ID as fakeCall prints it.
func idName(i uint32) string { return strconv.FormatUint(uint64(i), 10) }
