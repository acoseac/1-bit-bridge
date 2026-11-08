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

// A write of the bound DAC's DSD choice beside playback (B728 B9 round 3),
// through the engine's Run, the backend and the real sinks over the fake
// kernel: a successor dropped while its activation is held is taken back
// from its sink unwritten, its decoder closed, and the session goes on
// (point 1); and a release while a successor's adoption waits lets go of the
// device before it is acknowledged (point 3).

// firstClosed reports whether the first content of ref the resolver handed
// out was closed: the first occurrence of ref's.
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

// sessionOf is the session the engine's first Start began.
func (d *dsdRig) sessionOf() engine.SessionID {
	d.t.Helper()
	return d.waitCall("the engine's first Start", func(c call) bool { return c.kind == "start" }).s
}

// noteOf waits for the backend's note of kind for occurrence o's sink,
// reading the rig's notes, which nothing else in these tests reads.
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

// opensOrStopped waits until the fake node has been opened n times, or
// playback stopped, and fails t, naming the stop, where it stopped first.
func (d *dsdRig) opensOrStopped(n int, what string) {
	d.t.Helper()
	stopped := func() bool { s := d.eng.Snapshot(); return s.State() == engine.Stopped && s.LastStop().Reason != "" }
	waitOnEither(d.t, what, d.eng.Changed, func() <-chan struct{} { return d.k.When(func(v alsapcm.KernelView) bool { return v.Opens >= n }) },
		func() bool { return d.k.View().Opens >= n || stopped() })
	if s := d.eng.Snapshot(); d.k.View().Opens < n {
		d.t.Fatalf("playback stopped (%s: %s) before %s; faults %+v, events %s", s.LastStop().Reason, s.LastStop().Detail, what, s.Faults(), kinds(d.rec.events()))
	}
}

// heldSuccessor is a rig, its seams w's, playing a (DSD64), b (DSD128) and
// c (DSD64) natively, each for a quarter second: a has played out and b's
// own sink is adopted, b's activation held, and the DAC held, so nothing of
// what follows plays until the test frees it. It answers the entries and b's
// occurrence.
func heldSuccessor(t *testing.T, w *writeSeams) (*dsdRig, []uint32, engine.OccurrenceID, *gate) {
	t.Helper()
	d := newDSDRig(t, playbacktest.ChoiceState(dsdstate.ModeNative), func(o *rigOptions) { o.seams = w.seams(nil) })
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
	return d, ids, ob, act
}

// replaced waits until the engine has handed off, after a, an occurrence
// other than old: the backend's Prepare that dropped old has returned.
func (d *dsdRig) replaced(old engine.OccurrenceID, what string) {
	d.t.Helper()
	d.waitCall(what, func(c call) bool { return c.kind == "prepare" && c.next != nil && c.next.ID != old && c.next.ID > old })
}

