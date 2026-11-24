package alsapcm_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
	"github.com/acoseac/1-bit-bridge/internal/player/dsdstate"
	"github.com/acoseac/1-bit-bridge/internal/player/playback"
	"github.com/acoseac/1-bit-bridge/internal/player/playback/playbacktest"
	"github.com/acoseac/1-bit-bridge/internal/player/policy"
)

// The player's DSD check's backend half of B728 B12 PR 4 (PR 4a) through the
// backend and the real sinks over the fake kernel: the check judged by the
// path that admitted it, the listener's answer written by a choice's path,
// the check's member of the read side published at each transition, a stop's
// answer of how its cleanup ended, the check's handle, and the precheck the
// routes take from the backend's own admission. Each listener's answer is
// written through AnswerCheck, the route PR 4b adds being its one caller.

// answer asks the answer heard of the check id names, for the DAC the backend
// publishes now, by the read side it publishes.
func (r *checkRig) answer(id, heard string) playback.ChoiceResult {
	r.t.Helper()
	r.be.RefreshDSD(true)
	p := r.be.Policy()
	if p.DSD == nil || p.DSD.DAC == nil {
		r.t.Fatalf("the backend publishes no DAC: %+v", p.DSD)
	}
	ctx, cancel := within()
	defer cancel()
	return r.be.AnswerCheck(ctx, playback.AnswerRequest{DACKey: p.DSD.DAC.Key, DACToken: p.DSD.DAC.Token, CheckID: id, Heard: heard})
}

// completed plays a native check at DSD64 to complete, the DAC played freely
// and held again after, and answers its record.
func (r *checkRig) completed() playback.CheckStatus {
	r.t.Helper()
	r.k.Free()
	r.admitted(dsdstate.TransportNative, playbacktest.DSD64)
	st := r.endedAs(playback.CheckComplete, "", "the check played to complete")
	r.k.Hold()
	return st
}

// member is the check's member of the read side as the backend publishes it
// now, nil for none.
func (r *checkRig) member() *playback.DSDCheck {
	p := r.be.Policy()
	if p.DSD == nil || p.DSD.DAC == nil {
		return nil
	}
	return p.DSD.DAC.Check
}

// answerOf is the listener's answer the state holds for native at rate under
// the test DAC's firmware, in the entry of the test DAC's model and maker.
func answerOf(st dsdstate.State, rate int) (dsdstate.Answer, int) {
	d, _ := st.DAC(playbacktest.DACKey(), playbacktest.DACStamp())
	var found dsdstate.Answer
	n := 0
	for _, a := range d.Listener {
		if a.Transport == dsdstate.TransportNative && a.BitRate == rate && a.BCDDevice == playbacktest.DACBCD {
			found, n = a, n+1
		}
	}
	return found, n
}

// refreshedSet refreshes the policy and fails the test unless it published a
// new set.
func refreshedSet(r *checkRig, what string) {
	r.t.Helper()
	g := r.be.Policy().Generation
	r.be.RefreshDSD(true)
	if now := r.be.Policy().Generation; now == g {
		r.t.Fatalf("%s left the set at generation %d: the case needs a new set", what, g)
	}
}

// heardNative is native chosen, with a listener's clean answer for native at
// DSD64 under the test DAC's firmware.
func heardNative() dsdstate.State {
	st := playbacktest.ChoiceState(dsdstate.ModeNative)
	st.DACs[0].Listener = []dsdstate.Answer{{Transport: dsdstate.TransportNative, BitRate: playbacktest.DSD64, BCDDevice: playbacktest.DACBCD, Answer: dsdstate.AnswerClean, On: "2026-11-23"}}
	return st
}

// capAt is st with the DAC's cap at max.
func capAt(st dsdstate.State, max int) dsdstate.State {
	st = st.Clone()
	st.DACs[0].Choice.MaxBitRate = max
	return st
}

