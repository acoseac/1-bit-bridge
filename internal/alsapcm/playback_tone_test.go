package alsapcm_test

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"math"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/player/dsdstate"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/format"
	"github.com/acoseac/1-bit-bridge/internal/player/playback"
	"github.com/acoseac/1-bit-bridge/internal/player/playback/playbacktest"
)

// The console's test tone through the player's backend (B601): the device
// slot it shares with a session and the silent probe, the supervisor's gate,
// its opens, what it writes, how it ends and what its cleanup's failure
// fences, through the engine's Run, the backend and the fake kernel. No test
// sleeps: the backend's timers are held (toneTimers), the DAC plays the time a
// test grants, and the tone's steps are held at the backend's seams.

// toneOf is a tone of level dBFS for seconds.
func toneOf(level float64, seconds int) playback.ToneRequest {
	return playback.ToneRequest{LevelDBFS: level, Seconds: seconds}
}

// toneCall is a tone asked on a goroutine of its own, its request's context
// the test's to end.
type toneCall struct {
	ctx    context.Context
	cancel context.CancelFunc
	answer chan playback.ToneResult
}

// startTone asks the backend for req on a goroutine of its own.
func (r *rig) startTone(req playback.ToneRequest) *toneCall {
	r.t.Helper()
	ctx, cancel := context.WithCancel(bg)
	r.t.Cleanup(cancel)
	c := &toneCall{ctx: ctx, cancel: cancel, answer: make(chan playback.ToneResult, 1)}
	go func() { c.answer <- r.be.Tone(ctx, req) }()
	return c
}

// result waits for the tone's answer, under the watchdog.
func (c *toneCall) result(t *testing.T) playback.ToneResult {
	t.Helper()
	return receiveIn(t, c.answer, "the tone's answer")
}

// playingTone opens the tone's gate as the supervisor does once the engine
// has an available report, starts a tone of seconds at -40 dBFS, and waits for
// its writer to fill the hardware buffer and wait on the DAC, which the test
// holds.
func (r *rig) playingTone(seconds int) *toneCall {
	r.t.Helper()
	r.be.ToneOutput(true, "")
	c := r.startTone(toneOf(-40, seconds))
	r.k.Await(r.t, "the tone's writer to fill the hardware buffer and wait on the DAC", runningAndWaiting)
	return c
}

// toneRefused asks a tone and fails the test unless it is refused with want,
// at once, nothing asked of the kernel and nothing counted.
func (r *rig) toneRefused(want playback.ToneOutcome, what string) {
	r.t.Helper()
	before := r.k.View()
	res := r.startTone(toneOf(-40, 10)).result(r.t)
	if res.Outcome != want || res.Admitted {
		r.t.Errorf("%s: the tone answered %+v; want refused %s", what, res, want)
	}
	if v := r.k.View(); len(v.Opened) != len(before.Opened) {
		r.t.Errorf("%s: a refused tone asked %d opens", what, len(v.Opened)-len(before.Opened))
	}
}

// tonePlays asks a tone of a second, lets the DAC play as it needs, and
// fails the test unless it plays its length.
func (r *rig) tonePlays(what string) playback.ToneResult {
	r.t.Helper()
	r.be.ToneOutput(true, "")
	r.k.Free()
	res := r.startTone(toneOf(-40, 1)).result(r.t)
	if res.Outcome != playback.TonePlayed || !res.Admitted {
		r.t.Errorf("%s: the tone answered %+v; want it played", what, res)
	}
	r.k.Hold()
	return res
}

// joinTone waits, under the watchdog, until no goroutine the backend started
// runs: a tone's, which counts until it has given the slot back, among them.
func (r *rig) joinTone() {
	r.t.Helper()
	ctx, cancel := within()
	defer cancel()
	if err := r.be.Wait(ctx); err != nil {
		alsapcm.LogGoroutines(r.t)
		r.t.Fatalf("the backend's goroutines: %v", err)
	}
}

// heldTimer is one of the backend's timers a test holds (toneTimers).
type heldTimer struct {
	d       time.Duration
	f       func()
	stopped atomic.Bool
}

// toneTimers stands in for the backend's timers (Config.AfterFunc): the
// waits it holds, a Start's for the tone (playback.ToneYield, which a wait for
// a probe shares) and Tone's for its stop (playback.ToneStopWait), are kept
// for the test to fire, each in the order armed; every other timer is
// time.AfterFunc's.
type toneTimers struct {
	arms map[time.Duration]chan *heldTimer
}

func newToneTimers() *toneTimers {
	return &toneTimers{arms: map[time.Duration]chan *heldTimer{
		playback.ToneYield:    make(chan *heldTimer, 64),
		playback.ToneStopWait: make(chan *heldTimer, 64),
	}}
}

func (w *toneTimers) afterFunc(d time.Duration, f func()) func() bool {
	ch, ok := w.arms[d]
	if !ok {
		return time.AfterFunc(d, f).Stop
	}
	h := &heldTimer{d: d, f: f}
	ch <- h
	return func() bool { return h.stopped.CompareAndSwap(false, true) }
}

// fire fires the next timer of d armed and not stopped, waiting for one under
// the watchdog: a timer stopped before the test came to it is passed over.
func (w *toneTimers) fire(t *testing.T, d time.Duration, what string) {
	t.Helper()
	for {
		h := receiveIn(t, w.arms[d], what)
		if !h.stopped.Load() {
			h.f()
			return
		}
	}
}

// parkAt is a hold of one of the backend's steps: in is closed once a
// goroutine is held there, and letGo lets it on, once.
type parkAt struct {
	in, gate   chan struct{}
	held, free sync.Once
}

func newParkAt() *parkAt { return &parkAt{in: make(chan struct{}), gate: make(chan struct{})} }

// pass holds the first goroutine that reaches it, and lets every later one by.
func (p *parkAt) pass() {
	first := false
	p.held.Do(func() { first = true; close(p.in) })
	if first {
		<-p.gate
	}
}

