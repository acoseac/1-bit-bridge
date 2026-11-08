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

// A reconciliation that revokes a session's DSD at the end of an occurrence's
// own sink's activation (B728 B9 round 7), through the engine's Run, the
// backend and the real sinks over the fake kernel: a session's first
// occurrence whose activation succeeded before the revocation was recorded
// fails its Start, naming the revocation, before anything is launched; and a
// converted successor's PCM sink, which the reconciliation does not fence, is
// given nothing whether the revocation comes at its admission or at its
// activation's last step.

// installHold holds, once armed, the backend's note that an occurrence's
// sink installed it (playback.Config.Installed), on the goroutine that
// activates it: the last step of a successful activation, before the
// activation returns, the occurrence still claimed.
type installHold struct {
	mu  sync.Mutex
	any bool
	o   engine.OccurrenceID
	g   *gate
}

// armNext holds the next occurrence's install, whichever it is.
func (h *installHold) armNext(t *testing.T) *gate {
	g := newGate(t)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.any, h.g = true, g
	return g
}

// arm holds o's install.
func (h *installHold) arm(t *testing.T, o engine.OccurrenceID) *gate {
	g := newGate(t)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.any, h.o, h.g = false, o, g
	return g
}

// note is the rig's installed (rigOptions.installed).
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

// startedOrStopped waits for the Start's answer or playback stopped: a
// session that faults before its Start answers is released, and the engine
// then takes no answer.
func (d *dsdRig) startedOrStopped() {
	d.t.Helper()
	d.waitSnap("the Start's answer, or playback stopped", func(s *engine.Snapshot) bool {
		return s.LastAttempt().Seq > 0 || s.State() == engine.Stopped && s.LastStop().Reason != ""
	})
}

// TestARevocationOnceTheFirstOccurrencesSinkActivatedFailsItsStart (B728 B9
// round 7, both reviews of round 6): a session's first occurrence is claimed
// until its Start has taken its activation's answer, and a reconciliation
// that revokes the session's DSD in that window records its fault and fences
// the sink, which may have activated already. The Start then fails whether or
// not its activation did, naming the revocation, and launches nothing: here
// the activation succeeds (its install held, the node given its prefill), the
// revocation is recorded and the sink fenced while it is held, and the
// session's next fault is held short of the engine so the Start's answer is
// taken first. Until round 7 the Start read the fault only beside an
// activation's error: it launched its producer, which stopped before its
// first frames, and its reporter, and the engine took the Start's failure in
// generic words, the revocation unnamed.
func TestARevocationOnceTheFirstOccurrencesSinkActivatedFailsItsStart(t *testing.T) {
	w, log, inst := newWriteSeams(), &stopsLog{}, &installHold{}
	d := newDSDRigWith(t, playbacktest.ChoiceState(dsdstate.ModeNative), func(p *playback.DSDPolicy) { p.Log = log },
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
	res := d.choose(choiceReq(t, d, dsdstate.ModeNative, 1, capAt(playbacktest.DSD64), goAhead))
	reachedGate(t, stops, res, "the reconciliation's revocation of the session's DSD, before its call to Revoke")
	installed.letGo()
	d.startedOrStopped()
	stops.letGo()
	if got := answer(t, res); got.Outcome != playback.ChoiceWritten {
		t.Fatalf("the cap with the go-ahead: %s, want written", got.Outcome)
	}
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

// convertedRig is a rig, its seams w's and its installs inst's, whose DSD
// state converts every rate, its engine handing nothing off again: a (PCM),
// then b (DSD128) and c (DSD64), each converted to the PCM a transport of its
// own after a's. It answers the entries.
func convertedRig(t *testing.T, w *writeSeams, inst *installHold, log *stopsLog) (*dsdRig, []uint32) {
	t.Helper()
	d := newDSDRigWith(t, playbacktest.ChoiceState(dsdstate.ModeConvert), func(p *playback.DSDPolicy) { p.Log = log }, func(o *rigOptions) {
		o.seams, o.noReplan, o.installed = w.seams(nil), true, inst.note
	})
	d.files.add("a", wav(44100, 16, pattern(1, 4410, 16)))
	d.files.add("b", dsd(playbacktest.DSD128, 0.25))
	d.files.add("c", dsd(playbacktest.DSD64, 0.25))
	ids := d.insert("a", "b", "c")
	d.play()
	return d, ids
}

// TestARevocationOfAConvertedSuccessorGivesItsNodeNothing (B728 B9 round 7,
// Claude's review of round 6): the reconciliation does not fence a converted
// successor's own PCM sink, and its node is given nothing all the same. A PCM
// sink admits its node at its open and at a resume's reopen, never in an
// activation, and writes nothing of its own (it has no prefill): only the
// producer writes it, from its next step. Held at that admission, at its
// open, the occurrence has no output adopted: the reconciliation records no
// fault and marks it stale, its sink's adoption closes the sink unwritten,
// and Revoke drops it, refused native_revoked, nothing faulted. Held at its
// activation's last step, its install, the occurrence claimed with its sink
// adopted: the reconciliation records the revocation, once. Either way, once
// the rig has stopped, b's node was given nothing and every node is closed.
// (The second lets the reconciliation go on to Revoke as soon as the install
// is let go, so Revoke's drop of the PCM sink can come before the producer's
// next step: it does not tell that step's own stop on the recorded fault, which
// the code makes before it writes, from that drop; round 7's control K3.)
func TestARevocationOfAConvertedSuccessorGivesItsNodeNothing(t *testing.T) {
	t.Run("held at its PCM sink's admission", func(t *testing.T) {
		w, log, inst := newWriteSeams(), &stopsLog{}, &installHold{}
		d, ids := convertedRig(t, w, inst, log)
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
		res := d.choose(choiceReq(t, d, dsdstate.ModeOff, 1, goAhead))
		reachedGate(t, stops, res, "the reconciliation's revocation of the session's DSD, before its call to Revoke")
		letAdmit()
		d.closedOrGiven(ob, "b's sink closed by its adoption, b stale")
		stops.letGo()
		if got := answer(t, res); got.Outcome != playback.ChoiceWritten {
			t.Fatalf("DSD off with the go-ahead: %s, want written", got.Outcome)
		}
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
		w, log, inst := newWriteSeams(), &stopsLog{}, &installHold{}
		d, ids := convertedRig(t, w, inst, log)
		ob := d.occurrenceOf(ids[1])
		installed := inst.arm(t, ob)
		d.k.Free()
		awaitHeld(t, "b's own PCM sink installing it, its activation succeeded", installed.in)
		d.k.Hold()
		_, letFault := d.rec.holdNextFault()
		t.Cleanup(letFault)
		stops := newGate(t)
		log.arm(stops)
		res := d.choose(choiceReq(t, d, dsdstate.ModeOff, 1, goAhead))
		reachedGate(t, stops, res, "the reconciliation's revocation of the session's DSD, before its call to Revoke")
		installed.letGo()
		stops.letGo()
		if got := answer(t, res); got.Outcome != playback.ChoiceWritten {
			t.Fatalf("DSD off with the go-ahead: %s, want written", got.Outcome)
		}
		letFault()
		d.stoppedOrOpenedC("the revocation's fault did not stop the session before c")
		s := d.tickUntilStopped(engine.ReasonFault)
		d.rig.stop()
		wantRevoked(t, d, s, log)
		wantUnwritten(t, d, 2, "b's")
	})
}
