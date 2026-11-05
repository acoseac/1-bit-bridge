package alsapcm_test

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/format"
	"github.com/acoseac/1-bit-bridge/internal/player/playback"
)

// The silent probe through the player's backend (B728 B4): the backend's
// device slot, which a probe takes for its whole length and a Start shares,
// and the conditions of the design's section 5 under which a probe runs.

// probeRequest asks the design's whole set, the bound DAC available and the
// hotplug snapshot fresh.
func probeRequest() playback.ProbeRequest {
	return playback.ProbeRequest{Conditions: playback.ProbeConditions{Available: true, Fresh: true}, Candidates: alsapcm.ProbeCandidates()}
}

// wantSkipped fails the test unless the backend ran no probe, for why, and
// the report asked nothing.
func wantSkipped(t *testing.T, rep alsapcm.ProbeReport, err error, why playback.ProbeSkip) {
	t.Helper()
	var skip *playback.ProbeSkipped
	if !errors.As(err, &skip) || skip.Reason != why {
		t.Fatalf("the probe answered %v; want it not run: %s", err, why)
	}
	if rep.Opened {
		t.Errorf("a probe not run says it opened the node: %+v", rep)
	}
	for _, r := range rep.Results {
		if r.Answer != alsapcm.ProbeNotProbed {
			t.Errorf("a probe not run answered %s: %s", r.Candidate, r.Answer)
		}
	}
}

// probeAsksNothing probes through r's backend and fails the test unless it
// is refused for why with no open asked and no refine sent.
func (r *rig) probeAsksNothing(why playback.ProbeSkip) {
	r.t.Helper()
	before := r.k.View()
	rep, err := r.be.Probe(bg, probeRequest())
	wantSkipped(r.t, rep, err, why)
	if v := r.k.View(); len(v.Opened) != len(before.Opened) || v.Refines != before.Refines || v.HWParams != before.HWParams {
		r.t.Errorf("a probe refused for %s asked %d opens and sent %d refines and %d HW_PARAMS",
			why, len(v.Opened)-len(before.Opened), v.Refines-before.Refines, v.HWParams-before.HWParams)
	}
}

// probeRuns probes through r's backend and fails the test unless the probe
// ran: one open asked and opened, one close, a refine for each candidate,
// and the answers the fake DAC gives (S32_LE at 176.4 kHz taken, with 32
// significant bits and the kernel's PAUSE).
func (r *rig) probeRuns() {
	r.t.Helper()
	before := r.k.View()
	req := probeRequest()
	rep, err := r.be.Probe(bg, req)
	if err != nil || !rep.Opened {
		r.t.Fatalf("a probe of a free device: %+v, %v", rep, err)
	}
	v := r.k.View()
	if len(v.Opened) != len(before.Opened)+1 || v.Opens != before.Opens+1 || v.Closes != before.Closes+1 || v.Open ||
		v.Refines != before.Refines+len(req.Candidates) || v.HWParams != before.HWParams {
		r.t.Errorf("the probe: %d opens asked, %d opened, %d closes, %d refines, %d HW_PARAMS, open %v; want one of each, a refine a candidate, no HW_PARAMS",
			len(v.Opened)-len(before.Opened), v.Opens-before.Opens, v.Closes-before.Closes, v.Refines-before.Refines, v.HWParams-before.HWParams, v.Open)
	}
	carrier := rep.Result(alsapcm.ProbeCandidate{Format: format.S32LE, RateHz: 176400})
	if carrier.Answer != alsapcm.ProbeAccepted || carrier.Msbits != 32 || !carrier.Pause || !carrier.DoPCarrier() {
		r.t.Errorf("the carrier at 176.4 kHz: %+v", carrier)
	}
}

// playing is a rig playing a, the writer waiting on the DAC.
func playing(t *testing.T, opts ...func(*rigOptions)) (*rig, []int32) {
	t.Helper()
	r := newRig(t, opts...)
	a := pattern(1, 48000, 24)
	r.files.add("a", wav(48000, 24, a))
	r.insert("a")
	r.play()
	r.k.Await(t, "the writer to fill the hardware buffer and wait on the DAC", runningAndWaiting)
	return r, a
}