func (p *parkAt) letGo() { p.free.Do(func() { close(p.gate) }) }

// TestEachStateThePlayerIsNotStoppedInRefusesTheTone (B601): through the
// engine's Run, the backend and the fake kernel, a tone is refused by its word,
// nothing opened, while PCM plays, native DSD plays, a pause holds the device
// open, a pause's release has let it go, a release plays out a native tail
// (each a session's record: session_holds), and a release was left
// unconfirmed (release_unconfirmed); once a Stop is confirmed a tone plays.
func TestEachStateThePlayerIsNotStoppedInRefusesTheTone(t *testing.T) {
	stopAndPlay := func(d *dsdRig) {
		t.Helper()
		ctx, cancel := within()
		defer cancel()
		if err := d.eng.Stop(ctx); err != nil {
			t.Fatalf("stop: %v", err)
		}
		d.waitSnap("stopped, the release confirmed", func(s *engine.Snapshot) bool {
			_, unconfirmed := s.ReleaseUnconfirmed()
			return s.State() == engine.Stopped && !unconfirmed
		})
		waitOn(t, "the backend to let go of the session's record", d.rec.ch, func() bool { return d.be.Stats().Sessions == 0 })
		d.tonePlays("a tone after a confirmed Stop")
	}
	t.Run("PCM playing", func(t *testing.T) {
		d := newDSDRig(t, initialPolicy(dsdstate.ModeNative))
		d.be.ToneOutput(true, "")
		d.playingPCM()
		d.toneRefused(playback.ToneSessionHolds, "PCM playing")
		stopAndPlay(d)
	})
	t.Run("native DSD playing", func(t *testing.T) {
		d := newDSDRig(t, initialPolicy(dsdstate.ModeNative))
		d.be.ToneOutput(true, "")
		d.playingDSD()
		d.toneRefused(playback.ToneSessionHolds, "native DSD playing")
		stopAndPlay(d)
	})
	t.Run("paused with the device open", func(t *testing.T) {
		d := newDSDRig(t, initialPolicy(dsdstate.ModeNative))
		d.be.ToneOutput(true, "")
		d.playingPCM()
		d.k.Play(t, 100*time.Millisecond)
		if err := d.eng.Pause(bg); err != nil {
			t.Fatalf("pause: %v", err)
		}
		d.waitSnap("paused", func(s *engine.Snapshot) bool { return s.State() == engine.Paused })
		if v := d.k.View(); !v.Open {
			t.Fatal("the pause let the device go: the case needs it held")
		}
		d.toneRefused(playback.ToneSessionHolds, "paused with the device open")
		stopAndPlay(d)
	})
	t.Run("paused, the pause's release having let the device go", func(t *testing.T) {
		d := newDSDRig(t, initialPolicy(dsdstate.ModeNative), func(o *rigOptions) { o.pauseRelease = time.Second })
		d.be.ToneOutput(true, "")
		d.playingPCM()
		d.k.Play(t, 100*time.Millisecond)
		if err := d.eng.Pause(bg); err != nil {
			t.Fatalf("pause: %v", err)
		}
		d.waitSnap("paused", func(s *engine.Snapshot) bool { return s.State() == engine.Paused })
		d.releaseDevice()
		d.toneRefused(playback.ToneSessionHolds, "paused, the device let go")
		stopAndPlay(d)
	})
	t.Run("a release playing out a native tail", func(t *testing.T) {
		w := newLifecycleSeams()
		d := newDSDRigWith(t, initialPolicy(dsdstate.ModeNative), nil, func(o *rigOptions) { o.seams = w.seams(nil) })
		d.be.ToneOutput(true, "")
		d.playingDSD()
		rel := w.holdReleasing(t)
		done := make(chan error, 1)
		go func() {
			done <- d.be.ReleaseNative(bg, engine.ReleaseRequest{Token: engine.ReleaseToken{Session: d.sessionOf()}, Mode: engine.ReleaseTrackChange})
		}()
		awaitOrdered(t, rel.in, "the release, its native tail retiring, before the session's life ends")
		d.toneRefused(playback.ToneSessionHolds, "a release playing out a native tail")
		rel.letGo()
		d.k.Free()
		receiveIn(t, done, "the release's end")
		waitOn(t, "the backend to let go of the session's record", d.rec.ch, func() bool { return d.be.Stats().Sessions == 0 })
		d.tonePlays("a tone once the release ended")
	})
	t.Run("a release left unconfirmed", func(t *testing.T) {
		d := newDSDRig(t, initialPolicy(dsdstate.ModeNative))
		d.be.ToneOutput(true, "")
		d.mu.Lock()
		d.cleanup = "close"
		d.mu.Unlock()
		d.playingDSD()
		ctx, cancel := within()
		defer cancel()
		_ = d.eng.Stop(ctx)
		d.waitSnap("stopped", func(s *engine.Snapshot) bool { return s.State() == engine.Stopped })
		waitOn(t, "the backend to let go of the session's record", d.rec.ch, func() bool { return d.be.Stats().Sessions == 0 })
		d.toneRefused(playback.ToneReleaseUnconfirmed, "a release left unconfirmed")
	})
}

