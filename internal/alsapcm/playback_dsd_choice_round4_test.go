package alsapcm_test

import (
	"bytes"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/player/dacprofile"
	"github.com/acoseac/1-bit-bridge/internal/player/dsdstate"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/playback"
	"github.com/acoseac/1-bit-bridge/internal/player/playback/playbacktest"
)

// A successor dropped while its own sink's activation is under way past the
// backend's checks (B728 B9 round 4), through the engine's Run, the backend
// and the real native sinks over the fake kernel: dropped while the sink's
// writer admits its node (the check after open the activation makes) or
// writes its prefill, it is taken back from its sink with none of it written,
// its decoder closed, and the session goes on (point 1); and an activation
// that fails beside a drop is judged by its cause, the drop's own answer
// taken back and every other kept.

// checks is how many checks after open the unit has answered.
func (u *unit) checks() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.checked)
}

// refuseNext makes the unit's next n checks after open refuse the node.
func (u *unit) refuseNext(n int) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.refuse = n
}

// activatedOf reports whether occurrence o's activation began.
func (w *writeSeams) activatedOf(o engine.OccurrenceID) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Contains(w.activated, o)
}

// heldAdmission is heldSuccessor (a DSD64, b DSD128 and c DSD64 natively, b's
// activation held as it begins, the DAC held) with b's activation let go and
// held again inside b's sink, at the check after open the native activation
// makes of its node on the sink's writer, past the backend's own checks. The
// check is armed once b's sink was opened, noted and adopted, so the one held
// is the activation's, not the open's: the first after b's activation began,
// b's node open and given nothing. It answers the entries, b's occurrence and
// the function that lets the admission go.
func heldAdmission(t *testing.T, w *writeSeams) (*dsdRig, []uint32, engine.OccurrenceID, func()) {
	t.Helper()
	d := newDSDRig(t, playbacktest.ChoiceState(dsdstate.ModeNative), func(o *rigOptions) { o.seams = w.seams(nil) })
	ids, ob, letAdmit := heldAdmissionOn(t, d, w)
	return d, ids, ob, letAdmit
}

// heldAdmissionOn is heldAdmission on a rig the caller made, its seams w's
// (B728 B9 round 5).
func heldAdmissionOn(t *testing.T, d *dsdRig, w *writeSeams) ([]uint32, engine.OccurrenceID, func()) {
	t.Helper()
	ids, ob, act := heldSuccessorOn(t, d, w)
	checked := d.unit.checks()
	admit, letAdmit := d.unit.holdNextAdmit()
	t.Cleanup(letAdmit)
	act.letGo()
	awaitHeld(t, "the admission b's sink's activation makes", admit)
	v := d.k.View()
	if !w.activatedOf(ob) || d.rec.openedSinks() != 2 || d.unit.checks() != checked || v.Opens != 2 || !v.Open || v.GivenBytes != v.OpenAt[1] {
		t.Fatalf("the check held: b's activation begun %v, %d sinks noted opened, %d checks answered since b's adoption, %d opens, open %v, %d bytes given to b's node; want the first check after b's activation began, b's sink opened, b's node open and given nothing",
			w.activatedOf(ob), d.rec.openedSinks(), d.unit.checks()-checked, v.Opens, v.Open, v.GivenBytes-v.OpenAt[1])
	}
	return ids, ob, letAdmit
}

