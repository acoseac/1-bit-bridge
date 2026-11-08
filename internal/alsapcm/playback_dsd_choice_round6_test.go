package alsapcm_test

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/player/dsdstate"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/format"
	"github.com/acoseac/1-bit-bridge/internal/player/playback"
	"github.com/acoseac/1-bit-bridge/internal/player/playback/playbacktest"
)

// A reconciliation that revokes a session's DSD while an occurrence's own
// sink activates (B728 B9 round 6), through the engine's Run, the backend and
// the real sinks over the fake kernel: the revocation's fault is recorded in
// the reconciliation's own step, before the occurrence is marked stale, and
// the native sink its activation is under way for is fenced there, so no
// order of the activation's answer, its take-back and Backend.Revoke loses the
// fault or lets the sink write.

// heldGate holds a seam's next caller once armed: the Activating seam's,
// which writeSeams does not hold (B728 B9 round 6).
type heldGate struct {
	mu   sync.Mutex
	next *gate
}

// arm holds the seam's next caller at g.
func (h *heldGate) arm(g *gate) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.next = g
}

// pass is the seam.
func (h *heldGate) pass() {
	h.mu.Lock()
	g := h.next
	h.next = nil
	h.mu.Unlock()
	if g != nil {
		g.pass()
	}
}

// revokeRig is a rig whose DSD state is st, its seams w's with the Activating
// seam hold's, its policy's log log, and whose engine hands nothing off again
// (no engine.ReplanEvents): a change of the set that takes a successor's plan
// away revokes the session's DSD.
func revokeRig(t *testing.T, st dsdstate.State, w *writeSeams, hold *heldGate, log *stopsLog) *dsdRig {
	t.Helper()
	return newDSDRigWith(t, st, func(p *playback.DSDPolicy) { p.Log = log }, func(o *rigOptions) {
		o.seams, o.noReplan = w.seams(nil), true
		o.seams.Activating = hold.pass
	})
}

// wantRevoked fails t unless the session faulted once, native_revoked, for
// the reason the reconciliation gave Backend.Revoke (its line saying the
// session's DSD stops), and the backend reported that one fault once.
func wantRevoked(t *testing.T, d *dsdRig, s *engine.Snapshot, log *stopsLog) {
	t.Helper()
	why := log.reasons()
	if f := s.Faults(); len(f) != 1 || f[0].Reason != "native_revoked" || len(why) != 1 || f[0].Detail != why[0] {
		t.Errorf("faults %+v, the revocations' reasons %q; want one fault, native_revoked, for the one reason Backend.Revoke was given", f, why)
	}
	if evs := d.rec.events(); count(evs, "fault", 0) != 1 {
		t.Errorf("the backend reported %d faults: %s; want the revocation's, once", count(evs, "fault", 0), kinds(evs))
	}
}

// wantUnwritten fails t unless, the rig stopped, the node was opened opens
// times, the last open's node, whose, was given nothing, and every node opened
// is closed.
func wantUnwritten(t *testing.T, d *dsdRig, opens int, whose string) {
	t.Helper()
	v := d.k.View()
	if len(v.OpenAt) != opens || v.GivenBytes != v.OpenAt[len(v.OpenAt)-1] || v.Open || v.Closes != v.Opens {
		t.Errorf("once the rig stopped: opens at bytes %v, %d bytes given, open %v, %d of %d opens closed; want %d opens, %s node given nothing, every node closed",
			v.OpenAt, v.GivenBytes, v.Open, v.Closes, v.Opens, opens, whose)
	}
}

// parkedOrGiven waits until parked, a node's close held in the kernel, is
// closed (true) or b's node, the second opened, has been given a byte
// (false), whichever comes first: it orders a test's steps, and what the node
// was given is read once the rig has stopped.
func (d *dsdRig) parkedOrGiven(parked <-chan struct{}, what string) bool {
	d.t.Helper()
	given := d.k.When(func(v alsapcm.KernelView) bool { return len(v.OpenAt) >= 2 && v.GivenBytes > v.OpenAt[1] })
	select {
	case <-parked:
		return true
	case <-given:
		return false
	case <-time.After(alsapcm.Watchdog()):
		alsapcm.LogGoroutines(d.t)
		d.t.Fatalf("waited %v for %s", alsapcm.Watchdog(), what)
	}
	return false
}