// TestAStartPreemptsTheTone (B601): a Play while the tone writes ends the
// tone (preempted), whose node is closed before the Start opens the device
// (the fake's exclusive open would refuse a second open busy), and the track
// plays every frame once.
func TestAStartPreemptsTheTone(t *testing.T) {
	var closesAtTheStart atomic.Int64
	closesAtTheStart.Store(-1)
	var k *alsapcm.FakeKernel
	r := newRig(t, func(o *rigOptions) {
		o.notify = func(n playback.Note) {
			if n.Kind == playback.NoteOpened {
				closesAtTheStart.Store(int64(k.View().Closes))
			}
		}
	})
	k = r.k
	r.k.Exclusive()
	a := pattern(1, 48000, 24)
	r.files.add("a", wav(48000, 24, a))
	r.insert("a")
	tone := r.playingTone(30)
	r.play()
	if res := tone.result(t); res.Outcome != playback.TonePreempted || !res.Admitted {
		t.Errorf("the tone a Start came beside answered %+v; want preempted", res)
	}
	if got := closesAtTheStart.Load(); got != 1 {
		t.Errorf("the session's sink opened with %d closes of the node; want the tone's one close first", got)
	}
	r.heardAll(format.S243LE, 24, a)
	if v := r.k.View(); v.Opens != 2 || v.Closes != 2 {
		t.Errorf("%d opens and %d closes; want the tone's and the session's, each closed", v.Opens, v.Closes)
	}
}

// TestAToneWhoseCloseIsHeldKeepsTheSlot (B601): a tone ended while its node's
// close is held in the kernel keeps the device slot until the close answers.
// Meanwhile a Start answers the device busy at ToneYield, nothing opened; a
// probe is skipped tone_holds and stays owed; a choice's write is refused; a
// tone is refused tone_playing; the tone's own request answers
// stop_unconfirmed at ToneStopWait, the status saying so; and the tone's
// goroutine stays counted. Once the close answers nil the slot comes back and
// nothing is fenced: a tone plays.
func TestAToneWhoseCloseIsHeldKeepsTheSlot(t *testing.T) {
	timers := newToneTimers()
	d := newDSDRig(t, initialPolicy(dsdstate.ModeNative), func(o *rigOptions) { o.afterFunc = timers.afterFunc })
	d.files.add("a", wav(48000, 24, pattern(1, 4800, 24)))
	d.insert("a")
	tone := d.playingTone(30)
	parked, letClose := d.k.ParkNextClose()
	t.Cleanup(letClose)
	tone.cancel()
	awaitOrdered(t, parked, "the tone's close held in the kernel")
	if st := d.be.ToneStatus(); st.State != playback.ToneStateStopping {
		t.Errorf("the status of a tone whose close is held: %+v; want stopping", st)
	}
	timers.fire(t, playback.ToneStopWait, "the tone's wait for its stop")
	if res := tone.result(t); res.Outcome != playback.ToneStopUnconfirmed || !res.Admitted {
		t.Errorf("the tone whose close is held answered %+v; want stop_unconfirmed", res)
	}
	if st := d.be.ToneStatus(); st.State != playback.ToneStateStopUnconfirmed {
		t.Errorf("the status past the stop's wait: %+v; want stop_unconfirmed", st)
	}
	opened := len(d.k.View().Opened)
	played := make(chan error, 1)
	go func() { played <- d.eng.Play(bg) }()
	timers.fire(t, playback.ToneYield, "the Start's wait for the tone")
	if err := receiveIn(t, played, "the play beside the held close"); !errors.Is(err, engine.ErrDeviceBusy) {
		t.Errorf("a Start beside the tone's held close answered %v; want the device busy", err)
	}
	d.waitSnap("the busy start's release confirmed", func(s *engine.Snapshot) bool {
		_, unconfirmed := s.ReleaseUnconfirmed()
		return s.State() == engine.Stopped && !unconfirmed
	})
	if !d.be.ProbeDSD(bg, playback.ProbeConditions{Available: true, Fresh: true}) {
		t.Error("a probe beside the tone's held close answered not pending")
	}
	rep, err := d.be.Probe(bg, probeRequest())
	wantSkipped(t, rep, err, playback.ProbeToneHolds)
	d.refusedNotStopped("a choice's write beside the tone's held close")
	d.toneRefused(playback.TonePlaying, "a tone beside the held close")
	if v := d.k.View(); len(v.Opened) != opened {
		t.Errorf("%d opens asked beside the tone's held close", len(v.Opened)-opened)
	}
	if n, _ := d.be.Running(); n < 1 {
		t.Errorf("%d goroutines counted: the tone held in its close is not", n)
	}
	letClose()
	d.joinTone()
	if st := d.be.ToneStatus(); st.State != playback.ToneIdle || st.Last.Outcome != playback.ToneRequestEnded {
		t.Errorf("the status once the held close answered nil: %+v; want idle, the tone's end request_ended", st)
	}
	d.tonePlays("a tone once the held close answered")
}

// TestAProbeBesideAToneStaysOwedAndIsMadeAfterIt (B601): through ProbeDSD, a
// probe due while the tone holds the device slot is refused by the slot and
// stays owed, nothing asked of the kernel; once the tone has ended, the next
// ProbeDSD probes the unit, one open and its refines, and settles it, so the
// one after is not due.
func TestAProbeBesideAToneStaysOwedAndIsMadeAfterIt(t *testing.T) {
	d := newDSDRig(t, initialPolicy(dsdstate.ModeNative))
	cond := playback.ProbeConditions{Available: true, Fresh: true}
	tone := d.playingTone(30)
	before := d.k.View()
	if !d.be.ProbeDSD(bg, cond) {
		t.Fatal("a probe due beside the tone answered not pending: it was taken for a probe made")
	}
	if v := d.k.View(); v.Opens != before.Opens || v.Refines != before.Refines {
		t.Errorf("the probe beside the tone asked the kernel %d opens and %d refines", v.Opens-before.Opens, v.Refines-before.Refines)
	}
	if !d.be.ProbeDSDPending() {
		t.Error("the probe refused for the tone is not owed")
	}
	tone.cancel()
	if res := tone.result(t); res.Outcome != playback.ToneRequestEnded {
		t.Fatalf("the tone answered %+v", res)
	}
	before = d.k.View()
	if d.be.ProbeDSD(bg, cond) {
		t.Fatal("the probe once the tone ended answered pending")
	}
	if v := d.k.View(); v.Opens != before.Opens+1 || v.Refines <= before.Refines {
		t.Errorf("the probe once the tone ended: %d opens and %d refines; want one open and its refines", v.Opens-before.Opens, v.Refines-before.Refines)
	}
	if d.be.ProbeDSD(bg, cond) || d.be.ProbeDSDPending() {
		t.Error("the probe made once the tone ended is still due")
	}
}