// TestASuccessorDroppedAtItsSinksAdmissionIsTakenBackUnwritten (B728 B9
// round 4, point 1): a successor dropped while its sink's activation admits
// its node, past the backend's checks, is taken back from its sink unwritten
// and the session goes on to what follows, never a fault: the drop revokes
// the sink, whose activation then answers its own fence. Each drop that
// reaches the admission: an edit that deletes b's entry, a write whose
// replacement of b drops it, a write that hands b off again with its
// replacement still to come (b stale, not yet dropped, the hand-off revoking
// the sink it activates as a drop does), and a cut, the backend's (Cut drops
// an occurrence none of whose frames is written at once while the producer is
// in no Write, and here it is in the activation), the engine, which replaces
// a successor it has not submitted through Prepare and so still names b, told
// by the edit that deletes b's entry.
func TestASuccessorDroppedAtItsSinksAdmissionIsTakenBackUnwritten(t *testing.T) {
	cap64 := func(t *testing.T, d *dsdRig) playback.ChoiceRequest {
		return choiceReq(t, d, dsdstate.ModeNative, 1, capAt(playbacktest.DSD64), goAhead)
	}
	t.Run("b's entry deleted", func(t *testing.T) {
		w := newWriteSeams()
		d, ids, ob, letAdmit := heldAdmission(t, w)
		if err := d.eng.Delete(bg, ids[1]); err != nil {
			t.Fatalf("deleting b's entry: %v", err)
		}
		d.replaced(ob, "c handed off in b's place")
		letAdmit()
		s := wentOn(t, d, ids)
		if _, ok := s.Skipped(ids[1]); ok {
			t.Errorf("b's entry, deleted, is listed skipped: %+v", s.Skips())
		}
	})
	t.Run("a write's replacement of b", func(t *testing.T) {
		w := newWriteSeams()
		d, ids, ob, letAdmit := heldAdmission(t, w)
		if got := answer(t, d.choose(cap64(t, d))); got.Outcome != playback.ChoiceWritten {
			t.Fatalf("the cap with the go-ahead: %s, want written", got.Outcome)
		}
		d.replaced(ob, "b's replacement once the cap landed")
		letAdmit()
		s := wentOn(t, d, ids)
		if _, ok := s.Skipped(ids[1]); !ok {
			t.Errorf("b's entry was not skipped: the cap refuses its DSD128; skips %+v", s.Skips())
		}
	})
	t.Run("a write that hands b off again, its replacement still to come", func(t *testing.T) {
		w := newWriteSeams()
		d, ids, ob, letAdmit := heldAdmission(t, w)
		prepare, letPrepare := d.rec.holdNextPrepare()
		t.Cleanup(letPrepare)
		if got := answer(t, d.choose(cap64(t, d))); got.Outcome != playback.ChoiceWritten {
			t.Fatalf("the cap with the go-ahead: %s, want written", got.Outcome)
		}
		awaitHeld(t, "the engine's replacement of b, held short of the backend", prepare)
		// b is being handed off again and is not dropped: its activation
		// answers, and its sink either closes its node unwritten or writes to it.
		letAdmit()
		d.k.Await(t, "b's node closed, or written to", func(v alsapcm.KernelView) bool { return v.Closes >= 2 || v.GivenBytes > v.OpenAt[1] })
		if v := d.k.View(); v.GivenBytes != v.OpenAt[1] {
			t.Fatalf("b's sink's activation gave b's node %d bytes while b was being handed off again: the hand-off left the sink to be activated", v.GivenBytes-v.OpenAt[1])
		}
		letPrepare()
		d.replaced(ob, "b's replacement once the cap landed")
		s := wentOn(t, d, ids)
		if _, ok := s.Skipped(ids[1]); !ok {
			t.Errorf("b's entry was not skipped: the cap refuses its DSD128; skips %+v", s.Skips())
		}
	})
	t.Run("a cut of b, the engine told by the edit that deletes its entry", func(t *testing.T) {
		w := newWriteSeams()
		d, ids, ob, letAdmit := heldAdmission(t, w)
		if ok, err := d.be.Cut(bg, d.sessionOf(), ob); !ok || err != nil {
			t.Fatalf("the cut of b, none of it written: %v %v, want true", ok, err)
		}
		// The cut alone, the engine not told yet (B728 B9 round 5, point 3): b
		// is taken back at its activation, its node closed and given nothing,
		// its decoder closed, and nothing faulted or stopped.
		letAdmit()
		d.k.Await(t, "b's node closed, or written to", func(v alsapcm.KernelView) bool { return v.Closes >= 2 || v.GivenBytes > v.OpenAt[1] })
		if v := d.k.View(); v.GivenBytes != v.OpenAt[1] {
			t.Fatalf("b's sink's activation gave b's node %d bytes after the cut: the cut left the sink to be activated", v.GivenBytes-v.OpenAt[1])
		}
		d.noteOf(playback.NoteClosed, ob, "b's sink closed, taken back from its activation")
		if s := d.eng.Snapshot(); s.State() == engine.Stopped || count(d.rec.events(), "fault", 0) != 0 {
			t.Fatalf("b cut at its sink's admission stopped the session (%s: %s): faults %+v, events %s", s.LastStop().Reason, s.LastStop().Detail, s.Faults(), kinds(d.rec.events()))
		}
		if !d.files.firstClosed("b") {
			t.Errorf("b's decoder and content are open once its sink was taken back: the cut occurrence's decoder was not closed")
		}
		if err := d.eng.Delete(bg, ids[1]); err != nil {
			t.Fatalf("deleting b's entry: %v", err)
		}
		d.replaced(ob, "c handed off in b's place")
		wentOn(t, d, ids)
	})
}