// TestACheckIsJudgedByThePathThatAdmittedIt (B728 B12 PR 4, item 6): on a
// generic board's IDs under an explicit native, a check the candidate of a
// native_check_required refusal admitted is kept only while the candidate
// published at its rate is its plan, and one the set's plan admitted only
// while policy.Reconcile keeps it: neither is kept through the other's. A
// candidate's check ends policy_changed once a listener's clean answer makes
// its plan the set's own, and a plan's check once its rate waits for a
// listener's answer again, its plan now that refusal's candidate; across a
// new set that keeps what admitted it, each plays to complete.
func TestACheckIsJudgedByThePathThatAdmittedIt(t *testing.T) {
	native := playbacktest.ChoiceState(dsdstate.ModeNative)
	for _, c := range []struct {
		name          string
		before, after dsdstate.State
		phase         playback.CheckPhase
		reason        playback.CheckReason
	}{
		{"a candidate's check once a clean answer makes its plan the set's", native, heardNative(), playback.CheckFailed, playback.CheckPolicyChanged},
		{"the set's plan's check once its rate waits for a listener's answer again", heardNative(), native, playback.CheckFailed, playback.CheckPolicyChanged},
		{"the set's plan's check across a set that keeps its plan", heardNative(), capAt(heardNative(), playbacktest.DSD128), playback.CheckComplete, ""},
		{"a candidate's check across a set that keeps its candidate", native, capAt(native, playbacktest.DSD128), playback.CheckComplete, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newCheckRig(t, c.before, onBoard)
			r.playingCheck(dsdstate.TransportNative)
			r.store.Set(c.after)
			refreshedSet(r, c.name)
			r.k.Free()
			r.endedAs(c.phase, c.reason, c.name)
		})
	}
}

// TestOnlyACompleteCheckIsAnswered: a listener's answer is written only for
// a check of this boot that completed, at most CheckAnswerable ago, and not
// answered already, its token the reading's: an ID no check has, a check
// that did not complete (stopped), one completed eleven minutes ago and one
// answered already are each check_not_answerable, nothing written, each with
// the pair published; a complete one's answer is written, the check then
// answered, published so, its answer dated by the policy's clock; one
// completed exactly CheckAnswerable ago is still answered.
func TestOnlyACompleteCheckIsAnswered(t *testing.T) {
	native := playbacktest.ChoiceState(dsdstate.ModeNative)
	var offset atomic.Int64
	clock := func(p *playback.DSDPolicy) {
		p.Now = func() time.Time { return playbacktest.T0.Add(time.Duration(offset.Load())) }
	}
	refused := func(r *checkRig, id, what string) {
		t.Helper()
		writes := r.store.Writes()
		res := r.answer(id, dsdstate.AnswerClean)
		if res.Outcome != playback.ChoiceCheckNotAnswerable || !res.Epoch || res.Err != nil {
			t.Errorf("%s: the answer %+v; want check_not_answerable with the pair published", what, res)
		}
		if r.store.Writes() != writes {
			t.Errorf("%s: an answer not taken wrote the state", what)
		}
	}
	t.Run("an ID no check of this boot has", func(t *testing.T) {
		r := newCheckRig(t, native, clock)
		refused(r, "0123456789abcdef", "no check")
	})
	t.Run("a check that did not complete", func(t *testing.T) {
		r := newCheckRig(t, native, clock)
		c := r.playingCheck(dsdstate.TransportNative)
		close(c.stop)
		st := r.endedAs(playback.CheckStopped, playback.CheckStop, "the stopped check")
		refused(r, st.ID, "a stopped check")
	})
	t.Run("a complete check, then again", func(t *testing.T) {
		offset.Store(0)
		r := newCheckRig(t, native, clock)
		st := r.completed()
		res := r.answer(st.ID, dsdstate.AnswerClean)
		if res.Outcome != playback.ChoiceWritten || !res.Epoch || res.Err != nil {
			t.Fatalf("the answer of a complete check: %+v; want written", res)
		}
		a, n := answerOf(r.store.State(), playbacktest.DSD64)
		if n != 1 || a.Answer != dsdstate.AnswerClean || a.On != playbacktest.T0.UTC().Format(time.DateOnly) {
			t.Errorf("the state holds %d answers, %+v; want one clean, dated %s", n, a, playbacktest.T0.Format(time.DateOnly))
		}
		if got, _ := r.be.CheckStatus(); got.Phase != playback.CheckAnswered {
			t.Errorf("the answered check is %s; want answered", got.Phase)
		}
		if m := res.Policy.DSD.DAC.Check; m == nil || m.ID != st.ID || m.Phase != playback.CheckAnswered {
			t.Errorf("the answer's pair publishes the check %+v; want it answered", m)
		}
		refused(r, st.ID, "a check answered already")
	})
	t.Run("a check completed eleven minutes ago", func(t *testing.T) {
		offset.Store(0)
		r := newCheckRig(t, native, clock)
		st := r.completed()
		offset.Store(int64(11 * time.Minute))
		refused(r, st.ID, "a check completed eleven minutes ago")
	})
	t.Run("a check completed exactly CheckAnswerable ago", func(t *testing.T) {
		offset.Store(0)
		r := newCheckRig(t, native, clock)
		st := r.completed()
		offset.Store(int64(playback.CheckAnswerable))
		if res := r.answer(st.ID, dsdstate.AnswerNothing); res.Outcome != playback.ChoiceWritten {
			t.Errorf("the answer at CheckAnswerable: %+v; want written", res)
		}
	})
}