// TestTheGateEndsTheToneForEachShut (B601): for each word the supervisor's
// gate shuts with, and for serve's stop, a tone that plays ends with that
// word, its node closed, the slot given back; a tone asked while the gate is
// shut is refused with it; and once the gate opens again a tone plays, but
// never after serve's stop.
func TestTheGateEndsTheToneForEachShut(t *testing.T) {
	for _, why := range []playback.ToneOutcome{playback.ToneWithheld, playback.ToneOutputUnavailable, playback.ToneOutputChanged,
		playback.ToneSnapshotStale, playback.ToneServeStopping} {
		t.Run(string(why), func(t *testing.T) {
			r := newRig(t)
			tone := r.playingTone(30)
			if why == playback.ToneServeStopping {
				r.be.CloseTones()
			} else {
				r.be.ToneOutput(false, why)
			}
			if res := tone.result(t); res.Outcome != why || !res.Admitted {
				t.Errorf("the tone the gate shut on answered %+v; want %s", res, why)
			}
			if v := r.k.View(); v.Open || v.Opens != v.Closes {
				t.Errorf("%d opens and %d closes, the node open %v: want the tone's node closed", v.Opens, v.Closes, v.Open)
			}
			if st := r.be.ToneStatus(); st.State != playback.ToneIdle || st.Last.Outcome != why {
				t.Errorf("the status once the gate ended the tone: %+v", st)
			}
			r.toneRefused(why, "a tone while the gate is shut")
			r.be.ToneOutput(true, "")
			if why == playback.ToneServeStopping {
				r.toneRefused(playback.ToneServeStopping, "a tone after serve's stop, the gate asked to open")
				return
			}
			r.tonePlays("a tone once the gate opened again")
		})
	}
}

// TestAUnitChangeWhileAvailableEndsTheToneAndTheNextOpensTheNewUnit (B601):
// a tone that plays on card 5 is ended by the supervisor's shut for a unit
// that changed while the output stays available (output_changed), which comes
// before the engine hears of the new unit; the DAC comes back as card 6, the
// gate opens once the engine has taken that report, and the next tone opens
// card 6 and passes its check after open there.
func TestAUnitChangeWhileAvailableEndsTheToneAndTheNextOpensTheNewUnit(t *testing.T) {
	r := newRig(t)
	tone := r.playingTone(30)
	if v := r.k.View(); v.Opened[len(v.Opened)-1] != (alsapcm.Endpoint{Card: 5}).Path() {
		t.Fatalf("the tone opened %q; want card 5's node", v.Opened)
	}
	r.be.ToneOutput(false, playback.ToneOutputChanged)
	if res := tone.result(t); res.Outcome != playback.ToneOutputChanged {
		t.Errorf("the tone on the unit that changed answered %+v; want output_changed", res)
	}
	r.k.Renumber(6)
	r.unit.mu.Lock()
	r.unit.at = alsapcm.Endpoint{Card: 6}
	checked := len(r.unit.checked)
	r.unit.mu.Unlock()
	r.toneRefused(playback.ToneOutputChanged, "a tone between the shut and the open")
	r.be.ToneOutput(true, "")
	if res := r.tonePlays("a tone on the new unit"); res.RateHz != 48000 {
		t.Errorf("the tone on the new unit: %+v", res)
	}
	if v := r.k.View(); v.Opened[len(v.Opened)-1] != (alsapcm.Endpoint{Card: 6}).Path() {
		t.Errorf("the next tone opened %q; want card 6's node", v.Opened[len(v.Opened)-1])
	}
	r.unit.mu.Lock()
	defer r.unit.mu.Unlock()
	if len(r.unit.checked) != checked+1 || r.unit.checked[checked].Card != 6 {
		t.Errorf("the checks after open: %+v; want the next tone's node checked as card 6", r.unit.checked)
	}
}

// TestAToneBesideServesStopIsJoinedOrRefused (B601): serve's stop closes the
// tones (CloseTones) and then waits for the backend's goroutines (Wait). A
// tone held before its count is not waited for, and answers serve_stopping
// once let go; one held after its count is waited for, and answers
// serve_stopping at its slot, its count given back, which ends the wait; and
// one held once it holds the slot is ended there, and waited for until it has
// given the slot back. No case opens the node after the wait returned.
func TestAToneBesideServesStopIsJoinedOrRefused(t *testing.T) {
	// wouldWait is the count Wait waits on: it returns at once while no
	// goroutine the backend started runs, and waits otherwise.
	wouldWait := func(be *playback.Backend) bool {
		n, _ := be.Running()
		return n > 0
	}
	for _, at := range []string{"before the count", "after the count", "after the slot"} {
		t.Run(at, func(t *testing.T) {
			p := newParkAt()
			seams := &playback.Seams{}
			switch at {
			case "before the count":
				seams.ToneEntering = p.pass
			case "after the count":
				seams.ToneCounted = p.pass
			case "after the slot":
				seams.ToneAdmitted = p.pass
			}
			r := newRig(t, func(o *rigOptions) { o.seams = seams })
			t.Cleanup(p.letGo)
			r.be.ToneOutput(true, "")
			tone := r.startTone(toneOf(-40, 10))
			awaitOrdered(t, p.in, "the tone held "+at)
			r.be.CloseTones()
			waited := make(chan int, 1)
			go func() {
				_ = r.be.Wait(bg)
				waited <- len(r.k.View().Opened)
			}()
			if want := at != "before the count"; wouldWait(r.be) != want {
				t.Errorf("the backend's wait waits %v; want %v for a tone held %s", !want, want, at)
			}
			if at == "before the count" {
				opened := receiveIn(t, waited, "the wait")
				p.letGo()
				if res := tone.result(t); res.Outcome != playback.ToneServeStopping || res.Admitted {
					t.Errorf("the tone held before its count answered %+v; want refused serve_stopping", res)
				}
				if n := len(r.k.View().Opened); n != opened || n != 0 {
					t.Errorf("%d opens after the wait returned with %d", n, opened)
				}
				return
			}
			p.letGo()
			res := tone.result(t)
			opened := receiveIn(t, waited, "the wait")
			if res.Outcome != playback.ToneServeStopping {
				t.Errorf("the tone held %s answered %+v; want serve_stopping", at, res)
			}
			if wantAdmitted := at == "after the slot"; res.Admitted != wantAdmitted {
				t.Errorf("the tone held %s: admitted %v, want %v", at, res.Admitted, wantAdmitted)
			}
			if n := len(r.k.View().Opened); n != opened || n != 0 {
				t.Errorf("the tone held %s asked %d opens, %d of them after the wait returned", at, n, n-opened)
			}
			if st := r.be.ToneStatus(); st.State != playback.ToneIdle {
				t.Errorf("the slot once the wait returned: %+v", st)
			}
		})
	}
}

