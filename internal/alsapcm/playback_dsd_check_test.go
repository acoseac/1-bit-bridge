package alsapcm_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsa"
	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/player/dsdchime"
	"github.com/acoseac/1-bit-bridge/internal/player/dsdstate"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/format"
	"github.com/acoseac/1-bit-bridge/internal/player/playback"
	"github.com/acoseac/1-bit-bridge/internal/player/playback/playbacktest"
	"github.com/acoseac/1-bit-bridge/internal/player/policy"
)

// The player's DSD check (B728 B12 PR 3) through the backend and the real
// native and DoP sinks over the fake kernel, the engine's Run beside it: what
// it admits, the device slot it holds against the other holders, each way it
// ends, the policy's judgement of it, the identity its node must be, its
// cleanup and the fence an unconfirmed one leaves, and serve's stop. No test
// sleeps: the check's bounds and the slot's waits are held timers
// (newCheckTimers), its preparation is held at its seam, a reading of the box
// at the box, and the DAC plays the time a test grants. Its admission's steps
// at the slot are internal/player/playback's (dsd_check_gate_test.go).

// newCheckTimers holds the timers the check and the device slot arm, for a
// test to fire, each in the order armed: the slot's waits (ProbeWait, which
// ToneYield shares), the check's preparation and StopCheck's wait (2 s, which
// ToneStopWait shares) and its playing (CheckPlayBound); every other timer is
// time.AfterFunc's.
func newCheckTimers() *toneTimers {
	return &toneTimers{arms: map[time.Duration]chan *heldTimer{
		playback.ProbeWait:         make(chan *heldTimer, 64),
		playback.CheckPrepareBound: make(chan *heldTimer, 64),
		playback.CheckPlayBound:    make(chan *heldTimer, 64),
	}}
}

// checkRig is a DSD rig whose check and slot timers are held, whose next
// check's preparation can be held at its seam (holdPreparation), and whose
// backend logs to logs.
type checkRig struct {
	*dsdRig
	timers *toneTimers
	logs   *lines

	mu   sync.Mutex
	prep *parkAt
}

// newCheckRig is a checkRig over st, edit applied to its policy, its tone and
// check gate open and its policy read.
func newCheckRig(t *testing.T, st dsdstate.State, edit func(*playback.DSDPolicy), opts ...func(*rigOptions)) *checkRig {
	t.Helper()
	r := newCheckRigShut(t, st, edit, opts...)
	r.be.ToneOutput(true, "")
	return r
}

// newCheckRigShut is newCheckRig with the gate as it starts: shut,
// output_unknown.
func newCheckRigShut(t *testing.T, st dsdstate.State, edit func(*playback.DSDPolicy), opts ...func(*rigOptions)) *checkRig {
	t.Helper()
	r := &checkRig{timers: newCheckTimers(), logs: &lines{}}
	opts = append(opts, withLog(r.logs), func(o *rigOptions) {
		o.afterFunc = r.timers.afterFunc
		if o.seams == nil {
			o.seams = &playback.Seams{}
		}
		o.seams.CheckPreparing = r.preparing
	})
	r.dsdRig = newDSDRigWith(t, st, edit, opts...)
	// Registered after the rig, so it runs before the rig's stop: a check a
	// failed case left running is ended, and the rig's join does not wait
	// out its watchdog on it.
	t.Cleanup(r.be.CloseChecks)
	r.be.RefreshDSD(true)
	return r
}

// preparing is the CheckPreparing seam: the hold armed, once, until it is let
// go or the check's context ends, as a build that asks its context does.
func (r *checkRig) preparing(ctx context.Context) {
	r.mu.Lock()
	p := r.prep
	r.prep = nil
	r.mu.Unlock()
	if p == nil {
		return
	}
	p.held.Do(func() { close(p.in) })
	select {
	case <-p.gate:
	case <-ctx.Done():
	}
}

// holdPreparation holds the next check's preparation before its chime is
// built; the test lets it go (letGo), and its cleanup does too.
func (r *checkRig) holdPreparation() *parkAt {
	p := newParkAt()
	r.mu.Lock()
	r.prep = p
	r.mu.Unlock()
	r.t.Cleanup(p.letGo)
	return p
}

// checkCall is a check asked and its answer, with the stop signal of its
// engine hold, which the test closes.
type checkCall struct {
	st   playback.CheckStatus
	err  error
	stop chan struct{}
}

// request is a check of transport at bitRate for the DAC the backend
// publishes now, with a stop signal of its own.
func (r *checkRig) request(transport string, bitRate int) (playback.CheckRequest, chan struct{}) {
	r.t.Helper()
	p := r.be.Policy()
	if p.DSD == nil || p.DSD.DAC == nil {
		r.t.Fatalf("the backend publishes no DAC: %+v", p.DSD)
	}
	stop := make(chan struct{})
	return playback.CheckRequest{DACKey: p.DSD.DAC.Key, DACToken: p.DSD.DAC.Token, Transport: transport, BitRate: bitRate, Stop: stop}, stop
}

// ask asks req, bounded by the watchdog.
func (r *checkRig) ask(req playback.CheckRequest, stop chan struct{}) checkCall {
	r.t.Helper()
	ctx, cancel := within()
	defer cancel()
	h, err := r.be.Check(ctx, req)
	return checkCall{st: statusOf(h), err: err, stop: stop}
}

// statusOf is the record Check's handle carries, none for no handle.
func statusOf(h *playback.CheckHandle) playback.CheckStatus {
	if h == nil {
		return playback.CheckStatus{}
	}
	return h.Status
}

// slotBack is StopCheck's answer read as the slot given back or fenced:
// every cleanup but one still pending, and no check at all.
func slotBack(c playback.CheckCleanup, _ playback.PlayerPolicy) bool {
	return c != playback.CheckCleanupPending
}

// admitted asks a check of transport at bitRate and fails the test unless it
// holds the slot.
func (r *checkRig) admitted(transport string, bitRate int) checkCall {
	r.t.Helper()
	c := r.ask(r.request(transport, bitRate))
	if c.err != nil {
		r.t.Fatalf("the check of %s at %d was refused: %v", transport, bitRate, c.err)
	}
	return c
}

// checkRefusal reports the word err refuses a check with, "" for none.
func checkRefusal(err error) string {
	var r *playback.CheckRefused
	if errors.As(err, &r) {
		return r.Word
	}
	return ""
}

// refused asks req and fails the test unless it is refused with word, the
// record as it was and no open asked.
func (r *checkRig) refused(req playback.CheckRequest, stop chan struct{}, word, what string) {
	r.t.Helper()
	before, had := r.be.CheckStatus()
	opened := len(r.k.View().Opened)
	c := r.ask(req, stop)
	if w := checkRefusal(c.err); w != word || !sameCheckStatus(c.st, playback.CheckStatus{}) {
		r.t.Errorf("%s: the check answered %+v, %v; want refused %s", what, c.st, c.err, word)
	}
	if after, has := r.be.CheckStatus(); has != had || !sameCheckStatus(after, before) {
		r.t.Errorf("%s: a refused check changed the record from %+v to %+v", what, before, after)
	}
	if n := len(r.k.View().Opened); n != opened {
		r.t.Errorf("%s: a refused check asked %d opens", what, n-opened)
	}
}

// refusedEnded fails the test unless c, a check whose request had ended, was
// refused with the request's error and an empty record, and, once the
// backend's work is joined, opened nothing and made no record; a check
// admitted in its place is stopped first, so the join answers.
func (r *checkRig) refusedEnded(c checkCall, what string) {
	r.t.Helper()
	if !errors.Is(c.err, context.Canceled) || !sameCheckStatus(c.st, playback.CheckStatus{}) {
		r.t.Errorf("%s: the check answered %+v, %v; want its request's error", what, c.st, c.err)
		if c.err == nil {
			close(c.stop)
		}
	}
	r.joined(what)
	if v := r.k.View(); v.Opens != 0 {
		r.t.Errorf("%s: %d opens asked; want none", what, v.Opens)
	}
	if st, ok := r.be.CheckStatus(); ok {
		r.t.Errorf("%s: a record was made: %+v", what, st)
	}
}

// sameCheckStatus reports whether two records say one thing, the identities
// opened compared by value.
func sameCheckStatus(a, b playback.CheckStatus) bool {
	if (a.Opened == nil) != (b.Opened == nil) || a.Opened != nil && *a.Opened != *b.Opened {
		return false
	}
	a.Opened, b.Opened = nil, nil
	return a == b
}

// joined waits until every goroutine the backend started has returned, the
// check's run, its watcher and its monitor among them.
func (r *checkRig) joined(what string) {
	r.t.Helper()
	ctx, cancel := within()
	defer cancel()
	if err := r.be.Wait(ctx); err != nil {
		alsapcm.LogGoroutines(r.t)
		r.t.Fatalf("%s: the backend's goroutines: %v", what, err)
	}
}

// endedAs joins the backend's goroutines and fails the test unless the
// check's record ended phase with reason, every node opened closed.
func (r *checkRig) endedAs(phase playback.CheckPhase, reason playback.CheckReason, what string) playback.CheckStatus {
	r.t.Helper()
	r.joined(what)
	st, ok := r.be.CheckStatus()
	if !ok || st.Phase != phase || st.Reason != reason {
		r.t.Errorf("%s: the check ended %s %q (a record %v); want %s %q", what, st.Phase, st.Reason, ok, phase, reason)
	}
	if v := r.k.View(); v.Open || v.Opens != v.Closes {
		r.t.Errorf("%s: %d opens and %d closes, the node open %v; want every open closed", what, v.Opens, v.Closes, v.Open)
	}
	return st
}

// phase is the check's phase now.
func (r *checkRig) phase() playback.CheckPhase {
	st, _ := r.be.CheckStatus()
	return st.Phase
}