// wentOn checks a session whose successor b was dropped at its activation:
// it went on to c's own sink, opened third, with nothing of b's given to b's
// node, b's decoder closed by then, before c played a frame (the DAC is
// held), so no retirement of b's closed it; then plays c out and checks that
// nothing faulted and c was heard.
func wentOn(t *testing.T, d *dsdRig, ids []uint32) *engine.Snapshot {
	t.Helper()
	d.opensOrStopped(3, "c's own sink opened, the session going on past b")
	if !d.files.firstClosed("b") {
		t.Errorf("b's decoder and content are still open once the session went on past it: the dropped occurrence's decoder was not closed")
	}
	if v := d.k.View(); len(v.OpenAt) < 3 || v.OpenAt[2] != v.OpenAt[1] {
		t.Errorf("b's node: opens at bytes %v; want b's node given nothing before c's opened", v.OpenAt)
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

// TestASuccessorDroppedAtItsActivationIsTakenBackUnwritten (B728 B9 round 3,
// point 1): a successor dropped while its sink's activation is held (an
// interrupt-authorized write that took its rate away landed and the engine
// replaced it, in either order with the activation's release; an edit that
// deleted its entry; a cut) is taken back from its sink unwritten, never a
// fault: its activation answers errReplanned, its decoder and content are
// closed as it is taken back, since the drop left them to the producer that
// writes it, and the session goes on to what follows. A release beside it
// keeps the activation's fence, and the session ends there, opening nothing
// more; the teardown closes the decoder.
func TestASuccessorDroppedAtItsActivationIsTakenBackUnwritten(t *testing.T) {
	cap64 := func(t *testing.T, d *dsdRig) playback.ChoiceRequest {
		return choiceReq(t, d, dsdstate.ModeNative, 1, capAt(playbacktest.DSD64), goAhead)
	}
	t.Run("the write landing and b replaced before its activation goes on", func(t *testing.T) {
		w := newWriteSeams()
		d, ids, ob, act := heldSuccessor(t, w)
		if got := answer(t, d.choose(cap64(t, d))); got.Outcome != playback.ChoiceWritten {
			t.Fatalf("the cap with the go-ahead: %s, want written", got.Outcome)
		}
		d.replaced(ob, "b's replacement once the cap landed")
		act.letGo()
		s := wentOn(t, d, ids)
		if _, ok := s.Skipped(ids[1]); !ok {
			t.Errorf("b's entry was not skipped: the cap refuses its DSD128; skips %+v", s.Skips())
		}
	})
	t.Run("b's activation waiting for the write, and b replaced before it wakes", func(t *testing.T) {
		w := newWriteSeams()
		d, ids, ob, act := heldSuccessor(t, w)
		stop := w.holdPending(t)
		woke := w.holdWoke(t, ob)
		res := d.choose(cap64(t, d))
		reachedGate(t, stop, res, "the write's pending window")
		act.letGo()
		w.waitAwaited(t, ob, 1)
		stop.letGo()
		if got := answer(t, res); got.Outcome != playback.ChoiceWritten {
			t.Fatalf("the cap with the go-ahead: %s, want written", got.Outcome)
		}
		awaitHeld(t, "b's activation woken from the write", woke.in)
		d.replaced(ob, "b's replacement once the cap landed")
		woke.letGo()
		s := wentOn(t, d, ids)
		if _, ok := s.Skipped(ids[1]); !ok {
			t.Errorf("b's entry was not skipped: the cap refuses its DSD128; skips %+v", s.Skips())
		}
	})
	t.Run("b's entry deleted while its activation is held", func(t *testing.T) {
		w := newWriteSeams()
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
		w := newWriteSeams()
		d, _, ob, act := heldSuccessor(t, w)
		if ok, err := d.be.Cut(bg, d.sessionOf(), ob); !ok || err != nil {
			t.Fatalf("the cut of b, none of it written: %v %v, want true", ok, err)
		}
		act.letGo()
		// The note of b's sink closed, which the taking back sends once b's
		// decoder is closed (unadopt): the cut's own revocation of b's sink
		// closes its node, and sends none.
		d.noteOf(playback.NoteClosed, ob, "b's sink closed")
		if s := d.eng.Snapshot(); s.State() == engine.Stopped || count(d.rec.events(), "fault", 0) != 0 {
			t.Fatalf("b cut at its activation stopped the session (%s: %s): faults %+v, events %s", s.LastStop().Reason, s.LastStop().Detail, s.Faults(), kinds(d.rec.events()))
		}
		if !d.files.firstClosed("b") {
			t.Errorf("b's decoder and content are still open once its node was closed: the dropped occurrence's decoder was not closed")
		}
		if v := d.k.View(); v.GivenBytes != v.OpenAt[1] {
			t.Errorf("b's node was given %d bytes, want none", v.GivenBytes-v.OpenAt[1])
		}
	})
	t.Run("a release beside b's replacement while its activation is held", func(t *testing.T) {
		w := newWriteSeams()
		d, ids, ob, act := heldSuccessor(t, w)
		if err := d.eng.Delete(bg, ids[1]); err != nil {
			t.Fatalf("deleting b's entry: %v", err)
		}
		d.replaced(ob, "c handed off in b's place")
		rel := w.holdReleasing(t)
		stopped := make(chan error, 1)
		go func() { stopped <- d.eng.Stop(bg) }()
		awaitHeld(t, "the session's release", rel.in)
		// The release has come and the session's life has not ended yet (its
		// release is held at the Releasing seam): b's activation goes on there,
		// and its fence's answer is the release's, noted. A fault it made would
		// be reported only once the release goes on (the teardown's last
		// report: the reporter sends nothing while a session is released), so
		// a step refused for the release is what this waits for.
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

// releasedAt checks a release acknowledged while a successor's sink was
// adopted: stopped answers the Stop, and then the node is closed, every open
// of it closed once, nothing of b's given to b's node (opened second), every
// content handed out closed, and the device free for a Start right after:
// played again, the engine's next session opens the node.
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

// startsOf are the Starts among calls, in order.
func startsOf(calls []call) []call {
	return slices.DeleteFunc(calls, func(c call) bool { return c.kind != "start" })
}

// TestAReleaseWhileASuccessorsAdoptionWaitsLetsGoOfTheDevice (B728 B9 round
// 3, point 3): a session released while a successor's own sink, opened and
// not yet the current one, waits to be adopted (for a write pending that
// plays its rate otherwise, or in the pause its sink takes first, the session
// having paused while it opened) is left by the producer as it is; the
// release begins each native sink's release and ends each PCM sink's life,
// and the teardown, which owns every sink opened (the session's outputs),
// closes it after the files and before it acknowledges, so the node is
// closed, every content closed and the device free for a Start once the
// release is acknowledged.
func TestAReleaseWhileASuccessorsAdoptionWaitsLetsGoOfTheDevice(t *testing.T) {
	t.Run("waiting for a write pending", func(t *testing.T) {
		w := newWriteSeams()
		d := newDSDRig(t, playbacktest.ChoiceState(dsdstate.ModeNative), func(o *rigOptions) { o.seams = w.seams(nil) })
		d.files.add("a", dsd(playbacktest.DSD64, 0.25))
		d.files.add("b", dsd(playbacktest.DSD128, 0.25))
		ids := d.insert("a", "b")
		d.play()
		ob := d.occurrenceOf(ids[1])
		admit, letAdmit := d.unit.holdNextAdmit()
		t.Cleanup(letAdmit)
		d.k.Free()
		awaitHeld(t, "the check after open of b's node", admit)
		stop := w.holdPending(t)
		res := d.choose(choiceReq(t, d, dsdstate.ModeNative, 1, capAt(playbacktest.DSD64)))
		reachedGate(t, stop, res, "the write's pending window")
		letAdmit()
		w.waitedUnactivated(t, ob)
		stopped := make(chan error, 1)
		go func() { stopped <- d.eng.Stop(bg) }()
		releasedAt(t, d, stopped)
		stop.letGo()
		answer(t, res)
	})
	t.Run("in the pause its sink takes first", func(t *testing.T) {
		w := newWriteSeams()
		d := newDSDRig(t, playbacktest.ChoiceState(dsdstate.ModeNative), func(o *rigOptions) { o.seams = w.seams(nil) })
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