// TestANoiseAnswerRefusesTheTransportInEveryMode: a noise answer replaces the
// clean one for native at the check's rate under its firmware, and takes
// native's first use away in the same write, DoP's kept: native at that rate
// is then listener_rejected under native, and under auto plays no native
// tuple there, while native's first use reads pending; nothing answers
// likewise.
func TestANoiseAnswerRefusesTheTransportInEveryMode(t *testing.T) {
	for _, heard := range []string{dsdstate.AnswerNoise, dsdstate.AnswerNothing} {
		t.Run(heard, func(t *testing.T) {
			r := newCheckRig(t, heardNative(), nil)
			st := r.completed()
			if res := r.answer(st.ID, heard); res.Outcome != playback.ChoiceWritten {
				t.Fatalf("the %s answer: %+v; want written", heard, res)
			}
			state := r.store.State()
			if a, n := answerOf(state, playbacktest.DSD64); n != 1 || a.Answer != heard {
				t.Errorf("the state holds %d answers for native at DSD64, %+v; want the %s one alone", n, a, heard)
			}
			d, _ := state.DAC(playbacktest.DACKey(), playbacktest.DACStamp())
			if slices.ContainsFunc(d.FirstUse, func(f dsdstate.FirstUse) bool { return f.Transport == dsdstate.TransportNative }) ||
				!slices.ContainsFunc(d.FirstUse, func(f dsdstate.FirstUse) bool { return f.Transport == dsdstate.TransportDoP }) {
				t.Errorf("first use after the %s answer: %+v; want native's taken away, DoP's kept", heard, d.FirstUse)
			}
			p := r.be.Policy()
			if !slices.Contains(p.DSD.Refused, playback.DSDRefusal{BitRate: playbacktest.DSD64, Reason: string(policy.ReasonListenerRejected)}) {
				t.Errorf("under native the published refusals %+v; want DSD64 listener_rejected", p.DSD.Refused)
			}
			for _, f := range p.DSD.DAC.FirstUse {
				if f.Transport == dsdstate.TransportNative && f.State != playback.FirstUsePending {
					t.Errorf("native's first use reads %s; want pending", f.State)
				}
			}
			auto := r.store.State()
			auto.DACs[0].Choice.Mode = dsdstate.ModeAuto
			r.store.Set(auto)
			r.be.RefreshDSD(true)
			for _, tp := range r.be.Policy().Tuples {
				if tp.Tuple.BitRate == playbacktest.DSD64 && tp.Tuple.Transport == "" {
					t.Errorf("under auto native plays at DSD64 after the %s answer: %+v", heard, tp)
				}
			}
		})
	}
}