// TestNoProbeWhileASessionHoldsTheDevice pins the probe's gate against a
// session (B728's design, section 5: never while a session holds the device
// or a pause still holds it, and no release pending): through the engine's
// Run, the backend and the real sink, a session that plays, one opening the
// device, a paused one, one whose pause has let the device go, one whose
// release the engine has taken before its call reached the backend, and one
// whose release the backend is still carrying out each refuse the probe, by
// its word, before anything is asked of the kernel; the session goes on as
// if no probe had been asked, and once a released session lets go of the
// slot the probe runs.
func TestNoProbeWhileASessionHoldsTheDevice(t *testing.T) {
	t.Run("it plays", func(t *testing.T) {
		r, a := playing(t)
		r.probeAsksNothing(playback.ProbeSessionHolds)
		r.heardAll(format.S243LE, 24, a)
	})
	t.Run("it is opening the device", func(t *testing.T) {
		r := newRig(t)
		a := pattern(1, 48000, 24)
		r.files.add("a", wav(48000, 24, a))
		r.insert("a")
		held, admit := r.unit.holdNextAdmit()
		t.Cleanup(admit)
		played := make(chan error, 1)
		go func() { played <- r.eng.Play(bg) }()
		receiveIn(t, held, "the Start's check after open held, the node open")
		r.probeAsksNothing(playback.ProbeSessionHolds)
		admit()
		if err := receiveIn(t, played, "the play"); err != nil {
			t.Fatalf("play: %v", err)
		}
		r.heardAll(format.S243LE, 24, a)
	})
	t.Run("it is paused", func(t *testing.T) {
		r, a := pausedRig(t)
		if v := r.k.View(); v.State != "PAUSED" || !v.Open {
			t.Fatalf("paused: the stream is %s, open %v", v.State, v.Open)
		}
		r.probeAsksNothing(playback.ProbeSessionPaused)
		r.play()
		r.heardAll(format.S243LE, 24, a)
	})
	t.Run("its pause has let the device go", func(t *testing.T) {
		r, a := pausedRig(t)
		r.releaseDevice()
		r.probeAsksNothing(playback.ProbeSessionPaused)
		r.play()
		r.heardAll(format.S243LE, 24, a)
	})
	t.Run("the engine has taken its release", func(t *testing.T) {
		r, _ := playing(t)
		stop, letRelease := r.stopWithItsReleaseHeld()
		r.probeAsksNothing(playback.ProbeReleasePending)
		r.finishStop(stop, letRelease)
		r.probeRuns()
	})
	t.Run("the backend is releasing it", func(t *testing.T) {
		r, _ := playing(t)
		parked, letClose := r.k.ParkNextClose()
		t.Cleanup(letClose)
		stopped := make(chan error, 1)
		go func() { stopped <- r.eng.Stop(bg) }()
		receiveIn(t, parked, "the session's device held in its close")
		r.probeAsksNothing(playback.ProbeReleasePending)
		letClose()
		if err := receiveIn(t, stopped, "the stop"); err != nil {
			t.Fatalf("stop: %v", err)
		}
		r.waitSnap("the release acknowledged", func(s *engine.Snapshot) bool { return s.State() == engine.Stopped })
		waitOn(t, "the backend to let go of the slot", r.rec.ch, func() bool { return r.be.Stats().Sessions == 0 })
		r.probeRuns()
	})
}

// probeWaits stands in for the backend's timers (Config.AfterFunc): the
// wait a Start arms for a probe (playback.ProbeWait) is held for the test to
// fire; every other timer is time.AfterFunc's.
type probeWaits struct {
	mu    sync.Mutex
	armed chan struct{}
	fire  func()
}

func newProbeWaits() *probeWaits { return &probeWaits{armed: make(chan struct{})} }