// playingCheck admits a check of transport at DSD64 and waits for its sink's
// writer to fill the hardware buffer and wait on the DAC, which the test
// holds.
func (r *checkRig) playingCheck(transport string) checkCall {
	r.t.Helper()
	c := r.admitted(transport, playbacktest.DSD64)
	r.k.Await(r.t, "the check's writer to fill the hardware buffer and wait on the DAC", runningAndWaiting)
	if p := r.phase(); p != playback.CheckPlaying {
		r.t.Fatalf("the check whose writer waits on the DAC is %s; want playing", p)
	}
	return c
}

// preparingCheck admits a native check at DSD64 and holds it in its
// preparation, nothing opened.
func (r *checkRig) preparingCheck() (checkCall, *parkAt) {
	r.t.Helper()
	p := r.holdPreparation()
	c := r.admitted(dsdstate.TransportNative, playbacktest.DSD64)
	awaitOrdered(r.t, p.in, "the check's preparation held")
	if ph := r.phase(); ph != playback.CheckPreparing {
		r.t.Fatalf("the held check is %s; want preparing", ph)
	}
	return c, p
}

// startBusy plays a PCM track beside a check that holds the slot and fails
// the test unless the Start waits for it at most ProbeWait, then answers the
// device busy, its release confirmed and its record gone.
func (r *checkRig) startBusy(what string) {
	r.t.Helper()
	r.files.add("busy", wav(48000, 24, pattern(1, 4800, 24)))
	r.insert("busy")
	played := make(chan error, 1)
	go func() { played <- r.eng.Play(bg) }()
	r.timers.fire(r.t, playback.ProbeWait, "the Start's wait for the check")
	if err := receiveIn(r.t, played, what); !errors.Is(err, engine.ErrDeviceBusy) {
		r.t.Errorf("%s: a Start answered %v; want the device busy", what, err)
	}
	r.waitSnap("the busy start's release confirmed", func(s *engine.Snapshot) bool {
		_, unconfirmed := s.ReleaseUnconfirmed()
		return s.State() == engine.Stopped && !unconfirmed
	})
	waitOn(r.t, "the backend to let go of the busy start's record", r.rec.ch, func() bool { return r.be.Stats().Sessions == 0 })
}

// checkFenceMessage is the line a check whose cleanup was not confirmed is
// said with.
const checkFenceMessage = "player: the DSD check's cleanup did not answer cleanly: nothing opens the DAC until bridge serve restarts"

// fenceSaid fails the test unless the check's fence is said once, at Warn,
// with its cleanup's error, which carries words when they are not "", and no
// line of the log carries a panic's value.
func (r *checkRig) fenceSaid(words string) {
	r.t.Helper()
	r.logs.mu.Lock()
	defer r.logs.mu.Unlock()
	var found []string
	for _, l := range r.logs.lines {
		if strings.Contains(l, checkFenceMessage) {
			found = append(found, l)
		}
		if strings.Contains(l, panicValue) {
			r.t.Errorf("a line of the log carries a panic's value: %s", l)
		}
	}
	if len(found) != 1 || !strings.HasPrefix(found[0], "WARN ") || !strings.Contains(found[0], " err=") || words != "" && !strings.Contains(found[0], words) {
		r.t.Errorf("the check's fence was said %q; want once, at Warn, with its cleanup's error %q", found, words)
	}
}

// fenceUnsaid fails the test unless no check's fence was said.
func (r *checkRig) fenceUnsaid(what string) {
	r.t.Helper()
	r.logs.mu.Lock()
	defer r.logs.mu.Unlock()
	for _, l := range r.logs.lines {
		if strings.Contains(l, checkFenceMessage) {
			r.t.Errorf("%s: the fence was said: %s", what, l)
		}
	}
}

// checkFenced fails the test unless the device is fenced as a release never
// confirmed fences it, nothing opened by any: a check and a tone are refused
// release_unconfirmed; a Start answers engine.ErrReleaseUnconfirmed; a probe
// is skipped release_unconfirmed and stays owed; a choice's write answers
// playback_not_stopped.
func (r *checkRig) checkFenced(what string) {
	r.t.Helper()
	opened := len(r.k.View().Opened)
	req, stop := r.request(dsdstate.TransportNative, playbacktest.DSD64)
	r.refused(req, stop, string(playback.CheckReleaseUnconfirmed), what+": a later check")
	r.toneRefused(playback.ToneReleaseUnconfirmed, what+": a later tone")
	r.files.add("fenced", wav(48000, 24, pattern(1, 4800, 24)))
	r.insert("fenced")
	if err := r.eng.Play(bg); !errors.Is(err, engine.ErrReleaseUnconfirmed) {
		r.t.Errorf("%s: a Start answered %v; want engine.ErrReleaseUnconfirmed", what, err)
	}
	r.waitSnap("the refused start's stop", func(s *engine.Snapshot) bool { return s.State() == engine.Stopped })
	if !r.be.ProbeDSD(bg, playback.ProbeConditions{Available: true, Fresh: true}) {
		r.t.Errorf("%s: a probe answered not owed", what)
	}
	rep, err := r.be.Probe(bg, probeRequest())
	wantSkipped(r.t, rep, err, playback.ProbeReleaseUnconfirmed)
	r.refusedNotStopped(what + ": a choice's write")
	if n := len(r.k.View().Opened); n != opened {
		r.t.Errorf("%s: %d opens asked of a fenced device", what, n-opened)
	}
}

// reusable fails the test unless the device is not fenced, said or not: a
// check is admitted and plays to complete.
func (r *checkRig) reusable(what string) {
	r.t.Helper()
	r.k.Free()
	r.admitted(dsdstate.TransportNative, playbacktest.DSD64)
	r.endedAs(playback.CheckComplete, "", what+": a later check")
	r.k.Hold()
	r.fenceUnsaid(what)
}

// checkBox is the rig's box whose next reading of the unit, once armed, is
// held (holdNext) or panics (panics): a reading of the box under the
// refreshes' lock that does not answer, or one that fails. reads counts the
// readings of the unit, and the unit reports the firmware bcd holds once it
// is set.
type checkBox struct {
	playback.DSDBox
	mu     sync.Mutex
	park   *parkAt
	panics atomic.Bool
	reads  atomic.Int64
	bcd    atomic.Pointer[string]
}

func (b *checkBox) holdNext() *parkAt {
	p := newParkAt()
	b.mu.Lock()
	b.park = p
	b.mu.Unlock()
	return p
}

func (b *checkBox) UnitModel(card int) (alsa.UnitModel, error) {
	b.reads.Add(1)
	b.mu.Lock()
	p := b.park
	b.park = nil
	b.mu.Unlock()
	if p != nil {
		p.pass()
	}
	if b.panics.CompareAndSwap(true, false) {
		panic(panicValue)
	}
	m, err := b.DSDBox.UnitModel(card)
	if v := b.bcd.Load(); v != nil {
		m.BCDDevice = *v
	}
	return m, err
}

// withCheckBox puts box between the policy and the rig's box.
func withCheckBox(box *checkBox) func(*playback.DSDPolicy) {
	return func(p *playback.DSDPolicy) { box.DSDBox = p.Box; p.Box = box }
}

// heldLocator is the rig's unit with its first Locate held until its context
// ends: a binding that resolves slowly.
type heldLocator struct {
	next *unit
	in   chan struct{}
	once sync.Once
}

func (l *heldLocator) Locate(ctx context.Context) (playback.Unit, error) {
	first := false
	l.once.Do(func() { first = true; close(l.in) })
	if first {
		<-ctx.Done()
		return playback.Unit{}, ctx.Err()
	}
	return l.next.Locate(ctx)
}

// checkIdentity is the test DAC's identity.
func checkIdentity() policy.Identity {
	return policy.Identity{Key: playbacktest.DACKey(), Stamp: playbacktest.DACStamp(), BCDDevice: playbacktest.DACBCD}
}