// TestEveryWayAToneEndsGivesTheSlotBackOnlyAfterItsClose (B601): each way a
// tone that took the slot ends answers its word, closes every node it opened,
// and gives the slot back only once the node's close has answered: at its
// close's note (Seams.ToneClosed) the node is closed and the slot still the
// tone's.
func TestEveryWayAToneEndsGivesTheSlotBackOnlyAfterItsClose(t *testing.T) {
	type tone = *toneCall
	for _, c := range []struct {
		name string
		want playback.ToneOutcome
		// before runs before the tone is asked; end ends the playing one, nil
		// for a tone that ends by itself as it is asked.
		before func(r *rig)
		end    func(r *rig, c tone)
	}{
		{name: "the request's end", want: playback.ToneRequestEnded, end: func(_ *rig, c tone) { c.cancel() }},
		{name: "the gate", want: playback.ToneWithheld, end: func(r *rig, _ tone) { r.be.ToneOutput(false, playback.ToneWithheld) }},
		{name: "serve's stop", want: playback.ToneServeStopping, end: func(r *rig, _ tone) { r.be.CloseTones() }},
		{name: "a disconnect", want: playback.ToneOutputGone, end: func(r *rig, _ tone) { r.k.Unplug() }},
		{name: "an xrun", want: playback.ToneDeviceError, before: func(r *rig) { r.k.XrunAt(48000 / 2) }, end: func(r *rig, _ tone) { r.k.Free() }},
		{name: "a cancel in the drain", want: playback.ToneRequestEnded, end: func(r *rig, c tone) {
			parked, release := r.k.ParkNextDrain()
			r.t.Cleanup(release)
			r.k.Free()
			awaitOrdered(r.t, parked, "the tone's drain")
			c.cancel()
			release()
		}},
		{name: "the check after open refusing", want: playback.ToneWrongDevice, before: func(r *rig) {
			r.unit.mu.Lock()
			r.unit.refuse = 1
			r.unit.mu.Unlock()
		}},
		{name: "a panic in the run, at the drain's read of the clock", want: playback.ToneFailed, end: func(r *rig, _ tone) {
			r.k.PanicAtNextClockRead(panicValue)
			r.k.Free()
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			var notes []string
			var mu sync.Mutex
			var r *rig
			seams := &playback.Seams{ToneClosed: func(ok bool) {
				v, st := r.k.View(), r.be.ToneStatus()
				mu.Lock()
				defer mu.Unlock()
				if !ok || v.Open || v.Opens != v.Closes || st.State == playback.ToneIdle {
					notes = append(notes, "the close's note: confirmed "+strconv.FormatBool(ok)+", node open "+strconv.FormatBool(v.Open)+
						", "+strconv.Itoa(v.Opens)+" opens and "+strconv.Itoa(v.Closes)+" closes, the tone "+string(st.State))
				}
				notes = append(notes, "noted")
			}}
			r = newRig(t, func(o *rigOptions) { o.seams = seams })
			r.be.ToneOutput(true, "")
			if c.before != nil {
				c.before(r)
			}
			call := r.startTone(toneOf(-40, 1))
			if c.end != nil {
				r.k.Await(t, "the tone's writer to fill the hardware buffer and wait on the DAC", runningAndWaiting)
				c.end(r, call)
			}
			if res := call.result(t); res.Outcome != c.want || !res.Admitted {
				t.Errorf("the tone answered %+v; want %s", res, c.want)
			}
			if v := r.k.View(); v.Open || v.Opens != 1 || v.Closes != 1 {
				t.Errorf("%d opens and %d closes, the node open %v: want the tone's one open closed", v.Opens, v.Closes, v.Open)
			}
			if st := r.be.ToneStatus(); st.State != playback.ToneIdle {
				t.Errorf("the slot after the tone's end: %+v", st)
			}
			mu.Lock()
			defer mu.Unlock()
			if !slices.Equal(notes, []string{"noted"}) {
				t.Errorf("the close's notes: %q; want one, the node closed and the slot still the tone's", notes)
			}
		})
	}
}