// TestAnAnswerIsWrittenOnlyWhileStopped: while a session plays, a listener's
// answer is refused playback_not_stopped by the configuration lease, nothing
// written and the check still answerable; stopped, it is written.
func TestAnAnswerIsWrittenOnlyWhileStopped(t *testing.T) {
	r := newCheckRig(t, playbacktest.ChoiceState(dsdstate.ModeNative), nil)
	st := r.completed()
	r.files.add("pcm", wav(48000, 24, pattern(1, 48000, 24)))
	r.insert("pcm")
	if err := r.eng.Play(bg); err != nil {
		t.Fatal(err)
	}
	writes := r.store.Writes()
	if res := r.answer(st.ID, dsdstate.AnswerClean); res.Outcome != playback.ChoicePlaybackNotStopped {
		t.Errorf("an answer while a session plays: %+v; want playback_not_stopped", res)
	}
	if r.store.Writes() != writes {
		t.Error("an answer refused while a session played wrote the state")
	}
	r.stop()
	if res := r.answer(st.ID, dsdstate.AnswerClean); res.Outcome != playback.ChoiceWritten {
		t.Errorf("the answer once stopped: %+v; want written", res)
	}
}

// TestALandedAnswerHoldsTheSetAndFencesTheWrites: an answer whose write
// landed and is not known to be durable is persist_uncertain, the check
// answered all the same, the landed state kept as a landed choice's is: the
// set held to what the two states play alike (a noise answer's native at
// DSD64 refused dsd_choice_uncertain) and every later write, a choice's,
// persist_uncertain; and a choice's write that landed fences a later answer,
// the check left answerable.
func TestALandedAnswerHoldsTheSetAndFencesTheWrites(t *testing.T) {
	landed := fmt.Errorf("the barrier failed: %w", atomicwrite.ErrLanded)
	t.Run("the answer landed", func(t *testing.T) {
		r := newCheckRig(t, heardNative(), nil)
		st := r.completed()
		r.store.Land(landed)
		res := r.answer(st.ID, dsdstate.AnswerNoise)
		if res.Outcome != playback.ChoicePersistUncertain || !errors.Is(res.Err, atomicwrite.ErrLanded) {
			t.Errorf("the landed answer: %+v; want persist_uncertain with its cause", res)
		}
		if got, _ := r.be.CheckStatus(); got.Phase != playback.CheckAnswered {
			t.Errorf("the check whose answer landed is %s; want answered", got.Phase)
		}
		if !slices.Contains(r.be.Policy().DSD.Refused, playback.DSDRefusal{BitRate: playbacktest.DSD64, Reason: string(policy.ReasonChoiceUncertain)}) {
			t.Errorf("the published refusals %+v; want DSD64 dsd_choice_uncertain", r.be.Policy().DSD.Refused)
		}
		if w := r.write(dsdstate.ModeConvert); w.Outcome != playback.ChoicePersistUncertain {
			t.Errorf("a choice's write after the landed answer: %+v; want persist_uncertain", w)
		}
	})
	t.Run("a choice's write landed", func(t *testing.T) {
		r := newCheckRig(t, playbacktest.ChoiceState(dsdstate.ModeNative), nil)
		st := r.completed()
		r.store.Land(landed)
		if w := r.write(dsdstate.ModeConvert); w.Outcome != playback.ChoicePersistUncertain {
			t.Fatalf("the landed choice: %+v; want persist_uncertain", w)
		}
		if res := r.answer(st.ID, dsdstate.AnswerClean); res.Outcome != playback.ChoicePersistUncertain {
			t.Errorf("an answer after a landed choice: %+v; want persist_uncertain", res)
		}
		if got, _ := r.be.CheckStatus(); got.Phase != playback.CheckComplete {
			t.Errorf("the check an answer was fenced from is %s; want complete", got.Phase)
		}
	})
}