// revokedAtTheCheck runs Codex's sequence of round 5's review on d, whose
// successor ob's activation act holds as it begins, its own sink adopted and
// the DAC held: b's activation let go and held again at its check, once its
// read of the policy has returned (the Activating seam, hold); the session's
// next fault held short of the engine, which releases nothing meanwhile; req
// written with the go-ahead, its reconciliation held at its line saying the
// session's DSD stops, before Backend.Revoke; b's check let go, which finds b
// stale and answers errReplanned, so b is taken back from its sink while the
// session's DSD is not revoked yet, its sink closed and noted so; and only then
// the reconciliation goes on to Revoke. It fails t where the session went on
// to c's own sink, and answers the snapshot once playback stopped for a fault,
// the rig stopped.
func revokedAtTheCheck(t *testing.T, d *dsdRig, act *gate, hold *heldGate, log *stopsLog, ob engine.OccurrenceID, req playback.ChoiceRequest) *engine.Snapshot {
	t.Helper()
	checking := newGate(t)
	hold.arm(checking)
	act.letGo()
	awaitHeld(t, "b's activation's check, its read of the policy returned", checking.in)
	_, letFault := d.rec.holdNextFault()
	t.Cleanup(letFault)
	stops := newGate(t)
	log.arm(stops)
	res := d.choose(req)
	reachedGate(t, stops, res, "the reconciliation's revocation of the session's DSD, before its call to Revoke")
	checking.letGo()
	d.closedOrGiven(ob, "b's check answered and b taken back")
	stops.letGo()
	if got := answer(t, res); got.Outcome != playback.ChoiceWritten {
		t.Fatalf("the write with the go-ahead: %s, want written", got.Outcome)
	}
	letFault()
	d.stoppedOrOpenedC("the revocation's fault was not the session's before b was taken back")
	s := d.tickUntilStopped(engine.ReasonFault)
	d.rig.stop()
	return s
}

// TestARevocationAtASuccessorsActivationCheckKeepsItsFault (B728 B9 round
// 6, Codex's review of round 5): where the engine hands nothing off again, a
// write that takes a successor's plan away revokes the session's DSD, and its
// fault is recorded in the reconciliation's own step, before the successor is
// marked stale, so the successor's activation check, which finds it stale and
// answers errReplanned, and its take-back cannot come before the fault: until
// round 6 they did, Backend.Revoke then found no open sink and recorded none,
// and the session went on with nothing faulted: to the end of its queue, the
// DSD after b refused, or to the PCM after it, whose own sink opened. A native
// successor's and a converted one's (its own PCM sink, a transport of its own
// after PCM, B728 B11) alike: the session stops native_revoked, once, for the
// reason Revoke is given, before anything more opens, and once the rig has
// stopped b's node was given nothing and every node is closed.
func TestARevocationAtASuccessorsActivationCheckKeepsItsFault(t *testing.T) {
	t.Run("a native successor", func(t *testing.T) {
		w, hold, log := newWriteSeams(), &heldGate{}, &stopsLog{}
		d := revokeRig(t, playbacktest.ChoiceState(dsdstate.ModeNative), w, hold, log)
		_, ob, act := heldSuccessorOn(t, d, w)
		s := revokedAtTheCheck(t, d, act, hold, log, ob, choiceReq(t, d, dsdstate.ModeNative, 1, capAt(playbacktest.DSD64), goAhead))
		wantRevoked(t, d, s, log)
		wantUnwritten(t, d, 2, "b's")
	})
	t.Run("a native successor, PCM after it", func(t *testing.T) {
		w, hold, log := newWriteSeams(), &heldGate{}, &stopsLog{}
		d := revokeRig(t, playbacktest.ChoiceState(dsdstate.ModeNative), w, hold, log)
		d.files.add("a", dsd(playbacktest.DSD64, 0.25))
		d.files.add("b", dsd(playbacktest.DSD128, 0.25))
		d.files.add("c", wav(44100, 16, pattern(1, 4410, 16)))
		ids := d.insert("a", "b", "c")
		d.play()
		ob := d.occurrenceOf(ids[1])
		act := w.holdActivation(t, ob)
		d.k.Free()
		awaitHeld(t, "b's activation, its sink adopted", act.in)
		d.k.Hold()
		s := revokedAtTheCheck(t, d, act, hold, log, ob, choiceReq(t, d, dsdstate.ModeNative, 1, capAt(playbacktest.DSD64), goAhead))
		wantRevoked(t, d, s, log)
		wantUnwritten(t, d, 2, "b's")
	})
	t.Run("a converted successor", func(t *testing.T) {
		w, hold, log := newWriteSeams(), &heldGate{}, &stopsLog{}
		d := revokeRig(t, playbacktest.ChoiceState(dsdstate.ModeConvert), w, hold, log)
		d.files.add("a", wav(44100, 16, pattern(1, 4410, 16)))
		d.files.add("b", dsd(playbacktest.DSD128, 0.25))
		d.files.add("c", dsd(playbacktest.DSD64, 0.25))
		ids := d.insert("a", "b", "c")
		d.play()
		ob := d.occurrenceOf(ids[1])
		act := w.holdActivation(t, ob)
		d.k.Free()
		awaitHeld(t, "b's activation, its own PCM sink adopted", act.in)
		d.k.Hold()
		s := revokedAtTheCheck(t, d, act, hold, log, ob, choiceReq(t, d, dsdstate.ModeOff, 1, goAhead))
		wantRevoked(t, d, s, log)
		wantUnwritten(t, d, 2, "b's")
	})
}