// TestTheCheckAdmitsOnlyWhatTheSetPlays: the check plays only a tuple the
// effective set plays now, by that very plan, for the DAC the read side
// names, behind the supervisor's gate: a tuple the set refuses is refused by
// the set's word for the rate, DoP unconfirmed on a DAC no profile knows
// among them (native chosen on one, whose IDs are a generic board's while no
// list reads, B990, is its refusal's candidate's check, round 3: the row of
// its own below, which plays it to complete; on a list that names the board,
// TestAGenericBoardsNativeCheckLetsItsAnswerBeEarned); a rate the set
// converts (an unknown DAC under auto among them) is refused converted; one
// it plays the other way,
// other_transport; a key or a token not the read side's, dac_changed; and the
// gate shut, by its word, as it starts and for each shut. No refusal opens
// anything, or makes or changes the record (the last check's, which stays as
// it ended). A tuple the set plays is admitted, under the set's identity, its
// plan's carrier opened on that identity, its driver_accepted recorded and
// nothing streaming_observed.
func TestTheCheckAdmitsOnlyWhatTheSetPlays(t *testing.T) {
	native := playbacktest.ChoiceState(dsdstate.ModeNative)
	unconfirmed := playbacktest.ChoiceState(dsdstate.ModeDoP)
	unconfirmed.DACs[0].DoPConfirmed = nil
	unknown := func(p *playback.DSDPolicy) { p.Profiles = nil }
	nativeReq := func(r *checkRig) (playback.CheckRequest, chan struct{}) {
		return r.request(dsdstate.TransportNative, playbacktest.DSD64)
	}
	dopReq := func(r *checkRig) (playback.CheckRequest, chan struct{}) {
		return r.request(dsdstate.TransportDoP, playbacktest.DSD64)
	}
	gate := func(why playback.ToneOutcome) func(be *playback.Backend) {
		return func(be *playback.Backend) { be.ToneOutput(false, why) }
	}
	for _, c := range []struct {
		name  string
		st    dsdstate.State
		edit  func(*playback.DSDPolicy)
		shut  bool
		req   func(r *checkRig) (playback.CheckRequest, chan struct{})
		gate  func(be *playback.Backend)
		want  string
		first bool // a check runs and ends first: its record stays
	}{
		{name: "DoP unconfirmed on a DAC no profile knows", st: unconfirmed, edit: unknown, req: dopReq, want: string(policy.ReasonConfirmationRequired)},
		{name: "a rate the set converts", st: playbacktest.ChoiceState(dsdstate.ModeConvert), req: nativeReq, want: playback.CheckConverted},
		{name: "an unknown DAC under auto", st: playbacktest.ChoiceState(dsdstate.ModeAuto), edit: unknown, req: nativeReq, want: playback.CheckConverted},
		{name: "a rate the set plays the other way", st: native, first: true, req: dopReq, want: playback.CheckOtherTransport},
		{name: "a token another reading made", st: native, first: true, req: func(r *checkRig) (playback.CheckRequest, chan struct{}) {
			req, stop := nativeReq(r)
			req.DACToken = "t1-00000000000000000000000000000000"
			return req, stop
		}, want: playback.CheckDACChanged},
		{name: "a key another DAC has", st: native, first: true, req: func(r *checkRig) (playback.CheckRequest, chan struct{}) {
			req, stop := nativeReq(r)
			req.DACKey = "d1-another"
			return req, stop
		}, want: playback.CheckDACChanged},
		{name: "the gate as it starts", st: native, shut: true, req: nativeReq, want: string(playback.ToneOutputUnknown)},
		{name: "the gate shut for the player turned off", st: native, first: true, req: nativeReq, gate: gate(playback.ToneWithheld), want: string(playback.ToneWithheld)},
		{name: "the gate shut for an output gone", st: native, first: true, req: nativeReq, gate: gate(playback.ToneOutputUnavailable), want: string(playback.ToneOutputUnavailable)},
		{name: "the gate shut for a unit that changed", st: native, first: true, req: nativeReq, gate: gate(playback.ToneOutputChanged), want: string(playback.ToneOutputChanged)},
		{name: "the gate shut for a report the engine did not take", st: native, first: true, req: nativeReq, gate: gate(playback.ToneOutputUnknown), want: string(playback.ToneOutputUnknown)},
		{name: "the gate shut for a stale snapshot", st: native, first: true, req: nativeReq, gate: gate(playback.ToneSnapshotStale), want: string(playback.ToneSnapshotStale)},
	} {
		t.Run(c.name, func(t *testing.T) {
			var r *checkRig
			if c.shut {
				r = newCheckRigShut(t, c.st, c.edit)
			} else {
				r = newCheckRig(t, c.st, c.edit)
			}
			if c.first {
				r.k.Free()
				r.admitted(dsdstate.TransportNative, playbacktest.DSD64)
				r.endedAs(playback.CheckComplete, "", "the first check")
				r.k.Hold()
			}
			if c.gate != nil {
				c.gate(r.be)
			}
			req, stop := c.req(r)
			r.refused(req, stop, c.want, c.name)
			if _, ok := r.be.CheckStatus(); ok != c.first {
				t.Errorf("a record %v; want one only where a check ran first", ok)
			}
		})
	}
	t.Run("a tuple the set plays is admitted by its plan", func(t *testing.T) {
		r := newCheckRig(t, playbacktest.ChoiceState(dsdstate.ModeDoP), nil)
		c := r.admitted(dsdstate.TransportDoP, playbacktest.DSD64)
		if c.st.Phase != playback.CheckPreparing && c.st.Phase != playback.CheckPlaying || c.st.Reason != "" || c.st.Transport != dsdstate.TransportDoP ||
			c.st.BitRate != playbacktest.DSD64 || c.st.ID == "" || c.st.Token != r.be.Policy().DSD.DAC.Token || !c.st.Identity.Same(checkIdentity()) {
			t.Errorf("the admitted check's record: %+v", c.st)
		}
		r.k.Free()
		st := r.endedAs(playback.CheckComplete, "", "the admitted check")
		if st.Opened == nil || !st.Opened.Same(checkIdentity()) || st.ID != c.st.ID {
			t.Errorf("the check's record once complete: %+v; want the identity it was admitted under opened", st)
		}
		if got := r.openFormats(); len(got) != 1 || got[0] != format.S32LE {
			t.Errorf("the check opened %v; want one S32_LE carrier, the set's DoP plan", got)
		}
		e := playbacktest.Evidence(r.store.State(), dsdstate.TransportDoP, string(format.S32LE), playbacktest.DSD64)
		if e == nil || e.DriverAcceptedAt.IsZero() || e.Kernel != playbacktest.Kernel || !e.StreamingObservedAt.IsZero() {
			t.Errorf("the check's evidence: %+v; want driver_accepted under the kernel, nothing streaming_observed", e)
		}
	})
	t.Run("native chosen on a DAC no profile knows while no list reads is its candidate's check", func(t *testing.T) {
		r := newCheckRig(t, native, unknown)
		r.admitted(dsdstate.TransportNative, playbacktest.DSD64)
		r.k.Free()
		r.endedAs(playback.CheckComplete, "", "the candidate's check while no list reads")
	})
}

// TestAnEndedRequestOpensNothing: through the engine's Run, the backend and
// the fake kernel, a check of a tuple the set plays whose request has ended
// is refused with the request's error and an empty record, and opens nothing,
// the fake's opens read once the backend's work is joined: one ended before
// Check reads nothing of the box (its reading of the policy asks the request
// once it holds the refreshes' lock), and one whose request ends while that
// reading is held in the box is refused as the reading ends, before the slot.
func TestAnEndedRequestOpensNothing(t *testing.T) {
	native := playbacktest.ChoiceState(dsdstate.ModeNative)
	t.Run("ended before Check", func(t *testing.T) {
		box := &checkBox{}
		r := newCheckRig(t, native, withCheckBox(box))
		req, stop := r.request(dsdstate.TransportNative, playbacktest.DSD64)
		reads := box.reads.Load()
		ctx, cancel := context.WithCancel(bg)
		cancel()
		h, err := r.be.Check(ctx, req)
		r.refusedEnded(checkCall{st: statusOf(h), err: err, stop: stop}, "a request ended before Check")
		if n := box.reads.Load() - reads; n != 0 {
			t.Errorf("a request ended before Check read the box %d times; want none", n)
		}
	})
	t.Run("ended while its reading of the policy is held", func(t *testing.T) {
		box := &checkBox{}
		r := newCheckRig(t, native, withCheckBox(box))
		req, stop := r.request(dsdstate.TransportNative, playbacktest.DSD64)
		p := box.holdNext()
		t.Cleanup(p.letGo)
		ctx, cancel := context.WithCancel(bg)
		defer cancel()
		answer := make(chan checkCall, 1)
		go func() {
			h, err := r.be.Check(ctx, req)
			answer <- checkCall{st: statusOf(h), err: err, stop: stop}
		}()
		awaitOrdered(t, p.in, "the check's reading of the policy held in the box")
		cancel()
		p.letGo()
		r.refusedEnded(receiveIn(t, answer, "the check's answer"), "a request ended while its reading was held")
	})
}

