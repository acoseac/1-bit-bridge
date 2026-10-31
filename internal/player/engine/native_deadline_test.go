package engine

import (
	"context"
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
			// A timer the advance fired has posted its message, and this
			// position is handled after it: messages reach the owner in order.
			// A command's reply is no such barrier, since the owner takes a
			// command and a message both waiting in either order (B605).
			m := h.appliedMark()
			s.ev.Position(s.id, Position{})
			h.waitApplied("handled position", m)
			if _, ok := h.snap().ReleaseUnconfirmed(); ok {
				t.Fatal("configured PCM deadline ended a legitimate DSD pause")
			}
			h.clk.Advance(29 * time.Second)
			sn := h.waitSnap("native pause overdue at 32s", func(s *Snapshot) bool { _, ok := s.ReleaseUnconfirmed(); return ok })
			if info, _ := sn.ReleaseUnconfirmed(); info.Session != s.id || !info.Paused || sn.State() != Paused {
				t.Fatalf("at 32s: %s, unconfirmed %+v; want session %d's own release of its pause unconfirmed, still paused", sn.State(), info, s.id)
			}
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

// TestNativePausedTransitionRearmsDeadline pins the deadline of a transport
// installed during a pause: the occurrence holding the transport changes, to
// native or back to PCM, while the session is paused, and the pause's own
// release is owed at the new transport's effective PauseRelease plus
// ReleaseTimeout, whatever the previous transport's timer said. A second
// before that deadline the engine is still paused, with no release
// unconfirmed and nothing stopped; at it the paused session's own release is
// overdue (ReleaseInfo.Paused), still paused. Any other unconfirmed release
// is not this deadline: a session released when a call timed out, its
// ordinary release then overdue, would pass a check for any (Codex on B605).
//
// The session's Prepare of b answers at once, but its answer can still be on
// its way to the owner when the test moves on. Each case runs twice: as the
// scheduler leaves that Prepare, and with its answer held until the test is
// about to move the clock or its deadline has passed, the latest a scheduler
// could leave it (B605: CI's race leg, twice in six runs).
func TestNativePausedTransitionRearmsDeadline(t *testing.T) {
	for _, configured := range []time.Duration{time.Second, time.Minute} {
		for _, toNative := range []bool{true, false} {
			for _, held := range []bool{false, true} {
				name := fmt.Sprintf("%s/%t", configured, toNative)
				if held {
					name += "/the Prepare answer held"
				}
				t.Run(name, func(t *testing.T) {
					h := newHarness(t)
					h.tweak = func(c *Config) { c.PauseRelease = configured }
					h.boot()
					answer := newGate()
					if held {
						h.be.script(func() {
							h.be.onPrepare = func(ctx context.Context, _ *fakeSession, _ OccurrenceID, _ *Occurrence) error {
								_ = answer.wait(ctx, h.be, true)
								return nil
							}
						})
					}
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
					// The clock moves past the Prepare's deadline only once the
					// owner has taken its answer: one still on its way then expires
					// and releases the session with backend_timeout (B605).
					answer.open()
					h.waitApplied("handled call prepare", 0)
					h.clk.Advance(effective + ReleaseTimeout - time.Second)
					// Every message the advance's timers posted is handled before
					// this position, which changes nothing: messages reach the owner
					// in order, where a command can be taken before them.
					m := h.appliedMark()
					s.ev.Position(s.id, Position{})
					h.waitApplied("handled position", m)
					sn := h.snap()
					if _, overdue := sn.ReleaseUnconfirmed(); overdue || sn.State() != Paused || sn.LastStop().Reason != "" {
						t.Fatalf("a second before the effective deadline: %s, a release unconfirmed %v, last stop %+v; want still paused, nothing unconfirmed or stopped", sn.State(), overdue, sn.LastStop())
					}
					h.clk.Advance(time.Second)
					sn = h.waitSnap("the effective paused transition deadline", func(sn *Snapshot) bool {
						_, overdue := sn.ReleaseUnconfirmed()
						return overdue || sn.State() != Paused
					})
					if info, _ := sn.ReleaseUnconfirmed(); info.Session != s.id || !info.Paused || sn.State() != Paused || sn.LastStop().Reason != "" {
						t.Fatalf("at the effective deadline: %s, unconfirmed %+v, last stop %+v; want session %d's own release of its pause unconfirmed, still paused, nothing stopped", sn.State(), info, sn.LastStop(), s.id)
					}
				})
			}
		}
	}
}