// fenced fails the test unless the device is fenced as a release never
// confirmed fences it, nothing opened by any: a tone is refused
// release_unconfirmed; a Start answers engine.ErrReleaseUnconfirmed; a probe
// is skipped release_unconfirmed and stays owed; a choice's write answers
// playback_not_stopped.
func (d *dsdRig) fenced(what string) {
	d.t.Helper()
	opened := len(d.k.View().Opened)
	d.toneRefused(playback.ToneReleaseUnconfirmed, what+": a later tone")
	if st := d.be.ToneStatus(); st.State != playback.ToneStateReleaseUnconfirmed {
		d.t.Errorf("%s: the status says %+v; want release_unconfirmed", what, st)
	}
	d.files.add("a", wav(48000, 24, pattern(1, 4800, 24)))
	d.insert("a")
	if err := d.eng.Play(bg); !errors.Is(err, engine.ErrReleaseUnconfirmed) {
		d.t.Errorf("%s: a Start answered %v; want engine.ErrReleaseUnconfirmed", what, err)
	}
	d.waitSnap("the refused start's stop", func(s *engine.Snapshot) bool { return s.State() == engine.Stopped })
	if !d.be.ProbeDSD(bg, playback.ProbeConditions{Available: true, Fresh: true}) {
		d.t.Errorf("%s: a probe answered not pending", what)
	}
	rep, err := d.be.Probe(bg, probeRequest())
	wantSkipped(d.t, rep, err, playback.ProbeReleaseUnconfirmed)
	d.refusedNotStopped(what + ": a choice's write")
	if v := d.k.View(); len(v.Opened) != opened {
		d.t.Errorf("%s: %d opens asked of a fenced device", what, len(v.Opened)-opened)
	}
}

// TestAToneWhoseCleanupFailsFencesTonesStartsProbesAndLeases (B601): a tone
// whose node's close answers an error, one whose close panics, and one whose
// refused open's own close fails or panics before any device reached the
// tone, each ends release_unconfirmed and fences the device until serve
// restarts: a later tone, a Start, a probe and a choice's write, nothing
// opened by any; the refused open asks no second pair. A panic after a close
// that answered nil fences nothing: a later tone and a later Start play.
func TestAToneWhoseCleanupFailsFencesTonesStartsProbesAndLeases(t *testing.T) {
	for _, c := range []struct {
		name string
		arm  func(k *alsapcm.FakeKernel)
		// refused is a first pair the DAC refuses, so the close that fails is
		// that refused open's own, before any device reaches the tone.
		refused bool
	}{
		{"its close answers an error", func(k *alsapcm.FakeKernel) { k.FailNextClose(syscall.EIO) }, false},
		{"its close panics", func(k *alsapcm.FakeKernel) { k.PanicAtNextClose(panicValue) }, false},
		{"a refused open's own close answers an error", func(k *alsapcm.FakeKernel) { k.FailNextClose(syscall.EIO) }, true},
		{"a refused open's own close panics", func(k *alsapcm.FakeKernel) { k.PanicAtNextClose(panicValue) }, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := newDSDRig(t, initialPolicy(dsdstate.ModeNative))
			d.be.ToneOutput(true, "")
			var tone *toneCall
			if c.refused {
				d.k.HardwareFormats(2)
				c.arm(d.k)
				tone = d.startTone(toneOf(-40, 1))
			} else {
				tone = d.playingTone(30)
				c.arm(d.k)
				tone.cancel()
			}
			if res := tone.result(t); res.Outcome != playback.ToneReleaseUnconfirmed || !res.Admitted {
				t.Errorf("the tone answered %+v; want release_unconfirmed", res)
			}
			if c.refused {
				if v := d.k.View(); len(v.Asked) != 1 || v.Asked[0] != (alsapcm.Ask{Format: format.S243LE, RateHz: 48000}) {
					t.Errorf("the tone asked %+v; want its first pair alone, the fallback stopped", v.Asked)
				}
				d.k.HardwareFormats(2, 10, 32, 49, 50, 52)
			}
			d.fenced(c.name)
		})
	}
	t.Run("a panic after a close that answered nil", func(t *testing.T) {
		var once sync.Once
		r := newRig(t, func(o *rigOptions) {
			o.seams = &playback.Seams{ToneClosed: func(ok bool) {
				if ok {
					once.Do(func() { panic(panicValue) })
				}
			}}
		})
		r.tonePlays("the tone whose close's note panics")
		if st := r.be.ToneStatus(); st.State != playback.ToneIdle {
			t.Errorf("the status after a panic past a confirmed close: %+v", st)
		}
		r.tonePlays("a tone after it")
		a := pattern(1, 4800, 24)
		r.files.add("a", wav(48000, 24, a))
		r.insert("a")
		r.play()
		r.k.Free()
		r.waitStopped(engine.ReasonEndOfQueue)
	})
}

// toneSample is frame n's sample of the 1 kHz sine this test computes for
// level at rate in bits valid bits, apart from the builder: the amplitude the
// floor of full scale times 10 to the level over 20, each sample rounded, the
// sine's phase taken within its period of whole cycles (rate over the greatest
// common divisor of rate and 1000), so a long tone is the same few frames.
func toneSample(level float64, rate, bits, n int) int64 {
	g, b := rate, 1000
	for b != 0 {
		g, b = b, g%b
	}
	period := rate / g
	full := int64(1)<<(bits-1) - 1
	amp := int64(math.Floor(float64(full) * math.Pow(10, level/20)))
	return int64(math.Round(float64(amp) * math.Sin(2*math.Pi*1000*float64(n%period)/float64(rate))))
}

// TestATonesPeakIsBoundedOnTheDevice (B601): every frame the kernel was given
// for a tone at -30 and at -40 dBFS, read back by its container apart from
// the packer, is this test's own sine for that level, both channels alike:
// no sample past the level's amplitude and no gain anywhere after the
// builder.
func TestATonesPeakIsBoundedOnTheDevice(t *testing.T) {
	for _, level := range []float64{-30, -40} {
		t.Run(strconv.FormatFloat(level, 'f', -1, 64), func(t *testing.T) {
			r := newRig(t)
			r.be.ToneOutput(true, "")
			r.k.Free()
			res := r.startTone(toneOf(level, 1)).result(t)
			if res.Outcome != playback.TonePlayed || res.Format != format.S243LE || res.RateHz != 48000 || res.ValidBits != 24 || res.Frames != 48000 {
				t.Fatalf("the tone answered %+v; want 48000 frames of S24_3LE at 48 kHz, 24 valid bits", res)
			}
			got := unpack(t, r.k.Given(), format.S243LE, 24)
			if len(got) != 2*48000 {
				t.Fatalf("the kernel was given %d samples; want 96000", len(got))
			}
			limit := int64(math.Floor(float64(int64(1)<<23-1) * math.Pow(10, level/20)))
			for i, s := range got {
				want := toneSample(level, 48000, 24, i/2)
				if int64(s) != want || int64(s) > limit || int64(s) < -limit {
					t.Fatalf("sample %d is %d; want %d, at most %d", i, s, want, limit)
				}
			}
		})
	}
}