// TestTheCheckOwnsTheSlotAgainstProbesAndTones: the check takes the device
// slot as a Start's hold does. Asked while a choice's write holds the lease,
// a session holds the device, another check runs or a release was never
// confirmed, it is refused (device_busy; release_unconfirmed for the fence);
// a probe in the slot is waited for at most ProbeWait and a tone is ended
// preempted and waited for at most ToneYield, then device_busy, or admitted
// once the slot is free, the check opening the DAC itself after the probe's
// open, its stop signal left open until then. While it holds the slot, a
// probe is skipped check_holds and stays owed, a tone is refused
// check_running, a choice's write playback_not_stopped, and a Start waits
// ProbeWait for it, then answers the device busy, the check going on. And the
// check's take voids the answers of a probe that decides them after it, as a
// Start's does.
func TestTheCheckOwnsTheSlotAgainstProbesAndTones(t *testing.T) {
	native := playbacktest.ChoiceState(dsdstate.ModeNative)
	cond := playback.ProbeConditions{Available: true, Fresh: true}
	t.Run("a choice's write holding the lease", func(t *testing.T) {
		leased := newParkAt()
		r := newCheckRig(t, native, nil, func(o *rigOptions) { o.seams = &playback.Seams{ChoiceLeased: leased.pass} })
		t.Cleanup(leased.letGo)
		wrote := make(chan playback.ChoiceResult, 1)
		go func() { wrote <- r.write(dsdstate.ModeNative) }()
		awaitOrdered(t, leased.in, "the write holding the lease")
		req, stop := r.request(dsdstate.TransportNative, playbacktest.DSD64)
		r.refused(req, stop, string(playback.CheckDeviceBusy), "beside a choice's lease")
		leased.letGo()
		receiveIn(t, wrote, "the write")
	})
	t.Run("a session holding the device", func(t *testing.T) {
		r := newCheckRig(t, native, nil)
		r.playingPCM()
		req, stop := r.request(dsdstate.TransportNative, playbacktest.DSD64)
		r.refused(req, stop, string(playback.CheckDeviceBusy), "beside a session")
	})
	t.Run("another check", func(t *testing.T) {
		r := newCheckRig(t, native, nil)
		first, _ := r.preparingCheck()
		req, stop := r.request(dsdstate.TransportNative, playbacktest.DSD64)
		r.refused(req, stop, string(playback.CheckDeviceBusy), "beside another check")
		close(first.stop)
		r.endedAs(playback.CheckStopped, playback.CheckStop, "the first check")
	})
	t.Run("a release never confirmed", func(t *testing.T) {
		r := newCheckRig(t, native, nil)
		tone := r.playingTone(30)
		r.k.FailNextClose(syscall.EIO)
		tone.cancel()
		if res := tone.result(t); res.Outcome != playback.ToneReleaseUnconfirmed {
			t.Fatalf("the tone answered %+v; want its fence", res)
		}
		r.joinTone()
		req, stop := r.request(dsdstate.TransportNative, playbacktest.DSD64)
		r.refused(req, stop, string(playback.CheckReleaseUnconfirmed), "beside the fence")
	})
	probeHeldOpen := func(r *checkRig) (done <-chan struct{}, letOpen func()) {
		parked, letOpen := r.k.ParkNextOpen()
		r.t.Cleanup(letOpen)
		ch := make(chan struct{})
		go func() {
			defer close(ch)
			_, _ = r.be.Probe(bg, probeRequest())
		}()
		awaitOrdered(r.t, parked, "the probe's open held")
		return ch, letOpen
	}
	t.Run("a probe in the slot is waited for", func(t *testing.T) {
		r := newCheckRig(t, native, nil)
		opens := r.k.View().Opens
		probed, letOpen := probeHeldOpen(r)
		asked := make(chan checkCall, 1)
		go func() { asked <- r.ask(r.request(dsdstate.TransportNative, playbacktest.DSD64)) }()
		if tm := receiveIn(t, r.timers.arms[playback.ProbeWait], "the check's wait for the probe"); tm.d != playback.ProbeWait {
			t.Errorf("the check waits %v for the probe", tm.d)
		}
		letOpen()
		receiveIn(t, probed, "the probe's end")
		c := receiveIn(t, asked, "the check once the probe let the slot go")
		if c.err != nil {
			t.Fatalf("the check once the probe ended answered %v", c.err)
		}
		r.k.Await(t, "the check's own open, after the probe's", func(v alsapcm.KernelView) bool { return v.Opens >= opens+2 })
		close(c.stop)
		r.endedAs(playback.CheckStopped, playback.CheckStop, "the check after the probe")
	})
	t.Run("a probe past ProbeWait", func(t *testing.T) {
		r := newCheckRig(t, native, nil)
		probed, letOpen := probeHeldOpen(r)
		req, stop := r.request(dsdstate.TransportNative, playbacktest.DSD64)
		refused := make(chan struct{})
		go func() {
			defer close(refused)
			r.refused(req, stop, string(playback.CheckDeviceBusy), "a check past its wait for the probe")
		}()
		r.timers.fire(t, playback.ProbeWait, "the check's wait for the probe")
		receiveIn(t, refused, "the refused check")
		letOpen()
		receiveIn(t, probed, "the probe's end")
		if _, ok := r.be.CheckStatus(); ok {
			t.Error("a check refused at its wait made a record")
		}
	})
	t.Run("a tone is ended and waited for", func(t *testing.T) {
		r := newCheckRig(t, native, nil)
		tone := r.playingTone(30)
		c := r.admitted(dsdstate.TransportNative, playbacktest.DSD64)
		if res := tone.result(t); res.Outcome != playback.TonePreempted {
			t.Errorf("the tone the check came beside answered %+v; want preempted", res)
		}
		close(c.stop)
		r.endedAs(playback.CheckStopped, playback.CheckStop, "the check after the tone")
	})
	t.Run("a tone past ToneYield", func(t *testing.T) {
		r := newCheckRig(t, native, nil)
		tone := r.playingTone(30)
		parked, letClose := r.k.ParkNextClose()
		t.Cleanup(letClose)
		req, stop := r.request(dsdstate.TransportNative, playbacktest.DSD64)
		refused := make(chan struct{})
		go func() {
			defer close(refused)
			r.refused(req, stop, string(playback.CheckDeviceBusy), "a check past its wait for the tone")
		}()
		awaitOrdered(t, parked, "the preempted tone's close held")
		r.timers.fire(t, playback.ToneYield, "the check's wait for the tone")
		receiveIn(t, refused, "the refused check")
		letClose()
		if res := tone.result(t); res.Outcome != playback.TonePreempted {
			t.Errorf("the tone answered %+v; want preempted", res)
		}
	})
	t.Run("held by the check", func(t *testing.T) {
		r := newCheckRig(t, native, nil)
		c, _ := r.preparingCheck()
		rep, err := r.be.Probe(bg, probeRequest())
		wantSkipped(t, rep, err, playback.ProbeCheckHolds)
		if !r.be.ProbeDSD(bg, cond) || !r.be.ProbeDSDPending() {
			t.Error("a probe beside the check is not owed")
		}
		r.toneRefused(playback.ToneCheckRunning, "a tone beside the check")
		r.refusedNotStopped("a choice's write beside the check")
		r.startBusy("a Start beside the check")
		if p := r.phase(); p != playback.CheckPreparing {
			t.Errorf("a Start ended the check: %s", p)
		}
		close(c.stop)
		r.endedAs(playback.CheckStopped, playback.CheckStop, "the check held against the others")
		if r.be.ProbeDSD(bg, cond) || r.be.ProbeDSDPending() {
			t.Error("the probe the check held off is still owed once it is made with the slot free")
		}
	})
	t.Run("the check's take voids an ending probe's answers", func(t *testing.T) {
		deciding := newParkAt()
		r := newCheckRig(t, native, nil, func(o *rigOptions) { o.seams = &playback.Seams{ProbeDeciding: deciding.pass} })
		t.Cleanup(deciding.letGo)
		owed := make(chan bool, 1)
		go func() { owed <- r.be.ProbeDSD(bg, cond) }()
		awaitOrdered(t, deciding.in, "the probe about to decide its answers, the slot given back")
		c, _ := r.preparingCheck()
		deciding.letGo()
		if !receiveIn(t, owed, "the probe's end") {
			t.Error("a probe whose answers were decided after the check took the slot kept them")
		}
		if !r.be.ProbeDSDPending() {
			t.Error("no probe is owed once the check's take voided the answers")
		}
		close(c.stop)
		r.endedAs(playback.CheckStopped, playback.CheckStop, "the check that voided the probe")
		if e := playbacktest.Evidence(r.store.State(), dsdstate.TransportNative, string(format.DSDU32BE), playbacktest.DSD64); e != nil {
			t.Errorf("the state holds %+v: the voided probe's answers were recorded", e)
		}
	})
}

// chimeFrames is the frames of rate frames a second that d covers, its last
// one whole: the count a sink writes for a stretch of silence.
func chimeFrames(rate int, d time.Duration) int {
	return int((int64(rate)*int64(d) + int64(time.Second) - 1) / int64(time.Second))
}

// chimeStream is the independent oracle of what the check's sink must give
// the kernel in f at bitRate: pre frames of 0x69 silence (the plan's
// prefill), the chime's canonical bits, tail frames of silence (the plan's
// tail), and nothing after. Each channel's next word is assembled one bit at
// a time, its earliest bit the word's most significant, then stored as f
// says: big-endian or little-endian for native DSD, and for DoP the S32_LE
// of 00, the 16-bit word's low byte, its high byte and the frame's marker,
// 05 then FA alternating from the hardware run's first frame. It never goes
// through the bridge's packers.
func chimeStream(t *testing.T, f format.ALSAFormat, bitRate, pre, tail int) []byte {
	t.Helper()
	chime, err := dsdchime.Get(context.Background(), bitRate)
	if err != nil {
		t.Fatal(err)
	}
	n := int(chime.Format().Frames / 8)
	planes := [][]byte{make([]byte, n), make([]byte, n)}
	if got, err := chime.ReadAt(planes, 0); got != n || err != nil {
		t.Fatalf("the chime read %d of %d bytes: %v", got, n, err)
	}
	wordBits, le, dop := 32, false, false
	switch f {
	case format.DSDU32BE:
	case format.DSDU32LE:
		le = true
	case format.DSDU16LE:
		wordBits, le = 16, true
	case format.S32LE:
		wordBits, dop = 16, true
	default:
		t.Fatalf("no oracle for %s", f)
	}
	w := wordBits / 8
	if n%w != 0 {
		t.Fatalf("the chime's %d bytes a channel are no whole number of %d-byte words", n, w)
	}
	var stream [2][]byte
	for ch := range 2 {
		stream[ch] = append(stream[ch], silenceBytes(pre*w)...)
		stream[ch] = append(stream[ch], planes[ch]...)
		stream[ch] = append(stream[ch], silenceBytes(tail*w)...)
	}
	frames := len(stream[0]) / w
	out := make([]byte, 0, frames*2*4)
	marker := byte(0x05)
	for i := range frames {
		for ch := range 2 {
			var word uint32
			for j := range wordBits {
				bit := (stream[ch][(i*wordBits+j)/8] >> (7 - j%8)) & 1
				word |= uint32(bit) << (wordBits - 1 - j)
			}
			switch {
			case dop:
				out = append(out, 0, byte(word), byte(word>>8), marker)
			case le:
				for k := range w {
					out = append(out, byte(word>>(8*k)))
				}
			default:
				for k := w - 1; k >= 0; k-- {
					out = append(out, byte(word>>(8*k)))
				}
			}
		}
		if marker == 0x05 {
			marker = 0xfa
		} else {
			marker = 0x05
		}
	}
	return out
}

// silenceBytes is n bytes of DSD silence, 0x69.
func silenceBytes(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = 0x69
	}
	return b
}