// TestTheAnswerGoesToTheIdentityTheCheckOpened: the answer is written to the
// entry of the model and maker the check was admitted for and opened, under
// its firmware, never to another maker's entry of the same model.
func TestTheAnswerGoesToTheIdentityTheCheckOpened(t *testing.T) {
	r := newCheckRig(t, identicalElsewhere(), nil)
	st := r.completed()
	if st.Opened == nil || !st.Opened.Same(checkIdentity()) {
		t.Fatalf("the check opened %+v; want the test DAC's identity", st.Opened)
	}
	if res := r.answer(st.ID, dsdstate.AnswerClean); res.Outcome != playback.ChoiceWritten {
		t.Fatalf("the answer: %+v; want written", res)
	}
	state := r.store.State()
	if _, n := answerOf(state, playbacktest.DSD64); n != 1 {
		t.Errorf("the check's entry holds %d answers; want one", n)
	}
	other, _ := state.DAC(playbacktest.DACKey(), dsdstate.StampOf("Another Maker"))
	if len(other.Listener) != 0 {
		t.Errorf("another maker's entry of the model holds %+v; want no answer", other.Listener)
	}
}

// TestAFirmwareChangeBeforeTheAnswerMakesItUnanswerable: between a check's
// chime and its answer the unit's firmware changes, so its token moves: an
// answer with the check's token is dac_changed, and one with the new reading's
// token and the check's ID check_not_answerable, nothing written.
func TestAFirmwareChangeBeforeTheAnswerMakesItUnanswerable(t *testing.T) {
	r := newCheckRig(t, playbacktest.ChoiceState(dsdstate.ModeNative), nil)
	st := r.completed()
	m := playbacktest.DACUnit()
	m.BCDDevice = "0200"
	r.box.SetUnit(card, m)
	writes := r.store.Writes()
	ctx, cancel := within()
	defer cancel()
	old := r.be.AnswerCheck(ctx, playback.AnswerRequest{DACKey: r.be.Policy().DSD.DAC.Key, DACToken: st.Token, CheckID: st.ID, Heard: dsdstate.AnswerClean})
	if old.Outcome != playback.ChoiceDACChanged {
		t.Errorf("an answer with the check's token after the firmware changed: %+v; want dac_changed", old)
	}
	if res := r.answer(st.ID, dsdstate.AnswerClean); res.Outcome != playback.ChoiceCheckNotAnswerable {
		t.Errorf("an answer with the new reading's token: %+v; want check_not_answerable", res)
	}
	if r.store.Writes() != writes {
		t.Error("an unanswerable answer wrote the state")
	}
}

