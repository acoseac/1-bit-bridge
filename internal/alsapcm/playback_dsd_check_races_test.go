package alsapcm_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/player/dsdstate"
	"github.com/acoseac/1-bit-bridge/internal/player/playback"
	"github.com/acoseac/1-bit-bridge/internal/player/playback/playbacktest"
)

// The DSD check's admission, stop and answer where an end and a step of the
// backend meet (B728 B12 PR 4a round 2), through the backend and the real
// native sink over the fake kernel: a request's end and the hold's stop signal
// answered while what the admission waits for is still held, a stop's answer
// read as one snapshot of its check, and an answer's ten minutes measured
// inside the state's one write.

// askAsync asks req on a goroutine of its own, under ctx.
func (r *checkRig) askAsync(ctx context.Context, req playback.CheckRequest, stop chan struct{}) <-chan checkCall {
	answer := make(chan checkCall, 1)
	go func() {
		h, err := r.be.Check(ctx, req)
		answer <- checkCall{st: statusOf(h), err: err, stop: stop}
	}()
	return answer
}

// probeHeld puts the silent probe in the device slot, its open held in the
// kernel until the test lets it go; probed closes once the probe returns.
func (r *checkRig) probeHeld() (probed <-chan struct{}, letOpen func()) {
	r.t.Helper()
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

// TestAnAdmissionsEndIsAnsweredWhileItsReadingIsHeld: a check whose request
// ends while its admission's reading of the box is held is answered with the
// request's error while that reading is still held, the reading counted until
// it is let go; let go, it publishes as a refresh does (here the firmware the
// box reports by then, under a token of its own) and takes no slot: nothing
// opened and no record made. Where the request ends once the check is
// admitted, the check is the slot's and goes on; StopCheck ends it, stopped
// deleted, its cleanup confirmed, nothing opened.
func TestAnAdmissionsEndIsAnsweredWhileItsReadingIsHeld(t *testing.T) {
	native := playbacktest.ChoiceState(dsdstate.ModeNative)
	t.Run("the request's end while the reading is held", func(t *testing.T) {
		box := &checkBox{}
		r := newCheckRig(t, native, withCheckBox(box))
		req, stop := r.request(dsdstate.TransportNative, playbacktest.DSD64)
		before := r.be.Policy()
		p := box.holdNext()
		t.Cleanup(p.letGo)
		ctx, cancel := context.WithCancel(bg)
		defer cancel()
		answer := r.askAsync(ctx, req, stop)
		awaitOrdered(t, p.in, "the admission's reading held in the box")
		firmware := "0200"
		box.bcd.Store(&firmware)
		cancel()
		c := receiveIn(t, answer, "the check's answer while its reading is held")
		if n, _ := r.be.Running(); n == 0 {
			t.Error("the reading held in the box is not counted once the check has answered")
		}
		p.letGo()
		r.refusedEnded(c, "a request ended while its reading was held")
		if now := r.be.Policy(); now.Publication <= before.Publication || now.DSD == nil || now.DSD.DAC == nil || now.DSD.DAC.Token == req.DACToken {
			t.Errorf("once let go, the reading published %+v; want the firmware it read, under a token of its own", now.DSD)
		}
	})
	t.Run("the request's end once the check is admitted", func(t *testing.T) {
		r := newCheckRig(t, native, nil)
		prep := r.holdPreparation()
		req, _ := r.request(dsdstate.TransportNative, playbacktest.DSD64)
		ctx, cancel := context.WithCancel(bg)
		h, err := r.be.Check(ctx, req)
		if err != nil {
			cancel()
			t.Fatalf("the check was refused: %v", err)
		}
		awaitOrdered(t, prep.in, "the check's preparation held")
		cancel()
		if p := r.phase(); p != playback.CheckPreparing {
			t.Errorf("the request's end after its admission made the check %s; want it preparing", p)
		}
		cleanup, pol := r.be.StopCheck(bg)
		if m := pol.DSD.DAC.Check; cleanup != playback.CheckCleanupConfirmed || m == nil || m.ID != h.Status.ID || m.Phase != playback.CheckStopped || m.Reason != playback.CheckDeleted {
			t.Errorf("StopCheck answered %s with %+v; want confirmed, the check stopped deleted", cleanup, m)
		}
		r.endedAs(playback.CheckStopped, playback.CheckDeleted, "the check stopped once admitted")
		if v := r.k.View(); v.Opens != 0 {
			t.Errorf("%d opens asked; want none", v.Opens)
		}
	})
}

// TestAStopBeforeTheSlotEndsTheAdmission: the hold's stop signal, the
// request's context live, ends a check's admission before it takes the device
// slot, refused with the stop's word while what it waits for is still held:
// the probe in the slot, the last check's record surviving and nothing opened
// for the check once the probe is let go (the probe's own open apart); and its
// reading of the box beside the console's tone, which plays on, never
// preempted, and ends by its own request's end.
func TestAStopBeforeTheSlotEndsTheAdmission(t *testing.T) {
	native := playbacktest.ChoiceState(dsdstate.ModeNative)
	stopped := func(t *testing.T, c checkCall, what string) {
		t.Helper()
		if w := checkRefusal(c.err); w != string(playback.CheckStop) || !sameCheckStatus(c.st, playback.CheckStatus{}) {
			t.Errorf("%s: the check answered %+v, %v; want refused %s", what, c.st, c.err, playback.CheckStop)
		}
	}
	t.Run("while the probe it waits for is held", func(t *testing.T) {
		r := newCheckRig(t, native, nil)
		last := r.completed()
		opened := len(r.k.View().Opened)
		probed, letOpen := r.probeHeld()
		req, stop := r.request(dsdstate.TransportNative, playbacktest.DSD64)
		answer := r.askAsync(bg, req, stop)
		receiveIn(t, r.timers.arms[playback.ProbeWait], "the check's wait for the probe")
		close(stop)
		stopped(t, receiveIn(t, answer, "the check's answer while the probe is held"), "a stop while the probe is held")
		letOpen()
		receiveIn(t, probed, "the probe's end")
		r.joined("the probe let go")
		if st, ok := r.be.CheckStatus(); !ok || !sameCheckStatus(st, last) {
			t.Errorf("the record is %+v; want the last check's, %+v", st, last)
		}
		if n := len(r.k.View().Opened) - opened; n != 1 {
			t.Errorf("%d opens once the probe was let go; want the probe's alone", n)
		}
	})
	t.Run("while its reading is held beside the console's tone", func(t *testing.T) {
		box := &checkBox{}
		r := newCheckRig(t, native, withCheckBox(box))
		req, stop := r.request(dsdstate.TransportNative, playbacktest.DSD64)
		tone := r.playingTone(30)
		p := box.holdNext()
		t.Cleanup(p.letGo)
		answer := r.askAsync(bg, req, stop)
		awaitOrdered(t, p.in, "the admission's reading held in the box")
		close(stop)
		stopped(t, receiveIn(t, answer, "the check's answer while its reading is held"), "a stop while its reading is held")
		p.letGo()
		r.be.RefreshDSD(true)
		select {
		case res := <-tone.answer:
			t.Errorf("the tone beside the stopped check answered %+v; want it playing", res)
		default:
		}
		tone.cancel()
		if res := tone.result(t); res.Outcome == playback.TonePreempted {
			t.Errorf("the tone ended %+v; want it ended by its own request, never preempted", res)
		}
		r.joinTone()
		if _, ok := r.be.CheckStatus(); ok {
			t.Error("a check stopped before the slot made a record")
		}
	})
}

// stopAnswer is StopCheck's answer.
type stopAnswer struct {
	cleanup playback.CheckCleanup
	policy  playback.PlayerPolicy
}

// stopAsync asks StopCheck on a goroutine of its own.
func (r *checkRig) stopAsync() <-chan stopAnswer {
	answer := make(chan stopAnswer, 1)
	go func() {
		cleanup, pol := r.be.StopCheck(bg)
		answer <- stopAnswer{cleanup, pol}
	}()
	return answer
}

// heldOnce is a seam that holds the first goroutine to reach it until
// released: in closes as it is held.
type heldOnce struct {
	in, gate chan struct{}
	once     sync.Once
}

func newHeldOnce(t *testing.T) *heldOnce {
	h := &heldOnce{in: make(chan struct{}), gate: make(chan struct{})}
	t.Cleanup(h.release)
	return h
}

func (h *heldOnce) hold() {
	first := false
	h.once.Do(func() { first = true; close(h.in) })
	if first {
		<-h.gate
	}
}

func (h *heldOnce) release() {
	select {
	case <-h.gate:
	default:
		close(h.gate)
	}
}

// fireStopWait fires StopCheck's wait, the 2 s timer armed after the check's
// preparation's bound (its first), whether or not a stop answered by its
// check's end has stopped it: closing it then changes nothing.
func (r *checkRig) fireStopWait() {
	r.t.Helper()
	receiveIn(r.t, r.timers.arms[playback.CheckStopWait], "the check's preparation's bound")
	receiveIn(r.t, r.timers.arms[playback.CheckStopWait], "StopCheck's wait").f()
}

// TestAStopsAnswerIsOneReadingOfItsCheck: StopCheck answers how the stopped
// check's cleanup ended and the pair published in one step of the backend's,
// so the two say one thing of that check and its token: pending beside
// stopping, or the end beside its own publication, never pending beside a
// published end. Its wait's bound fires as its check's failed cleanup has
// published its end and not yet returned; and its check's cleanup ends after
// its wait and before its answer is read.
func TestAStopsAnswerIsOneReadingOfItsCheck(t *testing.T) {
	native := playbacktest.ChoiceState(dsdstate.ModeNative)
	agrees := func(t *testing.T, a stopAnswer, c checkCall, end playback.CheckPhase, reason playback.CheckReason, what string) {
		t.Helper()
		d := a.policy.DSD.DAC
		if d == nil || d.Check == nil || d.Check.ID != c.st.ID || d.Token != c.st.Token {
			t.Fatalf("%s: StopCheck's pair publishes %+v; want the stopped check, of its token", what, d)
		}
		m := d.Check
		switch {
		case a.cleanup == playback.CheckCleanupPending && m.Phase == playback.CheckStopping:
		case a.cleanup != playback.CheckCleanupPending && m.Phase == end && m.Reason == reason:
		default:
			t.Errorf("%s: StopCheck answered %s beside the check published %s %q; want pending beside stopping, or its end beside %s %q",
				what, a.cleanup, m.Phase, m.Reason, end, reason)
		}
	}
	t.Run("its bound at a failed cleanup's published end", func(t *testing.T) {
		ended := (*heldOnce)(nil)
		r := newCheckRig(t, native, nil, func(o *rigOptions) {
			o.seams = &playback.Seams{CheckEnded: func() { ended.hold() }}
		})
		ended = newHeldOnce(t)
		r.dsdRig.mu.Lock()
		r.cleanup = "close"
		r.dsdRig.mu.Unlock()
		c := r.playingCheck(dsdstate.TransportNative)
		answer := r.stopAsync()
		awaitOrdered(t, ended.in, "the check's failed cleanup's end published")
		r.fireStopWait()
		a := receiveIn(t, answer, "StopCheck")
		ended.release()
		if a.cleanup != playback.CheckCleanupFailed {
			t.Errorf("StopCheck answered %s once its check's failed cleanup had published its end; want failed", a.cleanup)
		}
		agrees(t, a, c, playback.CheckFailed, playback.CheckReleaseUnconfirmed, "its bound at a failed cleanup's published end")
		r.endedAs(playback.CheckFailed, playback.CheckReleaseUnconfirmed, "the check whose close failed")
	})
	t.Run("its check's end between its wait and its answer", func(t *testing.T) {
		answering := (*heldOnce)(nil)
		r := newCheckRig(t, native, nil, func(o *rigOptions) {
			o.seams = &playback.Seams{CheckStopAnswering: func() { answering.hold() }}
		})
		answering = newHeldOnce(t)
		c := r.playingCheck(dsdstate.TransportNative)
		parked, letClose := r.k.ParkNextClose()
		t.Cleanup(letClose)
		answer := r.stopAsync()
		awaitOrdered(t, parked, "the check's close held in the kernel")
		r.fireStopWait()
		awaitOrdered(t, answering.in, "StopCheck past its wait")
		letClose()
		r.joined("the check's cleanup")
		answering.release()
		a := receiveIn(t, answer, "StopCheck")
		if a.cleanup != playback.CheckCleanupConfirmed {
			t.Errorf("StopCheck answered %s once its check's cleanup had ended; want confirmed", a.cleanup)
		}
		agrees(t, a, c, playback.CheckStopped, playback.CheckDeleted, "its check's end between its wait and its answer")
		r.endedAs(playback.CheckStopped, playback.CheckDeleted, "the check whose close was held")
	})
}

// heldStore is the rig's DSD state store whose next Update, once armed
// (holdNext), is held before the store takes it: a write waiting for the
// store's lock.
type heldStore struct {
	playback.DSDStore
	mu   sync.Mutex
	park *parkAt
}

func (s *heldStore) holdNext() *parkAt {
	p := newParkAt()
	s.mu.Lock()
	s.park = p
	s.mu.Unlock()
	return p
}

func (s *heldStore) Update(f func(*dsdstate.State) error) error {
	s.mu.Lock()
	p := s.park
	s.park = nil
	s.mu.Unlock()
	if p != nil {
		p.pass()
	}
	return s.DSDStore.Update(f)
}

// TestAnAnswersAgeIsDecidedInTheStoresWrite: the ten minutes a check is
// answerable are measured inside the state's one write, by the policy's clock
// read there: an answer held just before that write while the clock passes
// CheckAnswerable is check_not_answerable, nothing written and the check left
// complete; held while the clock reaches exactly CheckAnswerable, it is
// written and the check answered.
func TestAnAnswersAgeIsDecidedInTheStoresWrite(t *testing.T) {
	native := playbacktest.ChoiceState(dsdstate.ModeNative)
	for _, c := range []struct {
		name   string
		at     time.Duration
		writes int
		want   playback.ChoiceOutcome
		phase  playback.CheckPhase
	}{
		{"past CheckAnswerable at the write", playback.CheckAnswerable + time.Second, 0, playback.ChoiceCheckNotAnswerable, playback.CheckComplete},
		{"exactly CheckAnswerable at the write", playback.CheckAnswerable, 1, playback.ChoiceWritten, playback.CheckAnswered},
	} {
		t.Run(c.name, func(t *testing.T) {
			var offset atomic.Int64
			store := &heldStore{}
			r := newCheckRig(t, native, func(p *playback.DSDPolicy) {
				p.Now = func() time.Time { return playbacktest.T0.Add(time.Duration(offset.Load())) }
				store.DSDStore = p.Store
				p.Store = store
			})
			st := r.completed()
			writes := r.store.Writes()
			p := store.holdNext()
			t.Cleanup(p.letGo)
			answered := make(chan playback.ChoiceResult, 1)
			go func() { answered <- r.answer(st.ID, dsdstate.AnswerClean) }()
			awaitOrdered(t, p.in, "the answer's write held before the store's")
			offset.Store(int64(c.at))
			p.letGo()
			if res := receiveIn(t, answered, "the answer"); res.Outcome != c.want || !res.Epoch {
				t.Errorf("the answer whose write was held while the clock reached %v: %+v; want %s with the pair published", c.at, res, c.want)
			}
			if n := r.store.Writes() - writes; n != c.writes {
				t.Errorf("the state was written %d times; want %d", n, c.writes)
			}
			if got, _ := r.be.CheckStatus(); got.Phase != c.phase {
				t.Errorf("the check is %s; want %s", got.Phase, c.phase)
			}
			if c.writes == 0 {
				if _, n := answerOf(r.store.State(), playbacktest.DSD64); n != 0 {
					t.Errorf("the state holds %d answers; want none", n)
				}
			}
		})
	}
}

// TestAStoppedAdmissionsQueuedReadingReadsNothing: a check whose admission's
// reading waits for the refreshes' lock, a refresh holding it across its own
// reading of the box, and whose hold's stop signal then comes, its request's
// context live, is refused with the stop's word at once; once that refresh is
// let go and the backend's work joined, the box was read once past the rig's
// setup, by that refresh alone: the queued reading, holding the lock at last,
// read nothing.
func TestAStoppedAdmissionsQueuedReadingReadsNothing(t *testing.T) {
	queued := make(chan struct{})
	var once sync.Once
	box := &checkBox{}
	r := newCheckRig(t, playbacktest.ChoiceState(dsdstate.ModeNative), withCheckBox(box), func(o *rigOptions) {
		o.seams = &playback.Seams{DSDStep: func(step string) {
			if step == "queued" {
				once.Do(func() { close(queued) })
			}
		}}
	})
	req, stop := r.request(dsdstate.TransportNative, playbacktest.DSD64)
	setup := box.reads.Load()
	p := box.holdNext()
	t.Cleanup(p.letGo)
	refreshed := make(chan struct{})
	go func() {
		defer close(refreshed)
		r.be.RefreshDSD(true)
	}()
	awaitOrdered(t, p.in, "the refresh's reading held in the box")
	answer := r.askAsync(bg, req, stop)
	awaitOrdered(t, queued, "the admission's reading waiting for the refreshes' lock")
	close(stop)
	c := receiveIn(t, answer, "the check's answer while its reading waits for the lock")
	if w := checkRefusal(c.err); w != string(playback.CheckStop) || !sameCheckStatus(c.st, playback.CheckStatus{}) {
		t.Errorf("the check answered %+v, %v; want refused %s", c.st, c.err, playback.CheckStop)
	}
	p.letGo()
	receiveIn(t, refreshed, "the refresh let go")
	r.joined("the admission's reading once the lock was let go")
	if n := box.reads.Load() - setup; n != 1 {
		t.Errorf("the box was read %d times past the rig's setup; want the refresh's one reading", n)
	}
	if _, ok := r.be.CheckStatus(); ok {
		t.Error("a check stopped before its reading made a record")
	}
}

// TestTheAdmissionsReadingKeepsTheRefusalsOrder: a check refused as its
// admission's reading of the policy answers, or as that reading's wait ends,
// is refused in the order the slot refuses in: its request's error, serve's
// stop (serve_stopping, no epoch), the stop's word, then dac_unavailable for a
// reading that failed, with no handle and nothing opened: its request ended
// and its stop signal come, both before Check (in rounds, its select picking
// at random); serve's stop after Check's entry, before its stop signal or
// before its reading fails; its stop signal once a failed reading's answer
// has reached the admission and before its refusal is decided; and a reading
// that fails, the admissible pair before it standing.
func TestTheAdmissionsReadingKeepsTheRefusalsOrder(t *testing.T) {
	native := playbacktest.ChoiceState(dsdstate.ModeNative)
	refusedAs := func(t *testing.T, c checkCall, word string, epoch bool, what string) *playback.CheckRefused {
		t.Helper()
		var rf *playback.CheckRefused
		if !errors.As(c.err, &rf) || rf.Word != word || rf.Epoch != epoch || !sameCheckStatus(c.st, playback.CheckStatus{}) {
			t.Errorf("%s: the check answered %+v, %v; want refused %s, its epoch %v", what, c.st, c.err, word, epoch)
		}
		return rf
	}
	nothingOpened := func(t *testing.T, r *checkRig, what string) {
		t.Helper()
		r.joined(what)
		if v := r.k.View(); v.Opens != 0 {
			t.Errorf("%s: %d opens asked; want none", what, v.Opens)
		}
		if _, ok := r.be.CheckStatus(); ok {
			t.Errorf("%s: a record was made", what)
		}
	}
	t.Run("its request ended and its stop signal come before Check", func(t *testing.T) {
		r := newCheckRig(t, native, nil)
		for round := range 32 {
			req, stop := r.request(dsdstate.TransportNative, playbacktest.DSD64)
			close(stop)
			ctx, cancel := context.WithCancel(bg)
			cancel()
			h, err := r.be.Check(ctx, req)
			if !errors.Is(err, context.Canceled) || h != nil {
				t.Fatalf("round %d: the check answered %+v, %v; want its request's error", round, statusOf(h), err)
			}
		}
		nothingOpened(t, r, "checks whose request ended and whose stop signal came")
	})
	for _, c := range []struct {
		name string
		end  func(*checkBox, chan struct{})
	}{
		{"serve's stop before its stop signal", func(_ *checkBox, stop chan struct{}) { close(stop) }},
		{"serve's stop before its reading fails", func(box *checkBox, _ chan struct{}) { box.panics.Store(true) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			box := &checkBox{}
			r := newCheckRig(t, native, withCheckBox(box))
			req, stop := r.request(dsdstate.TransportNative, playbacktest.DSD64)
			p := box.holdNext()
			t.Cleanup(p.letGo)
			answer := r.askAsync(bg, req, stop)
			awaitOrdered(t, p.in, "the admission's reading held in the box")
			r.be.CloseChecks()
			c.end(box, stop)
			p.letGo()
			refusedAs(t, receiveIn(t, answer, "the check's answer"), string(playback.CheckServeStopping), false, c.name)
			nothingOpened(t, r, c.name)
		})
	}
	t.Run("its stop signal once a failed reading has answered", func(t *testing.T) {
		failed := (*heldOnce)(nil)
		box := &checkBox{}
		r := newCheckRig(t, native, withCheckBox(box), func(o *rigOptions) {
			o.seams = &playback.Seams{DSDStep: func(step string) {
				if step == "check read failed" {
					failed.hold()
				}
			}}
		})
		failed = newHeldOnce(t)
		req, stop := r.request(dsdstate.TransportNative, playbacktest.DSD64)
		box.panics.Store(true)
		answer := r.askAsync(bg, req, stop)
		awaitOrdered(t, failed.in, "the failed reading's answer at the admission")
		close(stop)
		failed.release()
		refusedAs(t, receiveIn(t, answer, "the check's answer"), string(playback.CheckStop), true, "its stop signal once a failed reading has answered")
		nothingOpened(t, r, "its stop signal once a failed reading has answered")
	})
	t.Run("a reading that fails", func(t *testing.T) {
		box := &checkBox{}
		r := newCheckRig(t, native, withCheckBox(box))
		req, stop := r.request(dsdstate.TransportNative, playbacktest.DSD64)
		box.panics.Store(true)
		rf := refusedAs(t, r.ask(req, stop), playback.CheckDACUnavailable, true, "a reading that fails")
		if rf != nil && (rf.Policy.DSD == nil || rf.Policy.DSD.DAC == nil || rf.Policy.DSD.DAC.Token != req.DACToken) {
			t.Errorf("the refusal carries %+v; want the admissible pair standing before the reading failed", rf.Policy.DSD)
		}
		nothingOpened(t, r, "a reading that fails")
	})
}