// TestTheChimeReachesTheDACAsItsPlanSays: what the check gives the kernel,
// natively in DSD_U32_BE and DSD_U16_LE and as DoP in S32_LE with its
// markers, is the independent oracle's stream byte for byte: the plan's
// prefill first, its frame count odd (the profile's 1 ms at the carrier's
// rate, rounded up), so the chime's first word sits at an odd frame index;
// the chime; the plan's tail; nothing after. The writes are partial: the
// buffer fills with the DAC held, which takes only what fits, and the DAC
// then plays odd stretches before it plays freely.
func TestTheChimeReachesTheDACAsItsPlanSays(t *testing.T) {
	u16 := func(p *playback.DSDPolicy) {
		a := playbacktest.NativeAltset()
		a.Formats = []string{"SPECIAL", "DSD_U16_LE"}
		a.Rates = []int{176400, 352800}
		p.Box.(*playbacktest.Box).SetAltsets(card, 0, playbacktest.S32Altset(), playbacktest.S24Altset(), a)
	}
	for _, c := range []struct {
		name      string
		mode      string
		edit      func(*playback.DSDPolicy)
		transport string
		f         format.ALSAFormat
		frameRate int
	}{
		{"native DSD_U32_BE", dsdstate.ModeNative, nil, dsdstate.TransportNative, format.DSDU32BE, playbacktest.DSD64 / 32},
		{"native DSD_U16_LE", dsdstate.ModeNative, u16, dsdstate.TransportNative, format.DSDU16LE, playbacktest.DSD64 / 16},
		{"DoP S32_LE", dsdstate.ModeDoP, nil, dsdstate.TransportDoP, format.S32LE, playbacktest.DSD64 / 16},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newCheckRig(t, playbacktest.ChoiceState(c.mode), c.edit)
			pre, tail := chimeFrames(c.frameRate, dsdPrefill), chimeFrames(c.frameRate, policy.DefaultTail)
			if pre%2 != 1 {
				t.Fatalf("the prefill is %d frames; the case needs the chime's first word at an odd frame index", pre)
			}
			want := chimeStream(t, c.f, playbacktest.DSD64, pre, tail)
			r.playingCheck(c.transport)
			for range 5 {
				r.k.Play(t, 3*time.Millisecond)
			}
			r.k.Free()
			r.endedAs(playback.CheckComplete, "", c.name)
			if got := r.openFormats(); len(got) != 1 || got[0] != c.f {
				t.Errorf("the check opened %v; want %s", got, c.f)
			}
			got := r.k.Given()
			if len(got) != len(want) {
				t.Errorf("the kernel was given %d bytes; want %d: the prefill's %d frames, the chime's and the tail's %d", len(got), len(want), pre, tail)
			}
			for i := range min(len(got), len(want)) {
				if got[i] != want[i] {
					t.Fatalf("byte %d (frame %d) is %#x; want %#x", i, i/8, got[i], want[i])
				}
			}
		})
	}
}

// gateShuts are the supervisor's shuts of the gate (cmd/bridge's
// toneShutFor, toneRefused and toneSnapshot), with the reason each ends an
// admitted check with: output_changed for a unit change, output_unavailable
// for every other, the player turned off among them.
var gateShuts = []struct {
	why  playback.ToneOutcome
	want playback.CheckReason
}{
	{playback.ToneWithheld, playback.CheckOutputUnavailable},
	{playback.ToneOutputUnavailable, playback.CheckOutputUnavailable},
	{playback.ToneOutputChanged, playback.CheckOutputChanged},
	{playback.ToneOutputUnknown, playback.CheckOutputUnavailable},
	{playback.ToneSnapshotStale, playback.CheckOutputUnavailable},
}

// TestEveryEndOfTheCheckEndsItWithinItsBound: each end of the check, in its
// preparation (the chime's build held at its seam, nothing opened) and in its
// playing (the DAC holding the writes), ends it with its phase and reason and
// lets the device go: the hold's stop signal (stopped, stop), StopCheck
// (stopped, deleted), CloseChecks (stopped, serve_stopping), the supervisor's
// gate (failed, output_changed for a unit change, output_unavailable for
// every other shut), its bound (failed, timeout: the preparation's, and the
// playing's, armed before the locate, which a locate that does not answer
// cannot outlast); in playing the sink's fault (failed, device_error); and
// the unit gone (failed, output_gone) as Locate fails or as the node's open
// finds it gone.
func TestEveryEndOfTheCheckEndsItWithinItsBound(t *testing.T) {
	native := playbacktest.ChoiceState(dsdstate.ModeNative)
	type end struct {
		name   string
		end    func(r *checkRig, c checkCall)
		phase  playback.CheckPhase
		reason playback.CheckReason
	}
	ends := []end{
		{"the hold's stop signal", func(_ *checkRig, c checkCall) { close(c.stop) }, playback.CheckStopped, playback.CheckStop},
		{"StopCheck", func(r *checkRig, _ checkCall) {
			if !slotBack(r.be.StopCheck(bg)) {
				r.t.Error("StopCheck answered the slot not given back")
			}
		}, playback.CheckStopped, playback.CheckDeleted},
		{"CloseChecks", func(r *checkRig, _ checkCall) { r.be.CloseChecks() }, playback.CheckStopped, playback.CheckServeStopping},
	}
	for _, g := range gateShuts {
		ends = append(ends, end{"the gate shut " + string(g.why), func(r *checkRig, _ checkCall) { r.be.ToneOutput(false, g.why) },
			playback.CheckFailed, g.want})
	}
	t.Run("preparing", func(t *testing.T) {
		preparing := append(append([]end{}, ends...), end{"its bound", func(r *checkRig, _ checkCall) {
			r.timers.fire(r.t, playback.CheckPrepareBound, "the preparation's bound")
		}, playback.CheckFailed, playback.CheckTimeout})
		for _, e := range preparing {
			t.Run(e.name, func(t *testing.T) {
				r := newCheckRig(t, native, nil)
				c, _ := r.preparingCheck()
				e.end(r, c)
				r.endedAs(e.phase, e.reason, e.name)
				if v := r.k.View(); len(v.Opened) != 0 {
					t.Errorf("%s: a check ended in its preparation asked %d opens", e.name, len(v.Opened))
				}
			})
		}
	})
	t.Run("playing", func(t *testing.T) {
		playing := append(append([]end{}, ends...),
			end{"its bound", func(r *checkRig, _ checkCall) { r.timers.fire(r.t, playback.CheckPlayBound, "the playing's bound") }, playback.CheckFailed, playback.CheckTimeout},
			end{"the sink's fault", func(r *checkRig, _ checkCall) { r.k.XrunAt(r.k.View().Total + 1); r.k.Free() }, playback.CheckFailed, playback.CheckDeviceError})
		for _, e := range playing {
			t.Run(e.name, func(t *testing.T) {
				r := newCheckRig(t, native, nil)
				c := r.playingCheck(dsdstate.TransportNative)
				e.end(r, c)
				r.endedAs(e.phase, e.reason, e.name)
				if v := r.k.View(); v.Opens != 1 {
					t.Errorf("%s: the check opened the node %d times; want once", e.name, v.Opens)
				}
				r.fenceUnsaid(e.name)
			})
		}
	})
	t.Run("the playing's bound counts the locate", func(t *testing.T) {
		loc := &heldLocator{in: make(chan struct{})}
		r := newCheckRig(t, native, nil, func(o *rigOptions) { o.locator = loc })
		loc.next = r.unit
		r.admitted(dsdstate.TransportNative, playbacktest.DSD64)
		awaitOrdered(t, loc.in, "the check's locate held")
		r.timers.fire(t, playback.CheckPlayBound, "the playing's bound, armed before the locate")
		r.endedAs(playback.CheckFailed, playback.CheckTimeout, "a check whose locate outlasts its bound")
		if v := r.k.View(); len(v.Opened) != 0 {
			t.Errorf("a check ended in its locate asked %d opens", len(v.Opened))
		}
	})
	t.Run("Locate fails", func(t *testing.T) {
		r := newCheckRig(t, native, nil)
		_, p := r.preparingCheck()
		r.unit.mu.Lock()
		r.unit.err = errors.New("the bound DAC is not on the box")
		r.unit.mu.Unlock()
		p.letGo()
		r.endedAs(playback.CheckFailed, playback.CheckOutputGone, "a check whose unit is not located")
		if v := r.k.View(); len(v.Opened) != 0 {
			t.Errorf("a check whose unit was not located asked %d opens", len(v.Opened))
		}
	})
	t.Run("the node gone at its open", func(t *testing.T) {
		r := newCheckRig(t, native, nil)
		_, p := r.preparingCheck()
		r.k.Unplug()
		p.letGo()
		r.endedAs(playback.CheckFailed, playback.CheckOutputGone, "a check whose node went before its open")
		if v := r.k.View(); v.GivenBytes != 0 {
			t.Errorf("a check whose node went was given %d bytes", v.GivenBytes)
		}
		r.fenceUnsaid("a node gone at its open")
	})
}

// withVolume puts a gain stage on the test DAC's playback path, its level at
// 0 dB: DoP plays while it stays there.
func withVolume(p *playback.DSDPolicy) {
	box := p.Box.(*playbacktest.Box)
	m := playbacktest.DACUnit()
	m.Descriptors = playbacktest.VolumeDescriptors()
	box.SetUnit(card, m)
	box.SetMixer(card, playbacktest.VolumeMixer())
	box.SetControls(card, playbacktest.VolumeControls(255)...)
}