func (w *probeWaits) afterFunc(d time.Duration, f func()) func() bool {
	if d != playback.ProbeWait {
		return time.AfterFunc(d, f).Stop
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.fire = f
	select {
	case <-w.armed:
	default:
		close(w.armed)
	}
	return func() bool { return true }
}

// fired runs the held wait's function, its time come.
func (w *probeWaits) fired() {
	w.mu.Lock()
	f := w.fire
	w.mu.Unlock()
	f()
}

// probeHeldOpen starts a probe through r's backend and waits until it holds
// the node open, in its check after open, for the test to let go on.
func (r *rig) probeHeldOpen() (answer <-chan probeAnswer, letGo func()) {
	r.t.Helper()
	held, admit := r.unit.holdNextAdmit()
	r.t.Cleanup(admit)
	ch := make(chan probeAnswer, 1)
	go func() {
		rep, err := r.be.Probe(bg, probeRequest())
		ch <- probeAnswer{rep, err}
	}()
	receiveIn(r.t, held, "the probe's check after open held, the node open")
	if v := r.k.View(); len(v.Opened) != 1 || !v.Open {
		r.t.Fatalf("the probe held: %d opens asked, open %v; want its one open", len(v.Opened), v.Open)
	}
	return ch, admit
}

type probeAnswer struct {
	rep alsapcm.ProbeReport
	err error
}

// startWaits starts a play and fails the test unless its Start waits for the
// probe: the backend arms the probe's wait before the Start asks for any
// open.
func (r *rig) startWaits(waits *probeWaits) <-chan error {
	r.t.Helper()
	played := make(chan error, 1)
	go func() { played <- r.eng.Play(bg) }()
	timer := time.NewTimer(alsapcm.Watchdog())
	defer timer.Stop()
	select {
	case <-waits.armed:
	case <-r.k.When(func(v alsapcm.KernelView) bool { return len(v.Opened) > 1 }):
		r.t.Fatalf("a Start asked to open the device while the probe held it: %q", r.k.View().Opened)
	case err := <-played:
		r.t.Fatalf("the play answered %v while the probe held the device; want its Start waiting", err)
	case <-timer.C:
		alsapcm.LogGoroutines(r.t)
		r.t.Fatalf("waited %v for the Start to wait for the probe", alsapcm.Watchdog())
	}
	if v := r.k.View(); len(v.Opened) != 1 {
		r.t.Fatalf("a waiting Start asked to open the device: %q", v.Opened)
	}
	return played
}

// TestAStartDuringAProbeWaitsForIt pins the slot a probe shares with a
// Start (B728 B4): the probe holds the backend's device slot from before its
// open to after its close, so a Start that comes while it holds the node
// open (here in its check after open) waits, asking for no open, and opens
// the device once the probe has closed it; the probe answers the kernel's
// answers, and the session plays every frame once. The node is exclusive, as
// a USB DAC's one substream is: a Start that opened beside the probe would be
// refused busy.
func TestAStartDuringAProbeWaitsForIt(t *testing.T) {
	waits := newProbeWaits()
	r := newRig(t, func(o *rigOptions) { o.afterFunc = waits.afterFunc })
	r.k.Exclusive()
	a := pattern(1, 48000, 24)
	r.files.add("a", wav(48000, 24, a))
	r.insert("a")
	probed, letGo := r.probeHeldOpen()
	played := r.startWaits(waits)
	letGo()
	ans := receiveIn(t, probed, "the probe's answer")
	if ans.err != nil || !ans.rep.Opened {
		t.Fatalf("the probe: %+v, %v", ans.rep, ans.err)
	}
	if c := ans.rep.Result(alsapcm.ProbeCandidate{Format: format.S32LE, RateHz: 176400}); c.Answer != alsapcm.ProbeAccepted {
		t.Errorf("the probe's carrier at 176.4 kHz: %+v", c)
	}
	if err := receiveIn(t, played, "the play after the probe"); err != nil {
		t.Fatalf("the Start after the probe: %v", err)
	}
	if v := r.k.View(); len(v.Opened) != 2 || v.Opens != 2 || v.Closes != 1 {
		t.Errorf("the probe, then the Start: %d opens asked, %d opened, %d closed; want the probe's open and close, then the Start's open", len(v.Opened), v.Opens, v.Closes)
	}
	r.heardAll(format.S243LE, 24, a)
}

// TestAStartWaitsForAProbeAtMostItsBound pins the bound of a Start's wait
// (B728's design, section 5: a probe is one open, a few ioctls and a close,
// and a Start waits for it at most a second): a probe held past it leaves
// the Start answering the device busy, asking for no open, which the engine
// records as a busy attempt; once the probe ends, a play opens the device and
// plays.
func TestAStartWaitsForAProbeAtMostItsBound(t *testing.T) {
	waits := newProbeWaits()
	r := newRig(t, func(o *rigOptions) { o.afterFunc = waits.afterFunc })
	r.k.Exclusive()
	a := pattern(1, 48000, 24)
	r.files.add("a", wav(48000, 24, a))
	r.insert("a")
	probed, letGo := r.probeHeldOpen()
	played := r.startWaits(waits)
	waits.fired()
	err := receiveIn(t, played, "the play past the probe's bound")
	if !errors.Is(err, engine.ErrDeviceBusy) {
		t.Fatalf("a Start past the probe's bound answered %v; want the device busy", err)
	}
	if at := r.waitAttempt(1); !at.Failed || !at.Busy {
		t.Errorf("its attempt: %+v, want failed and busy", at)
	}
	r.waitStopped(engine.ReasonBackend)
	if v := r.k.View(); len(v.Opened) != 1 {
		t.Errorf("a Start past the bound asked to open the device: %q", v.Opened)
	}
	letGo()
	if ans := receiveIn(t, probed, "the probe's answer"); ans.err != nil {
		t.Fatalf("the probe: %v", ans.err)
	}
	r.play()
	r.heardAll(format.S243LE, 24, a)
}

// TestNoProbeWithoutTheBoundDACOrAFreshSnapshot pins the conditions of the
// design's section 5 the backend is told: no probe while the bound DAC is not
// available, nor while the hotplug snapshot is stale; with both, the probe
// locates the bound unit, opens it, gives the node to the unit's check after
// open, refines, and closes.
func TestNoProbeWithoutTheBoundDACOrAFreshSnapshot(t *testing.T) {
	for _, c := range []struct {
		name string
		cond playback.ProbeConditions
		why  playback.ProbeSkip
	}{
		{"the DAC is not available", playback.ProbeConditions{Available: false, Fresh: true}, playback.ProbeOutputUnavailable},
		{"the snapshot is stale", playback.ProbeConditions{Available: true, Fresh: false}, playback.ProbeSnapshotStale},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := newDirect(t, nil)
			rep, err := d.be.Probe(bg, playback.ProbeRequest{Conditions: c.cond, Candidates: alsapcm.ProbeCandidates()})
			wantSkipped(t, rep, err, c.why)
			if v := d.k.View(); len(v.Opened) != 0 || v.Refines != 0 {
				t.Errorf("%d opens asked, %d refines; want none", len(v.Opened), v.Refines)
			}
			d.unit.mu.Lock()
			defer d.unit.mu.Unlock()
			if d.unit.located != 0 {
				t.Errorf("the binding was located %d times; want none", d.unit.located)
			}
		})
	}
	t.Run("both hold", func(t *testing.T) {
		d := newDirect(t, nil)
		rep, err := d.be.Probe(bg, probeRequest())
		if err != nil || !rep.Opened || rep.Endpoint != alsapcm.FakeEndpoint() {
			t.Fatalf("the probe: %+v, %v", rep, err)
		}
		if v := d.k.View(); len(v.Opened) != 1 || v.Closes != 1 || v.Open || v.Refines != len(alsapcm.ProbeCandidates()) || v.HWParams != 0 {
			t.Errorf("the probe: %d opens asked, %d closes, open %v, %d refines, %d HW_PARAMS", len(v.Opened), v.Closes, v.Open, v.Refines, v.HWParams)
		}
		d.unit.mu.Lock()
		defer d.unit.mu.Unlock()
		if d.unit.located != 1 || len(d.unit.checked) != 1 || d.unit.checked[0].Card != alsapcm.FakeEndpoint().Card {
			t.Errorf("located %d times, checked %+v; want the binding located and the node checked once", d.unit.located, d.unit.checked)
		}
	})
}

