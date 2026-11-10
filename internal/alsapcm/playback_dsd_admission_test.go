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

func (u *unit) checks() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.checked)
}

func (u *unit) refuseNext(n int) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.refuse = n
}

func (w *lifecycleSeams) activatedOf(o engine.OccurrenceID) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Contains(w.activated, o)
}

func heldAdmission(t *testing.T, w *lifecycleSeams) (*dsdRig, []uint32, engine.OccurrenceID, func()) {
	t.Helper()
	d := newDSDRig(t, initialPolicy(dsdstate.ModeNative), func(o *rigOptions) { o.seams = w.seams(nil) })
	ids, ob, letAdmit := heldAdmissionOn(t, d, w)
	return d, ids, ob, letAdmit
}

func heldAdmissionOn(t *testing.T, d *dsdRig, w *lifecycleSeams) ([]uint32, engine.OccurrenceID, func()) {
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

func TestASuccessorDroppedAtItsSinksAdmissionIsTakenBackUnwritten(t *testing.T) {
	t.Run("b's entry deleted", func(t *testing.T) {
		w := newLifecycleSeams()
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
	t.Run("hotplug replaces b", func(t *testing.T) {
		w := newLifecycleSeams()
		d, ids, ob, letAdmit := heldAdmission(t, w)
		refreshed(t, d.refreshAfterHotplug(false))
		d.replaced(ob, "b's replacement once the cap landed")
		letAdmit()
		s := wentOn(t, d, ids)
		if _, ok := s.Skipped(ids[1]); !ok {
			t.Errorf("b's entry was not skipped: the cap refuses its DSD128; skips %+v", s.Skips())
		}
	})
	t.Run("hotplug replans b before its replacement arrives", func(t *testing.T) {
		w := newLifecycleSeams()
		d, ids, ob, letAdmit := heldAdmission(t, w)
		prepare, letPrepare := d.rec.holdNextPrepare()
		t.Cleanup(letPrepare)
		refreshed(t, d.refreshAfterHotplug(false))
		awaitHeld(t, "the engine's replacement of b, held short of the backend", prepare)
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
		w := newLifecycleSeams()
		d, ids, ob, letAdmit := heldAdmission(t, w)
		if ok, err := d.be.Cut(bg, d.sessionOf(), ob); !ok || err != nil {
			t.Fatalf("the cut of b, none of it written: %v %v, want true", ok, err)
		}
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

func prefillRig(t *testing.T, w *lifecycleSeams, park bool) (*dsdRig, []uint32, engine.OccurrenceID, func()) {
	t.Helper()
	d := newDSDRigWith(t, initialPolicy(dsdstate.ModeNative), func(p *playback.DSDPolicy) {
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

func silence(b []byte) bool { return len(b) > 0 && bytes.Equal(b, bytes.Repeat([]byte{0x69}, len(b))) }

func TestASuccessorDroppedWhileItsSinkWritesItsPrefillIsTakenBack(t *testing.T) {
	w := newLifecycleSeams()
	d, ids, ob, _ := prefillRig(t, w, false)
	if err := d.eng.Delete(bg, ids[1]); err != nil {
		t.Fatalf("deleting b's entry: %v", err)
	}
	d.replaced(ob, "c handed off in b's place")
	wentOnGiven(t, d, ids, "its prefill's DSD silence alone", silence)
}

func (d *dsdRig) stoppedOrOpenedC(why string) {
	d.t.Helper()
	stopped := func() bool { s := d.eng.Snapshot(); return s.State() == engine.Stopped && s.LastStop().Reason != "" }
	waitOnEither(d.t, "playback to stop, or c's own sink to open", d.eng.Changed, func() <-chan struct{} { return d.k.When(func(v alsapcm.KernelView) bool { return v.Opens >= 3 }) },
		func() bool { return d.k.View().Opens >= 3 || stopped() })
	if v := d.k.View(); v.Opens >= 3 {
		d.t.Fatalf("c's own sink opened (%d opens), the session gone on past b: %s", v.Opens, why)
	}
}

func droppedAtAdmission(t *testing.T, w *lifecycleSeams) (*dsdRig, engine.OccurrenceID, func()) {
	t.Helper()
	d, ids, ob, letAdmit := heldAdmission(t, w)
	if err := d.eng.Delete(bg, ids[1]); err != nil {
		t.Fatalf("deleting b's entry: %v", err)
	}
	d.replaced(ob, "c handed off in b's place")
	return d, d.occurrenceOf(ids[2]), letAdmit
}

func TestAnActivationsFailureBesideADropIsJudgedByItsCause(t *testing.T) {
	t.Run("a release", func(t *testing.T) {
		w := newLifecycleSeams()
		d, oc, letAdmit := droppedAtAdmission(t, w)
		rel := w.holdReleasing(t)
		stopped := make(chan error, 1)
		go func() { stopped <- d.eng.Stop(bg) }()
		awaitHeld(t, "the session's release", rel.in)
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
		w := newLifecycleSeams()
		d, ids, ob, letWrite := prefillRig(t, w, true)
		if err := d.eng.Delete(bg, ids[1]); err != nil {
			t.Fatalf("deleting b's entry: %v", err)
		}
		d.replaced(ob, "c handed off in b's place")
		rel := w.holdReleasing(t)
		stopped := make(chan error, 1)
		go func() { stopped <- d.eng.Stop(bg) }()
		awaitHeld(t, "the session's release", rel.in)
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
		w := newLifecycleSeams()
		d, oc, letAdmit := droppedAtAdmission(t, w)
		fault, letFault := d.rec.holdNextFault()
		t.Cleanup(letFault)
		d.be.Revoke(engine.RevokeInput{Session: d.sessionOf(), Reason: "a test's revocation beside b's drop"})
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
		w := newLifecycleSeams()
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
		w := newLifecycleSeams()
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
		w := newLifecycleSeams()
		d := newDSDRig(t, initialPolicy(dsdstate.ModeNative), func(o *rigOptions) { o.seams = w.seams(nil) })
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
