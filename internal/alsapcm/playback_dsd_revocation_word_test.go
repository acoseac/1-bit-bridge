package alsapcm_test

import (
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/player/dsdstate"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/format"
	"github.com/acoseac/1-bit-bridge/internal/player/playback/playbacktest"
)

// TestADSDEntryAfterAnOrdinaryRevocationIsRefusedForIt (backlog B853): a
// prepared DSD successor revoked while the PCM before it plays on stops the
// session's DSD (Backend.Revoke), and a DSD entry the queue reaches after
// that, past a PCM entry, is refused with the revocation's own word and
// reason, native_revoked, as Revoke words the successor it drops: never as
// a policy that did not read (dsd_dac_unread), since the set did read, and
// still plays that entry's rate. The rig is
// TestARevocationOfAPreparedSuccessorLetsThePCMPlayOn's, its queue extended
// past b with a PCM entry and then a DSD64 one.
func TestADSDEntryAfterAnOrdinaryRevocationIsRefusedForIt(t *testing.T) {
	w, log := newLifecycleSeams(), &stopsLog{}
	d := revokeRig(t, initialPolicy(dsdstate.ModeNative), w, &heldGate{}, log)
	p, c := pattern(1, 22050, 16), pattern(3, 22050, 16)
	d.files.add("p", wav(44100, 16, p))
	d.files.add("b", dsd(playbacktest.DSD128, 0.25))
	d.files.add("c", wav(44100, 16, c))
	d.files.add("d", dsd(playbacktest.DSD64, 0.25))
	ids := d.insert("p", "b", "c", "d")
	d.play()
	ob := d.preparedOcc(ids[1])
	refreshed(t, d.refreshAfterHotplug(false))
	d.waitEvents("b refused once the session's DSD was revoked", func(evs []event) bool { return count(evs, "refused", ob) > 0 })
	plays64 := false
	for _, tu := range d.be.Policy().Tuples {
		plays64 = plays64 || tu.Tuple.BitRate == playbacktest.DSD64
	}
	if !plays64 {
		t.Fatalf("the set published after the hotplug %+v plays no DSD64: the case needs a set that read, and plays d's rate", d.be.Policy().Tuples)
	}
	d.k.Free()
	s := d.waitStopped(engine.ReasonEndOfQueue)
	d.rig.stop()
	why, evs := log.reasons(), d.rec.events()
	if len(why) != 1 {
		t.Fatalf("the session's DSD was revoked for %q, want once", why)
	}
	if sk, ok := s.Skipped(ids[3]); !ok || sk.Reason != "native_revoked" || sk.Detail != why[0] {
		t.Errorf("d's entry was %+v (skipped %v); want it refused native_revoked, for the revocation's reason %q", sk, ok, why[0])
	}
	if len(s.Faults()) != 0 || count(evs, "fault", 0) != 0 {
		t.Errorf("the revocation faulted the PCM: %+v; events %s", s.Faults(), kinds(evs))
	}
	if n := len(d.openFormats()); n != 0 {
		t.Errorf("%d native or DoP sinks were opened; want none, the session's DSD revoked before any of it played", n)
	}
	// p's sink may have drained for b's transport before b was dropped, and c
	// then opens one of its own: the DAC hears p and then c either way.
	same(t, "the DAC heard", unpack(t, d.k.Heard(), format.S16LE, 16), cat(p, c))
}