// TestEveryCheckTransitionIsPublished: each of the check's transitions is a
// publication of the read side, its member's state as its record stands
// (preparing, playing, then complete or stopped, and answered; stopping, its
// cleanup not answered, is TestAStopsAnswerSaysHowItsCleanupEnded's), the
// publication moving and the policy generation not; a refresh's own
// publication keeps the member, and a read side naming another unit's token
// carries none.
func TestEveryCheckTransitionIsPublished(t *testing.T) {
	native := playbacktest.ChoiceState(dsdstate.ModeNative)
	published := func(r *checkRig, want playback.CheckPhase, reason playback.CheckReason, since uint64, gen uint64, what string) uint64 {
		t.Helper()
		p := r.be.Policy()
		m := r.member()
		if m == nil || m.Phase != want || m.Reason != reason || m.Transport != dsdstate.TransportNative || m.BitRate != playbacktest.DSD64 {
			t.Errorf("%s: the published check %+v; want %s %q", what, m, want, reason)
		}
		if p.Publication <= since || p.Generation != gen {
			t.Errorf("%s: publication %d (after %d), generation %d (was %d); want a later publication at the same generation", what, p.Publication, since, p.Generation, gen)
		}
		return p.Publication
	}
	t.Run("from preparing to answered", func(t *testing.T) {
		r := newCheckRig(t, native, nil)
		before := r.be.Policy()
		if r.member() != nil {
			t.Fatalf("a backend with no check publishes %+v", r.member())
		}
		c, p := r.preparingCheck()
		seq := published(r, playback.CheckPreparing, "", before.Publication, before.Generation, "admitted")
		p.letGo()
		r.k.Await(t, "the check's writer to fill the hardware buffer and wait on the DAC", runningAndWaiting)
		seq = published(r, playback.CheckPlaying, "", seq, before.Generation, "playing")
		r.be.RefreshDSD(true)
		if m := r.member(); m == nil || m.Phase != playback.CheckPlaying {
			t.Errorf("a refresh's publication carries %+v; want the playing check", m)
		}
		r.k.Free()
		r.endedAs(playback.CheckComplete, "", "the check")
		seq = published(r, playback.CheckComplete, "", seq, before.Generation, "complete")
		r.k.Hold()
		res := r.answer(c.st.ID, dsdstate.AnswerClean)
		if res.Outcome != playback.ChoiceWritten {
			t.Fatalf("the answer: %+v", res)
		}
		published(r, playback.CheckAnswered, "", seq, before.Generation, "answered")
	})
	t.Run("stopped", func(t *testing.T) {
		r := newCheckRig(t, native, nil)
		before := r.be.Policy()
		_, p := r.preparingCheck()
		seq := published(r, playback.CheckPreparing, "", before.Publication, before.Generation, "admitted")
		if cleanup, pol := r.be.StopCheck(bg); cleanup != playback.CheckCleanupConfirmed || pol.DSD.DAC.Check == nil || pol.DSD.DAC.Check.Phase != playback.CheckStopped {
			t.Errorf("StopCheck answered %s with %+v; want confirmed, the check stopped", cleanup, pol.DSD.DAC.Check)
		}
		p.letGo()
		r.joined("the stopped check")
		published(r, playback.CheckStopped, playback.CheckDeleted, seq, before.Generation, "stopped")
	})
	t.Run("another unit's token", func(t *testing.T) {
		r := newCheckRig(t, native, nil)
		r.completed()
		m := playbacktest.DACUnit()
		m.BCDDevice = "0200"
		r.box.SetUnit(card, m)
		r.be.RefreshDSD(true)
		if got := r.member(); got != nil {
			t.Errorf("the read side of another unit's token carries %+v; want no check", got)
		}
	})
}