// prefillRig is a rig, its seams w's, playing a (PCM, which has no prefill,
// so its start answers with the DAC held), then b (DSD128) and c (DSD64)
// natively under a profile whose prefill is a second, past the hardware
// buffer, b's activation let go once its sink was adopted, the DAC held. With
// park, the first write of b's prefill is held inside the kernel, and the
// function that lets it go is answered; without, the prefill has filled the
// buffer, the stream has started and the writer waits on the DAC for room. It
// answers the entries and b's occurrence too.
func prefillRig(t *testing.T, w *writeSeams, park bool) (*dsdRig, []uint32, engine.OccurrenceID, func()) {
	t.Helper()
	d := newDSDRigWith(t, playbacktest.ChoiceState(dsdstate.ModeNative), func(p *playback.DSDPolicy) {
		p.Profiles = playbacktest.KnownProfiles(&dacprofile.Timing{DSDPrefillMs: 1000})
	}, func(o *rigOptions) { o.seams = w.seams(nil) })
	d.files.add("a", wav(44100, 16, pattern(1, 4410, 16)))
	d.files.add("b", dsd(playbacktest.DSD128, 0.25))
	d.files.add("c", dsd(playbacktest.DSD64, 0.25))
	ids := d.insert("a", "b", "c")
	d.play()
	ob := d.occurrenceOf(ids[1])
	act := w.holdActivation(t, ob)
	d.k.Free()
	awaitHeld(t, "b's activation, its sink adopted", act.in)
	d.k.Hold()
	if !park {
		act.letGo()
		d.k.Await(t, "b's prefill filling its node's buffer, the writer waiting on the DAC", func(v alsapcm.KernelView) bool { return v.Opens == 2 && runningAndWaiting(v) })
		return d, ids, ob, func() {}
	}
	parked, letWrite := d.k.ParkNextWrite()
	t.Cleanup(letWrite)
	act.letGo()
	awaitHeld(t, "the first write of b's prefill, held inside the kernel", parked)
	return d, ids, ob, letWrite
}

// silence reports whether b holds DSD silence (0x69) alone, and some.
func silence(b []byte) bool { return len(b) > 0 && bytes.Equal(b, bytes.Repeat([]byte{0x69}, len(b))) }

// TestASuccessorDroppedWhileItsSinkWritesItsPrefillIsTakenBack (B728 B9
// round 4, point 1): a native activation writes the run's prefill, the DSD
// silence the profile gives (up to 5 s), before it answers, as fast as the DAC
// plays it, so a successor whose entry is deleted then has its sink revoked
// mid-prefill: the activation answers its own fence, the prefill cut short,
// and b is taken back with none of its content written, its node given that
// silence alone, its decoder closed, and the session goes on to c.
func TestASuccessorDroppedWhileItsSinkWritesItsPrefillIsTakenBack(t *testing.T) {
	w := newWriteSeams()
	d, ids, ob, _ := prefillRig(t, w, false)
	if err := d.eng.Delete(bg, ids[1]); err != nil {
		t.Fatalf("deleting b's entry: %v", err)
	}
	d.replaced(ob, "c handed off in b's place")
	wentOnGiven(t, d, ids, "its prefill's DSD silence alone", silence)
}

// stoppedOrOpenedC waits until playback has stopped or the node has been
// opened a third time, for c, whichever comes first, and fails t, saying why,
// on the second: c's own sink opened, the session gone on past b.
func (d *dsdRig) stoppedOrOpenedC(why string) {
	d.t.Helper()
	stopped := func() bool { s := d.eng.Snapshot(); return s.State() == engine.Stopped && s.LastStop().Reason != "" }
	waitOnEither(d.t, "playback to stop, or c's own sink to open", d.eng.Changed, func() <-chan struct{} { return d.k.When(func(v alsapcm.KernelView) bool { return v.Opens >= 3 }) },
		func() bool { return d.k.View().Opens >= 3 || stopped() })
	if v := d.k.View(); v.Opens >= 3 {
		d.t.Fatalf("c's own sink opened (%d opens), the session gone on past b: %s", v.Opens, why)
	}
}

