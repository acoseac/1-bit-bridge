package alsapcm_test

import (
	"slices"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/player/dsdstate"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/playback"
	"github.com/acoseac/1-bit-bridge/internal/player/playback/playbacktest"
)

func (f *files) firstClosed(ref string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.opened {
		if c.ref == ref {
			return c.closed
		}
	}
	return false
}

func (d *dsdRig) sessionOf() engine.SessionID {
	d.t.Helper()
	return d.waitCall("the engine's first Start", func(c call) bool { return c.kind == "start" }).s
}

func (d *dsdRig) noteOf(kind playback.NoteKind, o engine.OccurrenceID, what string) {
	d.t.Helper()
	deadline := time.After(alsapcm.Watchdog())
	for {
		select {
		case n := <-d.notes:
			if n.Kind == kind && n.Occurrence == o {
				return
			}
		case <-deadline:
			alsapcm.LogGoroutines(d.t)
			d.t.Fatalf("waited %v for %s", alsapcm.Watchdog(), what)
		}
	}
}

func (d *dsdRig) opensOrStopped(n int, what string) {
	d.t.Helper()
	stopped := func() bool { s := d.eng.Snapshot(); return s.State() == engine.Stopped && s.LastStop().Reason != "" }
	waitOnEither(d.t, what, d.eng.Changed, func() <-chan struct{} { return d.k.When(func(v alsapcm.KernelView) bool { return v.Opens >= n }) },
		func() bool { return d.k.View().Opens >= n || stopped() })
	if s := d.eng.Snapshot(); d.k.View().Opens < n {
		d.t.Fatalf("playback stopped (%s: %s) before %s; faults %+v, events %s", s.LastStop().Reason, s.LastStop().Detail, what, s.Faults(), kinds(d.rec.events()))
	}
}

func heldSuccessor(t *testing.T, w *lifecycleSeams) (*dsdRig, []uint32, engine.OccurrenceID, *gate) {
	t.Helper()
	d := newDSDRig(t, initialPolicy(dsdstate.ModeNative), func(o *rigOptions) { o.seams = w.seams(nil) })
	ids, ob, act := heldSuccessorOn(t, d, w)
	return d, ids, ob, act
}

func heldSuccessorOn(t *testing.T, d *dsdRig, w *lifecycleSeams) ([]uint32, engine.OccurrenceID, *gate) {
	t.Helper()
	d.files.add("a", dsd(playbacktest.DSD64, 0.25))
	d.files.add("b", dsd(playbacktest.DSD128, 0.25))
	d.files.add("c", dsd(playbacktest.DSD64, 0.25))
	ids := d.insert("a", "b", "c")
	d.play()
	ob := d.occurrenceOf(ids[1])
	act := w.holdActivation(t, ob)
	d.k.Free()
	awaitHeld(t, "b's activation, its sink adopted", act.in)
	d.k.Hold()
	return ids, ob, act
}

func (d *dsdRig) replaced(old engine.OccurrenceID, what string) {
	d.t.Helper()
	d.waitCall(what, func(c call) bool { return c.kind == "prepare" && c.next != nil && c.next.ID != old && c.next.ID > old })
}

func wentOn(t *testing.T, d *dsdRig, ids []uint32) *engine.Snapshot {
	t.Helper()
	return wentOnGiven(t, d, ids, "nothing", func(b []byte) bool { return len(b) == 0 })
}

func wentOnGiven(t *testing.T, d *dsdRig, ids []uint32, what string, given func([]byte) bool) *engine.Snapshot {
	t.Helper()
	d.opensOrStopped(3, "c's own sink opened, the session going on past b")
	if !d.files.firstClosed("b") {
		t.Errorf("b's decoder and content are still open once the session went on past it: the dropped occurrence's decoder was not closed")
	}
	if v := d.k.View(); len(v.OpenAt) < 3 || !given(d.k.Given()[v.OpenAt[1]:v.OpenAt[2]]) {
		t.Errorf("b's node: opens at bytes %v; want b's node given %s before c's opened", v.OpenAt, what)
	}
	oc := d.occurrenceOf(ids[2])
	d.k.Free()
	s := d.waitEnded()
	d.rig.stop()
	evs := d.rec.events()
	if f := s.Faults(); len(f) != 0 || count(evs, "fault", 0) != 0 {
		t.Errorf("b dropped at its activation faulted the session: %+v; events %s", f, kinds(evs))
	}
	if count(evs, "audible", oc) != 1 {
		t.Errorf("c was not heard once the session went on: %s", kinds(evs))
	}
	return s
}