// TestTheBackendRefusesALevelAboveMinus30BeforeAnyOpen (B601): a tone louder
// than -30 dBFS, below -90 or no number is refused level_refused, and one
// shorter than a second or longer than 30 length_refused, before anything is
// counted, locked or opened. Each is asked on a goroutine of its own, so a
// tone its bounds should refuse that opens the DAC fails the case at that
// open, rather than playing on a DAC the test never lets play.
func TestTheBackendRefusesALevelAboveMinus30BeforeAnyOpen(t *testing.T) {
	r := newRig(t)
	r.be.ToneOutput(true, "")
	opened := func(v alsapcm.KernelView) bool { return len(v.Opened) > 0 }
	for _, c := range []struct {
		req  playback.ToneRequest
		want playback.ToneOutcome
	}{
		{toneOf(-29.99, 10), playback.ToneLevelRefused},
		{toneOf(-20, 10), playback.ToneLevelRefused},
		{toneOf(0, 10), playback.ToneLevelRefused},
		{toneOf(math.NaN(), 10), playback.ToneLevelRefused},
		{toneOf(math.Inf(1), 10), playback.ToneLevelRefused},
		{toneOf(-90.01, 10), playback.ToneLevelRefused},
		{toneOf(-40, 0), playback.ToneLengthRefused},
		{toneOf(-40, 31), playback.ToneLengthRefused},
	} {
		call := r.startTone(c.req)
		var res playback.ToneResult
		watchdog := time.NewTimer(alsapcm.Watchdog())
		select {
		case res = <-call.answer:
			watchdog.Stop()
		case <-r.k.When(opened):
			call.cancel()
			t.Fatalf("a tone %+v opened the DAC; want %s before anything is opened", c.req, c.want)
		case <-watchdog.C:
			t.Fatalf("a tone %+v never answered; want %s at once", c.req, c.want)
		}
		if res.Outcome != c.want || res.Admitted {
			t.Errorf("a tone %+v answered %+v; want %s", c.req, res, c.want)
		}
		if n, _ := r.be.Running(); n != 0 {
			t.Errorf("a tone %+v refused left %d goroutines counted", c.req, n)
		}
	}
	if v := r.k.View(); len(v.Opened) != 0 {
		t.Errorf("%d opens asked for tones refused by their bounds", len(v.Opened))
	}
}

// TestTheToneOpensPCMAloneBesideADSDSet (B601): beside an effective set that
// plays native DSD and DoP, the tone asks the DAC its eight PCM pairs and no
// DSD format, writes nothing to the player's DSD state and moves no choice's
// revision; and its source names no DSD path: no native sink, no DoP packer,
// no converter.
func TestTheToneOpensPCMAloneBesideADSDSet(t *testing.T) {
	d := newDSDRig(t, initialPolicy(dsdstate.ModeAuto))
	// Native DSD64 alone, and S32_LE beside it, which DoP rides at the rates
	// native does not take.
	native := playbacktest.NativeAltset()
	native.Rates = []int{88200}
	d.box.SetAltsets(card, 0, playbacktest.S32Altset(), playbacktest.S24Altset(), native)
	d.be.RefreshDSD(true)
	p := d.be.Policy()
	var transports []string
	for _, tp := range p.Tuples {
		transports = append(transports, playback.TupleTransport(tp.Tuple))
	}
	if !slices.Contains(transports, "") || !slices.Contains(transports, playback.TransportDoP) || p.DSD == nil || p.DSD.DAC == nil {
		t.Fatalf("the set plays %q (%+v); want native DSD and DoP", transports, p.Tuples)
	}
	writes, revision := d.store.Writes(), p.DSD.DAC.Choice.Revision
	d.k.Rates(88200)
	d.be.ToneOutput(true, "")
	if res := d.startTone(toneOf(-40, 1)).result(t); res.Outcome != playback.ToneOpenRefused {
		t.Fatalf("the tone on a DAC that takes none of its pairs answered %+v; want open_refused", res)
	}
	want := []alsapcm.Ask{}
	for _, rate := range []int{48000, 44100} {
		for _, f := range []format.ALSAFormat{format.S243LE, format.S24LE, format.S32LE, format.S16LE} {
			want = append(want, alsapcm.Ask{Format: f, RateHz: rate})
		}
	}
	if v := d.k.View(); !slices.Equal(v.Asked, want) || v.Opens != v.Closes {
		t.Errorf("the tone asked %+v (%d opens, %d closes); want its eight PCM pairs, in order, each closed", v.Asked, v.Opens, v.Closes)
	}
	d.be.RefreshDSD(true)
	if got := d.be.Policy().DSD.DAC.Choice.Revision; d.store.Writes() != writes || got != revision {
		t.Errorf("the tone wrote the DSD state %d times and moved the choice's revision to %d from %d", d.store.Writes()-writes, got, revision)
	}
	src := filepath.Join("..", "player", "playback", "tone.go")
	f, err := parser.ParseFile(token.NewFileSet(), src, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	for _, imp := range f.Imports {
		if path, _ := strconv.Unquote(imp.Path.Value); path == "github.com/acoseac/1-bit-bridge/internal/player/dsdconv" {
			t.Errorf("tone.go imports the converter")
		}
	}
	forbidden := []string{"OpenNative", "OpenDSDSink", "PackDoP", "DSDConfig", "KindDoP", "KindNativeDSD", "dsdconv"}
	ast.Inspect(f, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && slices.Contains(forbidden, id.Name) {
			t.Errorf("tone.go names %s at %v: the tone reaches a DSD path", id.Name, id.Pos())
		}
		return true
	})
}

