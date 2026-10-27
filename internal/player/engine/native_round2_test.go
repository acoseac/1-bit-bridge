package engine

import (
	"context"
	"testing"
)

func TestNativeEarlyReleaseStopsTimer(t *testing.T) {
	h := startHarness(t)
	_, s := h.playing(0, "a")
	m := h.appliedMark()
	s.ev.(NativePlanEvents).NativePlan(s.id, s.first.ID, NativePlan{PolicyID: "native"})
	s.ev.Position(s.id, Position{})
	h.waitApplied("handled position", m)
	h.be.script(func() {
		h.be.onRelease = func(_ context.Context, fs *fakeSession) error {
			fs.ev.Fault(fs.id, Fault{Reason: ReasonReleaseUnconfirmed})
			return ErrReleaseUnconfirmed
		}
	})
	h.must("stop", h.eng.Stop(bg))
	h.waitSnap("unconfirmed", func(sn *Snapshot) bool { _, ok := sn.ReleaseUnconfirmed(); return ok })
	h.must("barrier", h.eng.SetRepeat(bg, true))
	if heard, ok := h.snap().Audible(); ok && heard.Occurrence != 0 {
		t.Error("dead session remained audible after failed release")
	}
	if h.eng.o.sessions[s.id].releaseTimer != nil {
		t.Fatal("early failed release left timer armed")
	}
	h.clk.Advance(ReleaseTimeout)
	h.must("barrier", h.eng.SetRepeat(bg, false))
	if len(h.snap().Faults()) != 1 {
		t.Fatal("release failure recorded more than once")
	}
}

func TestNativeFaultReleaseCause(t *testing.T) {
	h := newHarness(t)
	var rec *nativeRecorder
	h.tweak = func(c *Config) {
		rec = &nativeRecorder{Backend: c.Backend, requests: make(chan ReleaseRequest, 8), escalations: make(chan ReleaseRequest, 8)}
		c.Backend = rec
	}
	h.boot()
	_, s := h.playing(0, "a")
	s.ev.(NativePlanEvents).NativePlan(s.id, s.first.ID, NativePlan{PolicyID: "native"})
	s.ev.Fault(s.id, Fault{Reason: ReasonXrun})
	r := <-rec.requests
	if r.Cause != ReleaseFault {
		t.Fatalf("fault release cause = %s", r.Cause)
	}
}

type pcmBudgetBackend struct{ Backend }

func (pcmBudgetBackend) OwnsOccurrenceReservations() bool { return true }
func TestPCMLookaheadHasNoNativeBudget(t *testing.T) {
	h := newHarness(t)
	h.tweak = func(c *Config) { c.Backend = pcmBudgetBackend{c.Backend}; c.OccurrenceLimit = 4 }
	h.boot()
	_, s := h.playing(0, "pcm")
	h.must("repeat", h.eng.SetRepeat(bg, true))
	h.must("pause PCM", h.eng.Pause(bg))
	after := s.first.ID
	for range 8 {
		next := s.prepared(after, 1)
		s.submitted(next.ID)
		after = next.ID
	}
	h.must("barrier", h.eng.SetRepeat(bg, false))
	if len(h.snap().Faults()) != 0 {
		t.Fatal("PCM lookahead acquired native refusal")
	}
}

func TestNativeRetiringOccurrenceBounds(t *testing.T) {
	for _, kind := range []string{"position", "audible", "details"} {
		t.Run(kind, func(t *testing.T) {
			e := &Engine{}
			a, b, c := &occ{id: 1}, &occ{id: 2}, &occ{id: 3}
			s := &session{id: 1, phase: sessReleasing, retiring: &retiringLedger{token: ReleaseToken{Session: 1, Generation: 1}, chain: []*occ{a, b, c}}}
			e.o.sessions = map[SessionID]*session{1: s}
			e.hear(b)
			e.o.pos = Position{Occurrence: 2, Frames: 1}
			token := ReleaseToken{Session: 1, Generation: 1, Cutoff: 32}
			e.onRetiring(token, NativeEvent{Kind: "cutoff", Prefixes: []NativePrefix{{Occurrence: 1, End: 31}, {Occurrence: 2, Start: 31, End: 32}}})
			switch kind {
			case "position":
				e.onRetiring(token, NativeEvent{Kind: "position", Position: Position{Occurrence: 2, Frames: 31}})
				if e.o.pos.Frames != 1 {
					t.Fatal("sink-wide cutoff admitted position beyond B's one-bit prefix")
				}
			case "audible":
				e.onRetiring(token, NativeEvent{Kind: "audible", Occurrence: 3})
				if e.o.audible.Occurrence != 2 {
					t.Fatal("unaccepted occurrence became audible")
				}
			case "details":
				e.onRetiring(token, NativeEvent{Kind: "details", Occurrence: 3, Details: Details{Bits: 1}})
				if c.details != nil {
					t.Fatal("unaccepted occurrence acquired retiring details")
				}
			}
		})
	}
}