// TestAPolicyChangeEndsTheCheck: the policy judges the check as it judges a
// session's occurrence and never plans it again. A DoP level control moved
// off 0 dB during the preparation ends it at the reading before its open,
// and one moved between that reading and its node's check at the open's gain
// check (failed, policy_changed), nothing written either way and nothing
// recorded of a node its open refused; one moved once
// audio reached the DAC, the DAC plugged in all along, at the check's own
// monitor's tick, its sink revoked before the chime's end; the state made
// unreadable, through the reading and the reconciliation a session meets
// (policy_changed); a reading that panics, contained (backend_failed); and a
// change of the set that keeps the check's plan leaves it playing to
// complete.
func TestAPolicyChangeEndsTheCheck(t *testing.T) {
	dop := playbacktest.ChoiceState(dsdstate.ModeDoP)
	offUnity := func(r *checkRig) { r.box.SetControls(card, playbacktest.VolumeControls(200)...) }
	t.Run("a level control moved during the preparation", func(t *testing.T) {
		r := newCheckRig(t, dop, withVolume)
		p := r.holdPreparation()
		r.admitted(dsdstate.TransportDoP, playbacktest.DSD64)
		awaitOrdered(t, p.in, "the check's preparation held")
		offUnity(r)
		p.letGo()
		r.endedAs(playback.CheckFailed, playback.CheckPolicyChanged, "a gain moved off 0 dB as the check prepared")
		if v := r.k.View(); len(v.Opened) != 0 || v.GivenBytes != 0 {
			t.Errorf("the check asked %d opens and gave %d bytes; want neither", len(v.Opened), v.GivenBytes)
		}
	})
	t.Run("a level control moved before the open's gain check", func(t *testing.T) {
		r := newCheckRig(t, dop, withVolume)
		held, release := r.unit.holdNextAdmit()
		t.Cleanup(release)
		r.admitted(dsdstate.TransportDoP, playbacktest.DSD64)
		awaitOrdered(t, held, "the open's check after open held")
		offUnity(r)
		release()
		r.endedAs(playback.CheckFailed, playback.CheckPolicyChanged, "a gain moved off 0 dB at the open")
		if v := r.k.View(); v.Opens != 1 || v.GivenBytes != 0 {
			t.Errorf("%d opens and %d bytes given; want the one open refused, nothing written", v.Opens, v.GivenBytes)
		}
		// The open's own check refused it: an open that passed would have
		// recorded its driver_accepted, the activation refusing it after.
		if e := playbacktest.Evidence(r.store.State(), dsdstate.TransportDoP, string(format.S32LE), playbacktest.DSD64); e != nil {
			t.Errorf("the open refused for its gain recorded %+v", e)
		}
	})
	t.Run("a level control moved once audio reached the DAC", func(t *testing.T) {
		r := newCheckRig(t, dop, withVolume)
		r.playingCheck(dsdstate.TransportDoP)
		r.k.Play(t, 50*time.Millisecond)
		offUnity(r)
		r.monitorTickThatRevokes()
		r.endedAs(playback.CheckFailed, playback.CheckPolicyChanged, "a gain moved off 0 dB as the chime played")
		if v := r.k.View(); v.Consumed == 0 || v.GivenBytes >= len(chimeStream(t, format.S32LE, playbacktest.DSD64, 0, 0)) {
			t.Errorf("the DAC played %d frames and was given %d bytes; want audio heard and the chime cut short", v.Consumed, v.GivenBytes)
		}
	})
	t.Run("the state made unreadable", func(t *testing.T) {
		r := newCheckRig(t, playbacktest.ChoiceState(dsdstate.ModeNative), nil)
		r.playingCheck(dsdstate.TransportNative)
		r.store.Set(dsdstate.Refusing(playbacktest.T0))
		r.monitorTickThatRevokes()
		r.endedAs(playback.CheckFailed, playback.CheckPolicyChanged, "the state unreadable")
	})
	t.Run("the state made unreadable between the open and its activation", func(t *testing.T) {
		// The node's check reads the box, never the state: the open passes,
		// and only the activation's judgement can see the change.
		r := newCheckRig(t, playbacktest.ChoiceState(dsdstate.ModeNative), nil)
		held, release := r.unit.holdNextAdmit()
		t.Cleanup(release)
		r.admitted(dsdstate.TransportNative, playbacktest.DSD64)
		awaitOrdered(t, held, "the open's check after open held")
		r.store.Set(dsdstate.Refusing(playbacktest.T0))
		release()
		r.endedAs(playback.CheckFailed, playback.CheckPolicyChanged, "the state unreadable at the activation")
		if v := r.k.View(); v.Opens != 1 || v.GivenBytes != 0 {
			t.Errorf("%d opens and %d bytes given; want the sink opened and nothing written, its prefill included", v.Opens, v.GivenBytes)
		}
	})
	t.Run("a reading that panics", func(t *testing.T) {
		box := &checkBox{}
		r := newCheckRig(t, playbacktest.ChoiceState(dsdstate.ModeNative), withCheckBox(box))
		r.playingCheck(dsdstate.TransportNative)
		box.panics.Store(true)
		r.monitorTickThatRevokes()
		r.endedAs(playback.CheckFailed, playback.CheckBackendFailed, "a reading that panicked")
		r.logs.panicLogged(t, backendPanicMessage, "the effective DSD policy's reading (the DSD check's monitor)")
	})
	t.Run("a set change that keeps the check's plan", func(t *testing.T) {
		r := newCheckRig(t, playbacktest.ChoiceState(dsdstate.ModeNative), nil)
		r.playingCheck(dsdstate.TransportNative)
		before := r.be.Policy().Generation
		st := r.store.State()
		st.DACs[0].Choice.MaxBitRate = playbacktest.DSD64
		r.store.Set(st)
		r.monitorTick()
		if g := r.be.Policy().Generation; g == before {
			t.Fatalf("the cap left the set at generation %d: the case needs a new set", g)
		}
		r.k.Free()
		r.endedAs(playback.CheckComplete, "", "a check whose plan the new set keeps")
	})
}

// identicalElsewhere is a native choice the test DAC's model holds for its
// own maker and firmware and, alike, for another maker's unit of the model
// and for another firmware of its own: a unit swapped for either is offered
// the same tuple.
func identicalElsewhere() dsdstate.State {
	st := playbacktest.ChoiceState(dsdstate.ModeNative)
	st.DACs[0].FirstUse = append(st.DACs[0].FirstUse, dsdstate.FirstUse{Transport: dsdstate.TransportNative, BCDDevice: "0200", AcknowledgedAt: playbacktest.T0})
	other := st.DACs[0]
	other.Stamp = dsdstate.StampOf("Another Maker")
	st.DACs = append(st.DACs, other)
	return st.Clone()
}

// TestAnotherMakersUnitDuringPreparationEndsTheCheck: the check plays only on
// the identity it was admitted under. Another maker's unit of the same model
// (the same dacKey, another stamp) swapped in as the check prepares ends it
// at the reading before its open (failed, policy_changed), nothing opened;
// one swapped in between that reading and the open is refused by the open's
// check (failed, wrong_device), nothing written and no driver_accepted
// recorded for the admitted unit; another maker or another
// firmware offered an identical tuple, reconciled by a refresh before any
// word of the supervisor's gate, ends it, never moved to the new set, nothing
// opened; and a unit of the same model and firmware at another serial or
// port is refused by the unit's own check after open (wrong_device).
func TestAnotherMakersUnitDuringPreparationEndsTheCheck(t *testing.T) {
	anotherMaker := func(r *checkRig) {
		m := playbacktest.DACUnit()
		m.Manufacturer = "Another Maker"
		r.box.SetUnit(card, m)
	}
	anotherFirmware := func(r *checkRig) {
		m := playbacktest.DACUnit()
		m.BCDDevice = "0200"
		r.box.SetUnit(card, m)
	}
	t.Run("swapped in as the check prepares", func(t *testing.T) {
		r := newCheckRig(t, playbacktest.ChoiceState(dsdstate.ModeNative), nil)
		_, p := r.preparingCheck()
		anotherMaker(r)
		p.letGo()
		r.endedAs(playback.CheckFailed, playback.CheckPolicyChanged, "another maker's unit as the check prepared")
		if v := r.k.View(); len(v.Opened) != 0 {
			t.Errorf("the check asked %d opens of another maker's unit", len(v.Opened))
		}
	})
	t.Run("swapped in at the open", func(t *testing.T) {
		r := newCheckRig(t, playbacktest.ChoiceState(dsdstate.ModeNative), nil)
		held, release := r.unit.holdNextAdmit()
		t.Cleanup(release)
		r.admitted(dsdstate.TransportNative, playbacktest.DSD64)
		awaitOrdered(t, held, "the open's check after open held")
		anotherMaker(r)
		release()
		r.endedAs(playback.CheckFailed, playback.CheckWrongDevice, "another maker's unit at the open")
		if v := r.k.View(); v.Opens != 1 || v.GivenBytes != 0 {
			t.Errorf("%d opens and %d bytes given; want the one open refused, nothing written", v.Opens, v.GivenBytes)
		}
		if e := playbacktest.Evidence(r.store.State(), dsdstate.TransportNative, string(format.DSDU32BE), playbacktest.DSD64); e != nil {
			t.Errorf("another maker's unit was recorded as the admitted one's: %+v", e)
		}
	})
	for _, c := range []struct {
		name string
		swap func(r *checkRig)
	}{{"another maker", anotherMaker}, {"another firmware", anotherFirmware}} {
		t.Run(c.name+" offered an identical tuple", func(t *testing.T) {
			r := newCheckRig(t, identicalElsewhere(), nil)
			was := r.be.Policy().Tuples
			_, p := r.preparingCheck()
			c.swap(r)
			r.be.RefreshDSD(true)
			if now := r.be.Policy().Tuples; len(now) == 0 || len(now) != len(was) || now[0].Tuple != was[0].Tuple {
				t.Fatalf("the new unit is offered %+v, the old one %+v: the case needs the identical tuple", now, was)
			}
			p.letGo()
			r.endedAs(playback.CheckFailed, playback.CheckPolicyChanged, c.name)
			if v := r.k.View(); len(v.Opened) != 0 {
				t.Errorf("a check reconciled out asked %d opens", len(v.Opened))
			}
		})
	}
	t.Run("another serial or port of the model at the open", func(t *testing.T) {
		r := newCheckRig(t, playbacktest.ChoiceState(dsdstate.ModeNative), nil)
		r.unit.mu.Lock()
		r.unit.refuse = 1
		r.unit.mu.Unlock()
		r.admitted(dsdstate.TransportNative, playbacktest.DSD64)
		r.endedAs(playback.CheckFailed, playback.CheckWrongDevice, "another unit of the model")
		if v := r.k.View(); v.Opens != 1 || v.GivenBytes != 0 {
			t.Errorf("%d opens and %d bytes given; want the one open refused, nothing written", v.Opens, v.GivenBytes)
		}
	})
}