// TestTheToneTriesEachRateAndContainerOnce (B601): a DAC that takes S16_LE at
// 44.1 kHz alone is asked exactly the tone's eight (rate, format) pairs, in
// order, each refused node closed, and plays the last; one whose one PCM
// format is S32_LE plays the first rate's third pair, at 24 valid bits.
func TestTheToneTriesEachRateAndContainerOnce(t *testing.T) {
	t.Run("S16_LE at 44.1 kHz alone", func(t *testing.T) {
		r := newRig(t)
		r.k.Formats(format.S16LE)
		r.k.Rates(44100)
		res := r.tonePlays("the tone on a DAC that takes S16_LE at 44.1 kHz alone")
		if res.Format != format.S16LE || res.RateHz != 44100 || res.ValidBits != 16 {
			t.Errorf("the tone played %+v; want S16_LE at 44.1 kHz, 16 valid bits", res)
		}
		var want []alsapcm.Ask
		for _, rate := range []int{48000, 44100} {
			for _, f := range []format.ALSAFormat{format.S243LE, format.S24LE, format.S32LE, format.S16LE} {
				want = append(want, alsapcm.Ask{Format: f, RateHz: rate})
			}
		}
		if v := r.k.View(); !slices.Equal(v.Asked, want) || v.Opens != 8 || v.Closes != 8 {
			t.Errorf("the tone asked %+v (%d opens, %d closes); want the eight pairs in order, each closed", v.Asked, v.Opens, v.Closes)
		}
	})
	t.Run("S32_LE alone", func(t *testing.T) {
		r := newRig(t)
		r.k.Formats(format.S32LE)
		res := r.tonePlays("the tone on a DAC whose one PCM format is S32_LE")
		if res.Format != format.S32LE || res.RateHz != 48000 || res.ValidBits != 24 {
			t.Errorf("the tone played %+v; want S32_LE at 48 kHz, 24 valid bits", res)
		}
		want := []alsapcm.Ask{{Format: format.S243LE, RateHz: 48000}, {Format: format.S24LE, RateHz: 48000}, {Format: format.S32LE, RateHz: 48000}}
		if v := r.k.View(); !slices.Equal(v.Asked, want) {
			t.Errorf("the tone asked %+v; want %+v", v.Asked, want)
		}
	})
}

// TestABusyOrDeniedOpenIsTheTonesLastError (B601): a tone whose open the
// kernel refuses busy, or denied with EACCES or EPERM, ends open_refused with
// its cause as its LastError, the status's too, nothing written and the
// engine's last play attempt untouched; the next tone's admission clears it.
func TestABusyOrDeniedOpenIsTheTonesLastError(t *testing.T) {
	for _, c := range []struct {
		name string
		set  func(k *alsapcm.FakeKernel, on bool)
		want string
	}{
		{"busy", func(k *alsapcm.FakeKernel, on bool) { k.Busy(on) }, playback.ToneLastErrorBusy},
		{"denied with EACCES", func(k *alsapcm.FakeKernel, on bool) { k.Deny(on, false) }, playback.ToneLastErrorDenied},
		{"denied with EPERM", func(k *alsapcm.FakeKernel, on bool) { k.Deny(on, true) }, playback.ToneLastErrorDenied},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t)
			r.be.ToneOutput(true, "")
			c.set(r.k, true)
			res := r.startTone(toneOf(-40, 1)).result(t)
			if res.Outcome != playback.ToneOpenRefused || res.LastError != c.want {
				t.Errorf("the tone answered %+v; want open_refused, %s", res, c.want)
			}
			if st := r.be.ToneStatus(); st.Last.LastError != c.want {
				t.Errorf("the status's last tone: %+v; want %s", st.Last, c.want)
			}
			if v := r.k.View(); v.GivenBytes != 0 || len(v.Opened) != 1 {
				t.Errorf("the refused tone gave the kernel %d bytes after %d opens asked", v.GivenBytes, len(v.Opened))
			}
			if at := r.eng.Snapshot().LastAttempt(); at.Seq != 0 {
				t.Errorf("the engine's last play attempt moved for a tone: %+v", at)
			}
			c.set(r.k, false)
			r.k.Hold()
			next := r.playingTone(30)
			if st := r.be.ToneStatus(); st.Last.LastError != "" || st.State != playback.ToneStatePlaying {
				t.Errorf("the status once the next tone was admitted: %+v; want its last error cleared", st)
			}
			next.cancel()
			next.result(t)
		})
	}
}

// TestALateEndOfAnEarlierTonesRequestLeavesTheNextPlaying (B601): every end
// of a tone is set on its own record: tone A plays its length, tone B is
// admitted, and A's request ends afterwards; B plays on, and answers its own
// end.
func TestALateEndOfAnEarlierTonesRequestLeavesTheNextPlaying(t *testing.T) {
	r := newRig(t)
	r.be.ToneOutput(true, "")
	r.k.Free()
	a := r.startTone(toneOf(-40, 1))
	if res := a.result(t); res.Outcome != playback.TonePlayed {
		t.Fatalf("tone A answered %+v; want it played", res)
	}
	r.k.Hold()
	b := r.playingTone(1)
	a.cancel()
	if st := r.be.ToneStatus(); st.State != playback.ToneStatePlaying {
		t.Errorf("tone B once A's request ended: %+v; want it playing", st)
	}
	r.k.Free()
	if res := b.result(t); res.Outcome != playback.TonePlayed {
		t.Errorf("tone B answered %+v; want its own end, played", res)
	}
}
