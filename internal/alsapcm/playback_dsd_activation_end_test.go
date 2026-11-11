package alsapcm_test

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/player/dsdstate"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/playback"
	"github.com/acoseac/1-bit-bridge/internal/player/playback/playbacktest"
)

type installHold struct {
	mu  sync.Mutex
	any bool
	o   engine.OccurrenceID
	g   *gate
}

func (h *installHold) armNext(t *testing.T) *gate {
	g := newGate(t)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.any, h.g = true, g
	return g
}

func (h *installHold) arm(t *testing.T, o engine.OccurrenceID) *gate {
	g := newGate(t)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.any, h.o, h.g = false, o, g
	return g
}

func (h *installHold) note(n playback.Note) {
	h.mu.Lock()
	g := h.g
	if g == nil || !h.any && n.Occurrence != h.o {
		h.mu.Unlock()
		return
	}
	h.g = nil
	h.mu.Unlock()
	g.pass()
}

func (d *dsdRig) startedOrStopped() {
	d.t.Helper()
	d.waitSnap("the Start's answer, or playback stopped", func(s *engine.Snapshot) bool {
		return s.LastAttempt().Seq > 0 || s.State() == engine.Stopped && s.LastStop().Reason != ""
	})
}

func TestARevocationOnceTheFirstOccurrencesSinkActivatedFailsItsStart(t *testing.T) {
	w, log, inst := newLifecycleSeams(), &stopsLog{}, &installHold{}
	d := newDSDRigWith(t, initialPolicy(dsdstate.ModeNative), func(p *playback.DSDPolicy) { p.Log = log },
		func(o *rigOptions) { o.seams, o.installed = w.seams(nil), inst.note })
	d.files.add("a", dsd(playbacktest.DSD128, 0.25))
	d.insert("a")
	installed := inst.armNext(t)
	played := make(chan error, 1)
	go func() { played <- d.eng.Play(bg) }()
	awaitHeld(t, "a's sink installing it, its activation succeeded", installed.in)
	_, letFault := d.rec.holdNextFault()
	t.Cleanup(letFault)
	stops := newGate(t)
	log.arm(stops)
	res := d.refreshAfterHotplug(false)
	reachedGate(t, stops, res, "the reconciliation's revocation of the session's DSD, before its call to Revoke")
	installed.letGo()
	d.startedOrStopped()
	stops.letGo()
	refreshed(t, res)
	letFault()
	s := d.waitEnded()
	select {
	case <-played:
	case <-time.After(alsapcm.Watchdog()):
		alsapcm.LogGoroutines(t)
		t.Fatalf("the play was not answered within %v", alsapcm.Watchdog())
	}
	d.rig.stop()
	why, evs := log.reasons(), d.rec.events()
	if a := s.LastAttempt(); !a.Failed {
		t.Errorf("the play attempt %+v: want the Start failed", a)
	}
	if f := s.Faults(); len(f) != 1 || f[0].Reason != engine.ReasonBackend || len(why) != 1 || !strings.Contains(f[0].Detail, "native_revoked: "+why[0]) {
		t.Errorf("faults %+v, the revocations' reasons %q; want the Start's failure, naming the revocation for the one reason Backend.Revoke was given", f, why)
	}
	for _, kind := range []string{"fault", "position", "audible", "submitted", "prepared", "refused"} {
		if n := count(evs, kind, 0); n != 0 {
			t.Errorf("the session reported %d %s events (%s): its Start launched its reporter", n, kind, kinds(evs))
		}
	}
	if v := d.k.View(); len(v.OpenAt) != 1 || v.Open || v.Closes != v.Opens || !silence(d.k.Given()) {
		t.Errorf("once the rig stopped: opens at bytes %v, open %v, %d of %d opens closed; want a's one open, given its prefill's DSD silence alone, closed", v.OpenAt, v.Open, v.Closes, v.Opens)
	}
}

type producerSteps struct {
	mu sync.Mutex
	ch chan string
}

func (p *producerSteps) arm() <-chan string {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ch = make(chan string, 64)
	return p.ch
}

func (p *producerSteps) note(_ engine.SessionID, step string, _ engine.OccurrenceID) {
	p.mu.Lock()
	ch := p.ch
	p.mu.Unlock()
	if ch == nil {
		return
	}
	select {
	case ch <- step:
	default:
	}
}