// droppedAtAdmission is heldAdmission with b's entry deleted and c handed off
// in b's place while the admission is held. It answers c's occurrence and the
// function that lets the admission go.
func droppedAtAdmission(t *testing.T, w *writeSeams) (*dsdRig, engine.OccurrenceID, func()) {
	t.Helper()
	d, ids, ob, letAdmit := heldAdmission(t, w)
	if err := d.eng.Delete(bg, ids[1]); err != nil {
		t.Fatalf("deleting b's entry: %v", err)
	}
	d.replaced(ob, "c handed off in b's place")
	return d, d.occurrenceOf(ids[2]), letAdmit
}

// TestAnActivationsFailureBesideADropIsJudgedByItsCause (B728 B9 round 4,
// point 1): an activation that fails while its successor is dropped is taken
// back only where its failure is the drop's own: a release in the admission
// ends the session there, the admission and the activation refused for it,
// nothing opened for c and no fault; so does a release while b's prefill is
// written, b dropped beside it, where the activation answers the sink's own
// fence for both and the release comes first; a revocation of the session's
// DSD keeps its fault, c never activated; and the device's own refusal of the
// node at the admission is the fault it was, the session going no further.
// A node whose close is unconfirmed beside the drop (round 5, point 2) is the
// session's fault, nothing opened for c: at the activation's take-back, which
// waits for the sink's end, and at a resume's reopen of the node, whose
// answer is the resume's.
func TestAnActivationsFailureBesideADropIsJudgedByItsCause(t *testing.T) {
	t.Run("a release", func(t *testing.T) {
		w := newWriteSeams()
		d, oc, letAdmit := droppedAtAdmission(t, w)
		rel := w.holdReleasing(t)
		stopped := make(chan error, 1)
		go func() { stopped <- d.eng.Stop(bg) }()
		awaitHeld(t, "the session's release", rel.in)
		// The release has come and the session's life has not ended (its
		// release is held at the Releasing seam): the admission answers the
		// release, the drop beside it, and the activation's answer is judged.
		letAdmit()
		waitOn(t, "the activation's step refused for the release", d.rec.ch, func() bool { return len(d.rec.fencedSteps()) >= 2 })
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
		if v := d.k.View(); v.Opens != 2 || v.Open || v.GivenBytes != v.OpenAt[1] {
			t.Errorf("after the release: %d opens, open %v, %d bytes given to b's node; want a's and b's opens alone, b's given nothing, every node closed", v.Opens, v.Open, v.GivenBytes-v.OpenAt[1])
		}
		if !d.files.firstClosed("b") {
			t.Errorf("b's decoder and content are open once the release was acknowledged")
		}
		if s := d.eng.Snapshot(); len(s.Faults()) != 0 || count(d.rec.events(), "fault", 0) != 0 {
			t.Errorf("the release faulted: %+v; events %s", s.Faults(), kinds(d.rec.events()))
		}
		if got := d.rec.fencedSteps(); !slices.Equal(got, []string{"admit", "activate"}) {
			t.Errorf("the steps refused for the release: %q, want b's admission's and its activation's, and nothing opened for c", got)
		}
		if w.activatedOf(oc) {
			t.Errorf("c's activation began in a session being released")
		}
	})
	t.Run("a release while b's prefill is written, b dropped beside it", func(t *testing.T) {
		w := newWriteSeams()
		d, ids, ob, letWrite := prefillRig(t, w, true)
		if err := d.eng.Delete(bg, ids[1]); err != nil {
			t.Fatalf("deleting b's entry: %v", err)
		}
		d.replaced(ob, "c handed off in b's place")
		rel := w.holdReleasing(t)
		stopped := make(chan error, 1)
		go func() { stopped <- d.eng.Stop(bg) }()
		awaitHeld(t, "the session's release", rel.in)
		// b dropped and the session released, its life not ended yet (its
		// release is held at the Releasing seam): the prefill's write the
		// kernel held returns, and the activation answers the sink's own fence,
		// which the drop and the release both revoked.
		letWrite()
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
		if got := d.rec.fencedSteps(); !slices.Equal(got, []string{"activate"}) {
			t.Errorf("the steps refused for the release: %q, want b's activation's alone, nothing opened for c", got)
		}
		if v := d.k.View(); v.Opens != 2 || v.Open || !silence(d.k.Given()[v.OpenAt[1]:]) {
			t.Errorf("after the release: %d opens, open %v; want a's and b's opens alone, every node closed, b's node given its prefill's silence alone", v.Opens, v.Open)
		}
		if !d.files.firstClosed("b") {
			t.Errorf("b's decoder and content are open once the release was acknowledged")
		}
		if s := d.eng.Snapshot(); len(s.Faults()) != 0 || count(d.rec.events(), "fault", 0) != 0 {
			t.Errorf("the release faulted: %+v; events %s", s.Faults(), kinds(d.rec.events()))
		}
	})
	t.Run("a revocation of the session's DSD", func(t *testing.T) {
		w := newWriteSeams()
		d, oc, letAdmit := droppedAtAdmission(t, w)
		fault, letFault := d.rec.holdNextFault()
		t.Cleanup(letFault)
		d.be.Revoke(engine.RevokeInput{Session: d.sessionOf(), Reason: "a test's revocation beside b's drop"})
		// The revocation's fault is held short of the engine, which releases
		// nothing meanwhile: b's admission answers, and the activation answers
		// the sink's own fence, the revocation's and the drop's both.
		awaitHeld(t, "the revocation's fault, held short of the engine", fault)
		letAdmit()
		d.k.Await(t, "b's node closed by its sink once its activation answered", func(v alsapcm.KernelView) bool { return v.Closes >= 2 })
		letFault()
		d.stoppedOrOpenedC("the revocation's fault did not stand")
		s := d.waitEnded()
		d.rig.stop()
		if f := s.Faults(); len(f) != 1 || f[0].Reason != "native_revoked" {
			t.Errorf("faults %+v; want the revocation's alone", f)
		}
		if v := d.k.View(); v.Opens != 2 || v.GivenBytes != v.OpenAt[1] {
			t.Errorf("%d opens, %d bytes given to b's node; want nothing opened for c, b's node given nothing", v.Opens, v.GivenBytes-v.OpenAt[1])
		}
		if w.activatedOf(oc) {
			t.Errorf("c's activation began in a session whose DSD was revoked")
		}
	})
	t.Run("the device's refusal at the admission", func(t *testing.T) {
		w := newWriteSeams()
		d, oc, letAdmit := droppedAtAdmission(t, w)
		d.unit.refuseNext(1)
		letAdmit()
		d.stoppedOrOpenedC("the device's refusal of b's node was taken back as the drop's")
		s := d.waitEnded()
		d.rig.stop()
		if f := s.Faults(); len(f) != 1 || f[0].Reason != engine.ReasonDeviceError || !strings.Contains(f[0].Detail, "card 5 is another unit now") {
			t.Errorf("faults %+v; want the admission's refusal, a device_error", f)
		}
		if v := d.k.View(); v.Opens != 2 || v.GivenBytes != v.OpenAt[1] {
			t.Errorf("%d opens, %d bytes given to b's node; want nothing opened for c, b's node given nothing", v.Opens, v.GivenBytes-v.OpenAt[1])
		}
		if w.activatedOf(oc) {
			t.Errorf("c's activation began once the device refused b's node: the refusal was taken back as the drop's")
		}
	})
	t.Run("the activation's node's close failing beside the drop", func(t *testing.T) {
		// B728 B9 round 5, point 2: b's activation answers the sink's own fence
		// and b is taken back, and its node's DROP fails as the sink lets it go:
		// the take-back waits for the sink's end (its Done), whose unconfirmed
		// release is the session's fault, and nothing opens for c.
		w := newWriteSeams()
		d, oc, letAdmit := droppedAtAdmission(t, w)
		d.k.FailDrops()
		letAdmit()
		d.stoppedOrOpenedC("b was taken back past a close its sink could not confirm")
		s := d.waitEnded()
		d.rig.stop()
		if f := s.Faults(); len(f) != 1 || f[0].Reason != engine.ReasonDeviceError || !strings.Contains(f[0].Detail, "release unconfirmed") {
			t.Errorf("faults %+v; want the unconfirmed release of b's node, a device_error", f)
		}
		if v := d.k.View(); v.Opens != 2 || v.GivenBytes != v.OpenAt[1] {
			t.Errorf("%d opens, %d bytes given to b's node; want nothing opened for c, b's node given nothing", v.Opens, v.GivenBytes-v.OpenAt[1])
		}
		if w.activatedOf(oc) {
			t.Errorf("c's activation began beside a node whose close was unconfirmed")
		}
	})
	t.Run("the reopened node's close failing beside the drop", func(t *testing.T) {
		// B728 B9 round 5, point 2, the review's sequence: b's sink takes the
		// session's pause before it is adopted and lets its node go once the
		// pause has lasted 30 s (the producer meanwhile hands it all of b, and
		// waits to drain it for c's transport); the resume reopens the node (the
		// sink's own activation, ActivatePaused, answered to the resume, never to
		// the producer's transition); b's entry is deleted while the reopened
		// node's check after open is held, which drops b and revokes its sink,
		// and the node's DROP fails as the fenced reopen lets it go, so the sink
		// answers its own fence joined with ErrReleaseUnconfirmed. The resume is
		// refused, the sink's end is the session's fault, and nothing opens for
		// c.
		w := newWriteSeams()
		d := newDSDRig(t, playbacktest.ChoiceState(dsdstate.ModeNative), func(o *rigOptions) { o.seams = w.seams(nil) })
		d.files.add("a", dsd(playbacktest.DSD64, 0.25))
		d.files.add("b", dsd(playbacktest.DSD128, 0.25))
		d.files.add("c", dsd(playbacktest.DSD64, 0.25))
		ids := d.insert("a", "b", "c")
		d.play()
		oa, ob := d.firstOcc(), d.occurrenceOf(ids[1])
		opened, letOpen := d.unit.holdNextAdmit()
		t.Cleanup(letOpen)
		d.k.Free()
		awaitHeld(t, "the check after open of b's node", opened)
		if err := d.eng.Pause(bg); err != nil {
			t.Fatalf("a pause while b's sink opens: %v", err)
		}
		d.waitSnap("paused", func(s *engine.Snapshot) bool { return s.State() == engine.Paused })
		letOpen()
		// A paused sink's waits take granted time alone, free or not: 31 s of
		// it, and b's paused sink lets its node go at 30.
		d.k.Grant(31 * time.Second)
		d.k.Await(t, "b's paused sink letting its node go", func(v alsapcm.KernelView) bool { return v.Opens == 2 && v.Closes == 2 && !v.Open })
		d.k.Hold()
		reopen, letReopen := d.unit.holdNextAdmit()
		t.Cleanup(letReopen)
		d.k.FailDrops()
		resumed := make(chan error, 1)
		go func() { resumed <- d.eng.Play(bg) }()
		awaitHeld(t, "the check after the resume's reopen of b's node", reopen)
		if err := d.eng.Delete(bg, ids[1]); err != nil {
			t.Fatalf("deleting b's entry: %v", err)
		}
		// The edit reaches the backend as a cut of b (its frames are in its
		// sink, so the engine cuts it) or as a Prepare after a that names
		// another occurrence: either drops b and revokes its sink. The call is
		// what is waited for: a Prepare naming c came before the edit, once b
		// was all in its sink, so a wait for a Prepare naming another than b
		// found that one, and the reopen went on before b was dropped.
		e := d.waitCall("the edit that deletes b's entry reaching the backend", func(c call) bool {
			return (c.kind == "cut" && c.from == ob) || (c.kind == "prepare" && c.after == oa && (c.next == nil || c.next.ID != ob))
		})
		if e.err != nil || (e.kind == "cut" && !e.ok) {
			t.Fatalf("the edit that deletes b's entry reached the backend as a %s answered %v %v; want b dropped", e.kind, e.ok, e.err)
		}
		letReopen()
		select {
		case err := <-resumed:
			if err == nil {
				t.Errorf("the resume whose reopened node's close was unconfirmed was taken: want it refused")
			}
		case <-time.After(alsapcm.Watchdog()):
			alsapcm.LogGoroutines(t)
			t.Fatalf("the resume was not answered within %v", alsapcm.Watchdog())
		}
		// The reporter reads b's sink as it ended, woken by its end or measuring
		// at a tick, and the fault stops playback; the engine then holds the
		// device until the late release's acknowledgement, the node's close
		// unconfirmed.
		s := d.tickUntilStopped(engine.ReasonFault)
		d.rig.stop()
		if f := s.Faults(); len(f) != 1 || f[0].Reason != engine.ReasonDeviceError || !strings.Contains(f[0].Detail, "release unconfirmed") {
			t.Errorf("faults %+v; want the unconfirmed release of b's reopened node, a device_error", f)
		}
		if v := d.k.View(); v.Opens != 3 || v.GivenBytes != v.OpenAt[2] {
			t.Errorf("opens at bytes %v of %d given; want a's, b's and its reopen alone, the reopened node given nothing", v.OpenAt, v.GivenBytes)
		}
	})
}