// TestARevocationThatRunsDuringATakeBackKeepsItsOneFault (B728 B9 round 6):
// the other order of Codex's. b's sink's activation answers the fence the
// reconciliation's own step set, and b is taken back: the take-back marks its
// output closed and waits on the sink's end, held here at the node's close in
// the kernel, while the reconciliation's line and Backend.Revoke run first,
// the node still open. Revoke finds the output closed and adds no fault, and
// the reconciliation's stands: the session stops native_revoked, once, for the
// reason Revoke is given, nothing more opens, and once the rig has stopped b's
// node was given nothing and every node is closed.
func TestARevocationThatRunsDuringATakeBackKeepsItsOneFault(t *testing.T) {
	w, log := newWriteSeams(), &stopsLog{}
	d := revokeRig(t, playbacktest.ChoiceState(dsdstate.ModeNative), w, &heldGate{}, log)
	_, _, letAdmit := heldAdmissionOn(t, d, w)
	_, letFault := d.rec.holdNextFault()
	t.Cleanup(letFault)
	closing, letClose := d.k.ParkNextClose()
	t.Cleanup(letClose)
	stops := newGate(t)
	log.arm(stops)
	res := d.choose(choiceReq(t, d, dsdstate.ModeNative, 1, capAt(playbacktest.DSD64), goAhead))
	reachedGate(t, stops, res, "the reconciliation's revocation of the session's DSD, before its call to Revoke")
	letAdmit()
	taken := d.parkedOrGiven(closing, "b's node's close as b is taken back, or a byte given to b's node")
	stops.letGo()
	if got := answer(t, res); got.Outcome != playback.ChoiceWritten {
		t.Fatalf("the cap with the go-ahead: %s, want written", got.Outcome)
	}
	if v := d.k.View(); taken && !v.Open {
		t.Fatalf("b's node was closed before Backend.Revoke returned: the take-back was not held")
	}
	letClose()
	letFault()
	s := d.tickUntilStopped(engine.ReasonFault)
	d.rig.stop()
	wantRevoked(t, d, s, log)
	wantUnwritten(t, d, 2, "b's")
}