// TestAStopsAnswerSaysHowItsCleanupEnded: StopCheck answers how the stopped
// check's own cleanup ended, with the pair published as it answers:
// confirmed; failed for a close that answers an error and for a cleanup that
// panicked (contained), the device fenced; pending past CheckStopWait while
// the close is held, the check published stopping; and none with no check.
func TestAStopsAnswerSaysHowItsCleanupEnded(t *testing.T) {
	native := playbacktest.ChoiceState(dsdstate.ModeNative)
	stopped := func(r *checkRig, cleanup playback.CheckCleanup, phase playback.CheckPhase, what string) {
		t.Helper()
		got, pol := r.be.StopCheck(bg)
		if got != cleanup {
			t.Errorf("%s: StopCheck answered %s; want %s", what, got, cleanup)
		}
		if m := pol.DSD.DAC.Check; m == nil || m.Phase != phase {
			t.Errorf("%s: StopCheck's pair publishes %+v; want the check %s", what, m, phase)
		}
	}
	alsapcm.RunEachCaseInAChild(t, "TestAStopsAnswerSaysHowItsCleanupEnded", map[string]func(t *testing.T){
		"none": func(t *testing.T) {
			r := newCheckRig(t, native, nil)
			if got, _ := r.be.StopCheck(bg); got != playback.CheckCleanupNone {
				t.Errorf("StopCheck with no check answered %s; want none", got)
			}
		},
		"confirmed": func(t *testing.T) {
			r := newCheckRig(t, native, nil)
			r.playingCheck(dsdstate.TransportNative)
			stopped(r, playback.CheckCleanupConfirmed, playback.CheckStopped, "a check whose close answers")
		},
		"a close that answers an error": func(t *testing.T) {
			r := newCheckRig(t, native, nil)
			r.dsdRig.mu.Lock()
			r.cleanup = "close"
			r.dsdRig.mu.Unlock()
			r.playingCheck(dsdstate.TransportNative)
			stopped(r, playback.CheckCleanupFailed, playback.CheckFailed, "a close that failed")
			r.checkFenced("a close that failed")
		},
		"a cleanup that panicked": func(t *testing.T) {
			r := newCheckRig(t, native, nil)
			in, gate := make(chan struct{}), make(chan struct{})
			r.dsdRig.mu.Lock()
			r.openNative = func(context.Context, alsapcm.DSDConfig) (*alsapcm.DSDSink, error) {
				close(in)
				<-gate
				return nil, panickyOpen{}
			}
			r.dsdRig.mu.Unlock()
			r.admitted(dsdstate.TransportNative, playbacktest.DSD64)
			awaitOrdered(t, in, "the check's open held")
			if bound := receiveIn(t, r.timers.arms[playback.CheckStopWait], "the preparation's bound"); !bound.stopped.Load() {
				t.Fatal("the preparation's bound is still armed at the open")
			}
			answer := make(chan playback.CheckCleanup, 1)
			go func() {
				got, _ := r.be.StopCheck(bg)
				answer <- got
			}()
			receiveIn(t, r.timers.arms[playback.CheckStopWait], "StopCheck's wait")
			close(gate)
			if got := receiveIn(t, answer, "StopCheck"); got != playback.CheckCleanupFailed {
				t.Errorf("StopCheck of a check whose cleanup panicked answered %s; want failed", got)
			}
			r.logs.panicLogged(t, backendPanicMessage, "the DSD check's cleanup")
		},
		"a close the kernel holds": func(t *testing.T) {
			r := newCheckRig(t, native, nil)
			r.playingCheck(dsdstate.TransportNative)
			parked, letClose := r.k.ParkNextClose()
			t.Cleanup(letClose)
			before := r.be.Policy()
			answer := make(chan playback.CheckCleanup, 1)
			go func() {
				got, pol := r.be.StopCheck(bg)
				if m := pol.DSD.DAC.Check; m == nil || m.Phase != playback.CheckStopping || pol.Publication <= before.Publication || pol.Generation != before.Generation {
					t.Errorf("StopCheck's pair past its wait publishes %+v at publication %d (was %d); want the check stopping, later, at the same generation", m, pol.Publication, before.Publication)
				}
				answer <- got
			}()
			awaitOrdered(t, parked, "the check's close held in the kernel")
			r.timers.fire(t, playback.CheckStopWait, "StopCheck's wait")
			if got := receiveIn(t, answer, "StopCheck"); got != playback.CheckCleanupPending {
				t.Errorf("StopCheck past its wait answered %s; want pending", got)
			}
			letClose()
			r.endedAs(playback.CheckStopped, playback.CheckDeleted, "the check once its close answered")
		},
	})
}