func TestASuccessorDroppedAtItsActivationIsTakenBackUnwritten(t *testing.T) {
	t.Run("b's entry deleted while its activation is held", func(t *testing.T) {
		w := newLifecycleSeams()
		d, ids, ob, act := heldSuccessor(t, w)
		if err := d.eng.Delete(bg, ids[1]); err != nil {
			t.Fatalf("deleting b's entry: %v", err)
		}
		d.replaced(ob, "c handed off in b's place")
		act.letGo()
		s := wentOn(t, d, ids)
		if _, ok := s.Skipped(ids[1]); ok {
			t.Errorf("b's entry, deleted, is listed skipped: %+v", s.Skips())
		}
	})
	t.Run("a cut of b while its activation is held", func(t *testing.T) {
		w := newLifecycleSeams()
		d, ids, ob, act := heldSuccessor(t, w)
		if ok, err := d.be.Cut(bg, d.sessionOf(), ob); !ok || err != nil {
			t.Fatalf("the cut of b, none of it written: %v %v, want true", ok, err)
		}
		act.letGo()
		d.noteOf(playback.NoteClosed, ob, "b's sink closed")
		if s := d.eng.Snapshot(); s.State() == engine.Stopped || count(d.rec.events(), "fault", 0) != 0 {
			t.Fatalf("b cut at its activation stopped the session (%s: %s): faults %+v, events %s", s.LastStop().Reason, s.LastStop().Detail, s.Faults(), kinds(d.rec.events()))
		}
		if err := d.eng.Delete(bg, ids[1]); err != nil {
			t.Fatalf("deleting b's entry: %v", err)
		}
		d.replaced(ob, "c handed off in b's place")
		wentOn(t, d, ids)
	})
	t.Run("a release beside b's replacement while its activation is held", func(t *testing.T) {
		w := newLifecycleSeams()
		d, ids, ob, act := heldSuccessor(t, w)
		if err := d.eng.Delete(bg, ids[1]); err != nil {
			t.Fatalf("deleting b's entry: %v", err)
		}
		d.replaced(ob, "c handed off in b's place")
		rel := w.holdReleasing(t)
		stopped := make(chan error, 1)
		go func() { stopped <- d.eng.Stop(bg) }()
		awaitHeld(t, "the session's release", rel.in)
		act.letGo()
		waitOn(t, "a step refused for the release", d.rec.ch, func() bool { return len(d.rec.fencedSteps()) > 0 })
		rel.letGo()
		select {
		case err := <-stopped:
			if err != nil {
				t.Fatalf("the stop: %v", err)
			}
		case <-time.After(alsapcm.Watchdog()):
			alsapcm.LogGoroutines(t)
			t.Fatalf("the stop was not answered within %v", alsapcm.Watchdog())
		}
		v := d.k.View()
		if v.Opens != 2 || v.Open || v.GivenBytes != v.OpenAt[1] {
			t.Errorf("after the release: %d opens, open %v, %d bytes given to b's node; want a's and b's opens alone, b's given nothing, every node closed", v.Opens, v.Open, v.GivenBytes-v.OpenAt[1])
		}
		if !d.files.firstClosed("b") {
			t.Errorf("b's decoder and content are open once the release was acknowledged")
		}
		if s := d.eng.Snapshot(); len(s.Faults()) != 0 || count(d.rec.events(), "fault", 0) != 0 {
			t.Errorf("the release faulted: %+v; events %s", s.Faults(), kinds(d.rec.events()))
		}
		if got := d.rec.fencedSteps(); !slices.Equal(got, []string{"activate"}) {
			t.Errorf("the steps refused for the release: %q, want b's activation's", got)
		}
	})
}

func releasedAt(t *testing.T, d *dsdRig, stopped <-chan error) {
	t.Helper()
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatalf("the stop: %v", err)
		}
	case <-time.After(alsapcm.Watchdog()):
		alsapcm.LogGoroutines(t)
		t.Fatalf("the stop was not answered within %v", alsapcm.Watchdog())
	}
	v := d.k.View()
	if v.Open || v.Closes != v.Opens || len(v.OpenAt) != 2 || v.GivenBytes != v.OpenAt[1] {
		t.Errorf("once the release was acknowledged: open %v, %d opens and %d closes, opens at bytes %v of %d given; want every open closed, b's node given nothing", v.Open, v.Opens, v.Closes, v.OpenAt, v.GivenBytes)
	}
	d.files.mu.Lock()
	for _, c := range d.files.opened {
		if !c.closed {
			t.Errorf("the content of %q is open once the release was acknowledged", c.ref)
		}
	}
	d.files.mu.Unlock()
	starts := startsOf(d.rec.callList())
	d.play()
	waitOn(t, "the next session's Start", d.rec.ch, func() bool { return len(startsOf(d.rec.callList())) > len(starts) })
	if c := startsOf(d.rec.callList())[len(starts)]; c.err != nil {
		t.Errorf("the Start right after the release: %v, want the device free", c.err)
	}
	if v := d.k.View(); v.Opens < 3 {
		t.Errorf("%d opens once the next session started, want the node opened for it", v.Opens)
	}
}

func startsOf(calls []call) []call {
	return slices.DeleteFunc(calls, func(c call) bool { return c.kind != "start" })
}

func TestAReleaseWhileASuccessorsAdoptionWaitsLetsGoOfTheDevice(t *testing.T) {
	t.Run("in the pause its sink takes first", func(t *testing.T) {
		w := newLifecycleSeams()
		d := newDSDRig(t, initialPolicy(dsdstate.ModeNative), func(o *rigOptions) { o.seams = w.seams(nil) })
		d.files.add("a", dsd(playbacktest.DSD64, 0.25))
		d.files.add("b", wav(44100, 16, pattern(2, 4410, 16)))
		d.insert("a", "b")
		d.play()
		admit, letAdmit := d.unit.holdNextAdmit()
		t.Cleanup(letAdmit)
		d.k.Free()
		awaitHeld(t, "the check after open of b's node", admit)
		if err := d.eng.Pause(bg); err != nil {
			t.Fatalf("a pause while b's sink opens: %v", err)
		}
		d.waitSnap("paused", func(s *engine.Snapshot) bool { return s.State() == engine.Paused })
		pause := newGate(t)
		d.k.OnCommand(func(_ alsapcm.SinkConfig, op string) {
			if op == "pause" {
				pause.pass()
			}
		})
		letAdmit()
		awaitHeld(t, "the pause b's sink takes before it is adopted", pause.in)
		stopped := make(chan error, 1)
		go func() { stopped <- d.eng.Stop(bg) }()
		releasedAt(t, d, stopped)
		pause.letGo()
	})
}