// panickyOpen is an open that answers no sink and a completion that panics:
// the check's own cleanup step failing, which only its recover answers.
type panickyOpen struct{}

func (panickyOpen) Error() string               { return "an open whose completion panics" }
func (panickyOpen) Completion() <-chan struct{} { panic(panicValue) }
func (panickyOpen) CompletionResult() error     { return nil }

// TestAChecksCleanupThatIsNotConfirmedKeepsTheDevice: the check gives the
// slot back only once its cleanup has answered, and fences the device when
// that answer is a failure: exactly where the cleanup's own result wraps
// alsapcm.ErrReleaseUnconfirmed (the sink's CleanupResult, a refused open's
// own error), or its own step panicked. A close the kernel holds keeps the
// slot (stopping): StopCheck answers not given back at CheckStopWait, a
// Start is busy, a probe owed, a check, a tone and a write refused; the close
// let go, nothing is fenced. So does a close held where no sink command waits
// for it, the check ended between its open and its first sink command (at the
// activation's judgement), where the cleanup's own wait for Done is all that
// keeps the slot. A close or a DROP that answers an error, a
// disconnect while the chime plays (the native release's DROP answers
// ENODEV), an open's panic whose close fails, a refused open whose close
// fails or panics, and a panic in the check's own cleanup step each end it
// failed, release_unconfirmed, fence the device until serve restarts, and say
// so at Warn; an open's panic whose release was confirmed (alsapcm.ErrPanicked
// alone) fences nothing. Each case runs in a child process
// (RunEachCaseInAChild): a panic that escaped would end only it. The sink's
// writer's panic is the opener's here: the fake's clock, the one way to make
// the writer panic, is read on the check's path first under the sink's lock
// (beginReleaseLocked), where a panic leaves that lock held for the writer's
// own recover (a residual of B728 B12 PR 0's).
func TestAChecksCleanupThatIsNotConfirmedKeepsTheDevice(t *testing.T) {
	native := playbacktest.ChoiceState(dsdstate.ModeNative)
	alsapcm.RunEachCaseInAChild(t, "TestAChecksCleanupThatIsNotConfirmedKeepsTheDevice", map[string]func(t *testing.T){
		"a close the kernel holds": func(t *testing.T) {
			r := newCheckRig(t, native, nil)
			c := r.playingCheck(dsdstate.TransportNative)
			parked, letClose := r.k.ParkNextClose()
			t.Cleanup(letClose)
			close(c.stop)
			awaitOrdered(t, parked, "the check's close held in the kernel")
			if p := r.phase(); p != playback.CheckStopping {
				t.Errorf("a check whose close is held is %s; want stopping", p)
			}
			stopped := make(chan bool, 1)
			go func() { stopped <- slotBack(r.be.StopCheck(bg)) }()
			r.timers.fire(t, playback.CheckStopWait, "StopCheck's wait")
			if receiveIn(t, stopped, "StopCheck") {
				t.Error("StopCheck answered the slot given back while the close is held")
			}
			req, stop := r.request(dsdstate.TransportNative, playbacktest.DSD64)
			r.refused(req, stop, string(playback.CheckDeviceBusy), "a check beside the held close")
			r.toneRefused(playback.ToneCheckRunning, "a tone beside the held close")
			rep, err := r.be.Probe(bg, probeRequest())
			wantSkipped(t, rep, err, playback.ProbeCheckHolds)
			r.refusedNotStopped("a choice's write beside the held close")
			r.startBusy("a Start beside the held close")
			letClose()
			r.endedAs(playback.CheckStopped, playback.CheckStop, "the check once its close answered")
			r.reusable("a close held, then answered")
		},
		"a check ended at its activation keeps the slot until its close answers": func(t *testing.T) {
			// No sink command waits for this cleanup: the check's own wait
			// for Done is the only one that keeps the slot.
			r := newCheckRig(t, native, nil)
			held, release := r.unit.holdNextAdmit()
			t.Cleanup(release)
			r.admitted(dsdstate.TransportNative, playbacktest.DSD64)
			awaitOrdered(t, held, "the open's check after open held")
			r.store.Set(dsdstate.Refusing(playbacktest.T0))
			parked, letClose := r.k.ParkNextClose()
			t.Cleanup(letClose)
			release()
			awaitOrdered(t, parked, "the sink's close held, the check ended at its activation")
			stopped := make(chan bool, 1)
			go func() { stopped <- slotBack(r.be.StopCheck(bg)) }()
			r.timers.fire(t, playback.CheckStopWait, "StopCheck's wait")
			if receiveIn(t, stopped, "StopCheck") {
				t.Error("StopCheck answered the slot given back while the sink's close is held")
			}
			r.toneRefused(playback.ToneCheckRunning, "a tone beside the held close")
			letClose()
			r.endedAs(playback.CheckFailed, playback.CheckPolicyChanged, "the check once its close answered")
		},
		"a close that answers an error": func(t *testing.T) {
			r := newCheckRig(t, native, nil)
			r.dsdRig.mu.Lock()
			r.cleanup = "close"
			r.dsdRig.mu.Unlock()
			c := r.playingCheck(dsdstate.TransportNative)
			close(c.stop)
			r.endedAs(playback.CheckFailed, playback.CheckReleaseUnconfirmed, "a close that failed")
			r.fenceSaid("injected close failure")
			r.checkFenced("a close that failed")
		},
		"a DROP that answers an error": func(t *testing.T) {
			r := newCheckRig(t, native, nil)
			r.dsdRig.mu.Lock()
			r.cleanup = "drop"
			r.dsdRig.mu.Unlock()
			c := r.playingCheck(dsdstate.TransportNative)
			close(c.stop)
			r.endedAs(playback.CheckFailed, playback.CheckReleaseUnconfirmed, "a DROP that failed")
			// The fake kernel's errno, by its own words.
			r.fenceSaid("errno 5")
			r.checkFenced("a DROP that failed")
		},
		"a disconnect while the chime plays": func(t *testing.T) {
			r := newCheckRig(t, native, nil)
			r.playingCheck(dsdstate.TransportNative)
			r.k.Unplug()
			r.endedAs(playback.CheckFailed, playback.CheckReleaseUnconfirmed, "a disconnect mid-chime")
			r.fenceSaid("ENODEV")
			r.checkFenced("a disconnect mid-chime")
		},
		"an open's panic, its release confirmed": func(t *testing.T) {
			r := newCheckRig(t, native, nil)
			r.unit.mu.Lock()
			r.unit.panics = panicValue
			r.unit.mu.Unlock()
			r.admitted(dsdstate.TransportNative, playbacktest.DSD64)
			r.endedAs(playback.CheckFailed, playback.CheckBackendFailed, "an open whose check panicked")
			r.logs.panicLogged(t, nativePanicMessage, "the native sink's opener")
			r.reusable("an open's panic, its release confirmed")
		},
		"an open's panic, its close failing": func(t *testing.T) {
			r := newCheckRig(t, native, nil)
			r.unit.mu.Lock()
			r.unit.panics = panicValue
			r.unit.mu.Unlock()
			r.k.FailNextClose(syscall.EIO)
			r.admitted(dsdstate.TransportNative, playbacktest.DSD64)
			r.endedAs(playback.CheckFailed, playback.CheckReleaseUnconfirmed, "an open's panic whose close failed")
			r.fenceSaid(syscall.EIO.Error())
			r.checkFenced("an open's panic whose close failed")
		},
		"a refused open whose close fails": func(t *testing.T) {
			r := newCheckRig(t, native, nil)
			r.unit.mu.Lock()
			r.unit.refuse = 1
			r.unit.mu.Unlock()
			r.k.FailNextClose(syscall.EIO)
			r.admitted(dsdstate.TransportNative, playbacktest.DSD64)
			r.endedAs(playback.CheckFailed, playback.CheckReleaseUnconfirmed, "a refused open whose close failed")
			r.fenceSaid(syscall.EIO.Error())
			r.checkFenced("a refused open whose close failed")
		},
		"a refused open whose close panics": func(t *testing.T) {
			r := newCheckRig(t, native, nil)
			r.unit.mu.Lock()
			r.unit.refuse = 1
			r.unit.mu.Unlock()
			r.k.PanicAtNextClose(panicValue)
			r.admitted(dsdstate.TransportNative, playbacktest.DSD64)
			r.endedAs(playback.CheckFailed, playback.CheckReleaseUnconfirmed, "a refused open whose close panicked")
			r.fenceSaid("")
			r.checkFenced("a refused open whose close panicked")
		},
		"a panic in the check's own cleanup step": func(t *testing.T) {
			r := newCheckRig(t, native, nil)
			r.dsdRig.mu.Lock()
			r.openNative = func(context.Context, alsapcm.DSDConfig) (*alsapcm.DSDSink, error) { return nil, panickyOpen{} }
			r.dsdRig.mu.Unlock()
			r.admitted(dsdstate.TransportNative, playbacktest.DSD64)
			r.endedAs(playback.CheckFailed, playback.CheckReleaseUnconfirmed, "a cleanup that panicked")
			// Its error wraps alsapcm.ErrReleaseUnconfirmed, as every fenced
			// cleanup's does.
			r.fenceSaid("the DSD check's cleanup panicked; the bridge's log has why: native release unconfirmed")
			r.logs.panicLogged(t, backendPanicMessage, "the DSD check's cleanup")
		},
	})
}

