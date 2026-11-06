package engine

import (
	"context"
	"slices"
	"testing"
)

// replan is the backend's request that occurrence o be handed off again
// (ReplanEvents, B728 B7), as the bridge's backend sends it when o's plan
// left the player's effective DSD set.
func (s *fakeSession) replan(o OccurrenceID) {
	s.ev.(ReplanEvents).Replan(s.id, o, "its plan left the effective set")
}

// TestAReplanHandsTheSuccessorOffAgainAndSkipsNothing (B728 B7): a
// backend's request to hand an occurrence off again takes the path an edit
// after hand-off takes: a successor not yet submitted is replaced by a new
// Prepare naming the same entry, a new occurrence, with no cut; a submitted
// one is cut, then the same entry is prepared again; nothing is skipped and
// the queue is untouched. A request for the session's first occurrence, or
// for one heard, changes nothing; and one cut too late stops playback once
// it is heard, as an edit's does.
func TestAReplanHandsTheSuccessorOffAgainAndSkipsNothing(t *testing.T) {
	t.Run("not submitted", func(t *testing.T) {
		h := startHarness(t)
		ids, s, a, b := handoff(t, h, false, "a", "b", "c")
		mark := h.be.mark()
		s.replan(b.ID)
		next := s.prepared(a, 2)
		if next == nil || next.Entry != ids[1] || next.ID == b.ID {
			t.Fatalf("the successor after the replan is %+v, want entry b again under a new occurrence (b was %d)", next, b.ID)
		}
		if got := h.be.callsSince(mark); !slices.Equal(got, []string{"prepare " + idName(ids[1])}) {
			t.Fatalf("calls %v, want the one Prepare and no cut", got)
		}
		if sn := h.eng.Snapshot(); len(sn.Skips()) != 0 || sn.Len() != 3 {
			t.Fatalf("skips %+v, %d entries: want nothing skipped and the queue as it was", sn.Skips(), sn.Len())
		}
	})
	t.Run("submitted", func(t *testing.T) {
		h := startHarness(t)
		ids, s, a, b := handoff(t, h, true, "a", "b", "c")
		mark := h.be.mark()
		s.replan(b.ID)
		next := s.prepared(a, 2)
		if next == nil || next.Entry != ids[1] || next.ID == b.ID {
			t.Fatalf("the successor after the replan is %+v, want entry b again under a new occurrence", next)
		}
		if got := h.be.callsSince(mark); !slices.Equal(got, []string{"cut " + occName(b.ID), "prepare " + idName(ids[1])}) {
			t.Fatalf("calls %v, want the cut and then the Prepare", got)
		}
		if sn := h.eng.Snapshot(); len(sn.Skips()) != 0 {
			t.Fatalf("skips %+v, want none", sn.Skips())
		}
	})
	t.Run("the first occurrence and one heard are passed over", func(t *testing.T) {
		h := startHarness(t)
		ids, s, a, b := handoff(t, h, false, "a", "b", "c")
		calls := h.be.mark()
		mark := h.appliedMark()
		s.replan(a)
		h.waitApplied("handled replan", mark)
		if got := h.be.callsSince(calls); len(got) != 0 {
			t.Fatalf("calls %v after a replan of the first occurrence, want none", got)
		}
		s.audible(b.ID)
		h.waitSnap("b heard", func(sn *Snapshot) bool { au, ok := sn.Audible(); return ok && au.Occurrence == b.ID })
		calls = h.be.mark()
		mark = h.appliedMark()
		s.replan(b.ID)
		h.waitApplied("handled replan", mark)
		for _, c := range h.be.callsSince(calls) {
			if c == "cut "+occName(b.ID) || c == "prepare "+idName(ids[1]) {
				t.Fatalf("calls %v after a replan of the heard occurrence, want b neither cut nor handed off again", h.be.callsSince(calls))
			}
		}
		if sn := h.eng.Snapshot(); sn.State() != Playing || len(sn.Skips()) != 0 {
			t.Fatalf("the transport is %v, skips %+v: want it playing on, nothing skipped", sn.State(), sn.Skips())
		}
	})
	t.Run("cut too late", func(t *testing.T) {
		h := startHarness(t)
		_, s, _, b := handoff(t, h, true, "a", "b", "c")
		h.be.script(func() {
			h.be.onCut = func(context.Context, *fakeSession, OccurrenceID) (bool, error) { return false, nil }
		})
		mark := h.appliedMark()
		s.replan(b.ID)
		h.waitApplied("handled call cut", mark)
		s.audible(b.ID)
		sn := h.waitSnap("stopped", func(sn *Snapshot) bool { return sn.State() == Stopped })
		if sn.LastStop().Reason != ReasonRemovedAudible {
			t.Fatalf("stopped for %+v, want %s", sn.LastStop(), ReasonRemovedAudible)
		}
	})
}
