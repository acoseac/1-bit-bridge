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
	d, ids, ob, act := heldSuccessor(t, w)
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
	return d, ids, ob, letAdmit
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
		if err := d.eng.Delete(bg, ids[1]); err != nil {
			t.Fatalf("deleting b's entry: %v", err)
		}
		d.replaced(ob, "c handed off in b's place")
		letAdmit()
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
}
