package engine

import "testing"

// unplayable scripts refs as refused before any content plays.
func unplayable(h *harness, refs ...string) {
	h.be.script(func() {
		for _, r := range refs {
			h.be.unplayable[r] = Unplayable{Reason: ReasonMissing, Detail: r + " is gone"}
		}
	})
}

// TestAnUnplayableEntryIsSkippedWithItsReason pins the skip, at a start and
// as a successor: the next entry plays and the snapshot says why the
// skipped one did not.
func TestAnUnplayableEntryIsSkippedWithItsReason(t *testing.T) {
	t.Run("at a start", func(t *testing.T) {
		h := startHarness(t)
		ids := h.insert("a", "u", "c")
		unplayable(h, "u")
		h.available()
		h.must("play u", h.eng.PlayID(bg, ids[1]))
		if s := h.be.nth(2); s.first.Entry != ids[2] {
			t.Fatalf("after the skip the session started %d, want c", s.first.Entry)
		}
		sn := h.snap()
		if sk, ok := sn.Skipped(ids[1]); !ok || sk.Reason != ReasonMissing {
			t.Fatalf("u's skip %+v %v", sk, ok)
		}
		if sn.Selected() != ids[2] || sn.State() != Playing {
			t.Fatalf("%s at %d", sn.State(), sn.Selected())
		}
	})
	t.Run("as a successor", func(t *testing.T) {
		h := startHarness(t)
		unplayable(h, "u")
		ids, s := h.playing(0, "a", "u", "c")
		if next := s.prepared(s.first.ID, 2); next == nil || next.Entry != ids[2] {
			t.Fatalf("the successor after the refused one is %+v, want c", next)
		}
		if sk, ok := h.snap().Skipped(ids[1]); !ok || sk.Reason != ReasonMissing {
			t.Fatalf("u's skip %+v %v", sk, ok)
		}
		if sel := h.snap().Selected(); sel != ids[0] {
			t.Fatalf("a refused successor moved the selection to %d", sel)
		}
	})
}

// TestAllUnplayableStops pins a pass in which every entry was refused.
func TestAllUnplayableStops(t *testing.T) {
	h := startHarness(t)
	h.insert("u1", "u2")
	unplayable(h, "u1", "u2")
	h.available()
	h.refused("play", h.result("play", async(func() error { return h.eng.Play(bg) })), ReasonAllUnplayable)
	sn := h.snap()
	if sn.State() != Stopped || sn.LastStop().Reason != ReasonAllUnplayable || h.be.starts() != 2 {
		t.Fatalf("%s, last stop %+v, %d sessions", sn.State(), sn.LastStop(), h.be.starts())
	}
}

// TestRepeatWithAllUnplayableStopsAfterOneLap pins one lap, never a spin.
func TestRepeatWithAllUnplayableStopsAfterOneLap(t *testing.T) {
	h := startHarness(t)
	h.insert("u1", "u2", "u3")
	unplayable(h, "u1", "u2", "u3")
	h.must("repeat", h.eng.SetRepeat(bg, true))
	h.available()
	// A spin never answers: the watchdog turns it into a failure.
	h.refused("play", h.result("play", async(func() error { return h.eng.Play(bg) })), ReasonAllUnplayable)
	if n := h.be.starts(); n != 3 {
		t.Fatalf("%d sessions for three unplayable entries with repeat on, want one lap of 3", n)
	}
	if sn := h.snap(); sn.State() != Stopped || len(sn.Skips()) != 3 {
		t.Fatalf("%s with %d skips", sn.State(), len(sn.Skips()))
	}
}

// TestOnePlayableAmongUnplayablePlaysWithoutSpinning pins the playable
// entry played and the others skipped once each per lap.
func TestOnePlayableAmongUnplayablePlaysWithoutSpinning(t *testing.T) {
	h := startHarness(t)
	ids := h.insert("u1", "p", "u2")
	unplayable(h, "u1", "u2")
	h.must("repeat", h.eng.SetRepeat(bg, true))
	h.available()
	h.must("play", h.eng.Play(bg))
	s := h.be.nth(2)
	if s.first.Entry != ids[1] || h.be.starts() != 2 {
		t.Fatalf("started %d after %d sessions, want p second", s.first.Entry, h.be.starts())
	}
	s.audible(s.first.ID)
	// p's successors: u2 refused, u1 refused, then p again.
	if next := s.prepared(s.first.ID, 3); next == nil || next.Entry != ids[1] {
		t.Fatalf("the third successor named is %+v, want p", next)
	}
	if n := len(s.preparesAfter(s.first.ID)); n != 3 {
		t.Fatalf("%d successors named after p, want 3", n)
	}
}

// TestAFailureAfterContentPlayedIsAFault pins a failure after content: a
// fault with its position, playback stopped, nothing skipped, nothing
// restarted.
func TestAFailureAfterContentPlayedIsAFault(t *testing.T) {
	h := startHarness(t)
	ids, s := h.playing(0, "a", "b")
	s.position(s.first.ID, 4410)
	s.fault(Fault{Reason: ReasonDecodeFailed, Detail: "a bad frame", Occurrence: s.first.ID, Frames: 4410})
	sn := h.waitSnap("stopped", func(sn *Snapshot) bool { return sn.State() == Stopped })
	fs := sn.Faults()
	if len(fs) != 1 || fs[0].Reason != ReasonDecodeFailed || fs[0].Entry != ids[0] || fs[0].Frames != 4410 {
		t.Fatalf("faults %+v", fs)
	}
	if sn.LastStop().Reason != ReasonFault || len(sn.Skips()) != 0 || h.be.starts() != 1 || sn.Selected() != ids[0] {
		t.Fatalf("last stop %+v, skips %v, %d sessions, selected %d", sn.LastStop(), sn.Skips(), h.be.starts(), sn.Selected())
	}
}

// TestFaultsAreBounded pins the record's bound.
func TestFaultsAreBounded(t *testing.T) {
	h := startHarness(t)
	h.insert("a")
	h.available()
	for i := 0; i < MaxFaults+3; i++ {
		h.must("play", h.eng.Play(bg))
		s := h.be.nth(i + 1)
		s.audible(s.first.ID)
		s.fault(Fault{Reason: ReasonXrun, Occurrence: s.first.ID, Frames: int64(i)})
		h.waitSnap("stopped", func(sn *Snapshot) bool {
			return sn.State() == Stopped && len(sn.Faults()) > 0 && sn.Faults()[len(sn.Faults())-1].Frames == int64(i)
		})
	}
	fs := h.snap().Faults()
	if len(fs) != MaxFaults || fs[0].Frames != 3 {
		t.Fatalf("%d faults kept from %d, want the last %d", len(fs), fs[0].Frames, MaxFaults)
	}
}
