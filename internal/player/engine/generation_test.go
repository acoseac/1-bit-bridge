package engine

import (
	"errors"
	"testing"
)

// TestAnExpectedGenerationHoldsThroughSteadyPlayback pins what does not
// move the generation: position updates, the first sound of a session and
// a successor's hand-off.
func TestAnExpectedGenerationHoldsThroughSteadyPlayback(t *testing.T) {
	h := startHarness(t)
	_, s := h.playing(0, "a", "b")
	g := h.snap().Generation()
	next := s.prepared(s.first.ID, 1)
	s.preparedEvent(next.ID, Join{Gapless: true})
	s.submitted(next.ID)
	for f := int64(1); f <= 50; f++ {
		s.position(s.first.ID, f*4410)
	}
	h.waitSnap("the positions", func(sn *Snapshot) bool { return sn.Position().Frames == 50*4410 })
	if got := h.snap().Generation(); got != g {
		t.Fatalf("steady playback moved the generation from %s to %s", g, got)
	}
	h.must("pause at the generation read before", h.eng.Pause(bg, IfGeneration(g)))
}

// TestAGenerationMismatchIsRefused pins the conflict: refused, typed, with
// the current generation, and nothing changed.
func TestAGenerationMismatchIsRefused(t *testing.T) {
	h := startHarness(t)
	ids := h.insert("a", "b")
	g := h.snap().Generation()
	h.insert("c")
	err := h.eng.Delete(bg, ids[0], IfGeneration(g))
	h.refused("delete at an old generation", err, ReasonConflict)
	var e *Error
	if !errors.As(err, &e) || e.Current != h.snap().Generation() {
		t.Fatalf("the conflict names %v, want the current generation %s", err, h.snap().Generation())
	}
	if h.snap().Len() != 3 {
		t.Fatal("the refused delete changed the queue")
	}
	if _, err := h.eng.InsertAfter(bg, 0, ref("d"), nil, IfGeneration(Generation{})); ReasonOf(err) != ReasonConflict {
		t.Fatalf("the zero generation matched: %v", err)
	}
	h.must("delete at the current generation", h.eng.Delete(bg, ids[0], IfGeneration(h.snap().Generation())))
}

// TestAGenerationFromBeforeARestartIsRefused pins the run ID: a pair from
// an earlier run never matches, even at the same count.
func TestAGenerationFromBeforeARestartIsRefused(t *testing.T) {
	h := startHarness(t)
	h.insert("a")
	old := h.snap().Generation()
	h.restart()
	now := h.snap().Generation()
	if now.run == old.run {
		t.Fatal("two runs drew the same run ID")
	}
	same := Generation{run: old.run, n: now.n}
	h.refused("an earlier run's generation", h.eng.SetRepeat(bg, true, IfGeneration(same)), ReasonConflict)
	h.refused("an earlier run's generation", h.eng.SetRepeat(bg, true, IfGeneration(old)), ReasonConflict)
	back, err := ParseGeneration(now.String())
	if err != nil || back != now {
		t.Fatalf("ParseGeneration(%q) = %v, %v", now.String(), back, err)
	}
	h.must("the current generation", h.eng.SetRepeat(bg, true, IfGeneration(back)))
}

// TestEachVisibleChangeMovesTheGeneration pins what does: the queue, the
// modes, the selection, the transport, the output and maintenance.
func TestEachVisibleChangeMovesTheGeneration(t *testing.T) {
	h := startHarness(t)
	g := h.snap().Generation()
	moved := func(what string) {
		t.Helper()
		now := h.snap().Generation()
		if now.n <= g.n {
			t.Fatalf("%s left the generation at %s", what, now)
		}
		g = now
	}
	ids := h.insert("a", "b")
	moved("an insert")
	h.must("repeat", h.eng.SetRepeat(bg, true))
	moved("repeat")
	h.available()
	moved("the output")
	h.must("next", h.eng.Next(bg))
	moved("a next, which selects and plays")
	h.must("play", h.eng.PlayID(bg, ids[0]))
	moved("play")
	h.must("stop", h.eng.Stop(bg))
	moved("stop")
	hold, err := h.eng.EnterMaintenance(bg, g, "the test")
	h.must("enter", err)
	moved("maintenance")
	h.must("exit", h.eng.ExitMaintenance(bg, hold.Lease))
	moved("its end")
}