// TestARevocationOfAPreparedSuccessorLetsThePCMPlayOn (B728 B9 round 6, what
// Backend.Revoke keeps): a write that takes away the plan of a DSD successor
// prepared behind PCM, its open done and no sink of its own, revokes the
// session's DSD through the reconciliation where the engine hands nothing off
// again, and the reconciliation records no fault for it: it has no output, so
// Revoke drops it, refused native_revoked, and the PCM plays on, every frame
// of it heard, nothing faulted and nothing more opened.
func TestARevocationOfAPreparedSuccessorLetsThePCMPlayOn(t *testing.T) {
	w, log := newWriteSeams(), &stopsLog{}
	d := revokeRig(t, playbacktest.ChoiceState(dsdstate.ModeNative), w, &heldGate{}, log)
	pcm := pattern(1, 22050, 16)
	d.files.add("p", wav(44100, 16, pcm))
	d.files.add("b", dsd(playbacktest.DSD128, 0.25))
	ids := d.insert("p", "b")
	d.play()
	ob := d.preparedOcc(ids[1])
	if got := answer(t, d.choose(choiceReq(t, d, dsdstate.ModeNative, 1, capAt(playbacktest.DSD64), goAhead))); got.Outcome != playback.ChoiceWritten {
		t.Fatalf("the cap with the go-ahead: %s, want written", got.Outcome)
	}
	d.waitEvents("b refused once the session's DSD was revoked", func(evs []event) bool { return count(evs, "refused", ob) > 0 })
	d.k.Free()
	s := d.waitStopped(engine.ReasonEndOfQueue)
	d.rig.stop()
	evs := d.rec.events()
	if len(s.Faults()) != 0 || count(evs, "fault", 0) != 0 {
		t.Errorf("the revocation of a prepared successor faulted the PCM: %+v; events %s", s.Faults(), kinds(evs))
	}
	if sk, ok := s.Skipped(ids[1]); !ok || sk.Reason != "native_revoked" {
		t.Errorf("b's entry was %+v (skipped %v), want it refused native_revoked", sk, ok)
	}
	if why := log.reasons(); len(why) != 1 {
		t.Errorf("the session's DSD was revoked for %q, want once", why)
	}
	if v := d.k.View(); v.Opens != 1 {
		t.Errorf("%d opens, want the PCM's alone", v.Opens)
	}
	same(t, "the DAC heard", unpack(t, d.k.Heard(), format.S16LE, 16), pcm)
}

// TestARevocationAtTheFirstOccurrencesActivationFencesItsSink (B728 B9 round
// 6): a session's first occurrence is revoked, never handed off again, by a
// change of the set that takes its plan away before any of it played, and its
// own sink's activation in its Start is under way as a successor's is in the
// producer's transition: the reconciliation records the revocation and fences
// the sink in its own step. Here the first occurrence's activation is held at
// its sink's admission and the reconciliation between its judgement and its
// call to Revoke, and the admission answers meanwhile: the activation answers
// the sink's own fence, the node given nothing, and the Start fails saying the
// session's DSD was revoked, for the reason Revoke is given (a native Start's
// failure is the engine's backend_error); until round 6 the activation,
// answered in that window, gave the node its prefill at the rate the write
// had taken away.
func TestARevocationAtTheFirstOccurrencesActivationFencesItsSink(t *testing.T) {
	w, log := newWriteSeams(), &stopsLog{}
	d := newDSDRigWith(t, playbacktest.ChoiceState(dsdstate.ModeNative), func(p *playback.DSDPolicy) { p.Log = log },
		func(o *rigOptions) { o.seams = w.seams(nil) })
	d.files.add("a", dsd(playbacktest.DSD128, 0.25))
	d.insert("a")
	opened, letOpen := d.unit.holdNextAdmit()
	t.Cleanup(letOpen)
	played := make(chan error, 1)
	go func() { played <- d.eng.Play(bg) }()
	awaitHeld(t, "the check after open of a's node", opened)
	admit, letAdmit := d.unit.holdNextAdmit()
	t.Cleanup(letAdmit)
	letOpen()
	awaitHeld(t, "the admission a's sink's activation makes", admit)
	if v := d.k.View(); d.rec.openedSinks() != 1 || v.Opens != 1 || !v.Open || v.GivenBytes != 0 {
		t.Fatalf("the check held: %d sinks noted opened, %d opens, open %v, %d bytes given; want the activation's admission of a's node, open and given nothing",
			d.rec.openedSinks(), v.Opens, v.Open, v.GivenBytes)
	}
	stops := newGate(t)
	log.arm(stops)
	res := d.choose(choiceReq(t, d, dsdstate.ModeNative, 1, capAt(playbacktest.DSD64), goAhead))
	reachedGate(t, stops, res, "the reconciliation's revocation of the session's DSD, before its call to Revoke")
	letAdmit()
	d.waitSnap("the Start's answer", func(s *engine.Snapshot) bool { return s.LastAttempt().Seq > 0 })
	stops.letGo()
	if got := answer(t, res); got.Outcome != playback.ChoiceWritten {
		t.Fatalf("the cap with the go-ahead: %s, want written", got.Outcome)
	}
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
	if count(evs, "fault", 0) != 0 || count(evs, "audible", 0) != 0 {
		t.Errorf("events %s: want none heard and no fault of the session's own, its Start failed", kinds(evs))
	}
	wantUnwritten(t, d, 1, "a's")
}
