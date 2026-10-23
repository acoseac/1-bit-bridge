package engine

import "testing"

// TestTheOutputStartsUnknownAndRefusesPlay pins the output's first state.
func TestTheOutputStartsUnknownAndRefusesPlay(t *testing.T) {
	h := startHarness(t)
	h.insert("a")
	if out := h.snap().Output(); out.Known || out.Available {
		t.Fatalf("output %+v before any report, want unknown", out)
	}
	h.refused("play while the output is unknown", h.eng.Play(bg), ReasonOutputUnknown)
	h.available()
	h.must("play once it is available", h.eng.Play(bg))
}

// TestTheOutputGoingAwayStopsAndFreezes pins unavailability, and a change
// of the bound unit, while playing and while paused: playback stops, the
// position stays where it was, and the snapshot says why.
func TestTheOutputGoingAwayStopsAndFreezes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		paused bool
		report OutputReport
		reason Reason
	}{
		{"unavailable while playing", false, OutputReport{Available: false, Reason: "device_missing"}, ReasonOutputUnavailable},
		{"unavailable while paused", true, OutputReport{Available: false, Reason: "device_missing"}, ReasonOutputUnavailable},
		{"the unit changing while playing", false, OutputReport{Available: true, UnitChanged: true}, ReasonUnitChanged},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := startHarness(t)
			_, s := h.playing(0, "a", "b")
			s.position(s.first.ID, 88200)
			h.waitSnap("the position", func(sn *Snapshot) bool { return sn.Position().Frames == 88200 })
			if tc.paused {
				h.must("pause", h.eng.Pause(bg))
			}
			h.report(tc.report)
			sn := h.waitSnap("stopped", func(sn *Snapshot) bool { return sn.State() == Stopped })
			if sn.LastStop().Reason != tc.reason || sn.Position().Frames != 88200 || !s.isAcked() {
				t.Fatalf("last stop %+v, position %+v", sn.LastStop(), sn.Position())
			}
		})
	}
}

// TestAStaleOutputReportIsDiscarded pins the report's generation.
func TestAStaleOutputReportIsDiscarded(t *testing.T) {
	h := startHarness(t)
	h.report(OutputReport{Generation: 5, Available: true})
	h.report(OutputReport{Generation: 3, Available: false, Reason: "stale"})
	h.report(OutputReport{Generation: 5, Available: false, Reason: "the same generation"})
	if out := h.snap().Output(); !out.Available || out.Generation != 5 {
		t.Fatalf("output %+v, want generation 5 available", out)
	}
	h.report(OutputReport{Generation: 6, Available: false, Reason: "device_missing"})
	if out := h.snap().Output(); out.Available || out.Reason != "device_missing" {
		t.Fatalf("output %+v after a newer report", out)
	}
}

// TestAUnitChangeWhileStoppedDiscardsPreparedWork pins what the output's
// change drops when nothing plays: the backend's prepared work, and a play
// waiting on a release.
func TestAUnitChangeWhileStoppedDiscardsPreparedWork(t *testing.T) {
	h := startHarness(t)
	ids, _ := h.playing(0, "a", "b")
	h.must("stop", h.eng.Stop(bg))
	h.report(OutputReport{Available: true, UnitChanged: true})
	h.be.waitFor("the discard", func() bool { return h.be.discards == 1 })

	h.must("play", h.eng.PlayID(bg, ids[1]))
	g := holdReleases(h)
	next := async(func() error { return h.eng.Previous(bg) })
	h.waitSnap("play pending", func(sn *Snapshot) bool { return sn.State() == Releasing && sn.Pending() == IntentPlay })
	h.report(OutputReport{Available: true, UnitChanged: true})
	h.waitSnap("the pending play dropped", func(sn *Snapshot) bool { return sn.Pending() == IntentStop })
	g.open()
	if err := h.result("previous", next); ReasonOf(err) != ReasonUnitChanged {
		t.Fatalf("the dropped play answered %v, want %s", err, ReasonUnitChanged)
	}
	h.waitSnap("stopped", func(sn *Snapshot) bool { return sn.State() == Stopped })
	if n := h.be.starts(); n != 2 {
		t.Fatalf("%d sessions: a play waiting on the release survived the unit change", n)
	}
}

// TestTheOutputComingBackPlaysNothing pins never autoplay.
func TestTheOutputComingBackPlaysNothing(t *testing.T) {
	h := startHarness(t)
	h.playing(0, "a", "b")
	h.report(OutputReport{Available: false, Reason: "device_missing"})
	h.waitSnap("stopped", func(sn *Snapshot) bool { return sn.State() == Stopped })
	h.available()
	if sn := h.snap(); sn.State() != Stopped || h.be.starts() != 1 {
		t.Fatalf("the output's return: %s with %d sessions", sn.State(), h.be.starts())
	}
}