func (d *dsdRig) producerStopped(steps <-chan string, what string) {
	d.t.Helper()
	given := d.k.When(func(v alsapcm.KernelView) bool { return len(v.OpenAt) >= 2 && v.GivenBytes > v.OpenAt[1] })
	deadline := time.After(alsapcm.Watchdog())
	var seen []string
	for {
		select {
		case st := <-steps:
			seen = append(seen, st)
			switch {
			case len(seen) == 1 && st == "stop":
				return
			case seen[0] != "write":
				d.t.Fatalf("the producer's steps were %q: want its stop on the recorded fault first", seen)
			}
		case <-given:
			v := d.k.View()
			d.t.Fatalf("b's node was given %d bytes, the producer's steps %q: it wrote past the recorded fault", v.GivenBytes-v.OpenAt[1], seen)
		case <-deadline:
			alsapcm.LogGoroutines(d.t)
			d.t.Fatalf("waited %v for %s (the producer's steps %q)", alsapcm.Watchdog(), what, seen)
		}
	}
}

func convertedRig(t *testing.T, w *lifecycleSeams, inst *installHold, log *stopsLog, steps *producerSteps) (*dsdRig, []uint32) {
	t.Helper()
	d := newDSDRigWith(t, initialPolicy(dsdstate.ModeConvert), func(p *playback.DSDPolicy) { p.Log = log }, func(o *rigOptions) {
		o.seams, o.noReplan, o.installed = w.seams(nil), true, inst.note
		if steps != nil {
			o.seams.ProducerStep = steps.note
		}
	})
	d.files.add("a", wav(44100, 16, pattern(1, 4410, 16)))
	d.files.add("b", dsd(playbacktest.DSD128, 0.25))
	d.files.add("c", dsd(playbacktest.DSD64, 0.25))
	ids := d.insert("a", "b", "c")
	d.play()
	return d, ids
}

func TestARevocationOfAConvertedSuccessorGivesItsNodeNothing(t *testing.T) {
	t.Run("held at its PCM sink's admission", func(t *testing.T) {
		w, log, inst := newLifecycleSeams(), &stopsLog{}, &installHold{}
		d, ids := convertedRig(t, w, inst, log, nil)
		ob := d.occurrenceOf(ids[1])
		admit, letAdmit := d.unit.holdNextAdmit()
		t.Cleanup(letAdmit)
		d.k.Free()
		awaitHeld(t, "the check after open of b's own PCM sink's node", admit)
		d.k.Hold()
		if v := d.k.View(); w.activatedOf(ob) || v.Opens != 2 || !v.Open || v.GivenBytes != v.OpenAt[1] {
			t.Fatalf("the check held: b's activation begun %v, %d opens, open %v, %d bytes to b's node; want b's sink's open, its node open and given nothing", w.activatedOf(ob), v.Opens, v.Open, v.GivenBytes-v.OpenAt[1])
		}
		stops := newGate(t)
		log.arm(stops)
		res := d.refreshAfterHotplug(true)
		reachedGate(t, stops, res, "the reconciliation's revocation of the session's DSD, before its call to Revoke")
		letAdmit()
		d.closedOrGiven(ob, "b's sink closed by its adoption, b stale")
		stops.letGo()
		refreshed(t, res)
		s := d.waitStopped(engine.ReasonEndOfQueue)
		d.rig.stop()
		if f, evs := s.Faults(), d.rec.events(); len(f) != 0 || count(evs, "fault", 0) != 0 {
			t.Errorf("faults %+v, events %s: want none, b no output of its own when the revocation came", f, kinds(evs))
		}
		if sk, ok := s.Skipped(ids[1]); !ok || sk.Reason != "native_revoked" {
			t.Errorf("b's entry was %+v (skipped %v), want it refused native_revoked", sk, ok)
		}
		wantUnwritten(t, d, 2, "b's")
	})
	t.Run("held as its activation installs it", func(t *testing.T) {
		w, log, inst, steps := newLifecycleSeams(), &stopsLog{}, &installHold{}, &producerSteps{}
		d, ids := convertedRig(t, w, inst, log, steps)
		ob := d.occurrenceOf(ids[1])
		installed := inst.arm(t, ob)
		d.k.Free()
		awaitHeld(t, "b's own PCM sink installing it, its activation succeeded", installed.in)
		d.k.Hold()
		_, letFault := d.rec.holdNextFault()
		t.Cleanup(letFault)
		stops := newGate(t)
		log.arm(stops)
		res := d.refreshAfterHotplug(true)
		reachedGate(t, stops, res, "the reconciliation's revocation of the session's DSD, before its call to Revoke")
		next := steps.arm()
		installed.letGo()
		d.k.Free()
		d.producerStopped(next, "b's producer stopping on the recorded fault, Revoke held")
		if v := d.k.View(); v.GivenBytes != v.OpenAt[1] {
			t.Fatalf("the producer stopped with b's node given %d bytes, want none", v.GivenBytes-v.OpenAt[1])
		}
		stops.letGo()
		refreshed(t, res)
		letFault()
		d.stoppedOrOpenedC("the revocation's fault did not stop the session before c")
		s := d.tickUntilStopped(engine.ReasonFault)
		d.rig.stop()
		wantRevoked(t, d, s, log)
		wantUnwritten(t, d, 2, "b's")
	})
}