// pendingCompletion is an open's answer whose worker finishes it later,
// alsapcm's pending open as the check's cleanup reads it.
type pendingCompletion interface {
	Completion() <-chan struct{}
	CompletionResult() error
}

// markedOpen is a pending open's answer as the rig's stand-in hands it on:
// the opener's own, its completion's first ask closing waiting.
type markedOpen struct {
	err     error
	pending pendingCompletion
	waiting chan struct{}
	once    sync.Once
}

func (m *markedOpen) Error() string { return m.err.Error() }
func (m *markedOpen) Unwrap() error { return m.err }
func (m *markedOpen) Completion() <-chan struct{} {
	m.once.Do(func() { close(m.waiting) })
	return m.pending.Completion()
}
func (m *markedOpen) CompletionResult() error { return m.pending.CompletionResult() }

// markerBound is how long a case waits for a marker a few goroutine steps
// after the step that causes it, short of the watchdog.
const markerBound = 5 * time.Second

// awaitMarker waits for ch to close within markerBound.
func awaitMarker(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	timer := time.NewTimer(markerBound)
	defer timer.Stop()
	select {
	case <-ch:
	case <-timer.C:
		alsapcm.LogGoroutines(t)
		t.Fatalf("waited %v for %s", markerBound, what)
	}
}

// TestAStopDuringAParkedOpenWaitsForItsCompletion: a check stopped while its
// sink's open is held in the kernel ends through the opener's completion
// fence: the open answers no sink and its worker keeps the node, so the check
// keeps the slot (stopping), refusing a tone, until the worker has finished.
// The rig's stand-in opens through the fake kernel and marks that the open
// answered pending (returned) and that the cleanup asked for its completion
// (waiting), each waited for within markerBound before the kernel is let go,
// so the case takes that branch on every run. An open refused for the stop
// whose node then closes cleanly leaves the slot reusable, its error no
// cleanup's (stopped, stop), and one whose node's close fails fences the
// device (failed, release_unconfirmed).
func TestAStopDuringAParkedOpenWaitsForItsCompletion(t *testing.T) {
	for _, c := range []struct {
		name      string
		closeFail bool
	}{{"its node closed cleanly", false}, {"its node's close failing", true}} {
		t.Run(c.name, func(t *testing.T) {
			r := newCheckRig(t, playbacktest.ChoiceState(dsdstate.ModeNative), nil)
			returned, waiting := make(chan struct{}), make(chan struct{})
			var once sync.Once
			r.dsdRig.mu.Lock()
			r.openNative = func(ctx context.Context, cfg alsapcm.DSDConfig) (*alsapcm.DSDSink, error) {
				s, err := r.k.OpenNativeSink(ctx, cfg)
				var p pendingCompletion
				if errors.As(err, &p) {
					once.Do(func() { close(returned) })
					return nil, &markedOpen{err: err, pending: p, waiting: waiting}
				}
				return s, err
			}
			r.dsdRig.mu.Unlock()
			parked, letOpen := r.k.ParkNextOpen()
			t.Cleanup(letOpen)
			call := r.admitted(dsdstate.TransportNative, playbacktest.DSD64)
			awaitOrdered(t, parked, "the check's open held in the kernel")
			if c.closeFail {
				r.k.FailNextClose(syscall.EIO)
			}
			close(call.stop)
			awaitMarker(t, returned, "the open answered pending, its worker keeping the node")
			awaitMarker(t, waiting, "the check's cleanup waiting for the open's completion")
			if p := r.phase(); p != playback.CheckStopping {
				t.Fatalf("the check whose open's completion is waited for is %s; want stopping, the slot still its own", p)
			}
			r.toneRefused(playback.ToneCheckRunning, "a tone while the open's worker holds the node")
			letOpen()
			if !c.closeFail {
				r.endedAs(playback.CheckStopped, playback.CheckStop, c.name)
				r.reusable(c.name)
				return
			}
			r.endedAs(playback.CheckFailed, playback.CheckReleaseUnconfirmed, c.name)
			r.fenceSaid(syscall.EIO.Error())
			r.checkFenced(c.name)
		})
	}
}

// TestAParkedMonitorReadingHoldsNoEndOfTheCheck: StopCheck, while the check's
// monitor's reading of the box is held under the refreshes' lock, ends the
// check and gives the slot back within its own bound, the reading still held,
// since no transition of the check waits for the refreshes' lock: its end is
// published (stopped, deleted) at a later publication, the generation as it
// was, and a tone then plays. The monitor itself is joined once its reading
// answers.
func TestAParkedMonitorReadingHoldsNoEndOfTheCheck(t *testing.T) {
	box := &checkBox{}
	r := newCheckRig(t, playbacktest.ChoiceState(dsdstate.ModeNative), withCheckBox(box))
	r.playingCheck(dsdstate.TransportNative)
	p := box.holdNext()
	t.Cleanup(p.letGo)
	r.monitorTickThatRevokes()
	awaitOrdered(t, p.in, "the monitor's reading held")
	before := r.be.Policy()
	if !slotBack(r.be.StopCheck(bg)) {
		t.Fatal("the check did not give the slot back while its monitor's reading was held")
	}
	if m, now := r.member(), r.be.Policy(); m == nil || m.Phase != playback.CheckStopped || m.Reason != playback.CheckDeleted ||
		now.Publication <= before.Publication || now.Generation != before.Generation {
		t.Errorf("with the monitor's reading held the check is published %+v at publication %d (was %d), generation %d (was %d); want stopped, deleted, later, at the same generation",
			m, now.Publication, before.Publication, now.Generation, before.Generation)
	}
	if st, _ := r.be.CheckStatus(); st.Phase != playback.CheckStopped || st.Reason != playback.CheckDeleted {
		t.Errorf("the check's record with its monitor's reading held: %+v; want stopped, deleted", st)
	}
	if n, _ := r.be.Running(); n == 0 {
		t.Error("the monitor held in its reading is not counted")
	}
	r.tonePlays("a tone while the check's monitor's reading is held")
	p.letGo()
	r.endedAs(playback.CheckStopped, playback.CheckDeleted, "the check once its monitor's reading answered")
}

// TestServesStopWaitsForEveryCheckItCounted: CloseChecks closes the checks for
// good, and serve's Wait then waits for every check counted: one held in its
// admission's reading of the box before any record exists, refused
// serve_stopping once let go, nothing opened; one whose monitor's reading is
// held after the check has ended; and one whose sink's close is held, which
// keeps the slot (stopping) until it answers. A check asked after CloseChecks
// is refused serve_stopping.
func TestServesStopWaitsForEveryCheckItCounted(t *testing.T) {
	native := playbacktest.ChoiceState(dsdstate.ModeNative)
	counted := func(r *checkRig, what string) {
		r.t.Helper()
		if n, _ := r.be.Running(); n == 0 {
			r.t.Errorf("%s: no goroutine counted, so Wait would not wait", what)
		}
	}
	closedAfter := func(r *checkRig) {
		r.t.Helper()
		req, stop := r.request(dsdstate.TransportNative, playbacktest.DSD64)
		r.refused(req, stop, string(playback.CheckServeStopping), "a check after CloseChecks")
	}
	t.Run("an admission held in its reading", func(t *testing.T) {
		box := &checkBox{}
		r := newCheckRig(t, native, withCheckBox(box))
		req, stop := r.request(dsdstate.TransportNative, playbacktest.DSD64)
		p := box.holdNext()
		t.Cleanup(p.letGo)
		asked := make(chan checkCall, 1)
		go func() { asked <- r.ask(req, stop) }()
		awaitOrdered(t, p.in, "the admission's reading held")
		if _, ok := r.be.CheckStatus(); ok {
			t.Error("a check held in its admission has a record")
		}
		r.be.CloseChecks()
		counted(r, "an admission held in its reading")
		p.letGo()
		if c := receiveIn(t, asked, "the admission's answer"); checkRefusal(c.err) != string(playback.CheckServeStopping) {
			t.Errorf("the admission let go after CloseChecks answered %v; want serve_stopping", c.err)
		}
		r.joined("an admission held in its reading")
		if _, ok := r.be.CheckStatus(); ok {
			t.Error("the admission refused after CloseChecks made a record")
		}
		if v := r.k.View(); len(v.Opened) != 0 {
			t.Errorf("%d opens asked", len(v.Opened))
		}
		closedAfter(r)
	})
	t.Run("a monitor held in its reading", func(t *testing.T) {
		box := &checkBox{}
		r := newCheckRig(t, native, withCheckBox(box))
		r.playingCheck(dsdstate.TransportNative)
		p := box.holdNext()
		t.Cleanup(p.letGo)
		r.monitorTickThatRevokes()
		awaitOrdered(t, p.in, "the monitor's reading held")
		r.be.CloseChecks()
		if !slotBack(r.be.StopCheck(bg)) {
			t.Fatal("the check did not give the slot back")
		}
		counted(r, "a monitor held in its reading")
		p.letGo()
		r.endedAs(playback.CheckStopped, playback.CheckServeStopping, "the check closed with its monitor held")
		closedAfter(r)
	})
	t.Run("a close held in the kernel", func(t *testing.T) {
		r := newCheckRig(t, native, nil)
		r.playingCheck(dsdstate.TransportNative)
		parked, letClose := r.k.ParkNextClose()
		t.Cleanup(letClose)
		r.be.CloseChecks()
		awaitOrdered(t, parked, "the check's close held")
		if p := r.phase(); p != playback.CheckStopping {
			t.Errorf("the check whose close is held is %s; want stopping", p)
		}
		counted(r, "a close held in the kernel")
		closedAfter(r)
		letClose()
		r.endedAs(playback.CheckStopped, playback.CheckServeStopping, "the check closed with its close held")
	})
}