// TestAProbeOfABusyNodeAnswersAtOnce pins a busy node through the backend:
// the probe answers busy, every candidate not probed, after one open asked,
// and gives the slot back, so the next probe of the node, free now, runs.
func TestAProbeOfABusyNodeAnswersAtOnce(t *testing.T) {
	d := newDirect(t, nil)
	d.k.Busy(true)
	rep, err := d.be.Probe(bg, probeRequest())
	if alsapcm.ReasonOf(err) != alsapcm.ReasonBusy || rep.Opened {
		t.Fatalf("a busy node: %+v, %v; want busy, nothing opened", rep, err)
	}
	for _, r := range rep.Results {
		if r.Answer != alsapcm.ProbeNotProbed {
			t.Errorf("%s on a busy node: %s", r.Candidate, r.Answer)
		}
	}
	if v := d.k.View(); len(v.Opened) != 1 || v.Opens != 0 {
		t.Errorf("a busy node: %d opens asked, %d opened; want one asked", len(v.Opened), v.Opens)
	}
	d.k.Busy(false)
	if rep, err := d.be.Probe(bg, probeRequest()); err != nil || !rep.Opened {
		t.Fatalf("the probe once the node is free: %+v, %v", rep, err)
	}
}

// TestAProbeTheBindingCannotPlaceIsNotRun pins the binding's part: a probe
// whose binding names no unit now opens nothing, and says why.
func TestAProbeTheBindingCannotPlaceIsNotRun(t *testing.T) {
	d := newDirect(t, nil)
	missing := errors.New("the bound DAC is not connected")
	d.unit.mu.Lock()
	d.unit.err = missing
	d.unit.mu.Unlock()
	rep, err := d.be.Probe(bg, probeRequest())
	wantSkipped(t, rep, err, playback.ProbeNotLocated)
	if !errors.Is(err, missing) {
		t.Errorf("the refusal %v does not carry the binding's", err)
	}
	if v := d.k.View(); len(v.Opened) != 0 {
		t.Errorf("%d opens asked; want none", len(v.Opened))
	}
}

// TestOneProbeAtATime pins the slot between two probes: a probe asked while
// another holds the slot is refused at once, and the first goes on.
func TestOneProbeAtATime(t *testing.T) {
	r := newRig(t)
	probed, letGo := r.probeHeldOpen()
	r.probeAsksNothing(playback.ProbeRunning)
	letGo()
	if ans := receiveIn(t, probed, "the first probe's answer"); ans.err != nil || !ans.rep.Opened {
		t.Fatalf("the first probe: %+v, %v", ans.rep, ans.err)
	}
}