// TestEachCheckHasItsOwnHandle: Check's handle is its own check's: the
// record and the pair its admission made (preparing), its Done and its
// Cleanup, pending until that check's cleanup has answered and confirmed
// after, whatever a later check does.
func TestEachCheckHasItsOwnHandle(t *testing.T) {
	r := newCheckRig(t, playbacktest.ChoiceState(dsdstate.ModeNative), nil)
	ctx, cancel := within()
	defer cancel()
	req, stop := r.request(dsdstate.TransportNative, playbacktest.DSD64)
	r.k.Free()
	first, err := r.be.Check(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if m := first.Policy.DSD.DAC.Check; first.Status.Phase != playback.CheckPreparing || m == nil || m.ID != first.Status.ID || m.Phase != playback.CheckPreparing {
		t.Errorf("the handle's record %+v and pair %+v; want the check preparing in both", first.Status, m)
	}
	r.endedAs(playback.CheckComplete, "", "the first check")
	close(stop)
	select {
	case <-first.Done():
	default:
		t.Error("the first check's Done is open once it ended")
	}
	r.k.Hold()
	p := r.holdPreparation()
	second := r.admitted(dsdstate.TransportNative, playbacktest.DSD64)
	awaitOrdered(t, p.in, "the second check's preparation held")
	if got := first.Cleanup(); got != playback.CheckCleanupConfirmed {
		t.Errorf("the first check's cleanup reads %s while the second runs; want confirmed", got)
	}
	if second.st.ID == first.Status.ID {
		t.Errorf("two checks share the ID %s", second.st.ID)
	}
	p.letGo()
	close(second.stop)
	r.endedAs(playback.CheckStopped, playback.CheckStop, "the second check")
}

// TestTheRoutesPrecheckIsTheBackendsAdmission: CheckAdmits, the routes'
// precheck by the pair published now, answers as Check's own admission does:
// "" for the candidate's check on a generic board's IDs, and the same word
// for native above the cap (no candidate), DoP where native waits for a
// listener's answer, a rate the set converts, the other transport and another
// token; and a token and a gate changed after it answers are judged as the
// design orders them, the token first.
func TestTheRoutesPrecheckIsTheBackendsAdmission(t *testing.T) {
	native := playbacktest.ChoiceState(dsdstate.ModeNative)
	for _, c := range []struct {
		name      string
		st        dsdstate.State
		edit      func(*playback.DSDPolicy)
		transport string
		rate      int
		token     playback.DACToken
		want      string
	}{
		{name: "the candidate's check", st: native, edit: onBoard, transport: dsdstate.TransportNative, rate: playbacktest.DSD64},
		{name: "native above the cap", st: capAt(native, playbacktest.DSD64), edit: onBoard, transport: dsdstate.TransportNative, rate: playbacktest.DSD128,
			want: string(policy.ReasonNativeCheckRequired)},
		{name: "DoP where native waits for a listener's answer", st: native, edit: onBoard, transport: dsdstate.TransportDoP, rate: playbacktest.DSD64,
			want: string(policy.ReasonNativeCheckRequired)},
		{name: "a rate the set converts", st: playbacktest.ChoiceState(dsdstate.ModeConvert), transport: dsdstate.TransportNative, rate: playbacktest.DSD64,
			want: playback.CheckConverted},
		{name: "the other transport", st: native, transport: dsdstate.TransportDoP, rate: playbacktest.DSD64, want: playback.CheckOtherTransport},
		{name: "another token", st: native, transport: dsdstate.TransportNative, rate: playbacktest.DSD64, token: "t1-00000000000000000000000000000000",
			want: playback.CheckDACChanged},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newCheckRig(t, c.st, c.edit)
			req, stop := r.request(c.transport, c.rate)
			if c.token != "" {
				req.DACToken = c.token
			}
			word, pol := r.be.CheckAdmits(req)
			if word != c.want || pol.Publication != r.be.Policy().Publication {
				t.Errorf("CheckAdmits answered %q with publication %d; want %q with %d", word, pol.Publication, c.want, r.be.Policy().Publication)
			}
			if c.want != "" {
				r.refused(req, stop, c.want, c.name)
				return
			}
			r.ask(req, stop)
			close(stop)
			r.joined(c.name)
		})
	}
	t.Run("a token and a gate changed after the precheck", func(t *testing.T) {
		r := newCheckRig(t, native, nil)
		req, stop := r.request(dsdstate.TransportNative, playbacktest.DSD64)
		if word, _ := r.be.CheckAdmits(req); word != "" {
			t.Fatalf("the precheck answered %q", word)
		}
		m := playbacktest.DACUnit()
		m.BCDDevice = "0200"
		r.box.SetUnit(card, m)
		r.be.ToneOutput(false, playback.ToneOutputUnavailable)
		r.refused(req, stop, playback.CheckDACChanged, "a token and a gate changed after the precheck")
	})
}
