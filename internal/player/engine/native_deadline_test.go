package engine

import (
	"fmt"
	"testing"
	"time"
)

func TestNativeEffectivePauseDeadline(t *testing.T) {
	for _, configured := range []time.Duration{time.Second, time.Minute} {
		t.Run(configured.String(), func(t *testing.T) {
			h := newHarness(t)
			h.tweak = func(c *Config) { c.PauseRelease = configured }
			h.boot()
			_, s := h.playing(0, "dsd")
			mark := h.appliedMark()
			s.ev.(DetailsEvents).Details(s.id, s.first.ID, Details{Bits: 1, Codec: "DSD", RateHz: 2822400, Channels: 2, Lossless: true})
			h.waitApplied("handled details", mark)
			h.waitApplied("handled call prepare", 0)
			h.must("pause", h.eng.Pause(bg))
			h.clk.Advance(3 * time.Second)
			// A command reply is an owner barrier after the timer's queued event.
			h.must("pause barrier", h.eng.Pause(bg))
			if _, ok := h.snap().ReleaseUnconfirmed(); ok {
				t.Fatal("configured PCM deadline ended a legitimate DSD pause")
			}
			h.clk.Advance(29 * time.Second)
			h.waitSnap("native pause overdue at 32s", func(s *Snapshot) bool { _, ok := s.ReleaseUnconfirmed(); return ok })
		})
	}
}

func TestNativePauseDeadlineTransitions(t *testing.T) {
	for _, configured := range []time.Duration{time.Second, time.Minute} {
		t.Run(configured.String(), func(t *testing.T) {
			h := newHarness(t)
			h.tweak = func(c *Config) { c.PauseRelease = configured }
			h.boot()
			_, s := h.playing(0, "pcm", "dsd", "pcm2")
			next := s.prepared(s.first.ID, 1)
			for i, native := range []bool{false, true, false} {
				o := s.first.ID
				if i > 0 {
					o = next.ID
				}
				var plan *NativePlan
				if native {
					plan = &NativePlan{PolicyID: "fixed"}
				}
				mark := h.appliedMark()
				s.ev.(CurrentOccurrenceEvents).CurrentOccurrence(s.id, o, plan)
				h.waitApplied("handled current occurrence", mark)
				if got := h.eng.effectivePauseRelease(h.eng.o.sessions[s.id]); got != map[bool]time.Duration{true: 30 * time.Second, false: configured}[native] {
					t.Fatalf("effective deadline %v", got)
				}
			}
		})
	}
}

func TestNativePausedTransitionRearmsDeadline(t *testing.T) {
	for _, configured := range []time.Duration{time.Second, time.Minute} {
		for _, toNative := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/%t", configured, toNative), func(t *testing.T) {
				h := newHarness(t)
				h.tweak = func(c *Config) { c.PauseRelease = configured }
				h.boot()
				_, s := h.playing(0, "a", "b")
				next := s.prepared(s.first.ID, 1)
				set := func(id OccurrenceID, native bool) {
					var p *NativePlan
					if native {
						p = &NativePlan{PolicyID: "same"}
					}
					m := h.appliedMark()
					s.ev.(CurrentOccurrenceEvents).CurrentOccurrence(s.id, id, p)
					h.waitApplied("handled current occurrence", m)
				}
				set(s.first.ID, !toNative)
				h.must("pause", h.eng.Pause(bg))
				set(next.ID, toNative)
				effective := configured
				if toNative {
					effective = 30 * time.Second
				}
				h.clk.Advance(effective + ReleaseTimeout - time.Second)
				h.must("barrier", h.eng.Pause(bg))
				if _, bad := h.snap().ReleaseUnconfirmed(); bad {
					t.Fatal("previous transport deadline ended current pause")
				}
				h.clk.Advance(time.Second)
				h.waitSnap("effective paused transition deadline", func(sn *Snapshot) bool { _, bad := sn.ReleaseUnconfirmed(); return bad })
			})
		}
	}
}
